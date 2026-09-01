// Copyright 2026 The AgentSessionizer Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package parse drives one round of assembly and publishes it to a
// conversation's round chain.
//
// Each round does the same three things: assemble the whole session from the
// index, compare that against what the chain already says, and write only the
// difference. Assembling everything each time is what makes a round
// reproducible - the same index always produces the same entities - and
// comparing before writing is what keeps a round small.
package parse

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/wu-sheng/AgentSessionizer/internal/assemble"
	"github.com/wu-sheng/AgentSessionizer/internal/index"
	"github.com/wu-sheng/AgentSessionizer/internal/storage"
	"github.com/wu-sheng/AgentSessionizer/pkg/asb"
)

// Parser is the interpretation version written into every round header.
//
// A change here that alters meaning starts a new chain generation. It does not
// retroactively correct earlier rounds and must not be folded together with
// them, so the version travels in the header where a reader can see the mix.
const Parser = "v1"

// Policy is the version of the choices that are settings rather than
// interpretation: the idle gap, the segment gates.
const Policy = "v1"

// Options configures one parse round.
type Options struct {
	Conversation string
	Session      string
	IdleGap      time.Duration

	// Now supplies the clock for chain state. Rounds themselves carry no time,
	// so this never reaches a digest.
	Now func() time.Time

	// Reindex rebuilds a session's index by re-reading its landed files.
	//
	// It is what makes the index genuinely disposable. Without it the index can
	// only be built while collecting, so landed data separated from its source -
	// an archive, a bundle sent to someone else, a restored backup - could be
	// read but never parsed again, even though every byte needed is present.
	//
	// It is a function rather than a direct call because interpreting a landed
	// record is the adapter's job, and the adapter that produced it is named in
	// the file's own header. Parse itself stays free of any runtime's vocabulary.
	Reindex func(z *storage.Zone, session string, ix *index.Index, afterSeq uint64) (int, error)
}

// Round reports what one parse round did.
type Round struct {
	Session      string
	Conversation string

	// Number is 0 when nothing changed and no round was written.
	Number uint64
	Digest string
	Path   string

	FromSeq    uint64
	ThroughSeq uint64

	Nodes      int
	Relations  int
	Unresolved int
	Tombstones int

	Stats assemble.Stats
}

// Changed reports whether the round wrote anything.
func (r *Round) Changed() bool { return r.Number != 0 }

// Session parses one session and appends a round to its conversation chain.
//
// It is safe to call when nothing has changed: the comparison finds no
// difference and no round is written, so a watch loop does not grow the chain
// with empty rounds.
func Session(z *storage.Zone, opt Options) (*Round, error) {
	if opt.Conversation == "" {
		return nil, fmt.Errorf("parse: conversation identity is supplied, never inferred")
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}

	ix, ok, err := index.Load(z.IndexDir(opt.Session), opt.Session)
	if err != nil {
		return nil, err
	}
	if !ok {
		ix, err = rebuild(z, opt)
		if err != nil {
			return nil, err
		}
	}

	// The index must not be read past what it actually covers. Its own state
	// records that; parsing beyond it would describe records the index does not
	// hold.
	ixState, err := storage.LoadIndexState(z.IndexStatePath(opt.Session), opt.Session)
	if err != nil {
		return nil, err
	}

	res, err := assemble.Session(ix, assemble.Options{
		Conversation: opt.Conversation,
		Session:      opt.Session,
		IdleGap:      opt.IdleGap,
	})
	if err != nil {
		return nil, err
	}
	through := res.ThroughSeq
	if ixState.IndexedSeq > 0 && ixState.IndexedSeq < through {
		through = ixState.IndexedSeq
	}

	chain := asb.OpenChain(z.Root(), opt.Conversation)
	// The filesystem is the authority on how far the chain got. A crash between
	// publishing a round and saving state leaves a round on disk that state does
	// not mention, and folding must see it.
	view, err := chain.Fold()
	if err != nil {
		return nil, err
	}
	state, err := chain.LoadState()
	if err != nil {
		return nil, err
	}

	out := &Round{
		Session: opt.Session, Conversation: opt.Conversation,
		FromSeq: view.ThroughSeq + 1, ThroughSeq: through, Stats: res.Stats,
	}
	if through < view.ThroughSeq {
		return nil, fmt.Errorf("parse: the chain covers landed sequence %d but the index only reaches %d",
			view.ThroughSeq, through)
	}

	d := diff(view, res)
	if d.empty() {
		out.ThroughSeq = view.ThroughSeq
		return out, nil
	}

	round := view.Round + 1
	inputDigest, err := inputDigestFor(z, opt.Session, state.InputDigest, view.ThroughSeq, through)
	if err != nil {
		return nil, err
	}

	w, err := asb.NewWriter(asb.Header{
		Conversation: opt.Conversation, Session: opt.Session,
		Round: round, Previous: view.Digest,
		FromSeq: view.ThroughSeq + 1, ThroughSeq: through,
		InputDigest: inputDigest, Parser: Parser, Policy: Policy,
	})
	if err != nil {
		return nil, err
	}
	for _, n := range d.nodes {
		if err := w.Node(n); err != nil {
			return nil, err
		}
	}
	for _, r := range d.relations {
		if err := w.Relation(r); err != nil {
			return nil, err
		}
	}
	for _, u := range d.unresolved {
		if err := w.Unresolved(u); err != nil {
			return nil, err
		}
	}
	data, digest, err := w.Close()
	if err != nil {
		return nil, err
	}
	path, err := chain.Publish(round, digest, data)
	if err != nil {
		return nil, err
	}

	// State is saved after the round is on disk. The reverse order would leave
	// state pointing at a round that does not exist.
	state.Head, state.HeadDigest = round, digest
	state.ThroughSeq, state.InputDigest = through, inputDigest
	state.Parser, state.Policy = Parser, Policy
	if err := chain.SaveState(state, opt.Now()); err != nil {
		return nil, err
	}

	out.Number, out.Digest, out.Path = round, digest, path
	out.Nodes, out.Relations, out.Unresolved = len(d.nodes), len(d.relations), len(d.unresolved)
	out.Tombstones = d.tombstones
	return out, nil
}

// rebuild reconstructs a missing index from the landed files themselves.
//
// The rebuilt index is written back, so this cost is paid once rather than on
// every round. It is safe to write because the index is derived: if it is wrong
// or stale, deleting it and coming back here reproduces it.
func rebuild(z *storage.Zone, opt Options) (*index.Index, error) {
	if opt.Reindex == nil {
		return nil, fmt.Errorf("parse: no index for session %s, and no way to rebuild one", opt.Session)
	}
	ix := index.New(opt.Session)
	n, err := opt.Reindex(z, opt.Session, ix, 0)
	if err != nil {
		return nil, fmt.Errorf("parse: rebuild index for %s: %w", opt.Session, err)
	}
	if n == 0 {
		return nil, fmt.Errorf("parse: no landed records for session %s", opt.Session)
	}
	if err := ix.Write(z.IndexDir(opt.Session)); err != nil {
		return nil, err
	}
	st := storage.NewIndexState(opt.Session)
	st.Schema, st.Entries = index.Schema, len(ix.Entries)
	for i := range ix.Entries {
		if s := uint64(ix.Entries[i].Seq); s > st.IndexedSeq {
			st.IndexedSeq = s
		}
	}
	if err := st.Save(z.IndexStatePath(opt.Session), opt.Now()); err != nil {
		return nil, err
	}
	return ix, nil
}

// inputDigestFor extends the chain's input digest with the evidence this round
// newly consumed.
//
// Only files above the previous watermark are read, so the cost is proportional
// to what is new rather than to the whole conversation. A landed file is written
// once and never appended to, so its digest is fixed from the moment it appears.
func inputDigestFor(z *storage.Zone, session, previous string, after, through uint64) (string, error) {
	files, err := storage.LandedFiles(z, session)
	if err != nil {
		return "", err
	}
	var added []string
	for _, f := range files {
		if f.Seq <= after || f.Seq > through {
			continue
		}
		d, err := storage.FileDigest(f.Path)
		if err != nil {
			return "", err
		}
		added = append(added, d)
	}
	return asb.ChainInputDigest(previous, added), nil
}

// delta is what one round must write.
type delta struct {
	nodes      []asb.Node
	relations  []asb.Relation
	unresolved []asb.Unresolved
	tombstones int
}

func (d *delta) empty() bool {
	return len(d.nodes) == 0 && len(d.relations) == 0 && len(d.unresolved) == 0
}

// diff compares a fresh assembly against what the chain already says.
//
// Only what changed is written. An entity the assembly produced unchanged is
// left out, because absence in a round means unchanged - which is exactly what
// keeps a round proportional to new work rather than to the whole conversation.
//
// An entity the chain holds that the assembly no longer produces gets a
// tombstone. Silence would mean unchanged, so removal has to be said.
func diff(view *asb.View, res *assemble.Result) *delta {
	d := &delta{}

	seenNode := map[string]bool{}
	for _, n := range res.Nodes {
		seenNode[n.ID] = true
		if prev, ok := view.Nodes[n.ID]; ok && sameNode(prev, &n) {
			continue
		}
		d.nodes = append(d.nodes, n)
	}
	for id, prev := range view.Nodes {
		if !seenNode[id] {
			d.nodes = append(d.nodes, asb.Node{Entity: asb.Entity{ID: id, Tombstone: true}})
			d.tombstones++
			_ = prev
		}
	}

	seenRel := map[string]bool{}
	for _, r := range res.Relations {
		seenRel[r.ID] = true
		if prev, ok := view.Relations[r.ID]; ok && sameRelation(prev, &r) {
			continue
		}
		d.relations = append(d.relations, r)
	}
	for id := range view.Relations {
		if !seenRel[id] {
			d.relations = append(d.relations, asb.Relation{Entity: asb.Entity{ID: id, Tombstone: true}})
			d.tombstones++
		}
	}

	seenUnres := map[string]bool{}
	for _, u := range res.Unresolved {
		seenUnres[u.ID] = true
		if prev, ok := view.Unresolved[u.ID]; ok && sameUnresolved(prev, &u) {
			continue
		}
		d.unresolved = append(d.unresolved, u)
	}
	// An entry the assembly stopped producing has been resolved. It is superseded
	// with that state rather than removed: a reader must be able to see that the
	// gap existed and how it closed, and absence cannot say that.
	for id, prev := range view.Unresolved {
		if seenUnres[id] || prev.State != asb.UnresolvedOpen {
			continue
		}
		done := *prev
		done.State = asb.UnresolvedResolved
		d.unresolved = append(d.unresolved, done)
	}

	sortDelta(d)
	return d
}

// Comparing an assembled entity against a folded one has to ignore the two
// fields that are not about the entity at all.
//
// Revision is derived from chain position, so comparing it would make every
// entity look changed in every round. The frame tag is set by the writer, so a
// folded entity carries it and a freshly assembled one does not - comparing it
// would ALSO make every entity look changed, and every round would rewrite the
// whole conversation.
func normalize(e *asb.Entity) {
	e.Revision, e.T = 0, ""
}

func sameNode(a, b *asb.Node) bool {
	x, y := *a, *b
	normalize(&x.Entity)
	normalize(&y.Entity)
	return jsonEqual(x, y)
}

func sameRelation(a, b *asb.Relation) bool {
	x, y := *a, *b
	normalize(&x.Entity)
	normalize(&y.Entity)
	return jsonEqual(x, y)
}

func sameUnresolved(a, b *asb.Unresolved) bool {
	x, y := *a, *b
	normalize(&x.Entity)
	normalize(&y.Entity)
	return jsonEqual(x, y)
}

func jsonEqual(a, b any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(x) == string(y)
}

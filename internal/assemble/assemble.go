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

// Package assemble turns a session's derived index into conversation
// structure.
//
// It reads the index, not the payloads. Structure resolution needs identifiers,
// not content: removing duplicate records needs record ids, grouping a provider
// call needs message ids, joining a tool needs tool-use ids, joining a spawn
// needs agent and run ids. None of it reads message text. Payloads are read
// only when the result is rendered, and only for the records that appear in it.
//
// The whole session is assembled every time. That is what makes the output
// reproducible: the same index always produces the same entities, so a round
// can be re-derived rather than only trusted. Assembly resolves against a few
// megabytes of index and takes milliseconds, so rebuilding costs little.
package assemble

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/wu-sheng/AgentSessionizer/internal/index"
	"github.com/wu-sheng/AgentSessionizer/pkg/asb"
	"github.com/wu-sheng/AgentSessionizer/pkg/model"
)

// Options configures one assembly.
type Options struct {
	// Conversation is supplied, never inferred. An adapter reports it, or an
	// application supplies it. Grouping by person, account, timestamp proximity
	// or session id is not permitted, so this has no default here; the caller
	// applies its adapter's documented mapping.
	Conversation string
	Session      string

	// IdleGap is how long a session must be quiet before a segment becomes a
	// candidate for commit. A candidate is not a commit: about a third of long
	// gaps fall in the middle of a turn, so the gates in stage 8 decide.
	IdleGap time.Duration
}

// DefaultIdleGap is the quiet period that makes a segment a close candidate.
const DefaultIdleGap = 10 * time.Minute

// Stats records what assembly saw. Every count carries its denominator, because
// "3 unresolved" means nothing and "3 of 8,045 tool uses" means something.
type Stats struct {
	Entries    int
	Duplicates int

	Streams int
	Epochs  int
	Talks   int
	Runs    int
	Steps   int

	ProviderCalls     int
	CallFragments     int
	CallsWithoutEnd   int // never received a terminal fragment: usage is unavailable
	SyntheticMessages int

	ToolUses       int
	ToolsResolved  int
	ToolsUnmatched int

	Spawns          int
	SpawnsResolved  int
	ChildrenOrphan  int
	Notifications   int
	NotifyUnmatched int

	SegmentsOpen      int
	SegmentsCandidate int
}

// Result is one assembly of a whole session.
type Result struct {
	Nodes      []asb.Node
	Relations  []asb.Relation
	Unresolved []asb.Unresolved
	Stats      Stats

	// ThroughSeq is the highest landed sequence the assembly covered. It is what
	// the round header records, and what tells the next round where to resume.
	ThroughSeq uint64
}

// cycleKey scopes a prompt cycle to the stream it was written in.
type cycleKey struct {
	stream uint32
	cycle  uint32
}

// builder holds the state the eight stages share.
type builder struct {
	ix  *index.Index
	opt Options

	nodes map[string]*asb.Node
	rels  map[string]*asb.Relation
	unres map[string]*asb.Unresolved
	stats Stats

	// canonical is the session's entries in landed order with replayed copies
	// removed. Every stage works from this, never from Entries directly.
	canonical []int32

	streams  []*streamInfo
	byStream map[uint32]*streamInfo
	calls    []*providerCall
	tools    []*toolUse
	talks    []*talk
	segments []*segment

	// spawnEdges are resolved in stage 5 and written after stage 7, because the
	// nodes they connect do not exist until then.
	spawnEdges []spawnEdge

	// container maps a landed record, by (seq, row), to the node that holds it.
	container map[[2]uint32]string

	toolByID map[uint32]*toolUse
	epochAt  map[[2]uint32]string
	talkByID map[string]*talk

	// cycleTalk maps a prompt cycle to the Talk that owns it. A Talk owns the
	// cycle that started it and every notification cycle that followed from the
	// work it delegated.
	//
	// The key is (stream, cycle) and not the cycle alone. A prompt cycle id is
	// NOT unique across a session: a child stream is written under a cycle id
	// that also appears in the parent, and one id was observed in three
	// different child streams. Keying on the cycle alone therefore puts a child's
	// records inside the parent's turn and leaves the child with no run of its
	// own.
	cycleTalk map[cycleKey]string
	// runOf maps a prompt cycle to its Run node id, keyed the same way.
	runOf map[cycleKey]string
}

// Session assembles one session into conversation structure.
func Session(ix *index.Index, opt Options) (*Result, error) {
	if opt.Conversation == "" {
		return nil, fmt.Errorf("assemble: conversation identity is supplied, never inferred")
	}
	if opt.IdleGap == 0 {
		opt.IdleGap = DefaultIdleGap
	}
	b := &builder{
		ix: ix, opt: opt,
		nodes:     map[string]*asb.Node{},
		rels:      map[string]*asb.Relation{},
		unres:     map[string]*asb.Unresolved{},
		byStream:  map[uint32]*streamInfo{},
		cycleTalk: map[cycleKey]string{},
		runOf:     map[cycleKey]string{},
	}
	ix.Build()

	// The order is forced: each stage depends on the one before it.
	b.stage1Canonical() // duplicates removed
	b.stage2Streams()   // lineages partitioned
	b.stage3Calls()     // provider calls grouped
	b.stage4Tools()     // tool uses joined to their results
	b.stage5Spawns()    // children joined to the calls that started them
	b.stage6Epochs()    // context resets cut
	b.stage7Talks()     // talks and runs built, steps placed
	b.stage8Segments()  // commit windows proposed
	b.emitSteps()       // leaves written, now that their containers are known

	return b.result(), nil
}

// result flattens the builder into sorted, reproducible output.
//
// Sorting by id is what makes two assemblies of the same index produce the same
// bytes. Map iteration order in Go is deliberately random, so emitting in map
// order would make every round differ from the last for no reason.
func (b *builder) result() *Result {
	r := &Result{Stats: b.stats}
	for _, n := range b.nodes {
		r.Nodes = append(r.Nodes, *n)
	}
	sort.Slice(r.Nodes, func(i, j int) bool { return r.Nodes[i].ID < r.Nodes[j].ID })
	for _, v := range b.rels {
		r.Relations = append(r.Relations, *v)
	}
	sort.Slice(r.Relations, func(i, j int) bool { return r.Relations[i].ID < r.Relations[j].ID })
	for _, u := range b.unres {
		r.Unresolved = append(r.Unresolved, *u)
	}
	sort.Slice(r.Unresolved, func(i, j int) bool { return r.Unresolved[i].ID < r.Unresolved[j].ID })

	for _, i := range b.canonical {
		if s := uint64(b.ix.Entries[i].Seq); s > r.ThroughSeq {
			r.ThroughSeq = s
		}
	}
	return r
}

// node adds or replaces a node.
func (b *builder) node(n asb.Node) *asb.Node {
	cp := n
	b.nodes[n.ID] = &cp
	return &cp
}

// relate records a typed edge, keyed by its endpoints so the same edge observed
// twice folds to one.
func (b *builder) relate(typ, from, to, quality, via string, evidence ...asb.Ref) {
	id := asb.RelationID(typ, from, to)
	if prev, ok := b.rels[id]; ok {
		// Two sources asserting the same edge is confirmation, not a conflict.
		// Keep the stronger qualification and merge the evidence.
		if qualityRank(quality) > qualityRank(prev.Quality) {
			prev.Quality, prev.Via = quality, via
		}
		prev.Evidence = append(prev.Evidence, evidence...)
		return
	}
	b.rels[id] = &asb.Relation{
		Entity: asb.Entity{ID: id}, Type: typ, From: from, To: to,
		Quality: quality, Via: via, Evidence: evidence,
	}
}

func qualityRank(q string) int {
	switch q {
	case model.ExactUnique:
		return 5
	case model.ExactAmbiguous:
		return 4
	case model.StrongInference:
		return 3
	case model.WeakInference:
		return 2
	case model.Conflict:
		return 1
	}
	return 0
}

// open records a reference that could not be resolved.
//
// Unresolved entries are data, not noise. An assembler that drops what it could
// not resolve presents a partial conversation as a complete one.
func (b *builder) open(kind, ref, reason string) {
	id := asb.UnresolvedID(kind, ref)
	if _, ok := b.unres[id]; ok {
		return
	}
	b.unres[id] = &asb.Unresolved{
		Entity: asb.Entity{ID: id}, Kind: kind, RefID: ref,
		Reason: reason, State: asb.UnresolvedOpen,
	}
}

// ref points at one landed record.
func ref(e *index.Entry) asb.Ref {
	return asb.Ref{Seq: uint64(e.Seq), Row: uint64(e.Row)}
}

// blockRef points at one content block within a landed record.
func blockRef(e *index.Entry, ord uint16) asb.Ref {
	o := int(ord)
	return asb.Ref{Seq: uint64(e.Seq), Row: uint64(e.Row), Block: &o}
}

// attrs marshals a map deterministically. encoding/json sorts map keys, so the
// same map always produces the same bytes - which a digest chain requires.
func attrs(m map[string]any) json.RawMessage {
	if len(m) == 0 {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}

// str resolves an interned id to its string.
func (b *builder) str(id uint32) string { return b.ix.Strings.String(id) }

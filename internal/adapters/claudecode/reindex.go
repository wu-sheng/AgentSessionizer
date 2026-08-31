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

package claudecode

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/wu-sheng/AgentSessionizer/internal/index"
	"github.com/wu-sheng/AgentSessionizer/internal/storage"
	"github.com/wu-sheng/AgentSessionizer/pkg/record"
)

// landedFile is one landed record file discovered in the storage zone.
type landedFile struct {
	Path   string
	Seq    uint64
	Kind   record.Kind
	Stream string
	RunID  string
}

// scanLanded lists every landed record file for a session, in sequence order.
//
// The sequence is monotonic across all streams and runs, so ordering by it
// reproduces the order the collector landed them - which is the order the index
// must hold them in.
func scanLanded(z *storage.Zone, session string) ([]landedFile, error) {
	var out []landedFile
	root := z.SessionDir(session)

	add := func(dir, owner string, isRun bool) error {
		items, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		for _, it := range items {
			name := it.Name()
			if it.IsDir() || !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			seq, ok := landedSeq(name)
			if !ok {
				continue
			}
			lf := landedFile{Path: filepath.Join(dir, name), Seq: seq}
			if isRun {
				lf.RunID = owner
			} else {
				lf.Stream = owner
			}
			out = append(out, lf)
		}
		return nil
	}

	for _, group := range []struct {
		sub   string
		isRun bool
	}{{"streams", false}, {"runs", true}} {
		base := filepath.Join(root, group.sub)
		owners, err := os.ReadDir(base)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, o := range owners {
			if !o.IsDir() {
				continue
			}
			if err := add(filepath.Join(base, o.Name()), o.Name(), group.isRun); err != nil {
				return nil, err
			}
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

// landedSeq extracts the sequence from "<kind>-<ts>-<seq>.jsonl".
func landedSeq(name string) (uint64, bool) {
	base := strings.TrimSuffix(name, ".jsonl")
	i := strings.LastIndex(base, "-")
	if i < 0 {
		return 0, false
	}
	n, err := strconv.ParseUint(base[i+1:], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// RebuildIndex indexes every landed file with a sequence above afterSeq,
// appending to ix.
//
// This is the recovery path, not the normal one. Indexing normally rides along
// with landing, where the bytes are already in hand. But cursors commit per
// source while the index is written once per session, so a crash between them
// advances the cursors without recording their entries - and because nothing
// re-lands, the gap would be permanent and silent. Re-reading the landed files
// is the only way to close it.
//
// Passing afterSeq = 0 rebuilds the whole index from scratch.
func RebuildIndex(z *storage.Zone, session string, ix *index.Index, afterSeq uint64) (int, error) {
	files, err := scanLanded(z, session)
	if err != nil {
		return 0, err
	}
	var indexed int
	for _, lf := range files {
		if lf.Seq <= afterSeq {
			continue
		}
		n, err := indexLandedFile(ix, lf)
		if err != nil {
			return indexed, err
		}
		indexed += n
	}
	return indexed, nil
}

// indexLandedFile indexes one landed file.
func indexLandedFile(ix *index.Index, lf landedFile) (int, error) {
	f, err := os.Open(lf.Path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	r, err := record.NewReader(f)
	if err != nil {
		// A landed file we cannot read is a real problem, but it must not stop
		// the rebuild: the remaining files still describe recoverable data.
		return 0, nil
	}
	hdr := r.Header()

	src := Source{
		Kind:    sourceKindOf(hdr.Kind),
		Rel:     hdr.Src,
		Session: hdr.Session,
		Stream:  hdr.Stream,
		RunID:   lf.RunID,
	}
	if src.Stream == "" {
		src.Stream = lf.Stream
	}

	var n int
	for row := uint32(1); ; row++ {
		rec, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			break
		}
		payload, perr := rec.SourceBytes()
		if perr != nil {
			payload = rec.Payload
		}
		e, blocks := IndexEntry(ix, src, uint32(lf.Seq), row, payload)
		ix.Append(e, blocks...)
		n++
	}
	return n, nil
}

// sourceKindOf maps a landed envelope kind back to a source kind.
func sourceKindOf(k record.Kind) SourceKind {
	switch k {
	case record.KindAgentMeta:
		return SrcAgentMeta
	case record.KindJournal:
		return SrcJournal
	case record.KindWorkflowManifest:
		return SrcWorkflowManifest
	case record.KindWorkflowScript:
		return SrcWorkflowScript
	}
	return SrcMainTranscript
}

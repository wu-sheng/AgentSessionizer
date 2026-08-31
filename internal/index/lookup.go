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

package index

import "sort"

// Index is a session's derived lookup structure.
//
// Entries are held in landed order, so a stream's records are already in the
// order the assembler must reason about. Lookup maps are rebuilt on load rather
// than persisted: rebuilding every map for the largest observed session takes
// about 4 ms, which is not worth the format complexity of storing them.
type Index struct {
	Session string
	Strings *Interner
	Entries []Entry
	Blocks  []Block

	byRecord map[uint32]int32
	byMsg    map[uint32][]int32
	byTool   map[uint32][]int32 // tool id -> block indices
	byRun    map[uint32][]int32
	stream   map[uint32][]int32
}

// New returns an empty Index for a session.
func New(session string) *Index {
	return &Index{Session: session, Strings: NewInterner()}
}

// Append adds one record and its blocks, wiring the block range.
func (ix *Index) Append(e Entry, blocks ...Block) {
	ei := uint32(len(ix.Entries))
	e.BlockFirst = uint32(len(ix.Blocks))
	e.BlockCount = uint32(len(blocks))
	for _, b := range blocks {
		b.Entry = ei
		ix.Blocks = append(ix.Blocks, b)
	}
	ix.Entries = append(ix.Entries, e)
	ix.byRecord = nil // invalidate; Build rebuilds on demand
}

// Build constructs the lookup maps. It is idempotent.
func (ix *Index) Build() {
	if ix.byRecord != nil {
		return
	}
	n := len(ix.Entries)
	ix.byRecord = make(map[uint32]int32, n)
	ix.byMsg = make(map[uint32][]int32, n/2+1)
	ix.byRun = make(map[uint32][]int32)
	ix.stream = make(map[uint32][]int32)
	// Record ids are NOT unique within a source file: a resume replays an
	// earlier block of records. First occurrence wins, and a later copy is
	// excluded from EVERY derived lookup - not just byRecord. A duplicate that
	// reaches byMsg inflates a provider call's fragment count; one that reaches
	// byTool makes an exact join look ambiguous; one that reaches stream
	// duplicates a record in its own stream.
	//
	// The duplicate entries themselves stay in Entries: they are evidence of
	// what the runtime re-emitted, and only the canonical view excludes them.
	dup := make([]bool, len(ix.Entries))
	for i := range ix.Entries {
		e := &ix.Entries[i]
		if e.Record == 0 {
			continue // no identity to deduplicate on; every such entry is distinct
		}
		if _, seen := ix.byRecord[e.Record]; seen {
			dup[i] = true
			continue
		}
		ix.byRecord[e.Record] = int32(i)
	}
	for i := range ix.Entries {
		if dup[i] {
			continue
		}
		e := &ix.Entries[i]
		if e.Call != 0 {
			ix.byMsg[e.Call] = append(ix.byMsg[e.Call], int32(i))
		}
		if e.Run != 0 {
			ix.byRun[e.Run] = append(ix.byRun[e.Run], int32(i))
		}
		if e.Stream != 0 {
			ix.stream[e.Stream] = append(ix.stream[e.Stream], int32(i))
		}
	}
	ix.byTool = make(map[uint32][]int32, len(ix.Blocks))
	for i := range ix.Blocks {
		b := &ix.Blocks[i]
		if b.ToolID == 0 || dup[b.Entry] {
			continue
		}
		ix.byTool[b.ToolID] = append(ix.byTool[b.ToolID], int32(i))
	}
}

// EntryByRecord returns the first entry with a record id, and whether it exists.
func (ix *Index) EntryByRecord(recordID string) (*Entry, bool) {
	ix.Build()
	id, ok := ix.Strings.Lookup(recordID)
	if !ok || id == 0 {
		return nil, false
	}
	i, ok := ix.byRecord[id]
	if !ok {
		return nil, false
	}
	return &ix.Entries[i], true
}

// ProviderCall returns the entries of one provider call, in landed order.
//
// Grouping is by message id, never by request id: a synthetic companion record
// can reuse a real call's request id, which would splice fabricated content
// into a genuine call.
func (ix *Index) ProviderCall(callID string) []*Entry {
	ix.Build()
	id, ok := ix.Strings.Lookup(callID)
	if !ok || id == 0 {
		return nil
	}
	idxs := ix.byMsg[id]
	out := make([]*Entry, 0, len(idxs))
	for _, i := range idxs {
		out = append(out, &ix.Entries[i])
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Seq != out[b].Seq {
			return out[a].Seq < out[b].Seq
		}
		return out[a].Row < out[b].Row
	})
	return out
}

// ToolBlocks returns every block carrying a tool-use id.
func (ix *Index) ToolBlocks(toolUseID string) []*Block {
	ix.Build()
	id, ok := ix.Strings.Lookup(toolUseID)
	if !ok || id == 0 {
		return nil
	}
	out := make([]*Block, 0, 2)
	for _, i := range ix.byTool[id] {
		out = append(out, &ix.Blocks[i])
	}
	return out
}

// Stream returns every entry of one execution stream, in landed order.
func (ix *Index) Stream(name string) []*Entry {
	ix.Build()
	id, ok := ix.Strings.Lookup(name)
	if !ok || id == 0 {
		return nil
	}
	idxs := ix.stream[id]
	out := make([]*Entry, 0, len(idxs))
	for _, i := range idxs {
		out = append(out, &ix.Entries[i])
	}
	return out
}

// Streams returns the names of every indexed stream.
func (ix *Index) Streams() []string {
	ix.Build()
	out := make([]string, 0, len(ix.stream))
	for id := range ix.stream {
		out = append(out, ix.Strings.String(id))
	}
	sort.Strings(out)
	return out
}

// Run returns every entry belonging to a workflow run.
func (ix *Index) Run(runID string) []*Entry {
	ix.Build()
	id, ok := ix.Strings.Lookup(runID)
	if !ok || id == 0 {
		return nil
	}
	out := make([]*Entry, 0, len(ix.byRun[id]))
	for _, i := range ix.byRun[id] {
		out = append(out, &ix.Entries[i])
	}
	return out
}

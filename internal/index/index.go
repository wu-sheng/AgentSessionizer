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

// Package index holds the derived lookup structure the assembler resolves
// conversation structure against.
//
// The index exists because structure resolution needs identifiers, not content:
// deduplication needs record ids, provider-call grouping needs message ids,
// tool joins need tool-use ids, spawn joins need agent and run ids. None of it
// reads message text. Measured on a real corpus, the index is ~12x smaller than
// the payloads it describes - 16 MB for the largest session, 95 MB for 1.1 GB
// of conversation - which is what lets the whole thing stay resident with
// random access.
//
// It is DERIVED and DISPOSABLE. Deleting it loses nothing; it rebuilds from the
// landed files. Nothing downstream may treat it as authoritative.
package index

// Schema is the on-disk index version. Bump it when Entry or Block changes;
// a mismatch discards the index and rebuilds rather than migrating.
const Schema = 4

// Kind classifies a record without reading it.
type Kind uint8

const (
	KindUnknown Kind = iota
	KindUser
	KindAssistant
	KindAttachment
	KindSystem
	KindMeta     // agent sidecar
	KindJournal  // workflow journal record
	KindManifest // workflow manifest
	KindScript   // workflow script
	KindOther    // an out-of-band record type carrying no uuid
)

// Entry is one indexed record.
//
// Every field is fixed width with no pointers or strings - identifiers are
// interned to uint32 - so the entry array is one contiguous allocation and the
// persisted form is a flat block.
type Entry struct {
	// Landed location: which landed file, and which line within it. This is the
	// only position the index needs - resolution never reads source positions,
	// and ordering by (Seq, Row) already reproduces source order because landed
	// sequences are monotonic and deltas are contiguous.
	//
	// Source position and integrity live in the landed record's own envelope,
	// beside the bytes they describe. Duplicating them here would cost 12 of
	// 69 bytes per entry for data one dereference away that nothing reads.
	Seq uint32
	Row uint32

	Stream uint32 // interned: "main" or an agent id
	Run    uint32 // interned: workflow run id, for journal/manifest/script
	Kind   Kind
	TS     int64 // record timestamp, unix nanoseconds; 0 when absent

	// Interned join keys, named for the ROLE they play rather than for any one
	// runtime's field. An adapter maps its own identifiers onto these; nothing
	// here may carry a runtime's vocabulary, because a rename in that runtime
	// must not reach past its adapter.
	//
	// 0 means absent, which is common: many record types carry no identity.
	Record uint32 // this record's own id
	Parent uint32 // its containment parent
	Call   uint32 // the provider call this record is a fragment of
	Cycle  uint32 // the prompt cycle: a trigger through to the model's reply

	// Blocks indexes into Index.Blocks: [BlockFirst, BlockFirst+BlockCount).
	BlockFirst uint32
	BlockCount uint32
}

// BlockKind classifies a joinable content block.
type BlockKind uint8

const (
	BlockUnknown BlockKind = iota
	BlockToolUse
	BlockToolResult
)

// Block is one joinable content block.
//
// Blocks are indexed separately rather than folded into Entry because a single
// record can carry several - a line usually holds one content block, but not
// always - and collapsing them would silently drop tool ids and break the join
// they exist to serve.
type Block struct {
	Entry  uint32 // index into Index.Entries
	Ord    uint16 // position within the record's content array
	Kind   BlockKind
	ToolID uint32 // interned tool_use id
	Name   uint32 // interned tool name, for tool_use
}

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

package index_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wu-sheng/AgentSessionizer/internal/index"
)

func build(t *testing.T) *index.Index {
	t.Helper()
	ix := index.New("sess-1")
	in := ix.Strings
	main := in.ID("main")
	// two fragments of one provider call, then its tool result
	ix.Append(index.Entry{
		Seq: 1, Row: 1, Stream: main, Kind: index.KindAssistant,
		TS: 1000, Record: in.ID("u-a1"), Call: in.ID("msg_1"),
	}, index.Block{Ord: 0, Kind: index.BlockToolUse, ToolID: in.ID("toolu_1"), Name: in.ID("Bash")})
	ix.Append(index.Entry{
		Seq: 1, Row: 2, Stream: main, Kind: index.KindAssistant,
		TS: 1001, Record: in.ID("u-a2"), Parent: in.ID("u-a1"), Call: in.ID("msg_1"),
	})
	ix.Append(index.Entry{
		Seq: 1, Row: 3, Stream: main, Kind: index.KindUser,
		TS: 1002, Record: in.ID("u-r1"), Parent: in.ID("u-a1"),
	}, index.Block{Ord: 0, Kind: index.BlockToolResult, ToolID: in.ID("toolu_1")})
	// a child stream
	ix.Append(index.Entry{
		Seq: 2, Row: 1, Stream: in.ID("a1111111111111111"),
		Kind: index.KindUser, TS: 1010, Record: in.ID("u-c1"), Cycle: in.ID("a1111111111111111"),
	})
	return ix
}

func TestLookups(t *testing.T) {
	ix := build(t)
	if e, ok := ix.EntryByRecord("u-a1"); !ok || e.Row != 1 {
		t.Errorf("EntryByRecord: %+v ok=%v", e, ok)
	}
	if _, ok := ix.EntryByRecord("nope"); ok {
		t.Error("unknown uuid resolved")
	}
	// A provider call is grouped by message id, and both fragments belong to it.
	if got := ix.ProviderCall("msg_1"); len(got) != 2 {
		t.Errorf("ProviderCall: %d entries, want 2", len(got))
	}
	// The tool id appears on both the use and the result.
	if got := ix.ToolBlocks("toolu_1"); len(got) != 2 {
		t.Errorf("ToolBlocks: %d, want 2", len(got))
	}
	if got := ix.Stream("main"); len(got) != 3 {
		t.Errorf("Stream(main): %d, want 3", len(got))
	}
	if got := ix.Streams(); len(got) != 2 {
		t.Errorf("Streams: %v, want 2", got)
	}
}

// TestFirstOccurrenceWins pins the deduplication rule into the index: record
// ids are not unique within a source file, because a resume replays an earlier
// block. The original must never be displaced by its replayed copy.
func TestFirstOccurrenceWins(t *testing.T) {
	ix := index.New("s")
	in := ix.Strings
	ix.Append(index.Entry{Seq: 1, Row: 5, Record: in.ID("dup")})
	ix.Append(index.Entry{Seq: 9, Row: 900, Record: in.ID("dup")})
	e, ok := ix.EntryByRecord("dup")
	if !ok {
		t.Fatal("not found")
	}
	if e.Seq != 1 || e.Row != 5 {
		t.Errorf("got seq=%d row=%d, want seq=1 row=5 (first occurrence must win)", e.Seq, e.Row)
	}
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	ix := build(t)
	if err := ix.Write(dir); err != nil {
		t.Fatal(err)
	}
	got, ok, err := index.Load(dir, "sess-1")
	if err != nil || !ok {
		t.Fatalf("Load: ok=%v err=%v", ok, err)
	}
	if len(got.Entries) != len(ix.Entries) || len(got.Blocks) != len(ix.Blocks) {
		t.Fatalf("entries %d/%d blocks %d/%d", len(got.Entries), len(ix.Entries),
			len(got.Blocks), len(ix.Blocks))
	}
	for i := range ix.Entries {
		if got.Entries[i] != ix.Entries[i] {
			t.Errorf("entry %d:\n got %+v\nwant %+v", i, got.Entries[i], ix.Entries[i])
		}
	}
	// identifiers must survive interning
	if e, ok := got.EntryByRecord("u-a1"); !ok || e.Row != 1 {
		t.Error("uuid lookup did not survive the round trip")
	}
	if got.Strings.String(got.Entries[0].Call) != "msg_1" {
		t.Error("string table did not survive the round trip")
	}
}

func TestLoadRejectsForeignAndStale(t *testing.T) {
	dir := t.TempDir()
	// A missing index is not an error - the index is derived, so the caller
	// rebuilds rather than failing.
	if _, ok, err := index.Load(dir, "s"); ok || err != nil {
		t.Errorf("missing index: ok=%v err=%v", ok, err)
	}
	if err := writeFile(filepath.Join(dir, "entries.bin"), "not an index"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := index.Load(dir, "s"); ok || err != nil {
		t.Errorf("foreign file: ok=%v err=%v (must rebuild, not fail)", ok, err)
	}
}

func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o644)
}

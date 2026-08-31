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

package verify_test

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/wu-sheng/AgentSessionizer/internal/verify"
	"github.com/wu-sheng/AgentSessionizer/pkg/record"
)

// landStream writes a landed transcript delta covering source lines from..to.
func landStream(t *testing.T, dir string, seq uint64, from, to int) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w, err := record.NewWriter(&buf, &record.Header{
		H: 1, Seq: seq, At: "2026-08-31T12:00:00Z", Kind: record.KindTranscript,
		Adapter: "test/0", Src: "proj/s.jsonl", Session: "s", Stream: "main",
		State: record.StateAvailable,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Offsets must be exactly what a real source would produce: each line's
	// payload plus the newline the source carried.
	var off uint64
	for i := 1; i < from; i++ {
		off += uint64(len(payloadFor(i))) + 1
	}
	for i := from; i <= to; i++ {
		p := payloadFor(i)
		if err := w.Write(uint64(i), off, p); err != nil {
			t.Fatal(err)
		}
		off += uint64(len(p)) + 1
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(dir, "transcript-20260831T120000.000000000Z-"+pad(seq)+".jsonl")
	if err := os.WriteFile(name, buf.Bytes(), 0o444); err != nil {
		t.Fatal(err)
	}
}

func payloadFor(i int) []byte {
	return []byte(`{"uuid":"u` + strconv.Itoa(i) + `","n":` + strconv.Itoa(i) + `}`)
}

func pad(s uint64) string { return fmt.Sprintf("%06d", s) }

// TestCleanStreamPasses is the control: without it, a verifier that always
// reported a problem would look just as "working" as one that never does.
func TestCleanStreamPasses(t *testing.T) {
	dir := t.TempDir()
	landStream(t, dir, 1, 1, 10)
	landStream(t, dir, 2, 11, 20) // a second delta continuing the same source
	rep, err := verify.Stream(dir, "transcript")
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Fatalf("clean stream reported problems: ord=%v byte=%v sha=%v",
			rep.OrdGaps, rep.ByteGaps, rep.ShaBad)
	}
	if rep.Records != 20 || rep.FirstOrd != 1 || rep.LastOrd != 20 {
		t.Errorf("records=%d ord=%d..%d, want 20 records ord 1..20",
			rep.Records, rep.FirstOrd, rep.LastOrd)
	}
}

// TestDetectsMissingDelta covers the failure the check exists for: a whole
// delta lost, which is otherwise invisible - no error, no conflict, just a
// shorter conversation.
func TestDetectsMissingDelta(t *testing.T) {
	dir := t.TempDir()
	landStream(t, dir, 1, 1, 10)
	landStream(t, dir, 3, 21, 30) // lines 11..20 never landed
	rep, err := verify.Stream(dir, "transcript")
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() {
		t.Fatal("a missing delta was not detected")
	}
	if len(rep.OrdGaps) == 0 {
		t.Error("expected an ord gap")
	}
	if len(rep.ByteGaps) == 0 {
		t.Error("expected a byte gap")
	}
}

// TestDetectsDroppedLine covers a single skipped record inside a delta - the
// subtlest version of the same failure.
func TestDetectsDroppedLine(t *testing.T) {
	dir := t.TempDir()
	landStream(t, dir, 1, 1, 10)
	path := filepath.Join(dir, "transcript-20260831T120000.000000000Z-000001.jsonl")
	dropLine(t, path, 6) // header + 5 records, then drop the 6th

	rep, err := verify.Stream(dir, "transcript")
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() {
		t.Fatal("a dropped line was not detected")
	}
	if len(rep.OrdGaps) != 1 {
		t.Errorf("ord gaps = %d, want 1: %v", len(rep.OrdGaps), rep.OrdGaps)
	}
}

// TestDetectsTamperedPayload covers a landed file edited after the fact, which
// chmod 0444 discourages but cannot prevent.
func TestDetectsTamperedPayload(t *testing.T) {
	dir := t.TempDir()
	landStream(t, dir, 1, 1, 5)
	path := filepath.Join(dir, "transcript-20260831T120000.000000000Z-000001.jsonl")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Change a payload byte without touching its recorded digest.
	tampered := bytes.Replace(b, []byte(`"n":3`), []byte(`"n":9`), 1)
	if bytes.Equal(tampered, b) {
		t.Fatal("test setup: nothing was tampered")
	}
	_ = os.Chmod(path, 0o644)
	if err := os.WriteFile(path, tampered, 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := verify.Stream(dir, "transcript")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.ShaBad) == 0 {
		t.Fatal("a tampered payload was not detected")
	}
}

// dropLine removes the nth line (1-based) from a landed file.
func dropLine(t *testing.T, path string, n int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for i := 1; sc.Scan(); i++ {
		if i == n {
			continue
		}
		out.Write(sc.Bytes())
		out.WriteByte('\n')
	}
	f.Close()
	_ = os.Chmod(path, 0o644)
	if err := os.WriteFile(path, out.Bytes(), 0o444); err != nil {
		t.Fatal(err)
	}
}

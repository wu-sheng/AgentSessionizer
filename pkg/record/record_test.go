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

package record_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wu-sheng/AgentSessionizer/pkg/record"
)

func testHeader() *record.Header {
	return &record.Header{
		H: 1, Seq: 1, At: "2026-08-31T12:00:00.000000000Z",
		Kind: record.KindTranscript, Adapter: "claude-code-local/0.1.0",
		Src: "proj/sess.jsonl", Session: "sess", Stream: "main",
		State: record.StateAvailable,
	}
}

// landAndRestore wraps every line of src into a landed file, reads it back and
// returns the reconstructed source bytes.
func landAndRestore(t *testing.T, src []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := record.NewWriter(&buf, testHeader())
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	var off uint64
	for i, line := range bytes.SplitAfter(src, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		body := bytes.TrimSuffix(line, []byte("\n"))
		if err := w.Write(uint64(i+1), off, body); err != nil {
			t.Fatalf("Write: %v", err)
		}
		off += uint64(len(line))
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	r, err := record.NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	var out bytes.Buffer
	for {
		rec, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		src, err := rec.SourceBytes()
		if err != nil {
			t.Fatalf("SourceBytes: %v", err)
		}
		out.Write(src)
		out.WriteByte('\n')
	}
	return out.Bytes()
}

// TestRoundTripBytesIdentical is the load-bearing property of the envelope:
// wrapping and unwrapping must not change a single byte of the source, or
// every provenance digest we record becomes a lie.
func TestRoundTripBytesIdentical(t *testing.T) {
	cases := []struct{ name, src string }{
		{"simple", "{\"a\":1}\n{\"b\":2}\n"},
		// Key order must survive: a re-encode would sort these.
		{"key order", "{\"z\":1,\"a\":2,\"m\":3}\n"},
		// Large integers lose precision through float64 if re-encoded.
		{"big int", "{\"n\":12345678901234567890}\n"},
		// Escapes must not be normalised.
		{"escapes", "{\"s\":\"a\\u00e9b\\n\\t\\\"q\\\"\"}\n"},
		{"unicode", "{\"s\":\"日本語 🎉 emoji\"}\n"},
		// Insignificant whitespace inside a value is part of the source bytes.
		{"spacing", "{\"a\": 1,  \"b\":  [1, 2] }\n"},
		{"nested empty", "{\"a\":{},\"b\":[],\"c\":null}\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := landAndRestore(t, []byte(tc.src))
			if !bytes.Equal(got, []byte(tc.src)) {
				t.Errorf("round trip changed bytes\n got: %q\nwant: %q", got, tc.src)
			}
		})
	}
}

// TestRoundTripLargeLine guards the bufio.Scanner trap: the largest real
// transcript line measured is 979,632 bytes, well past Scanner's 64 KB limit.
func TestRoundTripLargeLine(t *testing.T) {
	src := []byte("{\"big\":\"" + strings.Repeat("x", 1_500_000) + "\"}\n")
	if got := landAndRestore(t, src); !bytes.Equal(got, src) {
		t.Fatalf("large line round trip failed: got %d bytes, want %d", len(got), len(src))
	}
}

// TestRoundTripRealTranscript runs the property over a real Claude Code
// transcript fixture when one is present.
func TestRoundTripRealTranscript(t *testing.T) {
	matches, _ := filepath.Glob(filepath.Join("testdata", "*.jsonl"))
	if len(matches) == 0 {
		t.Skip("no testdata/*.jsonl fixture present")
	}
	for _, path := range matches {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		got := landAndRestore(t, src)
		if sha256.Sum256(got) != sha256.Sum256(src) {
			t.Errorf("%s: round trip is not byte-identical (%d vs %d bytes)", path, len(got), len(src))
		}
	}
}

func TestHeaderValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*record.Header)
	}{
		{"bad version", func(h *record.Header) { h.H = 2 }},
		{"no kind", func(h *record.Header) { h.Kind = "" }},
		{"no session", func(h *record.Header) { h.Session = "" }},
		{"no src", func(h *record.Header) { h.Src = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := testHeader()
			tc.mut(h)
			if _, err := record.NewWriter(io.Discard, h); err == nil {
				t.Error("expected validation error, got nil")
			}
		})
	}
}

// TestRoundTripSurvivesMalformedSource covers what a torn writer, a blank line
// or a pretty-printed document does to the envelope.
//
// A landed file is fsynced and made read-only before anything reads it, so
// splicing invalid JSON would produce a permanently unparseable file that
// silently truncates every record after it. These inputs must round trip, and
// the file must stay parseable.
func TestRoundTripSurvivesMalformedSource(t *testing.T) {
	cases := map[string]string{
		"torn write then append": "{\"a\":1}\n{\"trunc\"{\"b\":2}\n{\"c\":3}\n",
		"blank line":             "{\"a\":1}\n\n{\"b\":2}\n",
		"crlf":                   "{\"a\":1}\r\n{\"b\":2}\r\n",
		"bare scalar":            "{\"a\":1}\n42\n\"str\"\n",
		"not json at all":        "{\"a\":1}\nplain text line\n",
		"leading whitespace":     "  {\"a\":1}\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			got := landAndRestore(t, []byte(src))
			if !bytes.Equal(got, []byte(src)) {
				t.Errorf("round trip changed bytes\n got: %q\nwant: %q", got, src)
			}
		})
	}
}

// TestMalformedSourceKeepsLandedFileParseable is the property that matters: a
// bad source record must not cost us the records that follow it.
func TestMalformedSourceKeepsLandedFileParseable(t *testing.T) {
	var buf bytes.Buffer
	w, err := record.NewWriter(&buf, testHeader())
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range [][]byte{
		[]byte(`{"a":1}`),
		[]byte(`{"trunc"{"b":2}`), // a torn write concatenated with the next record
		[]byte(`{"c":3}`),
	} {
		if err := w.Write(uint64(i+1), 0, line); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}

	r, err := record.NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	var n int
	var sawRaw bool
	for {
		rec, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("record %d unreadable - a malformed source line must not truncate the file: %v", n+1, err)
		}
		if rec.State == record.StateRaw {
			sawRaw = true
		}
		n++
	}
	if n != 3 {
		t.Errorf("read %d records, want 3 - records after a malformed one were lost", n)
	}
	if !sawRaw {
		t.Error("the malformed record should be marked StateRaw")
	}
}

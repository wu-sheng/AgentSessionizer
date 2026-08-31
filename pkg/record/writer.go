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

package record

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
)

// payloadKey is written literally so the payload bytes can be appended without
// being re-encoded. Preserving the source bytes exactly - key order, spacing,
// number precision, escaping - is what makes the sha meaningful.
var payloadKey = []byte(`,"payload":`)

// Writer emits a landed file: one Header line, then one line per Record.
type Writer struct {
	bw    *bufio.Writer
	wrote bool
}

// NewWriter returns a Writer that emits h as the first line.
func NewWriter(w io.Writer, h *Header) (*Writer, error) {
	if err := h.Validate(); err != nil {
		return nil, err
	}
	bw := bufio.NewWriterSize(w, 1<<20)
	enc, err := json.Marshal(h)
	if err != nil {
		return nil, fmt.Errorf("record: encode header: %w", err)
	}
	if _, err := bw.Write(enc); err != nil {
		return nil, err
	}
	if err := bw.WriteByte('\n'); err != nil {
		return nil, err
	}
	return &Writer{bw: bw}, nil
}

// Write appends one record. payload must be the source bytes with any trailing
// newline already removed.
//
// When payload is one complete JSON value it is copied through verbatim, which
// is what makes the landed record byte-identical to its source. When it is not
// - a torn write, a blank line, a pretty-printed document, CRLF line endings -
// it is wrapped as a JSON string and marked StateRaw.
//
// The bytes are preserved either way; the wrapping exists because a landed file
// is immutable and fsynced before anything reads it, so writing invalid JSON
// would produce a permanently unparseable file that silently truncates every
// record after it.
func (w *Writer) Write(ord, off uint64, payload []byte) error {
	sum := sha256.Sum256(payload)
	rec := Record{Ord: ord, Off: off, Sha: hex.EncodeToString(sum[:])[:12]}

	if !isSingleJSONValue(payload) {
		rec.State = StateRaw
		enc, err := json.Marshal(string(payload))
		if err != nil {
			return fmt.Errorf("record: wrap raw payload: %w", err)
		}
		return w.emit(rec, enc)
	}
	return w.emit(rec, payload)
}

func (w *Writer) emit(rec Record, payload []byte) error {
	if _, err := fmt.Fprintf(w.bw, `{"ord":%d,"off":%d,"sha":%q`, rec.Ord, rec.Off, rec.Sha); err != nil {
		return err
	}
	if rec.State != "" {
		if _, err := fmt.Fprintf(w.bw, `,"state":%q`, rec.State); err != nil {
			return err
		}
	}
	if _, err := w.bw.Write(payloadKey); err != nil {
		return err
	}
	if _, err := w.bw.Write(payload); err != nil {
		return err
	}
	if _, err := w.bw.Write([]byte("}\n")); err != nil {
		return err
	}
	w.wrote = true
	return nil
}

// isSingleJSONValue reports whether b is exactly one complete JSON value with
// no leading or trailing bytes.
//
// json.Valid alone is not enough: it tolerates surrounding whitespace, and a
// trailing '\r' or a newline inside a pretty-printed document would still be
// written into the landed line and break it.
func isSingleJSONValue(b []byte) bool {
	if len(b) == 0 || !json.Valid(b) {
		return false
	}
	for _, c := range b {
		if c == '\n' || c == '\r' {
			return false
		}
	}
	// Reject surrounding whitespace so the stored sha always matches the bytes
	// that read back.
	return b[0] != ' ' && b[0] != '\t' && b[len(b)-1] != ' ' && b[len(b)-1] != '\t'
}

// Empty reports whether no record has been written.
func (w *Writer) Empty() bool { return !w.wrote }

// Flush writes any buffered data to the underlying writer.
func (w *Writer) Flush() error { return w.bw.Flush() }

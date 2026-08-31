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

package asb

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Writer builds one round in memory, computes its digest, and emits it.
//
// The digest covers every line before the commit frame, so it cannot be known
// until the round is complete - which is why the round is buffered rather than
// streamed. A round is a bounded delta, not a snapshot, so this stays small.
type Writer struct {
	buf    bytes.Buffer
	header Header
	counts Counts
	closed bool
}

// NewWriter starts a round.
func NewWriter(h Header) (*Writer, error) {
	h.T = FrameHeader
	h.Schema = Schema
	if err := h.Validate(); err != nil {
		return nil, err
	}
	w := &Writer{header: h}
	if err := w.emit(h); err != nil {
		return nil, err
	}
	return w, nil
}

// Node appends a node revision.
func (w *Writer) Node(n Node) error {
	n.T, n.Revision = FrameNode, w.header.Round
	if n.ID == "" {
		return fmt.Errorf("asb: node has no id; it could never be superseded")
	}
	w.counts.Nodes++
	return w.emit(n)
}

// Relation appends a relation revision.
func (w *Writer) Relation(r Relation) error {
	r.T, r.Revision = FrameRelation, w.header.Round
	if r.ID == "" {
		return fmt.Errorf("asb: relation has no id; it could never be superseded")
	}
	w.counts.Relations++
	return w.emit(r)
}

// Unresolved appends an unresolved-reference revision.
func (w *Writer) Unresolved(u Unresolved) error {
	u.T, u.Revision = FrameUnresolved, w.header.Round
	if u.ID == "" {
		return fmt.Errorf("asb: unresolved entry has no id; it could never be resolved")
	}
	if u.State == "" {
		u.State = UnresolvedOpen
	}
	w.counts.Unresolved++
	return w.emit(u)
}

// Close writes the commit frame and returns the round's bytes and digest.
//
// A round with no entities is valid: it advances the processed-input watermark,
// which is how a pass that found nothing new still records that it looked.
func (w *Writer) Close() (data []byte, digest string, err error) {
	if w.closed {
		return nil, "", fmt.Errorf("asb: round already closed")
	}
	sum := sha256.Sum256(w.buf.Bytes())
	digest = hex.EncodeToString(sum[:])
	if err := w.emit(Commit{T: FrameCommit, Digest: digest, Counts: w.counts}); err != nil {
		return nil, "", err
	}
	w.closed = true
	return w.buf.Bytes(), digest, nil
}

// Counts reports what has been written so far.
func (w *Writer) Counts() Counts { return w.counts }

func (w *Writer) emit(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	w.buf.Write(b)
	w.buf.WriteByte('\n')
	return nil
}

// Round is a parsed round file.
type Round struct {
	Header     Header
	Nodes      []Node
	Relations  []Relation
	Unresolved []Unresolved
	Commit     Commit
}

// Read parses a round and verifies its self-digest.
//
// The digest covers every line before the commit frame, so a truncated or
// edited round fails here rather than folding into a wrong conversation.
func Read(r io.Reader) (*Round, error) {
	br := bufio.NewReaderSize(r, 1<<20)
	var (
		out       Round
		hashed    bytes.Buffer
		sawHead   bool
		sawCommit bool
	)
	for {
		line, err := readLine(br)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(line) == 0 {
			continue
		}
		var probe struct {
			T FrameType `json:"t"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			return nil, fmt.Errorf("asb: undecodable frame: %w", err)
		}
		if sawCommit {
			return nil, fmt.Errorf("asb: content after the commit frame")
		}
		switch probe.T {
		case FrameHeader:
			if sawHead {
				return nil, fmt.Errorf("asb: more than one header")
			}
			if err := json.Unmarshal(line, &out.Header); err != nil {
				return nil, err
			}
			if err := out.Header.Validate(); err != nil {
				return nil, err
			}
			sawHead = true
		case FrameNode:
			var n Node
			if err := json.Unmarshal(line, &n); err != nil {
				return nil, err
			}
			out.Nodes = append(out.Nodes, n)
		case FrameRelation:
			var v Relation
			if err := json.Unmarshal(line, &v); err != nil {
				return nil, err
			}
			out.Relations = append(out.Relations, v)
		case FrameUnresolved:
			var u Unresolved
			if err := json.Unmarshal(line, &u); err != nil {
				return nil, err
			}
			out.Unresolved = append(out.Unresolved, u)
		case FrameCommit:
			if err := json.Unmarshal(line, &out.Commit); err != nil {
				return nil, err
			}
			sawCommit = true
			continue // the commit frame is not covered by its own digest
		default:
			return nil, fmt.Errorf("asb: unknown frame type %q", probe.T)
		}
		hashed.Write(line)
		hashed.WriteByte('\n')
	}
	if !sawHead {
		return nil, fmt.Errorf("asb: round has no header")
	}
	if !sawCommit {
		return nil, fmt.Errorf("asb: round has no commit frame; it is truncated")
	}
	sum := sha256.Sum256(hashed.Bytes())
	if got := hex.EncodeToString(sum[:]); got != out.Commit.Digest {
		return nil, fmt.Errorf("asb: digest mismatch: computed %s, round claims %s", got[:12], firstN(out.Commit.Digest, 12))
	}
	want := Counts{Nodes: len(out.Nodes), Relations: len(out.Relations), Unresolved: len(out.Unresolved)}
	if want != out.Commit.Counts {
		return nil, fmt.Errorf("asb: counts mismatch: read %+v, round claims %+v", want, out.Commit.Counts)
	}
	return &out, nil
}

func readLine(br *bufio.Reader) ([]byte, error) {
	line, err := br.ReadBytes('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && len(line) > 0 {
			return line, nil
		}
		return nil, err
	}
	return line[:len(line)-1], nil
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

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
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Reader reads a landed file back.
//
// It deliberately uses bufio.Reader rather than bufio.Scanner: real transcript
// lines reach ~1 MB, far past Scanner's 64 KB default token limit, where Scanner
// fails silently rather than loudly.
type Reader struct {
	br  *bufio.Reader
	hdr Header
}

// NewReader consumes the header line and returns a Reader positioned at the
// first record.
func NewReader(r io.Reader) (*Reader, error) {
	br := bufio.NewReaderSize(r, 1<<20)
	line, err := readLine(br)
	if err != nil {
		return nil, fmt.Errorf("record: read header: %w", err)
	}
	rd := &Reader{br: br}
	if err := json.Unmarshal(line, &rd.hdr); err != nil {
		return nil, fmt.Errorf("record: decode header: %w", err)
	}
	if err := rd.hdr.Validate(); err != nil {
		return nil, err
	}
	return rd, nil
}

// Header returns the file's header.
func (r *Reader) Header() Header { return r.hdr }

// Next returns the next record, or io.EOF when the file is exhausted.
//
// Payload aliases the decoded line's bytes and is byte-identical to the source
// record: encoding/json captures a RawMessage verbatim rather than re-encoding.
func (r *Reader) Next() (Record, error) {
	var rec Record
	line, err := readLine(r.br)
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(line, &rec); err != nil {
		return rec, fmt.Errorf("record: decode record: %w", err)
	}
	return rec, nil
}

// readLine returns one newline-terminated line with the newline stripped.
// A final line without a trailing newline is returned; an empty tail is EOF.
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

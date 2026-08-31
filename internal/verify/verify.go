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

// Package verify checks that landed data is internally consistent.
//
// The checks here deliberately need only the landed files. Claude Code prunes
// transcripts - the great majority of session ids in its own prompt history
// have none - so any check that requires the original source is a check that
// usually cannot run.
package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/wu-sheng/AgentSessionizer/pkg/record"
)

// Gap is a discontinuity found while walking a stream's landed records.
type Gap struct {
	File     string
	Row      uint32
	Expected uint64
	Got      uint64
}

func (g Gap) String() string {
	return fmt.Sprintf("%s row %d: expected %d, got %d", filepath.Base(g.File), g.Row, g.Expected, g.Got)
}

// StreamReport is the result of checking one stream or run directory.
type StreamReport struct {
	Dir     string
	Files   int
	Records int

	FirstOrd, LastOrd uint64
	// BytesCovered is the source byte range the landed records account for.
	BytesCovered uint64

	OrdGaps  []Gap // a source line was skipped or repeated
	ByteGaps []Gap // a source byte range is unaccounted for
	ShaBad   []Gap // a payload does not match its recorded digest
}

// OK reports whether the stream is contiguous and intact.
func (r *StreamReport) OK() bool {
	return len(r.OrdGaps) == 0 && len(r.ByteGaps) == 0 && len(r.ShaBad) == 0
}

// Stream checks one stream or run directory for a given landed kind.
//
// It asserts three properties, all provable from the landed files alone:
//
//   - ORD CONTIGUITY: source line numbers run 1, 2, 3 ... with no gap. A gap
//     means the tailer skipped a line, which is otherwise invisible: no error,
//     no conflict, just a slightly shorter conversation.
//   - BYTE CONTIGUITY: off[n] + len(payload[n]) + 1 == off[n+1], the +1 being
//     the newline the source had. This proves every byte of the source prefix
//     is accounted for, without needing the source.
//   - DIGEST: each payload still hashes to the sha recorded beside it,
//     catching a landed file edited after the fact.
func Stream(dir, kind string) (*StreamReport, error) {
	files, err := landedFiles(dir, kind)
	if err != nil {
		return nil, err
	}
	rep := &StreamReport{Dir: dir, Files: len(files)}
	if len(files) == 0 {
		return rep, nil
	}

	var prevOrd uint64
	var prevEnd uint64
	first := true

	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		r, err := record.NewReader(f)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("verify: %s: %w", path, err)
		}
		for row := uint32(1); ; row++ {
			rec, rerr := r.Next()
			if errors.Is(rerr, io.EOF) {
				break
			}
			if rerr != nil {
				f.Close()
				return nil, fmt.Errorf("verify: %s row %d: %w", path, row, rerr)
			}
			rep.Records++

			if first {
				rep.FirstOrd = rec.Ord
				prevOrd = rec.Ord - 1
				prevEnd = rec.Off
				first = false
			}
			if rec.Ord != prevOrd+1 {
				rep.OrdGaps = append(rep.OrdGaps, Gap{path, row, prevOrd + 1, rec.Ord})
			}
			if rec.Off != prevEnd {
				rep.ByteGaps = append(rep.ByteGaps, Gap{path, row, prevEnd, rec.Off})
			}

			body, berr := rec.SourceBytes()
			if berr != nil {
				body = rec.Payload
			}
			sum := sha256.Sum256(body)
			if got := hex.EncodeToString(sum[:])[:12]; got != rec.Sha {
				rep.ShaBad = append(rep.ShaBad, Gap{File: path, Row: row})
			}

			prevOrd = rec.Ord
			// +1 for the newline the source carried but the payload does not.
			prevEnd = rec.Off + uint64(len(body)) + 1
			rep.LastOrd = rec.Ord
		}
		f.Close()
	}
	rep.BytesCovered = prevEnd
	return rep, nil
}

// landedFiles lists a directory's landed files of one kind, in sequence order.
func landedFiles(dir, kind string) ([]string, error) {
	items, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	type sf struct {
		path string
		seq  uint64
	}
	var out []sf
	for _, it := range items {
		name := it.Name()
		if it.IsDir() || !strings.HasPrefix(name, kind+"-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		base := strings.TrimSuffix(name, ".jsonl")
		i := strings.LastIndex(base, "-")
		if i < 0 {
			continue
		}
		seq, err := strconv.ParseUint(base[i+1:], 10, 64)
		if err != nil {
			continue
		}
		out = append(out, sf{filepath.Join(dir, name), seq})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].seq < out[j].seq })
	paths := make([]string, len(out))
	for i, x := range out {
		paths[i] = x.path
	}
	return paths, nil
}

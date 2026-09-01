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
	"fmt"
	"io"
	"os"

	"github.com/wu-sheng/AgentSessionizer/internal/index"
	"github.com/wu-sheng/AgentSessionizer/internal/storage"
	"github.com/wu-sheng/AgentSessionizer/pkg/record"
)

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
	files, err := storage.LandedFiles(z, session)
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
func indexLandedFile(ix *index.Index, lf storage.LandedFile) (int, error) {
	f, err := os.Open(lf.Path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	r, err := record.NewReader(f)
	if err != nil {
		// A landed file whose header will not read describes an unknown amount
		// of the conversation. Continuing would produce an index that silently
		// omits it, and an assembly that looks complete.
		return 0, fmt.Errorf("claudecode: landed file %s is unreadable: %w", lf.Path, err)
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
			return n, fmt.Errorf("claudecode: %s row %d: %w", lf.Path, row, err)
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

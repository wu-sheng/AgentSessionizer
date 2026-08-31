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
	"encoding/json"
	"time"

	"github.com/wu-sheng/AgentSessionizer/internal/index"
)

// indexRecord is the subset of a Claude Code record the index needs.
//
// Only identifiers and shape are decoded. Message text, tool inputs and results
// are deliberately left in the payload: structure resolution needs ids, not
// content, and decoding content would cost far more than it buys.
type indexRecord struct {
	Type       string `json:"type"`
	UUID       string `json:"uuid"`
	ParentUUID string `json:"parentUuid"`
	Timestamp  string `json:"timestamp"`
	PromptID   string `json:"promptId"`
	AgentID    string `json:"agentId"`
	Subtype    string `json:"subtype"`
	Message    struct {
		ID      string          `json:"id"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// contentBlock is one element of a record's content array.
type contentBlock struct {
	Type      string `json:"type"`
	ID        string `json:"id"`          // tool_use
	Name      string `json:"name"`        // tool_use
	ToolUseID string `json:"tool_use_id"` // tool_result
}

var recordKinds = map[string]index.Kind{
	"user":       index.KindUser,
	"assistant":  index.KindAssistant,
	"attachment": index.KindAttachment,
	"system":     index.KindSystem,
}

// IndexEntry extracts an index entry and its joinable blocks from one landed
// record.
//
// It is called while the collector still holds the bytes, so indexing costs a
// parse but no second read of the corpus.
func IndexEntry(ix *index.Index, src Source, seq uint32, row uint32,
	payload []byte) (index.Entry, []index.Block) {

	in := ix.Strings
	e := index.Entry{
		Seq: seq, Row: row,
		Stream: in.ID(src.Stream), Run: in.ID(src.RunID),
		Kind: sourceKind(src),
	}

	// A workflow script is JavaScript, and a manifest is a whole document; there
	// are no record identifiers to extract from either.
	if src.Kind == SrcWorkflowScript || src.Kind == SrcWorkflowManifest {
		return e, nil
	}

	var d indexRecord
	if err := json.Unmarshal(payload, &d); err != nil {
		// A record that will not parse still gets an entry: its position and
		// stream are known, and losing it would make the index disagree with
		// the landed data it describes.
		return e, nil
	}

	if k, ok := recordKinds[d.Type]; ok {
		e.Kind = k
	} else if src.Kind == SrcAgentMeta {
		e.Kind = index.KindMeta
	} else if src.Kind == SrcJournal {
		e.Kind = index.KindJournal
	} else if d.Type != "" {
		e.Kind = index.KindOther
	}

	// This is the only place Claude Code's field names meet the index. Each maps
	// onto a role; nothing downstream learns where the value came from, so a
	// rename in Claude Code stops here.
	//
	//   uuid       -> Record    this record's own id
	//   parentUuid -> Parent    its containment parent
	//   message.id -> Call      the provider call it is a fragment of
	//   promptId   -> Cycle     the prompt cycle
	//
	// Two fields are deliberately NOT mapped. agentId equals the stream name for
	// a child stream, so indexing it would store the same value twice. requestId
	// identifies a provider attempt, which no resolution step reads - calls group
	// by message.id. It is the join key to captured provider bodies, so it will
	// return when those do; the index is derived, so adding a field back costs
	// one rebuild.
	e.Record = in.ID(d.UUID)
	e.Parent = in.ID(d.ParentUUID)
	e.Call = in.ID(d.Message.ID)
	e.Cycle = in.ID(d.PromptID)
	if d.Timestamp != "" {
		if t, err := time.Parse(time.RFC3339Nano, d.Timestamp); err == nil {
			e.TS = t.UnixNano()
		}
	}

	return e, indexBlocks(in, d.Message.Content)
}

// indexBlocks extracts the joinable blocks of a record's content array.
//
// Every element is inspected. A line usually carries one content block but not
// always, and reading only the first would silently drop tool ids and break the
// join they exist to serve.
func indexBlocks(in *index.Interner, content json.RawMessage) []index.Block {
	if len(content) == 0 {
		return nil
	}
	var blocks []contentBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil // content is a bare string, which carries no joinable id
	}
	var out []index.Block
	for i, b := range blocks {
		switch b.Type {
		case "tool_use", "server_tool_use":
			out = append(out, index.Block{
				Ord: uint16(i), Kind: index.BlockToolUse,
				ToolID: in.ID(b.ID), Name: in.ID(b.Name),
			})
		case "tool_result":
			out = append(out, index.Block{
				Ord: uint16(i), Kind: index.BlockToolResult, ToolID: in.ID(b.ToolUseID),
			})
		}
	}
	return out
}

func sourceKind(src Source) index.Kind {
	switch src.Kind {
	case SrcAgentMeta:
		return index.KindMeta
	case SrcJournal:
		return index.KindJournal
	case SrcWorkflowManifest:
		return index.KindManifest
	case SrcWorkflowScript:
		return index.KindScript
	}
	return index.KindUnknown
}

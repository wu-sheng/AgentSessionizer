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
	"strings"
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

	// LogicalParentUUID appears on context-reset boundaries and on no other
	// record type. It is the explicit pointer to the last message before the
	// reset, which is what makes an epoch observed rather than guessed. A
	// boundary's own parentUuid is null on every boundary measured, so this is
	// the only link back.
	LogicalParentUUID string `json:"logicalParentUuid"`
	IsCompactSummary  bool   `json:"isCompactSummary"`
	IsMeta            bool   `json:"isMeta"`

	// ParentAgentID appears on a nested child's sidecar. Without it a child of a
	// child is attributed to the session's main stream, which flattens the whole
	// nesting.
	ParentAgentID string `json:"parentAgentId"`

	Origin origin `json:"origin"`

	Message struct {
		ID         string          `json:"id"`
		Model      string          `json:"model"`
		StopReason *string         `json:"stop_reason"`
		Content    json.RawMessage `json:"content"`
	} `json:"message"`

	// ToolUseResult must stay raw. It is an OBJECT most of the time and a bare
	// string the rest of the time, so decoding it into a struct fails on those
	// records - and a failed decode of the whole record loses every identifier
	// on it, not just this field.
	ToolUseResult json.RawMessage `json:"toolUseResult"`

	Attachment struct {
		Type        string `json:"type"`
		CommandMode string `json:"commandMode"`
		Origin      origin `json:"origin"`
	} `json:"attachment"`
}

// origin says what caused a prompt cycle. It is carried on the record itself and
// also, for a large share of cases, on the attachment inside it.
type origin struct {
	Kind string `json:"kind"`
}

// toolResult is the object form of a tool result's runtime enrichment.
type toolResult struct {
	AgentID       string `json:"agentId"`
	ToolUseID     string `json:"toolUseId"`
	Status        string `json:"status"`
	RunID         string `json:"runId"`
	TranscriptDir string `json:"transcriptDir"`
	TaskID        string `json:"taskId"`
}

// decodeToolResult reads the enrichment only when it is an object.
func decodeToolResult(raw json.RawMessage) (toolResult, bool) {
	var t toolResult
	if len(raw) == 0 || raw[0] != '{' {
		return t, false
	}
	if err := json.Unmarshal(raw, &t); err != nil {
		return t, false
	}
	return t, true
}

// Launch statuses a tool result can report.
const (
	statusAsyncLaunched = "async_launched" // a child started; this is NOT its result
	statusForked        = "forked"         // a skill fork; the child has no sidecar pointer
	statusCompleted     = "completed"      // a synchronous return; this IS the result
)

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

// syntheticModel is the model name on an assistant-role record the CLIENT
// produced - a connection loss, a provider error, a quota or authentication
// failure - rather than one a provider returned.
//
// The test is the model name, not a missing request id: about a quarter of these
// records carry a request id, and a genuine call exists that carries none, so
// request id misclassifies in both directions.
const syntheticModel = "<synthetic>"

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
		Stream: in.ID(src.Stream), Batch: in.ID(src.RunID),
		Kind: sourceKind(src),
	}

	// A workflow script is JavaScript and a manifest is a whole document; there
	// are no record identifiers to extract from either.
	if src.Kind == SrcWorkflowScript || src.Kind == SrcWorkflowManifest {
		return e, nil
	}

	var d indexRecord
	if err := json.Unmarshal(payload, &d); err != nil {
		// A record that will not parse still gets an entry: its position and
		// stream are known, and losing it would make the index disagree with the
		// landed data it describes.
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
	//   uuid              -> Record    this record's own id
	//   parentUuid        -> Parent    its containment parent
	//   message.id        -> Call      the provider call it is a fragment of
	//   promptId          -> Run       the agent loop this record is part of
	//   logicalParentUuid -> Logical   the continuation point across a reset
	//   origin.kind       -> Trigger   what triggered that loop
	//
	// Two fields are deliberately NOT mapped. agentId equals the stream name for
	// a child stream, so indexing it would store the same value twice. requestId
	// identifies a provider attempt, which no resolution step reads - calls group
	// by message.id, because a record the client produced can carry a real call's
	// request id and put invented content inside it. requestId is the join key to
	// captured provider bodies, so it will return when those do; the index is
	// derived, so adding a field back costs one rebuild.
	e.Record = in.ID(d.UUID)
	e.Parent = in.ID(d.ParentUUID)
	e.Call = in.ID(d.Message.ID)
	e.Run = in.ID(d.PromptID)
	e.Continues = in.ID(d.LogicalParentUUID)
	if d.Timestamp != "" {
		if t, err := time.Parse(time.RFC3339Nano, d.Timestamp); err == nil {
			e.TS = t.UnixNano()
		}
	}

	tur, hasTUR := decodeToolResult(d.ToolUseResult)
	e.Trigger = triggerOf(&d)
	e.Flags = flagsOf(&d, &tur, hasTUR, src)
	anchor, spawn := linksOf(&d, &tur, src)
	e.Tool, e.Child = in.ID(anchor), in.ID(spawn)
	if hasTUR && tur.RunID != "" && e.Batch == 0 {
		// A workflow's launch result names the orchestration whose journal and
		// child streams landed under their own directory. This is what connects
		// the parent's call to that group without reading either.
		e.Batch = in.ID(tur.RunID)
	}

	return e, indexBlocks(in, d.Message.Content)
}

// triggerOf reads what caused this record's prompt cycle.
//
// The marker sits on the record and, for a substantial share, on the attachment
// inside it instead. Reading only the record misses more than a quarter of them.
// The value is an open set - a third kind exists beyond the two obvious ones -
// so anything unrecognised maps to "other" rather than being treated as absent.
func triggerOf(d *indexRecord) index.Trigger {
	kind := d.Origin.Kind
	if kind == "" {
		kind = d.Attachment.Origin.Kind
	}
	switch kind {
	case "":
		return index.TriggerNone
	case "human":
		return index.TriggerExternal
	case "task-notification":
		return index.TriggerNotification
	}
	return index.TriggerOther
}

// flagsOf reduces the record's shape to the bits a lookup needs.
func flagsOf(d *indexRecord, tur *toolResult, hasTUR bool, src Source) index.Flags {
	var f index.Flags
	if d.Type == "assistant" {
		if d.Message.Model == syntheticModel {
			f |= index.FlagSynthetic
		}
		// A stop reason says the provider finished, which is what makes the usage
		// numbers on this call real. It does NOT identify the last fragment: a
		// main transcript stamps the same stop reason on every fragment of a call.
		// Only line order finds the last one.
		if d.Message.StopReason != nil && *d.Message.StopReason != "" {
			f |= index.FlagStopReason
		}
	}
	if d.Type == "system" {
		switch d.Subtype {
		case "compact_boundary":
			f |= index.FlagEpochBoundary
		case "api_error":
			f |= index.FlagError
		}
	}
	if d.IsCompactSummary {
		f |= index.FlagEpochSummary
	}
	if hasTUR {
		switch tur.Status {
		case statusAsyncLaunched:
			// The acknowledgement is not the result. Counting it as one records an
			// empty output for every asynchronous delegation.
			f |= index.FlagLaunchAck
		case statusCompleted:
			// A synchronous return. This one IS the child's result, and it repeats
			// the child's own output, so a renderer has to drop the copy rather
			// than show the same work twice.
			f |= index.FlagChildResult
		}
	}

	// An attachment is material the harness put into model context. One kind is
	// not: a queued command the person typed, which exists ONLY as an attachment
	// and is otherwise lost.
	//
	// The command mode is what separates them, and both halves of it are needed.
	// Matching the attachment type alone reads completion-notification blobs as
	// things the user typed, and in some sessions there are more of those than
	// genuine prompts.
	//
	// Everything else is injected. That is the DEFAULT rather than a list,
	// because a list has to be right about types that do not exist yet. An
	// earlier version of this code kept one, and it was wrong in both
	// directions at once: 635 records across 13 real attachment types fell
	// through it and were dropped from the conversation entirely, while 5 types
	// on the list appear nowhere in the corpus. Defaulting to injection cannot
	// lose a record; at worst it over-reports one, and a reader is told what it
	// was either way.
	if d.Type == "attachment" {
		if d.Attachment.Type == "queued_command" && d.Attachment.CommandMode == "prompt" {
			f |= index.FlagExternalInput
		} else {
			f |= index.FlagInjection
		}
	}
	if d.Type == "user" && d.Origin.Kind == "human" && !d.IsMeta {
		f |= index.FlagExternalInput
	}
	if d.IsMeta {
		// A meta record is text the harness wrote, placed in the user role. It is
		// injected context, not something a person said.
		f |= index.FlagInjection
		f &^= index.FlagExternalInput
	}
	if src.Kind == SrcAgentMeta || src.Kind == SrcJournal {
		f &^= index.FlagExternalInput
	}
	return f
}

// linksOf extracts the two cross-record links that are not content blocks.
//
// Several mechanisms start a child agent and no single field covers them, so
// each is read where it lives:
//
//	Workflow    the launch result names a run; the run's journal names every
//	            child it starts. This is by far the largest source - most
//	            children on disk are reached this way and no other.
//	Agent tool  the parent's result names the child. The status says whether it
//	            is an acknowledgement or a completed return.
//	Skill fork  only the parent's result names the child.
//	Nesting     a child's sidecar names its parent agent. Without it a child of
//	            a child is attributed to the main stream.
//
// Completion is a further path: a notification names the call it completes and
// the task that finished, and it can land in a different file from that call,
// so both ids are extracted here and resolved across the whole session.
//
// It returns strings; the caller interns them, so this stays a statement about
// Claude Code alone.
func linksOf(d *indexRecord, tur *toolResult, src Source) (anchor, spawn string) {
	switch src.Kind {
	case SrcAgentMeta:
		// The sidecar is not a general parent pointer: it names a parent only for
		// a nested child. For everything else it holds only the agent's type and
		// depth, and the join has to come from the parent side or the journal.
		spawn = d.AgentID
		if spawn == "" {
			spawn = src.Stream
		}
		anchor = tur.ToolUseID
	case SrcJournal:
		// A journal record announces a child before its transcript exists, so an
		// unresolved reference here is expected for a while rather than an error.
		spawn = d.AgentID
	default:
		anchor, spawn = tur.ToolUseID, tur.AgentID
	}

	if a, s := notificationIDs(d.Message.Content); a != "" || s != "" {
		if anchor == "" {
			anchor = a
		}
		if spawn == "" {
			spawn = s
		}
	}
	return anchor, spawn
}

// ParentAgent returns the agent a nested child's sidecar names as its parent.
func ParentAgent(payload []byte) string {
	var d indexRecord
	if err := json.Unmarshal(payload, &d); err != nil {
		return ""
	}
	return d.ParentAgentID
}

// notificationIDs pulls the call id and the task id out of a completion
// notification.
//
// The call id is the primary key. The task id is only sometimes an agent id -
// it is a generic handle whose meaning depends on what was launched, so joining
// on it unconditionally matches the wrong thing most of the time. It is returned
// only when it has an agent id's shape.
//
// The status element is deliberately not read: it is missing entirely on a large
// class of notifications, so anything requiring it would fail on them.
func notificationIDs(content json.RawMessage) (anchor, spawn string) {
	if len(content) == 0 || content[0] != '"' {
		return "", "" // structured content carries no notification
	}
	var text string
	if err := json.Unmarshal(content, &text); err != nil {
		return "", ""
	}
	if !strings.Contains(text, "<task-notification>") {
		return "", ""
	}
	anchor = between(text, "<tool-use-id>", "</tool-use-id>")
	// A single notification can name several tasks. Only one that looks like an
	// agent identifier is usable as a stream name.
	rest := text
	for {
		id := between(rest, "<task-id>", "</task-id>")
		if id == "" {
			break
		}
		if IsAgentID(id) {
			spawn = id
			break
		}
		i := strings.Index(rest, "</task-id>")
		if i < 0 {
			break
		}
		rest = rest[i+len("</task-id>"):]
	}
	return anchor, spawn
}

func between(s, openTag, closeTag string) string {
	i := strings.Index(s, openTag)
	if i < 0 {
		return ""
	}
	rest := s[i+len(openTag):]
	j := strings.Index(rest, closeTag)
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:j])
}

// indexBlocks extracts the joinable blocks of a record's content array.
//
// Every element is inspected. A line usually carries one content block but not
// always - a provider call fans out to as many as sixteen tool uses - and
// reading only the first would silently drop tool ids and break the join they
// exist to serve.
func indexBlocks(in *index.Interner, content json.RawMessage) []index.Block {
	if len(content) == 0 {
		return nil
	}
	var blocks []contentBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil // content is a bare string, which carries no joinable id
	}
	out := make([]index.Block, 0, len(blocks))
	for i, b := range blocks {
		blk := index.Block{Ord: uint16(i)}
		switch b.Type {
		case "tool_use", "server_tool_use":
			blk.Kind, blk.ToolID, blk.Name = index.BlockToolUse, in.ID(b.ID), in.ID(b.Name)
		case "tool_result":
			blk.Kind, blk.ToolID = index.BlockToolResult, in.ID(b.ToolUseID)
		case "text":
			blk.Kind = index.BlockText
		case "thinking", "redacted_thinking":
			blk.Kind = index.BlockThinking
		default:
			blk.Kind = index.BlockOther
		}
		out = append(out, blk)
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

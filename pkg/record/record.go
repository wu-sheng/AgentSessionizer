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

// Package record defines the landed record envelope: the single on-disk shape
// every collected source record takes, regardless of which adapter produced it.
//
// A landed file is JSON Lines. The first line is a Header carrying everything
// invariant for the file; every subsequent line is a Record carrying only what
// varies, with the source bytes preserved verbatim in Payload.
package record

import (
	"encoding/json"
	"fmt"
)

// Kind identifies what a landed file was collected from.
type Kind string

const (
	KindTranscript       Kind = "transcript"
	KindAgentMeta        Kind = "agent_meta"
	KindJournal          Kind = "journal"
	KindWorkflowManifest Kind = "workflow_manifest"
	KindWorkflowScript   Kind = "workflow_script"
	KindOTLPLog          Kind = "otlp_log"  // reserved, Phase 2
	KindOTLPSpan         Kind = "otlp_span" // reserved, Phase 2
	KindProviderBody     Kind = "provider_body"
	KindDerived          Kind = "derived"
)

// ContentState describes payload availability. Collection never redacts, so
// Phase 1 always emits StateAvailable; the field exists for privacy modes.
type ContentState string

const (
	StateAvailable ContentState = "available"
	// StateRaw means the payload is a JSON string holding source bytes that were
	// not themselves valid JSON. The bytes are preserved exactly; only their
	// encoding differs, so a landed file always stays parseable.
	StateRaw         ContentState = "raw"
	StateRedacted    ContentState = "redacted"
	StateTruncated   ContentState = "truncated"
	StateOmitted     ContentState = "omitted"
	StateUnavailable ContentState = "unavailable"
)

// Header is the first line of a landed file. Fields here are constant for the
// whole file, which is what keeps per-record overhead small and makes a file
// interpretable if it is ever moved out of its directory.
type Header struct {
	H          int          `json:"h"` // envelope schema version, always 1
	Seq        uint64       `json:"seq"`
	At         string       `json:"at"` // receipt time, RFC3339 with nanoseconds
	Kind       Kind         `json:"kind"`
	Adapter    string       `json:"adapter"` // e.g. "claude-code-local/0.1.0"
	Src        string       `json:"src"`     // source path, relative to the adapter's source root
	Session    string       `json:"session"`
	Stream     string       `json:"stream,omitempty"` // "main" or an agent id; empty for non-stream kinds
	State      ContentState `json:"state"`
	Derivation string       `json:"derivation,omitempty"` // set when the payload is not the raw source bytes
}

// Record is one landed source record.
//
// Ord and Off are positions in the SOURCE, not in the landed file: a delta
// starts mid-source, so the two diverge after the first pass. They are what
// lets any landed record be pointed back at its exact origin.
type Record struct {
	Ord uint64 `json:"ord"` // 1-based line number in the source
	Off uint64 `json:"off"` // byte offset in the source
	Sha string `json:"sha"` // sha256 of the SOURCE bytes, first 12 hex chars

	// State overrides the header's content state for this record alone. It is
	// set to StateRaw when the source bytes were not valid JSON and had to be
	// wrapped as a string to keep the landed file parseable.
	State ContentState `json:"state,omitempty"`

	Payload json.RawMessage `json:"payload"`
}

// SourceBytes returns the original source bytes for a record, undoing the
// string wrapping applied to a StateRaw payload.
func (r *Record) SourceBytes() ([]byte, error) {
	if r.State != StateRaw {
		return r.Payload, nil
	}
	var s string
	if err := json.Unmarshal(r.Payload, &s); err != nil {
		return nil, fmt.Errorf("record: decode raw payload: %w", err)
	}
	return []byte(s), nil
}

// Validate reports whether a header is well formed.
func (h *Header) Validate() error {
	switch {
	case h.H != 1:
		return fmt.Errorf("record: unsupported envelope version %d", h.H)
	case h.Kind == "":
		return fmt.Errorf("record: header missing kind")
	case h.Session == "":
		return fmt.Errorf("record: header missing session")
	case h.Src == "":
		return fmt.Errorf("record: header missing src")
	}
	return nil
}

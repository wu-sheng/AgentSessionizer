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

package local_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wu-sheng/AgentSessionizer/internal/adapters/claudecode"
	"github.com/wu-sheng/AgentSessionizer/internal/index"
	"github.com/wu-sheng/AgentSessionizer/internal/storage"
)

// The shapes below are all present in the fixture. Presence is not coverage, so
// each has an assertion here naming the wrong output it prevents.

// TestSyntheticRecordIsDistinguishable guards a record that carries the
// assistant role but that no model produced. Counted as agent output it
// corrupts token statistics and attributes text to the agent it never wrote.
func TestSyntheticRecordIsDistinguishable(t *testing.T) {
	c := collect(t)
	var synthetic, genuine int
	for _, rec := range c.mainRecords() {
		b, err := rec.SourceBytes()
		if err != nil {
			t.Fatal(err)
		}
		var d struct {
			Type    string `json:"type"`
			Message struct {
				ID    string `json:"id"`
				Model string `json:"model"`
			} `json:"message"`
		}
		if json.Unmarshal(b, &d) != nil || d.Type != "assistant" {
			continue
		}
		// The discriminator is the model, not a null request id: real corpora
		// contain synthetics carrying a request id, and genuine calls carrying none.
		if d.Message.Model == "<synthetic>" {
			synthetic++
			if strings.HasPrefix(d.Message.ID, "msg_") {
				t.Error("a synthetic record carries a provider message id")
			}
		} else {
			genuine++
		}
	}
	if synthetic != 1 {
		t.Errorf("found %d synthetic records, want 1", synthetic)
	}
	if genuine == 0 {
		t.Error("no genuine assistant records — the discriminator matched everything")
	}
}

// TestUsageFragmentsArePreservedInOrder pins the shape the "take the last
// fragment" rule depends on. A child stream stamps streaming partials on every
// fragment but the last; summing them inflates output tokens several-fold.
func TestUsageFragmentsArePreservedInOrder(t *testing.T) {
	c := collect(t)
	var outputs []int
	for _, rec := range c.records(c.zone.StreamDir(case1, agentChecker), "transcript") {
		b, _ := rec.SourceBytes()
		var d struct {
			Message struct {
				ID    string `json:"id"`
				Usage struct {
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(b, &d) == nil && d.Message.ID == "checker-call1" {
			outputs = append(outputs, d.Message.Usage.OutputTokens)
		}
	}
	if len(outputs) != 2 {
		t.Fatalf("call has %d fragments, want 2", len(outputs))
	}
	// Monotone non-decreasing, last is the authoritative total.
	if outputs[0] >= outputs[1] {
		t.Errorf("output_tokens = %v, want a partial followed by a larger total", outputs)
	}
	if sum := outputs[0] + outputs[1]; sum == outputs[1] {
		t.Error("fixture does not exercise the summing hazard")
	}
}

// TestCompactionSummaryFollowsItsBoundary covers a pair whose order cannot be
// taken from timestamps: the summary is written with an EARLIER timestamp than
// the boundary that owns it.
func TestCompactionSummaryFollowsItsBoundary(t *testing.T) {
	c := collect(t)
	recs := c.mainRecords()
	boundaryAt := -1
	var boundaryUUID, boundaryTS string
	for i, rec := range recs {
		b, _ := rec.SourceBytes()
		var d struct {
			Type, Subtype, UUID, Timestamp string
		}
		if json.Unmarshal(b, &d) == nil && d.Subtype == "compact_boundary" {
			boundaryAt, boundaryUUID, boundaryTS = i, d.UUID, d.Timestamp
		}
	}
	if boundaryAt < 0 {
		t.Fatal("no compaction boundary in the fixture")
	}
	if boundaryAt+1 >= len(recs) {
		t.Fatal("boundary is the last record; no summary follows")
	}
	b, _ := recs[boundaryAt+1].SourceBytes()
	var s struct {
		ParentUUID       string `json:"parentUuid"`
		IsCompactSummary bool   `json:"isCompactSummary"`
		Timestamp        string `json:"timestamp"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if !s.IsCompactSummary {
		t.Error("the record after a boundary is not its summary")
	}
	if s.ParentUUID != boundaryUUID {
		t.Errorf("summary parent = %q, want the boundary %q", s.ParentUUID, boundaryUUID)
	}
	if s.Timestamp >= boundaryTS {
		t.Errorf("fixture does not reproduce the inverted timestamps that make "+
			"ordering an epoch by time wrong: summary %s, boundary %s", s.Timestamp, boundaryTS)
	}
}

// TestInjectedContextIsCollected covers material the harness pushed into model
// context. It shapes a response as much as the human turn does, and dropping it
// loses part of what the model actually saw.
func TestInjectedContextIsCollected(t *testing.T) {
	c := collect(t)
	var attachments int
	for _, rec := range c.mainRecords() {
		b, _ := rec.SourceBytes()
		var d struct {
			Type       string `json:"type"`
			Attachment struct {
				Type string `json:"type"`
			} `json:"attachment"`
		}
		if json.Unmarshal(b, &d) == nil && d.Type == "attachment" {
			attachments++
			if d.Attachment.Type == "" {
				t.Error("an attachment landed without its type")
			}
		}
	}
	if attachments == 0 {
		t.Error("injected context was not collected")
	}
}

// TestOutOfBandRecordsAreCollectedButCarryNoIdentity covers host artefacts that
// must survive collection yet become no conversation node: they carry no record
// id, so nothing can attach to them.
func TestOutOfBandRecordsAreCollectedButCarryNoIdentity(t *testing.T) {
	c := collect(t)
	seen := map[string]bool{}
	for _, rec := range c.mainRecords() {
		b, _ := rec.SourceBytes()
		var d struct{ Type, UUID string }
		if json.Unmarshal(b, &d) != nil {
			continue
		}
		switch d.Type {
		case "bridge-session", "queue-operation", "ai-title":
			seen[d.Type] = true
			if d.UUID != "" {
				t.Errorf("%s carries a record id; it would become a conversation node", d.Type)
			}
		}
	}
	for _, want := range []string{"bridge-session", "queue-operation", "ai-title"} {
		if !seen[want] {
			t.Errorf("out-of-band record %q was not collected", want)
		}
	}

	// They are indexed for completeness, but with no identity to join on.
	ix, ok, _ := index.Load(c.zone.IndexDir(case1), case1)
	if !ok {
		t.Fatal("no index")
	}
	ix.Build()
	var noIdentity int
	for i := range ix.Entries {
		if ix.Entries[i].Record == 0 {
			noIdentity++
		}
	}
	if noIdentity == 0 {
		t.Error("expected indexed entries with no record id")
	}
}

// TestNonSessionFilesAreIgnored covers a file inside a project directory whose
// name is not a session id. Treating it as one would produce an empty
// conversation with a nonsense identity.
func TestNonSessionFilesAreIgnored(t *testing.T) {
	src := fixtureRoot(t)
	sessions, err := claudecode.Discover(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sessions {
		if !claudecode.IsSessionID(s.ID) {
			t.Errorf("discovered %q, which is not a session id", s.ID)
		}
		for _, srcFile := range s.Sources {
			if strings.Contains(srcFile.Rel, "not-a-uuid") {
				t.Errorf("collected %s, which is not part of any session", srcFile.Rel)
			}
		}
	}
	if len(sessions) != 2 {
		t.Errorf("discovered %d sessions, want 2", len(sessions))
	}
}

var _ = storage.StreamMain

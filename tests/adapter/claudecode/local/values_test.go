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

	"github.com/wu-sheng/AgentSessionizer/internal/index"
	"github.com/wu-sheng/AgentSessionizer/internal/storage"
	"github.com/wu-sheng/AgentSessionizer/internal/verify"
)

// find returns the first landed record whose payload contains all substrings.
func (c *collected) find(dir, kind string, want ...string) []byte {
	c.t.Helper()
	for _, rec := range c.records(dir, kind) {
		body, err := rec.SourceBytes()
		if err != nil {
			continue
		}
		ok := true
		for _, w := range want {
			if !strings.Contains(string(body), w) {
				ok = false
				break
			}
		}
		if ok {
			return body
		}
	}
	c.t.Fatalf("no landed record in %s matching %v", dir, want)
	return nil
}

// TestConversationValuesSurvive asserts the material a person would actually
// read comes through intact: what they typed, what the agent said, the command
// it ran, and what came back.
func TestConversationValuesSurvive(t *testing.T) {
	c := collect(t)
	mainDir := c.zone.StreamDir(case1, storage.StreamMain)

	t.Run("human input", func(t *testing.T) {
		b := c.find(mainDir, "transcript", `"origin":{"kind":"human"}`)
		var d struct {
			Message struct {
				Content []struct{ Text string } `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(b, &d); err != nil {
			t.Fatal(err)
		}
		if got := d.Message.Content[0].Text; got != "run the build" {
			t.Errorf("human input = %q, want %q", got, "run the build")
		}
	})

	t.Run("tool command and result", func(t *testing.T) {
		// The command IS the tool's input - for a shell tool there is nothing else.
		call := c.find(mainDir, "transcript", `"name":"Bash"`)
		if !strings.Contains(string(call), `"command":"make build"`) {
			t.Errorf("tool command lost: %s", call)
		}
		res := c.find(mainDir, "transcript", `"tool_use_id":"tool-run-make-build"`, `"tool_result"`)
		if !strings.Contains(string(res), `"stdout":"build succeeded"`) {
			t.Errorf("toolUseResult enrichment lost: %s", res)
		}
	})

	t.Run("agent output lives in the child stream", func(t *testing.T) {
		child := c.find(c.zone.StreamDir(case1, agentChecker), "transcript", "Tests pass.")
		if !strings.Contains(string(child), `"checker-call1"`) {
			t.Errorf("child output not in the child stream: %s", child)
		}
		// The parent must reference the child, never absorb its messages.
		for _, rec := range c.mainRecords() {
			b, _ := rec.SourceBytes()
			if strings.Contains(string(b), "Tests pass.") {
				t.Error("the parent stream absorbed the child's output")
			}
		}
	})

	t.Run("spawn chain is exact", func(t *testing.T) {
		launch := c.find(mainDir, "transcript", `"status":"async_launched"`)
		if !strings.Contains(string(launch), `"agentId":"`+agentChecker+`"`) {
			t.Error("launch acknowledgement lost its agent id")
		}
		note := c.find(mainDir, "transcript", "task-notification")
		for _, want := range []string{"<task-id>" + agentChecker, "<tool-use-id>tool-spawn-checker", "<status>completed"} {
			if !strings.Contains(string(note), want) {
				t.Errorf("completion notification missing %q", want)
			}
		}
		// The child's sidecar carries the reverse edge.
		meta := c.find(c.zone.StreamDir(case1, agentChecker), "meta", "toolUseId")
		if !strings.Contains(string(meta), `"toolUseId":"tool-spawn-checker"`) {
			t.Errorf("sidecar lost its parent pointer: %s", meta)
		}
	})

	t.Run("epoch boundary keeps its explicit backward pointer", func(t *testing.T) {
		b := c.find(mainDir, "transcript", "compact_boundary")
		if !strings.Contains(string(b), `"logicalParentUuid":"call4-final-answer"`) {
			t.Errorf("logicalParentUuid lost - epochs would have to be inferred: %s", b)
		}
	})

	t.Run("workflow run keeps journal, manifest and script together", func(t *testing.T) {
		runDir := c.zone.RunDir(case1, runBuildCheck)
		if j := c.find(runDir, "journal", `"type":"started"`); !strings.Contains(string(j), agentWFStep) {
			t.Error("journal lost its agent id")
		}
		if m := c.find(runDir, "manifest", `"runId"`); !strings.Contains(string(m), `"taskId":"task-buildcheck"`) {
			t.Error("manifest lost the taskId that resolves task notifications")
		}
		// A script is JavaScript, not JSON: it must round trip as raw.
		s := c.find(runDir, "script", "export const meta")
		if !strings.Contains(string(s), "name: 'build-check'") {
			t.Errorf("script content lost: %s", s)
		}
	})
}

// TestIndexMapsRolesNotRuntimeFields asserts the vocabulary boundary: the index
// stores roles, and a runtime's field names stop at the adapter.
func TestIndexMapsRolesNotRuntimeFields(t *testing.T) {
	c := collect(t)
	ix, ok, err := index.Load(c.zone.IndexDir(case1), case1)
	if err != nil || !ok {
		t.Fatalf("no index: ok=%v err=%v", ok, err)
	}
	ix.Build()

	// uuid -> Record
	if _, found := ix.EntryByRecord("turn1-human-input"); !found {
		t.Error("record id not indexed")
	}
	// message.id -> Call. One call, three fragments.
	if got := ix.ProviderCall("call1"); len(got) != 3 {
		t.Errorf("provider call msg_A has %d fragments, want 3", len(got))
	}
	// promptId -> Cycle. Two cycles in one Talk: the human turn and the
	// notification that woke the agent when the child finished.
	cycles := map[uint32]bool{}
	for i := range ix.Entries {
		if e := &ix.Entries[i]; e.Cycle != 0 && ix.Strings.String(e.Stream) == storage.StreamMain {
			cycles[e.Cycle] = true
		}
	}
	if len(cycles) != 2 {
		t.Errorf("main stream has %d prompt cycles, want 2", len(cycles))
	}
	// A server-side tool id would be dropped by a ^toolu_ regex.
	if got := ix.ToolBlocks("srvtool-websearch"); len(got) != 2 {
		t.Errorf("srvtool-websearch resolved to %d blocks, want 2 (use + result)", len(got))
	}
	if got := ix.ToolBlocks("tool-run-make-build"); len(got) != 2 {
		t.Errorf("tool-run-make-build resolved to %d blocks, want 2", len(got))
	}
	// Streams: main plus two children, all under one session.
	if got := ix.Streams(); len(got) != 3 {
		t.Errorf("streams = %v, want main + 2 children", got)
	}
}

// TestLandedDataIsContiguousAndIntact runs the same audit `asz verify` runs.
func TestLandedDataIsContiguousAndIntact(t *testing.T) {
	c := collect(t)
	for _, s := range []string{case1, case2} {
		rep, err := verify.Session(c.zone, s)
		if err != nil {
			t.Fatal(err)
		}
		if !rep.OK() {
			t.Errorf("%s: %d problem(s): %v", s, rep.Problems, rep.Details())
		}
		if rep.Records == 0 {
			t.Errorf("%s: no records verified", s)
		}
	}
}

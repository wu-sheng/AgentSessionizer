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
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// transcript builds a Claude Code session incrementally, so a test can append
// turns between collection rounds - which is the mode that actually runs.
//
// Ids are shaped the way discovery requires (session ids are UUID-shaped, agent
// ids are `a` plus 16 hex) but chosen to be recognisable. Everything
// unconstrained says what it is.
type transcript struct {
	t       *testing.T
	root    string // the fixture source root
	slug    string // project directory, a slugified working directory
	session string
	lines   []string // main-stream records, appended across turns
	turn    int
	agents  map[string][]string // child stream -> its records
}

const growSession = "add1c7ed-0003-4000-8000-000000000003" // the growing session

func newTranscript(t *testing.T, root string) *transcript {
	return &transcript{
		t: t, root: root, slug: "-Users-dev-growing-work", session: growSession,
		agents: map[string][]string{},
	}
}

func (x *transcript) rec(m map[string]any) string {
	for k, v := range map[string]any{
		"sessionId": x.session, "version": "2.1.245", "userType": "external",
		"entrypoint": "cli", "cwd": "/Users/dev/growing-work", "gitBranch": "main",
	} {
		if _, ok := m[k]; !ok {
			m[k] = v
		}
	}
	if _, ok := m["isSidechain"]; !ok {
		m["isSidechain"] = false
	}
	b, err := json.Marshal(m)
	if err != nil {
		x.t.Fatal(err)
	}
	return string(b)
}

func usageFull(out int) map[string]any {
	return map[string]any{
		"input_tokens": 2, "cache_creation_input_tokens": 100,
		"cache_read_input_tokens": 900, "output_tokens": out,
		"service_tier": "standard", "speed": "standard",
		"iterations": []map[string]any{{"type": "message"}}, "server_tool_use": map[string]any{},
	}
}

// AddTurn appends one complete human turn: the prompt, a provider call split
// across fragments, a tool call and its result, and a final answer.
//
// withAgent additionally spawns a child agent and lands the async return chain -
// launch acknowledgement, child stream, completion notification, parent resume -
// which is a second prompt cycle inside the SAME Talk.
func (x *transcript) AddTurn(prompt string, withAgent bool) {
	x.turn++
	n := x.turn
	id := func(f string, a ...any) string { return fmt.Sprintf("t%d-%s", n, fmt.Sprintf(f, a...)) }
	ts := func(s int) string { return fmt.Sprintf("2026-02-%02dT10:%02d:%02d.000Z", n, s/60, s%60) }

	x.lines = append(x.lines,
		x.rec(map[string]any{
			"type": "user", "uuid": id("human-input"), "parentUuid": nil,
			"promptId": id("cycle-human"), "origin": map[string]any{"kind": "human"},
			"timestamp": ts(1), "permissionMode": "auto",
			"message": map[string]any{"role": "user",
				"content": []map[string]any{{"type": "text", "text": prompt}}},
		}),
		// one provider call, two fragments - usage repeated, as the main stream does
		x.rec(map[string]any{
			"type": "assistant", "uuid": id("call-frag1"), "parentUuid": id("human-input"),
			"requestId": id("req"), "timestamp": ts(2),
			"message": map[string]any{"id": id("call"), "type": "message", "role": "assistant",
				"model": "claude-opus-5", "stop_reason": nil,
				"content": []map[string]any{{"type": "text", "text": "Working on turn " + fmt.Sprint(n)}},
				"usage":   usageFull(40)},
		}),
		x.rec(map[string]any{
			"type": "assistant", "uuid": id("call-frag2"), "parentUuid": id("call-frag1"),
			"requestId": id("req"), "timestamp": ts(3),
			"message": map[string]any{"id": id("call"), "type": "message", "role": "assistant",
				"model": "claude-opus-5", "stop_reason": "tool_use",
				"content": []map[string]any{{"type": "tool_use", "id": id("tool"), "name": "Bash",
					"input": map[string]any{"command": fmt.Sprintf("echo turn-%d", n)}}},
				"usage": usageFull(40)},
		}),
		x.rec(map[string]any{
			"type": "user", "uuid": id("tool-result"), "parentUuid": id("call-frag2"),
			"promptId": id("cycle-human"), "sourceToolAssistantUUID": id("call-frag2"),
			"timestamp": ts(4),
			"message": map[string]any{"role": "user",
				"content": []map[string]any{{"tool_use_id": id("tool"), "type": "tool_result",
					"content": fmt.Sprintf("turn-%d", n)}}},
			"toolUseResult": map[string]any{"stdout": fmt.Sprintf("turn-%d", n), "stderr": ""},
		}),
	)

	last := id("tool-result")
	if withAgent {
		agent := fmt.Sprintf("a%016x", 0xadd1c7ed0000+n) // `a` + 16 hex, as required
		x.lines = append(x.lines,
			x.rec(map[string]any{
				"type": "assistant", "uuid": id("spawn-call"), "parentUuid": last,
				"requestId": id("req-spawn"), "timestamp": ts(5),
				"message": map[string]any{"id": id("call-spawn"), "type": "message", "role": "assistant",
					"model": "claude-opus-5", "stop_reason": "tool_use",
					"content": []map[string]any{{"type": "tool_use", "id": id("tool-spawn"), "name": "Agent",
						"input": map[string]any{"description": "helper", "prompt": "help with turn " + fmt.Sprint(n)}}},
					"usage": usageFull(15)},
			}),
			x.rec(map[string]any{
				"type": "user", "uuid": id("spawn-ack"), "parentUuid": id("spawn-call"),
				"promptId": id("cycle-human"), "sourceToolAssistantUUID": id("spawn-call"),
				"timestamp": ts(6),
				"message": map[string]any{"role": "user",
					"content": []map[string]any{{"tool_use_id": id("tool-spawn"), "type": "tool_result",
						"content": "launched"}}},
				"toolUseResult": map[string]any{"agentId": agent, "isAsync": true,
					"status": "async_launched", "prompt": "help with turn " + fmt.Sprint(n)},
			}),
			// a NEW prompt cycle, but the human said nothing - same Talk
			x.rec(map[string]any{
				"type": "user", "uuid": id("agent-done-notice"), "parentUuid": id("spawn-ack"),
				"promptId": id("cycle-notification"), "origin": map[string]any{"kind": "task-notification"},
				"timestamp": ts(30),
				"message": map[string]any{"role": "user",
					"content": "<task-notification>\n<task-id>" + agent + "</task-id>\n<tool-use-id>" +
						id("tool-spawn") + "</tool-use-id>\n<status>completed</status>\n</task-notification>"},
			}),
		)
		last = id("agent-done-notice")

		x.agents[agent] = []string{
			x.rec(map[string]any{
				"type": "user", "uuid": id("agent-prompt"), "parentUuid": nil,
				"promptId": id("agent-cycle"), "agentId": agent, "isSidechain": true,
				"timestamp": ts(7),
				"message":   map[string]any{"role": "user", "content": "help with turn " + fmt.Sprint(n)},
			}),
			x.rec(map[string]any{
				"type": "assistant", "uuid": id("agent-answer"), "parentUuid": id("agent-prompt"),
				"requestId": id("agent-req"), "agentId": agent, "isSidechain": true,
				"timestamp": ts(8),
				"message": map[string]any{"id": id("agent-call"), "type": "message", "role": "assistant",
					"model": "claude-opus-5", "stop_reason": "end_turn",
					"content": []map[string]any{{"type": "text",
						"text": fmt.Sprintf("helper finished turn %d", n)}},
					"usage": usageFull(20)},
			}),
		}
	}

	x.lines = append(x.lines, x.rec(map[string]any{
		"type": "assistant", "uuid": id("final"), "parentUuid": last,
		"requestId": id("req-final"), "timestamp": ts(40),
		"message": map[string]any{"id": id("call-final"), "type": "message", "role": "assistant",
			"model": "claude-opus-5", "stop_reason": "end_turn",
			"content": []map[string]any{{"type": "text",
				"text": fmt.Sprintf("Turn %d complete.", n)}},
			"usage": usageFull(25)},
	}))
}

// Flush writes the accumulated transcript, appending rather than rewriting -
// which is exactly what Claude Code does to a live session.
func (x *transcript) Flush() {
	x.t.Helper()
	write := func(path string, lines []string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			x.t.Fatal(err)
		}
		var buf []byte
		for _, l := range lines {
			buf = append(buf, l...)
			buf = append(buf, '\n')
		}
		if err := os.WriteFile(path, buf, 0o644); err != nil {
			x.t.Fatal(err)
		}
	}
	proj := filepath.Join(x.root, x.slug)
	write(filepath.Join(proj, x.session+".jsonl"), x.lines)
	for agent, lines := range x.agents {
		write(filepath.Join(proj, x.session, "subagents", "agent-"+agent+".jsonl"), lines)
	}
}

// Turns reports how many human turns have been written.
func (x *transcript) Turns() int { return x.turn }

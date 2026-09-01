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

package view

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/wu-sheng/AgentSessionizer/internal/storage"
	"github.com/wu-sheng/AgentSessionizer/pkg/model"
	"github.com/wu-sheng/AgentSessionizer/pkg/sessiondata"
	"github.com/wu-sheng/AgentSessionizer/pkg/sessionflow"
)

// Handler serves the page and the four questions a reader asks: which
// conversation, what happened in it, what happened in this turn, and what
// exactly did this step say.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.page)
	mux.HandleFunc("/api/conversations", s.apiList)
	mux.HandleFunc("/api/c/", s.apiConversation)
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func fail(w http.ResponseWriter, err error, code int) {
	writeJSONStatus(w, code, map[string]string{"error": err.Error()})
}

func writeJSONStatus(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// summary is one row in the conversation list.
type summary struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Talks  int    `json:"talks"`
	Steps  int    `json:"steps"`
	Stream int    `json:"streams"`
	Rounds uint64 `json:"rounds"`
	From   int64  `json:"from"`
	To     int64  `json:"to"`
	Open   int    `json:"unresolved"`
}

func (s *Server) apiList(w http.ResponseWriter, _ *http.Request) {
	ids, err := s.List()
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	out := make([]summary, 0, len(ids))
	for _, id := range ids {
		c, err := s.Load(id)
		if err != nil {
			continue
		}
		row := summary{ID: id, Rounds: c.View.Round, Open: len(c.View.OpenUnresolved())}
		for _, n := range c.View.Nodes {
			switch n.Kind {
			case model.KindTalk:
				row.Talks++
			case model.KindStream:
				row.Stream++
			case model.KindSession:
				row.Title = attrString(n, "title")
			}
			if isStep(n.Kind) {
				row.Steps++
			}
		}
		for _, t := range c.Talks() {
			lo, hi := c.Span(t)
			if lo != 0 && (row.From == 0 || lo < row.From) {
				row.From = lo
			}
			if hi > row.To {
				row.To = hi
			}
		}
		row.From, row.To = Millis(row.From), Millis(row.To)
		if row.Title == "" {
			row.Title = "(untitled)"
		}
		out = append(out, row)
	}
	writeJSON(w, out)
}

var recordPath = regexp.MustCompile(`^/api/c/([^/]+)/record/(\d+)/(\d+)$`)
var talkPath = regexp.MustCompile(`^/api/c/([^/]+)/talk/(.+)$`)

func (s *Server) apiConversation(w http.ResponseWriter, r *http.Request) {
	if m := recordPath.FindStringSubmatch(r.URL.Path); m != nil {
		seq, _ := strconv.ParseUint(m[2], 10, 64)
		row, _ := strconv.ParseUint(m[3], 10, 64)
		s.apiRecord(w, m[1], seq, row)
		return
	}
	if m := talkPath.FindStringSubmatch(r.URL.Path); m != nil {
		s.apiTalk(w, m[1], m[2])
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/c/")
	if id == "" || strings.Contains(id, "/") {
		fail(w, fmt.Errorf("not found"), http.StatusNotFound)
		return
	}
	s.apiOverview(w, id)
}

// talkRow is one talk in the list down the side.
type talkRow struct {
	ID     string `json:"id"`
	Stream string `json:"stream"`
	Label  string `json:"label"`
	Runs   int    `json:"runs"`
	Steps  int    `json:"steps"`
	Tools  int    `json:"tools"`
	From   int64  `json:"from"`
	To     int64  `json:"to"`
	Child  bool   `json:"child"`
}

func (s *Server) apiOverview(w http.ResponseWriter, id string) {
	c, err := s.Load(id)
	if err != nil {
		fail(w, err, http.StatusNotFound)
		return
	}
	var title string
	kinds := map[string]int{}
	for _, n := range c.View.Nodes {
		kinds[n.Kind]++
		if n.Kind == model.KindSession {
			title = attrString(n, "title")
		}
	}
	quality := map[string]int{}
	rels := map[string]int{}
	for _, r := range c.View.Relations {
		rels[r.Type]++
		quality[r.Quality]++
	}

	childStream := map[string]bool{}
	for _, st := range c.Streams() {
		if !strings.Contains(string(st.Attrs), `"role":"main"`) {
			childStream[st.Stream] = true
		}
	}

	talks := make([]talkRow, 0, 64)
	for _, t := range c.Talks() {
		lo, hi := c.Span(t)
		row := talkRow{ID: t.ID, Stream: t.Stream, Child: childStream[t.Stream],
			From: Millis(lo), To: Millis(hi)}
		var walk func(string)
		walk = func(nid string) {
			for _, k := range c.View.Children(nid) {
				if k.Kind == model.KindRun {
					row.Runs++
				}
				if isStep(k.Kind) {
					row.Steps++
				}
				if k.Kind == model.KindTool || k.Kind == model.KindAgentCall {
					row.Tools++
				}
				walk(k.ID)
			}
		}
		walk(t.ID)
		row.Label = c.talkLabel(t)
		talks = append(talks, row)
	}

	open := []map[string]string{}
	for _, u := range c.View.OpenUnresolved() {
		open = append(open, map[string]string{"kind": u.Kind, "ref": u.RefID, "reason": u.Reason})
	}

	writeJSON(w, map[string]any{
		"id": id, "title": title, "session": c.Session,
		"rounds": c.View.Round, "digest": c.View.Digest,
		"parser": c.View.Parser, "policy": c.View.Policy,
		"through_seq": c.View.ThroughSeq,
		"nodes":       len(c.View.Nodes), "relations": len(c.View.Relations),
		"kinds": kinds, "relation_types": rels, "quality": quality,
		"talks": talks, "unresolved": open,
		"streams": streamRows(c),
	})
}

func streamRows(c *Conversation) []map[string]any {
	out := []map[string]any{}
	for _, st := range c.Streams() {
		out = append(out, map[string]any{
			"id": st.ID, "name": st.Stream,
			"role":    attrString(st, "role"),
			"label":   attrString(st, "label"),
			"records": attrNumber(st, "records"),
		})
	}
	return out
}

// talkLabel names a talk by the first thing said in it.
//
// It is a quotation, not a summary: the opening of the first input, verbatim,
// with the position it came from. Nothing is generated - there is no model in
// the read path - so a talk that opened with no external input has no name and
// says so rather than being given a plausible one.
func (c *Conversation) talkLabel(t *sessionflow.Node) string {
	var found string
	var walk func(string) bool
	walk = func(nid string) bool {
		for _, k := range c.View.Children(nid) {
			if k.Kind == model.KindMessageExternal && k.Ref != nil {
				if txt := c.textAt(k.Ref); txt != "" {
					found = txt
					return true
				}
			}
			if walk(k.ID) {
				return true
			}
		}
		return false
	}
	walk(t.ID)
	return found
}

// textAt reads the readable text of one landed position.
func (c *Conversation) textAt(r *sessionflow.Ref) string {
	rec, err := c.record(r.Seq, r.Row)
	if err != nil || rec == nil {
		return ""
	}
	return strings.TrimSpace(rec.Text())
}

func isStep(kind string) bool {
	switch kind {
	case model.KindSession, model.KindSegment, model.KindStream,
		model.KindEpoch, model.KindTalk, model.KindRun:
		return false
	}
	return true
}

func attrString(n *sessionflow.Node, key string) string {
	if len(n.Attrs) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(n.Attrs, &m) != nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func attrNumber(n *sessionflow.Node, key string) float64 {
	if len(n.Attrs) == 0 {
		return 0
	}
	var m map[string]any
	if json.Unmarshal(n.Attrs, &m) != nil {
		return 0
	}
	f, _ := m[key].(float64)
	return f
}

var _ = errors.Is
var _ = io.EOF
var _ = os.Open
var _ = filepath.Join
var _ = storage.PermState
var _ = sessiondata.Schema

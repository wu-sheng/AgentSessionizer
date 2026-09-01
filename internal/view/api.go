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
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/wu-sheng/AgentSessionizer/pkg/model"
	"github.com/wu-sheng/AgentSessionizer/pkg/sessiondata"
	"github.com/wu-sheng/AgentSessionizer/pkg/sessionflow"
)

// Handler serves the page and the four questions a reader asks: which
// conversation, what happened in it, what happened in this turn, and what
// exactly did this step say.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.index)
	mux.HandleFunc("/c/", s.page)
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
	ID       string `json:"id"`
	Title    string `json:"title"`
	Talks    int    `json:"talks"`
	Steps    int    `json:"steps"`
	Stream   int    `json:"streams"`
	Segments int    `json:"segments"`
	Rounds   uint64 `json:"rounds"`
	From     int64  `json:"from"`
	To       int64  `json:"to"`
	Open     int    `json:"unresolved"`
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
			case model.KindSegment:
				row.Segments++
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
	ID      string `json:"id"`
	Stream  string `json:"stream"`
	Label   string `json:"label"`
	Runs    int    `json:"runs"`
	Steps   int    `json:"steps"`
	Tools   int    `json:"tools"`
	From    int64  `json:"from"`
	To      int64  `json:"to"`
	Child   bool   `json:"child"`
	Segment string `json:"segment,omitempty"`

	// labelAt is where the label is read from. It never leaves the server.
	labelAt *sessionflow.Ref
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

	// Which segment a talk sits in. The assembler relates the talk to the
	// segment, so this is read, never recomputed from time.
	inSegment := map[string]string{}
	for _, r := range c.View.Relations {
		if r.Type == model.RelInSegment {
			inSegment[r.From] = r.To
		}
	}

	talks := make([]talkRow, 0, 64)
	var labelRefs []*sessionflow.Ref
	for _, t := range c.Talks() {
		lo, hi := c.Span(t)
		row := talkRow{ID: t.ID, Stream: t.Stream, Child: childStream[t.Stream],
			From: Millis(lo), To: Millis(hi), Segment: inSegment[t.ID]}
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
		if ref := c.labelRef(t); ref != nil {
			row.labelAt = ref
			labelRefs = append(labelRefs, ref)
		}
		talks = append(talks, row)
	}
	// One pass per landed file, not one per talk.
	texts := c.texts(labelRefs)
	for i := range talks {
		if r := talks[i].labelAt; r != nil {
			talks[i].Label = clip(texts[[2]uint64{r.Seq, r.Row}])
		}
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
		"streams":  streamRows(c, talks),
		"segments": segmentRows(c, talks),
	})
}

// segmentRows lists the conversation's segments, each with the span of the
// talks the assembler placed in it.
//
// A segment's own time is not recomputed here. Its bounds are the bounds of its
// talks, which is what "in_segment" already decided.
func segmentRows(c *Conversation, talks []talkRow) []map[string]any {
	span := map[string][2]int64{}
	count := map[string]int{}
	for _, t := range talks {
		if t.Segment == "" {
			continue
		}
		count[t.Segment]++
		s := span[t.Segment]
		if t.From != 0 && (s[0] == 0 || t.From < s[0]) {
			s[0] = t.From
		}
		if t.To > s[1] {
			s[1] = t.To
		}
		span[t.Segment] = s
	}
	var segs []*sessionflow.Node
	for _, n := range c.View.Nodes {
		if n.Kind == model.KindSegment {
			segs = append(segs, n)
		}
	}
	out := []map[string]any{}
	for _, n := range sessionflow.InOrder(segs) {
		s := span[n.ID]
		out = append(out, map[string]any{
			"id": n.ID, "state": attrString(n, "state"),
			"talks": count[n.ID], "from": s[0], "to": s[1],
			"committable": attrBool(n, "committable"),
		})
	}
	return out
}

// streamRows lists the execution streams, each with the parent that started it
// and the talk that opens it.
//
// The parent is read from the "starts" relation, never from the stream name. A
// call that could have started several streams reports each of them, because
// the assembler does not choose one and neither may this.
func streamRows(c *Conversation, talks []talkRow) []map[string]any {
	firstTalk := map[string]string{}
	steps := map[string]int{}
	for i := range talks {
		if _, seen := firstTalk[talks[i].Stream]; !seen {
			firstTalk[talks[i].Stream] = talks[i].ID
		}
		steps[talks[i].Stream] += talks[i].Steps
	}
	// Which step started a stream, and how well that is known. A call the
	// assembler could not tie to one stream leaves several candidates, and
	// every one is reported rather than one being chosen here.
	type origin struct {
		Step, Stream, Quality, Talk string
	}
	parent := map[string]string{}
	from := map[string][]origin{}
	for _, r := range c.View.Relations {
		if r.Type != model.RelStarts {
			continue
		}
		if n := c.View.Nodes[r.From]; n != nil && n.Stream != "" {
			parent[r.To] = n.Stream
			from[r.To] = append(from[r.To], origin{r.From, n.Stream, r.Quality, c.talkOf(r.From)})
		}
	}
	out := []map[string]any{}
	for _, st := range c.Streams() {
		out = append(out, map[string]any{
			"id": st.ID, "name": st.Stream,
			"role":    attrString(st, "role"),
			"label":   attrString(st, "label"),
			"records": attrNumber(st, "records"),
			"parent":  parent[st.ID],
			"talk":    firstTalk[st.Stream],
			"steps":   steps[st.Stream],
			"opened_by": func() []map[string]string {
				out := []map[string]string{}
				for _, o := range from[st.ID] {
					out = append(out, map[string]string{
						"step": o.Step, "stream": o.Stream,
						"quality": o.Quality, "talk": o.Talk,
					})
				}
				return out
			}(),
		})
	}
	return out
}

func attrBool(n *sessionflow.Node, key string) bool {
	if len(n.Attrs) == 0 {
		return false
	}
	var m map[string]any
	if json.Unmarshal(n.Attrs, &m) != nil {
		return false
	}
	b, _ := m[key].(bool)
	return b
}

// labelRef finds where a talk's name should be read from: the first external
// input under it.
//
// This returns the position rather than the text because resolving one position
// means opening a landed file and reading forward to a row. Done per talk, that
// is one file scan per talk - measured at six seconds for a conversation with
// 922 of them. The positions are collected first and read together instead.
func (c *Conversation) labelRef(t *sessionflow.Node) *sessionflow.Ref {
	var found *sessionflow.Ref
	var walk func(string) bool
	walk = func(nid string) bool {
		for _, k := range c.View.Children(nid) {
			if k.Kind == model.KindMessageExternal && k.Ref != nil {
				found = k.Ref
				return true
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

// texts reads many landed positions with one pass per file.
//
// A landed file is a sequence of records with no row offsets, so a row is
// reached by reading forward. Reading every wanted row of one file in a single
// pass turns a scan per position into a scan per file.
func (c *Conversation) texts(refs []*sessionflow.Ref) map[[2]uint64]string {
	want := map[uint64]map[uint64]bool{}
	for _, r := range refs {
		if r == nil {
			continue
		}
		if want[r.Seq] == nil {
			want[r.Seq] = map[uint64]bool{}
		}
		want[r.Seq][r.Row] = true
	}
	out := map[[2]uint64]string{}
	for seq, rows := range want {
		path, err := c.landedPath(seq)
		if err != nil {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		rd, err := sessiondata.NewReader(f)
		if err != nil {
			f.Close()
			continue
		}
		left := len(rows)
		for i := uint64(1); left > 0; i++ {
			rec, rerr := rd.Next()
			if rerr != nil {
				break
			}
			if rows[i] {
				out[[2]uint64{seq, i}] = strings.TrimSpace(rec.Text())
				left--
			}
		}
		f.Close()
	}
	return out
}

// talkOf walks up from a node to the talk that contains it.
//
// A reader sent to a step needs the talk holding it, because a talk is what
// the page loads. Without it, following a child stream back to the step that
// opened it lands on a stream whose talk was never fetched, and the page shows
// nothing at all.
func (c *Conversation) talkOf(id string) string {
	for i := 0; i < 24 && id != ""; i++ {
		n := c.View.Nodes[id]
		if n == nil {
			return ""
		}
		if n.Kind == model.KindTalk {
			return n.ID
		}
		id = n.Parent
	}
	return ""
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

var _ = io.EOF
var _ = filepath.Join
var _ = sessiondata.Schema

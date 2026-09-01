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

	"github.com/wu-sheng/AgentSessionizer/pkg/model"
	"github.com/wu-sheng/AgentSessionizer/pkg/sessiondata"
	"github.com/wu-sheng/AgentSessionizer/pkg/sessionflow"
)

// step is one node as a reader meets it.
type step struct {
	ID       string            `json:"id"`
	Kind     string            `json:"kind"`
	Parent   string            `json:"parent,omitempty"`
	Stream   string            `json:"stream,omitempty"`
	At       int64             `json:"at"`
	Ref      *sessionflow.Ref  `json:"ref,omitempty"`
	Refs     []sessionflow.Ref `json:"refs,omitempty"`
	Attrs    json.RawMessage   `json:"attrs,omitempty"`
	Text     string            `json:"text,omitempty"`
	Name     string            `json:"name,omitempty"`
	Failed   *bool             `json:"failed,omitempty"`
	State    string            `json:"state,omitempty"`
	Bytes    int               `json:"bytes,omitempty"`
	Children []step            `json:"children,omitempty"`
	Edges    []edge            `json:"edges,omitempty"`
}

// edge is one typed relation, as it reads from this node's side.
type edge struct {
	Type    string `json:"type"`
	Other   string `json:"other"`
	Dir     string `json:"dir"` // "out" or "in"
	Quality string `json:"quality"`
	Via     string `json:"via,omitempty"`
}

// preview is how much of a step's text is sent with the tree.
//
// A tool result reaches a megabyte and a session's results can be four fifths
// of its bytes, so the tree carries an opening and the reader asks for the rest
// when it wants it. The state and the true size travel with it, so nothing is
// shown as complete when it is not.
const preview = 2000

func (s *Server) apiTalk(w http.ResponseWriter, id, talk string) {
	c, err := s.Load(id)
	if err != nil {
		fail(w, err, http.StatusNotFound)
		return
	}
	n := c.View.Nodes[talk]
	if n == nil {
		fail(w, fmt.Errorf("no such talk: %s", talk), http.StatusNotFound)
		return
	}
	writeJSON(w, c.step(n, 0))
}

// step renders one node and everything under it.
func (c *Conversation) step(n *sessionflow.Node, depth int) step {
	out := step{
		ID: n.ID, Kind: n.Kind, Parent: n.Parent, Stream: n.Stream,
		At: Millis(c.Time(n)), Ref: n.Ref, Refs: n.Refs, Attrs: n.Attrs,
	}
	for _, r := range c.from[n.ID] {
		out.Edges = append(out.Edges, edge{r.Type, r.To, "out", r.Quality, r.Via})
	}
	for _, r := range c.to[n.ID] {
		out.Edges = append(out.Edges, edge{r.Type, r.From, "in", r.Quality, r.Via})
	}
	// The content a reader sees, taken from the part this node points at.
	//
	// Only a leaf carries content. A provider call is a container - its text,
	// its reasoning and its tools are its children - so filling it from the
	// record it starts at would show a tool's command as the call's own words.
	if n.Ref != nil && carriesContent(n.Kind) {
		if rec, err := c.record(n.Ref.Seq, n.Ref.Row); err == nil && rec != nil {
			out.fill(rec, n.Ref.Block)
		}
	}
	if depth < 12 {
		for _, k := range c.View.Children(n.ID) {
			out.Children = append(out.Children, c.step(k, depth+1))
		}
	}
	return out
}

// fill takes a step's readable content from the record it points at.
func (s *step) fill(rec *sessiondata.Record, block *int) {
	var p *sessiondata.Part
	if block != nil && *block < len(rec.Parts) {
		p = &rec.Parts[*block]
	} else if len(rec.Parts) == 1 {
		p = &rec.Parts[0]
	}
	if p == nil {
		s.Text = clip(rec.Text())
		return
	}
	s.Name, s.Failed, s.State, s.Bytes = p.Name, p.Failed, p.State, p.Bytes
	switch {
	case p.Text != "":
		s.Text = clip(p.Text)
	case len(p.Data) > 0:
		s.Text = clip(string(p.Data))
	}
}

// carriesContent reports whether a step is something a reader reads, rather
// than something that contains what they read.
func carriesContent(kind string) bool {
	switch kind {
	case model.KindLLMCall, model.KindSession, model.KindSegment,
		model.KindStream, model.KindEpoch, model.KindTalk, model.KindRun:
		return false
	}
	return true
}

func clip(t string) string {
	if len(t) <= preview {
		return t
	}
	return t[:preview]
}

func (s *Server) apiRecord(w http.ResponseWriter, id string, seq, row uint64) {
	c, err := s.Load(id)
	if err != nil {
		fail(w, err, http.StatusNotFound)
		return
	}
	rec, err := c.record(seq, row)
	if err != nil {
		fail(w, err, http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{
		"seq": seq, "row": row, "record": rec, "at": Millis(c.at[[2]uint64{seq, row}]),
	})
}

// record reads one landed record whole.
//
// Nothing is cached: a record is read when a reader asks to see it, which is
// the only time its bytes are wanted. The landed file is found by its sequence,
// which is unique across every stream in a session.
func (c *Conversation) record(seq, row uint64) (*sessiondata.Record, error) {
	path, err := c.landedPath(seq)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r, err := sessiondata.NewReader(f)
	if err != nil {
		return nil, err
	}
	for i := uint64(1); ; i++ {
		rec, rerr := r.Next()
		if errors.Is(rerr, io.EOF) {
			return nil, fmt.Errorf("view: no row %d in sequence %d", row, seq)
		}
		if rerr != nil {
			return nil, rerr
		}
		if i == row {
			return rec, nil
		}
	}
}

var seqSuffix = regexp.MustCompile(`-(\d{6,})\.sd$`)

// landedPath finds the landed file carrying a sequence.
//
// The sequence is monotonic across every stream and run in a session, so it
// identifies a file on its own - which stream it belongs to does not have to be
// known, and must not be assumed.
func (c *Conversation) landedPath(seq uint64) (string, error) {
	if c.paths == nil {
		c.paths = map[uint64]string{}
	}
	if p, ok := c.paths[seq]; ok {
		return p, nil
	}
	root := c.zone.SessionDir(c.Session)
	var found string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || found != "" {
			return nil
		}
		m := seqSuffix.FindStringSubmatch(d.Name())
		if m == nil {
			return nil
		}
		if n, cerr := strconv.ParseUint(m[1], 10, 64); cerr == nil && n == seq {
			found = p
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("view: no landed file with sequence %d", seq)
	}
	c.paths[seq] = found
	return found, nil
}

var _ = strings.TrimSpace
var _ = model.KindTalk

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

package asb

import (
	"fmt"
	"sort"
)

// View is the materialised result of folding a chain.
//
// It is derived and disposable: nothing here is authoritative, the rounds are.
// Deleting a view and refolding must reproduce it exactly, which is the
// property that lets a view be cached, rebuilt or discarded freely.
type View struct {
	Conversation string
	Session      string

	// Round and Digest identify the last round folded in - a view is always a
	// view AS OF a round, never of "the chain" in general.
	Round      uint64
	Digest     string
	ThroughSeq uint64

	Nodes      map[string]*Node
	Relations  map[string]*Relation
	Unresolved map[string]*Unresolved
}

// Fold semantics.
//
// Three rules, and each exists because its opposite loses information:
//
//  1. LAST WRITER WINS, per entity id. A later revision of the same id fully
//     replaces the earlier one. It does not merge field-by-field: a partial
//     merge cannot express "this attribute is now absent", so a correction that
//     removes something could never be recorded.
//
//  2. ABSENCE MEANS UNCHANGED. A round carries only what it changed. An entity
//     missing from round 7 is exactly the entity round 4 published. Removal is
//     therefore explicit - a revision with Tombstone set - and never inferred
//     from silence.
//
//  3. ORDER IS CHAIN ORDER, NOT TIMESTAMP ORDER. Rounds fold in round order and
//     entities within a round in file order. Timestamps come from the observed
//     runtime and can go backwards across sources; the chain cannot.
//
// The consequence worth stating plainly: fold(rounds 1..N) is the whole
// definition of the conversation as of round N. There is no other state.

// NewView returns an empty view.
func NewView(conversation string) *View {
	return &View{
		Conversation: conversation,
		Nodes:        map[string]*Node{},
		Relations:    map[string]*Relation{},
		Unresolved:   map[string]*Unresolved{},
	}
}

// Apply folds one round into the view.
//
// It refuses a round that does not follow the view's current head: applying
// round 5 to a view that stopped at round 3 would silently skip round 4's
// revisions, and the result would look complete while being wrong.
func (v *View) Apply(r *Round) error {
	if r.Header.Round != v.Round+1 {
		return fmt.Errorf("asb: cannot apply round %d to a view at round %d: rounds must fold in order",
			r.Header.Round, v.Round)
	}
	if v.Round > 0 && r.Header.Previous != v.Digest {
		return fmt.Errorf("asb: round %d names previous %q, but the view's head is %q",
			r.Header.Round, firstN(r.Header.Previous, 12), firstN(v.Digest, 12))
	}
	if v.Session == "" {
		v.Session = r.Header.Session
	}

	for i := range r.Nodes {
		n := r.Nodes[i]
		if n.Tombstone {
			delete(v.Nodes, n.ID)
			continue
		}
		v.Nodes[n.ID] = &n
	}
	for i := range r.Relations {
		rel := r.Relations[i]
		if rel.Tombstone {
			delete(v.Relations, rel.ID)
			continue
		}
		v.Relations[rel.ID] = &rel
	}
	for i := range r.Unresolved {
		u := r.Unresolved[i]
		if u.Tombstone {
			delete(v.Unresolved, u.ID)
			continue
		}
		// A resolved or terminal entry is kept, not deleted: it records that the
		// gap existed and how it closed. OpenUnresolved filters for what is still
		// outstanding.
		v.Unresolved[u.ID] = &u
	}

	v.Round = r.Header.Round
	v.Digest = r.Commit.Digest
	v.ThroughSeq = r.Header.ThroughSeq
	return nil
}

// Fold reads every round of a chain in order and returns the view.
func (c *Chain) Fold() (*View, error) {
	files, err := c.List()
	if err != nil {
		return nil, err
	}
	v := NewView(c.id)
	for _, rf := range files {
		r, err := c.Open(rf.Path)
		if err != nil {
			return nil, err
		}
		if err := v.Apply(r); err != nil {
			return nil, err
		}
	}
	return v, nil
}

// OpenUnresolved returns the entries still outstanding, sorted by id.
//
// Reporting these is not optional. An assembler that silently drops what it
// could not resolve presents a partial conversation as a complete one; carrying
// them as data is what makes the gaps visible to a reader.
func (v *View) OpenUnresolved() []*Unresolved {
	var out []*Unresolved
	for _, u := range v.Unresolved {
		if u.State == UnresolvedOpen {
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// NodesByKind returns the view's nodes of one kind, sorted by id.
func (v *View) NodesByKind(kind string) []*Node {
	var out []*Node
	for _, n := range v.Nodes {
		if n.Kind == kind {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Children returns the nodes whose containment parent is id, sorted by id.
//
// Containment only. Cross-stream flow - spawn, delivery, retry, join - is a
// relation, never a parent, so a child agent's steps never appear beneath the
// parent's call here.
func (v *View) Children(id string) []*Node {
	var out []*Node
	for _, n := range v.Nodes {
		if n.Parent == id {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

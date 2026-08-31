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

package assemble

import (
	"github.com/wu-sheng/AgentSessionizer/internal/index"
	"github.com/wu-sheng/AgentSessionizer/pkg/asb"
	"github.com/wu-sheng/AgentSessionizer/pkg/model"
)

// talk is one readable interaction: an input from outside the agent, the work
// that followed, and the output.
type talk struct {
	NodeID string
	Stream *streamInfo
	Epoch  *epoch
	Cycles []uint32 // prompt cycles that belong to this talk, in order
	Runs   []uint32 // cycles that produced a run node, in order
	First  int64
	Last   int64
}

// Stage 7 - build talks and runs.
//
// This stage produces the conversation a person reads, and it has the least
// support from the runtime: neither a Talk nor a Run is something Claude Code
// records. Both are derived, so the rule has to be stated exactly.
//
// A TALK STARTS ONLY ON INPUT FROM OUTSIDE THE AGENT. A background agent
// finishing and the parent picking up again is mechanically a new prompt cycle,
// but nobody said anything - it is the same interaction continuing. Starting a
// Talk on every prompt cycle would break one interaction into as many pieces as
// it delegated work, where a reader sees one. The whole chain of call,
// acknowledgement, child stream, child output, notification and the parent's
// next call lives INSIDE one Talk.
//
// MEMBERSHIP IS RESOLVED BY WALKING BACKWARD, NEVER BY LINE PROXIMITY. Cycle
// ids sit on trigger records; a model response carries none, but every one
// reaches a cycle through its containment parents. Proximity would agree in the
// simple case and disagree exactly where it matters - a forked chain, a
// replayed block, an interrupt that reattaches to an earlier record - putting
// records in the wrong turn.
//
// A CHILD STREAM IS EXACTLY ONE TALK: the delegated prompt in, the final output
// out, nobody outside the agent involved. Child streams never reset context, so
// each is also exactly one epoch.
func (b *builder) stage7Talks() {
	b.container = map[[2]uint32]string{}
	b.talkByID = map[string]*talk{}

	for _, s := range b.streams {
		entries := b.entriesOf(s)
		if len(entries) == 0 {
			continue
		}
		cycleAt := make([]uint32, len(entries))
		for i, e := range entries {
			cycleAt[i] = b.cycleFor(e)
		}

		// A child stream is exactly one Talk, created up front. The delegated
		// prompt is the interaction; nothing from outside the agent arrives in the
		// middle of it, so no later cycle can start a second one. Some child
		// streams do carry several cycles, and every one of them belongs to this
		// single Talk.
		var cur *talk
		if s.Role == model.StreamChild {
			cur = b.newTalkKeyed(s, s.epochs[0], asb.NodeID("talk", s.Name))
		}

		for _, ep := range s.epochs {
			for i := ep.Start; i < ep.End; i++ {
				e, cyc := entries[i], cycleAt[i]
				if cyc == 0 {
					b.place(e, cur, ep, s)
					continue
				}
				key := cycleKey{s.ID, cyc}
				if _, known := b.cycleTalk[key]; !known {
					switch {
					case cur == nil || b.startsTalk(cyc, s):
						cur = b.newTalk(s, ep, cyc)
					default:
						cur.Cycles = append(cur.Cycles, cyc)
						b.cycleTalk[key] = cur.NodeID
					}
					b.newRun(cur, s, cyc)
				}
				t := b.talkOf(s, cyc)
				b.place(e, t, ep, s)
				if e.TS != 0 && t != nil {
					if t.First == 0 || e.TS < t.First {
						t.First = e.TS
					}
					if e.TS > t.Last {
						t.Last = e.TS
					}
				}
			}
		}
	}
	b.emitTalks()
	b.stats.Talks = len(b.talks)
}

// cycleFor resolves the prompt cycle a record belongs to.
func (b *builder) cycleFor(e *index.Entry) uint32 {
	if e.Cycle != 0 {
		return e.Cycle
	}
	if a, ok := b.ix.CycleOf(e); ok {
		return a.Cycle
	}
	return 0
}

// startsTalk reports whether a cycle begins a new interaction.
//
// A child stream is never split: its single Talk is created before this runs.
//
// On a parent lineage two cases begin one. The obvious one is a cycle whose
// records say the trigger came from outside the agent. The second is a cycle
// where NO record states a trigger at all - a command typed locally is a person
// acting, and the runtime records no origin for it, so requiring an explicit
// external marker would leave those cycles attached to whatever came before.
//
// Everything else continues the interaction in progress. A background agent
// finishing is mechanically a new cycle, but nobody said anything.
func (b *builder) startsTalk(cycle uint32, s *streamInfo) bool {
	if s.Role == model.StreamChild {
		return false
	}
	stated := false
	for _, i := range s.Entries {
		e := &b.ix.Entries[i]
		if e.Cycle != cycle || e.Trigger == index.TriggerNone {
			continue
		}
		if e.Trigger == index.TriggerExternal {
			return true
		}
		stated = true
	}
	return !stated
}

func (b *builder) newTalk(s *streamInfo, ep *epoch, cycle uint32) *talk {
	t := b.newTalkKeyed(s, ep, asb.NodeID("talk", s.Name, b.str(cycle)))
	t.Cycles = append(t.Cycles, cycle)
	b.cycleTalk[cycleKey{s.ID, cycle}] = t.NodeID
	return t
}

func (b *builder) newTalkKeyed(s *streamInfo, ep *epoch, id string) *talk {
	if t, ok := b.talkByID[id]; ok {
		return t
	}
	t := &talk{NodeID: id, Stream: s, Epoch: ep}
	b.talkByID[id] = t
	s.talks = append(s.talks, t)
	b.talks = append(b.talks, t)
	b.stats.Talks++
	return t
}

// newRun records one agent loop inside a talk.
//
// One prompt cycle is one run: a trigger, whatever the agent did in response,
// and the reply. A run is not a single provider call - a run usually contains
// several, because each tool result starts another.
func (b *builder) newRun(t *talk, s *streamInfo, cycle uint32) {
	id := asb.NodeID("run", t.NodeID, b.str(cycle))
	if _, exists := b.runOf[cycleKey{s.ID, cycle}]; exists {
		return
	}
	b.runOf[cycleKey{s.ID, cycle}] = id
	t.Runs = append(t.Runs, cycle)
	b.stats.Runs++
}

func (b *builder) talkOf(s *streamInfo, cycle uint32) *talk {
	return b.talkByID[b.cycleTalk[cycleKey{s.ID, cycle}]]
}

// place records which node contains a record.
//
// The container is the innermost resolved level: the run, or the talk, or the
// epoch, or the stream. A record that arrived before any interaction started -
// context the harness injected at the beginning - genuinely belongs to the
// epoch, so it is placed there rather than attached to an invented turn.
func (b *builder) place(e *index.Entry, t *talk, ep *epoch, s *streamInfo) {
	key := [2]uint32{e.Seq, e.Row}
	switch {
	case t != nil:
		if cyc := b.cycleFor(e); cyc != 0 {
			if run, ok := b.runOf[cycleKey{s.ID, cyc}]; ok {
				b.container[key] = run
				return
			}
		}
		b.container[key] = t.NodeID
	case ep != nil:
		b.container[key] = ep.NodeID
	default:
		b.container[key] = s.NodeID
	}
}

// containerOf returns the node that contains a record.
func (b *builder) containerOf(e *index.Entry) string {
	if id, ok := b.container[[2]uint32{e.Seq, e.Row}]; ok {
		return id
	}
	if s, ok := b.byStream[e.Stream]; ok {
		return s.NodeID
	}
	return asb.NodeID("session", b.opt.Session)
}

// emitTalks writes the talk and run nodes.
func (b *builder) emitTalks() {
	for _, t := range b.talks {
		parent := t.Stream.NodeID
		if t.Epoch != nil {
			parent = t.Epoch.NodeID
		}
		trigger := model.TriggerExternal
		if t.Stream.Role == model.StreamChild {
			// A delegated prompt is input to the child, but it did not come from
			// outside the agent - the parent wrote it.
			trigger = model.TriggerUnknown
		}
		b.node(asb.Node{
			Entity: asb.Entity{ID: t.NodeID}, Kind: model.KindTalk,
			Parent: parent, Stream: t.Stream.Name,
			Attrs: attrs(map[string]any{
				"cycles":  len(t.Cycles),
				"runs":    len(t.Runs),
				"trigger": trigger,
			}),
		})
		for _, cyc := range t.Runs {
			id, ok := b.runOf[cycleKey{t.Stream.ID, cyc}]
			if !ok {
				continue
			}
			trigger := model.TriggerNotification
			if len(t.Cycles) > 0 && cyc == t.Cycles[0] {
				trigger = model.TriggerExternal
			}
			b.node(asb.Node{
				Entity: asb.Entity{ID: id}, Kind: model.KindRun,
				Parent: t.NodeID, Stream: t.Stream.Name,
				Attrs: attrs(map[string]any{"trigger": trigger}),
			})
		}
	}
}

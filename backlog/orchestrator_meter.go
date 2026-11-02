package backlog

import (
	"github.com/sachiniyer/agent-factory/planusage"
	"maps"
	"slices"
)

// Cost sums the orchestrator's metered transcript costs: its conversation from
// when it was bound, across every session that orchestrated the group.
func (o *Orchestrator) Cost() float64 { return RunRef{Meter: o.Meter}.Cost() }

// SetOrchestratorMeters stores per-file meters on the orchestrators of the
// given sessions. It returns whether anything changed.
func (b *Backlog) SetOrchestratorMeters(meters map[string]map[string]Meter) bool {
	changed := false
	for i := range b.Items {
		o := b.Items[i].Orchestrator
		if o == nil || o.SessionID == "" {
			continue
		}
		if m, ok := meters[o.SessionID]; ok && !maps.Equal(o.Meter, m) {
			o.Meter = maps.Clone(m)
			changed = true
		}
	}
	return changed
}

// WithConversations is o with mains, the main transcript files of its
// session's conversation, recorded as conversations that orchestrate from its
// bind. A main recorded already whose spans all ended gets a new span from this
// bind: the group was bound back to a session that orchestrated it before.
func (o Orchestrator) WithConversations(mains []string) Orchestrator {
	o.Conversations = cloneConversations(o.Conversations)
	o.addConversations(mains)
	return o
}

func (o *Orchestrator) addConversations(mains []string) bool {
	changed := false
	for _, m := range mains {
		i := slices.IndexFunc(o.Conversations, func(c OrchConversation) bool { return c.Main == m })
		switch {
		case i < 0:
			o.Conversations = append(o.Conversations, OrchConversation{Main: m, Spans: []planusage.Span{{From: o.BoundAt}}})
			changed = true
		case !o.Conversations[i].open():
			// Bound back to a session that orchestrated before: a new span, and the
			// earlier ones stay (Codex P2 on #6616).
			c := &o.Conversations[i]
			c.Spans = append(append([]planusage.Span(nil), c.Spans...), planusage.Span{From: o.BoundAt})
			changed = true
		}
	}
	return changed
}

// NoteOrchConversations records mains on group's orchestrator while sessionID
// is still the session that orchestrates it (see WithConversations). It
// returns whether anything changed.
func (b *Backlog) NoteOrchConversations(group int, sessionID string, mains []string) bool {
	it := b.Item(group)
	if it == nil || it.Orchestrator == nil || it.Orchestrator.SessionID != sessionID {
		return false
	}
	return it.Orchestrator.addConversations(mains)
}

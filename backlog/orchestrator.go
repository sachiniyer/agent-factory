package backlog

import (
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"
)

const (
	EventQuestion           = "question"
	EventReview             = "review"
	EventStalled            = "stalled"
	EventFinished           = "finished"
	EventEscalationAnswered = "escalation_answered"
)

// Orchestrator is the session that owns a group: af sends it the group's
// events and restores or replaces it when needed. Only a top-level parent has
// one, and a session orchestrates at most one group in a project.
type Orchestrator struct {
	SessionID string    `json:"session_id"`
	Title     string    `json:"title"`
	Program   string    `json:"program"`
	BoundAt   time.Time `json:"bound_at"`
	// Handoff is the latest notes for a session that replaces this one.
	Handoff string           `json:"handoff,omitempty"`
	Meter   map[string]Meter `json:"meter,omitempty"`
	// Events is the outbox of events not yet delivered, oldest first, so a
	// daemon restart can finish delivery.
	Events []OrchEvent `json:"events,omitempty"`
	// Failures holds when recent deliveries to the session failed, within
	// OrchFailureWindow, oldest first.
	Failures []time.Time `json:"failures,omitempty"`
	// Sending is how many of the first Events a delivery in flight carries.
	Sending int `json:"sending,omitempty"`
}

// OrchEvent is one group event for the orchestrator. Item is the item it is
// about; Message is the question it names, if any.
type OrchEvent struct {
	Kind    string    `json:"kind"`
	Item    int       `json:"item,omitempty"`
	Message int       `json:"message,omitempty"`
	At      time.Time `json:"at"`
}

func (o *Orchestrator) clone() *Orchestrator {
	if o == nil {
		return nil
	}
	c := *o
	c.Meter = maps.Clone(o.Meter)
	c.Events = append([]OrchEvent(nil), o.Events...)
	c.Failures = append([]time.Time(nil), o.Failures...)
	return &c
}

// OrchestratedBy returns the group sessionID orchestrates, or 0.
func (b *Backlog) OrchestratedBy(sessionID string) int {
	for _, it := range b.Items {
		if it.Orchestrator != nil && it.Orchestrator.SessionID == sessionID {
			return it.ID
		}
	}
	return 0
}

func (b *Backlog) isGroup(id int) bool {
	it := b.Item(id)
	return it != nil && it.Parent == 0 && b.IsParent(id)
}

// BindOrchestrator makes o's session the orchestrator of group id. Binding a
// group that already has one replaces its session and keeps its handoff,
// meter and pending events. On error b is unchanged.
func (b *Backlog) BindOrchestrator(id int, o Orchestrator, now time.Time) error {
	it, err := b.get(id)
	if err != nil {
		return err
	}
	if !b.isGroup(id) {
		return fmt.Errorf("%s is not a top-level parent; only groups can have an orchestrator", FormatID(id))
	}
	o.SessionID = strings.TrimSpace(o.SessionID)
	if o.SessionID == "" {
		return errors.New("an orchestrator needs a session id")
	}
	if g := b.OrchestratedBy(o.SessionID); g != 0 && g != id {
		return fmt.Errorf("session %q already orchestrates %s; clear it first", o.SessionID, FormatID(g))
	}
	if prev := it.Orchestrator; prev != nil {
		o.Handoff, o.Meter, o.Events = prev.Handoff, prev.Meter, prev.Events
	}
	o.Failures, o.Sending = nil, 0
	it.closeAFQuestions(ClosedRebound)
	o.BoundAt = now
	it.Orchestrator = &o
	it.UpdatedAt = now
	return nil
}

// ClearOrchestrator unbinds group id and returns the binding it had, or nil
// when it had none.
func (b *Backlog) ClearOrchestrator(id int, now time.Time) (*Orchestrator, error) {
	it, err := b.get(id)
	if err != nil {
		return nil, err
	}
	prev := it.Orchestrator
	if prev != nil {
		it.Orchestrator = nil
		it.closeAFQuestions(ClosedUnbound)
		it.UpdatedAt = now
	}
	return prev, nil
}

// checkOrchestrators refuses a tree where an orchestrated item is no longer
// a top-level parent.
func (b *Backlog) checkOrchestrators() error {
	for _, it := range b.Items {
		switch {
		case it.Orchestrator == nil:
		case it.Parent != 0:
			return fmt.Errorf("%s has an orchestrator and must stay a top-level parent; clear it first", FormatID(it.ID))
		case !b.IsParent(it.ID):
			return fmt.Errorf("%s has an orchestrator and must keep at least one child; clear it first", FormatID(it.ID))
		}
	}
	return nil
}

// normalizeOrchestrators drops what a hand edit can leave: a binding on an
// item that is not a top-level parent, or a second group bound to the same
// session (the first in tree order keeps it).
func (b *Backlog) normalizeOrchestrators() {
	seen := map[string]bool{}
	for i := range b.Items {
		it := &b.Items[i]
		if it.Orchestrator == nil {
			continue
		}
		it.Orchestrator.SessionID = strings.TrimSpace(it.Orchestrator.SessionID)
		if !b.isGroup(it.ID) || it.Orchestrator.SessionID == "" || seen[it.Orchestrator.SessionID] {
			it.Orchestrator = nil
			continue
		}
		seen[it.Orchestrator.SessionID] = true
	}
}

// savedSchemaVersion is the schema_version a save writes: the newest only
// while some group has an orchestrator, so a backlog without one stays a
// version 2 file that an older af reads and writes back with nothing lost.
// Clearing the last binding brings the next save back to version 2.
func (b *Backlog) savedSchemaVersion() int {
	for i := range b.Items {
		if b.Items[i].Orchestrator != nil {
			return BacklogSchemaVersion
		}
	}
	return BacklogUnboundSchemaVersion
}

// SetOrchestratorHandoff stores the notes for a session that replaces
// sessionID as orchestrator, and returns the group it orchestrates.
func (b *Backlog) SetOrchestratorHandoff(sessionID, handoff string, now time.Time) (int, error) {
	sessionID = strings.TrimSpace(sessionID)
	id := b.OrchestratedBy(sessionID)
	if sessionID == "" || id == 0 {
		return 0, fmt.Errorf("session %q orchestrates no group; bind it with \"af backlog orchestrate <group> --self\"", sessionID)
	}
	handoff = strings.TrimSpace(handoff)
	if n := len(handoff); n > MaxHandoffBytes {
		return 0, fmt.Errorf("the handoff is %d bytes; the limit is 8 KB (%d bytes)", n, MaxHandoffBytes)
	}
	it := b.Item(id)
	it.Orchestrator.Handoff = handoff
	it.UpdatedAt = now
	return id, nil
}

// rootOf returns the top-level item above id, or id itself.
func (b *Backlog) rootOf(id int) int {
	for {
		it := b.Item(id)
		if it == nil || it.Parent == 0 {
			return id
		}
		id = it.Parent
	}
}

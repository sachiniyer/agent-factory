package backlog

import (
	"errors"
	"fmt"
	"github.com/sachiniyer/agent-factory/planusage"
	"maps"
	"slices"
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
	// Conversations are the agent conversations the group's orchestrators had,
	// each with the span in which it orchestrated: only its messages from that
	// span are the group's, and backlog spend.
	Conversations []OrchConversation `json:"conversations,omitempty"`
	// Events is the outbox of events not yet delivered, oldest first, so a
	// daemon restart can finish delivery.
	Events []OrchEvent `json:"events,omitempty"`
	// Failures holds when recent deliveries to the session failed, within
	// OrchFailureWindow, oldest first.
	Failures []time.Time `json:"failures,omitempty"`
	// Sending is how many of the first Events a delivery in flight carries.
	Sending int `json:"sending,omitempty"`
	// Unstarted marks a session that has not had its start prompt (the
	// instructions and the handoff), and why: OrchUnstartedReplaced for a
	// replacement af started whose first prompt was proven not delivered, or
	// that af adopted after a restart; OrchUnstartedRestarted for a session af
	// restored without its agent conversation. Every delivery sends the start
	// prompt while it is set, and only a confirmed one clears it.
	Unstarted string `json:"unstarted,omitempty"`
	// Replacing records a replacement af began to start for the gone session,
	// written before the create, so a daemon that stops before binding it
	// adopts that session instead of starting a second one.
	Replacing *OrchReplacing `json:"replacing,omitempty"`
	// Stay keeps the session live when its current turn ends; the turn's end
	// uses it up.
	Stay bool `json:"stay,omitempty"`
}

// Why an orchestrator has not had its start prompt (Orchestrator.Unstarted).
const (
	OrchUnstartedReplaced  = "replaced"
	OrchUnstartedRestarted = "restarted"
)

// OrchReplacing is a replacement af began to start: its session's title base,
// and the time af began, which the new session's creation is not before.
type OrchReplacing struct {
	TitleBase string    `json:"title_base"`
	Since     time.Time `json:"since"`
}

// OrchEvent is one group event for the orchestrator. Item is the item it is
// about; Message is the question it names, if any.
type OrchEvent struct {
	Kind    string    `json:"kind"`
	Item    int       `json:"item,omitempty"`
	Message int       `json:"message,omitempty"`
	At      time.Time `json:"at"`
}

// OrchConversation is one agent conversation a group's orchestrators had: its
// main transcript file, and the spans in which it orchestrated, each from its
// session's bind until the group was bound to another session or cleared (open
// while it still orchestrates). A group bound back to a session that
// orchestrated before adds a span.
type OrchConversation struct {
	Main  string           `json:"main"`
	Spans []planusage.Span `json:"spans"`
}

// open is whether the conversation still orchestrates.
func (c OrchConversation) open() bool {
	return len(c.Spans) > 0 && c.Spans[len(c.Spans)-1].Until.IsZero()
}

// closeConversations ends the open spans of cs at now, in place.
func closeConversations(cs []OrchConversation, now time.Time) {
	for i := range cs {
		if cs[i].open() {
			cs[i].Spans = append([]planusage.Span(nil), cs[i].Spans...)
			cs[i].Spans[len(cs[i].Spans)-1].Until = now
		}
	}
}

// cloneConversations is a deep copy of cs.
func cloneConversations(cs []OrchConversation) []OrchConversation {
	out := make([]OrchConversation, len(cs))
	for i, c := range cs {
		out[i] = OrchConversation{Main: c.Main, Spans: append([]planusage.Span(nil), c.Spans...)}
	}
	return out
}

// OrchCleared is what a cleared binding leaves on its group until a meter pass
// has scanned past the clear: the conversations it had, their spans ended at
// At, so their last messages before the clear still count as backlog spend.
type OrchCleared struct {
	At            time.Time          `json:"at"`
	Conversations []OrchConversation `json:"conversations"`
}

func (o *Orchestrator) clone() *Orchestrator {
	if o == nil {
		return nil
	}
	c := *o
	c.Meter = maps.Clone(o.Meter)
	c.Conversations = cloneConversations(o.Conversations)
	if o.Replacing != nil {
		r := *o.Replacing
		c.Replacing = &r
	}
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
		o.Conversations = cloneConversations(prev.Conversations)
		if prev.SessionID != o.SessionID {
			// The previous session stops orchestrating now: what it writes from
			// here on is not the group's (it may go on working for the user).
			closeConversations(o.Conversations, now)
		}
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
		if len(prev.Conversations) > 0 {
			// Its last messages before the clear are still the group's spend: the
			// next meter pass classifies them (Codex P2 on #6616).
			cs := cloneConversations(prev.Conversations)
			closeConversations(cs, now)
			if it.OrchCleared == nil {
				it.OrchCleared = &OrchCleared{}
			}
			it.OrchCleared.At = now
			it.OrchCleared.Conversations = append(it.OrchCleared.Conversations, cs...)
		}
	}
	return prev, nil
}

// DropOrchCleared removes the cleared bindings' records that a meter pass
// started at passStart has scanned past, keeping one while pending says one of
// its conversations still has messages the scan read past unpriced: a price
// added later makes the scan read them again, and they must still be
// classified (Codex P2 on #6616). It returns the items it changed.
func (b *Backlog) DropOrchCleared(passStart time.Time, pending func(OrchConversation) bool) []int {
	var ids []int
	for i := range b.Items {
		it := &b.Items[i]
		if it.OrchCleared != nil && it.OrchCleared.At.Before(passStart) && !slices.ContainsFunc(it.OrchCleared.Conversations, pending) {
			it.OrchCleared = nil
			ids = append(ids, it.ID)
		}
	}
	return ids
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
		return 0, errNotOrchestrator(sessionID)
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

func errNotOrchestrator(sessionID string) error {
	return fmt.Errorf("session %q orchestrates no group; bind it with \"af backlog orchestrate <group> --self\"", sessionID)
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

package backlog

import (
	"fmt"
	"strings"
	"time"
)

// WithEffortSessionReason is the reason an item shows while its question waits
// for the group's orchestrator.
const WithEffortSessionReason = "With effort session"

// orchestratorFor is the orchestrator of the group above id, or nil.
func (b *Backlog) orchestratorFor(id int) *Orchestrator {
	if g := b.Item(b.rootOf(id)); g != nil {
		return g.Orchestrator
	}
	return nil
}

// withOrchestrator is whether question q of item id waits for the group's
// orchestrator rather than the user. A routed question in a group that lost
// its orchestrator is the user's.
func (b *Backlog) withOrchestrator(id int, q *Message) bool {
	return q.Route == RouteOrchestrator && b.orchestratorFor(id) != nil
}

// userQuestion is the item's oldest open question that is with the user, or
// nil.
func (b *Backlog) userQuestion(it *Item) *Message {
	for _, q := range it.openQuestions() {
		if !b.withOrchestrator(it.ID, q) {
			return q
		}
	}
	return nil
}

// orchestratorQuestion is the item's oldest open question that waits for the
// group's orchestrator, or nil.
func (b *Backlog) orchestratorQuestion(it *Item) *Message {
	for _, q := range it.openQuestions() {
		if b.withOrchestrator(it.ID, q) {
			return q
		}
	}
	return nil
}

// AttentionRef is what item id's session attention shows: the id and text of
// its oldest open question that is with the user. When every open question
// waits for the group's orchestrator, it is the oldest one's id with no text:
// the attention clears, but the id still says the session waits on an answer.
// It is 0 and "" only when no question is open.
func (b *Backlog) AttentionRef(id int) (int, string) {
	it := b.Item(id)
	if it == nil {
		return 0, ""
	}
	if q := b.userQuestion(it); q != nil {
		return q.ID, q.Text
	}
	if q := it.OpenQuestion(); q != nil {
		return q.ID, ""
	}
	return 0, ""
}

// Escalate sends item id's open question from the group's orchestrator to the
// user, with a note when note is not empty. callerSessionID is the session
// that escalates: the note is the orchestrator's when it is that session, and
// the user's otherwise. It returns the question.
func (b *Backlog) Escalate(id int, note, callerSessionID string, now time.Time) (*Message, error) {
	it, err := b.get(id)
	if err != nil {
		return nil, err
	}
	q := b.orchestratorQuestion(it)
	if q == nil {
		if b.userQuestion(it) != nil {
			return nil, fmt.Errorf("the open question on %s is already with the user", FormatID(id))
		}
		return nil, fmt.Errorf("%s has no open question to escalate", FormatID(id))
	}
	author := AuthorUser
	if o := b.orchestratorFor(id); callerSessionID != "" && o.SessionID == callerSessionID {
		author = "agent:" + callerSessionID
	}
	q.Route = RouteUser
	it.UpdatedAt = now
	out := *q
	if note = strings.TrimSpace(note); note != "" {
		it.appendMessage(Message{Kind: KindNote, Author: author, Text: note}, now)
	}
	return &out, nil
}

// escalateStranded sends to the user the open questions an orchestrator no
// longer owns after a write: a question routed to an orchestrator its group
// no longer has, as after an unbind, goes to the user as escalated. A routed
// or escalated question whose item moved to another group since prev goes to
// the user as a plain question, so the new group's orchestrator hears nothing
// of a question it never had. It returns the items it changed.
func (b *Backlog) escalateStranded(prev *Backlog, now time.Time) []int {
	var ids []int
	for i := range b.Items {
		it := &b.Items[i]
		moved := false
		if prev != nil && prev.Item(it.ID) != nil {
			moved = prev.rootOf(it.ID) != b.rootOf(it.ID)
		}
		changed := false
		for _, q := range it.openQuestions() {
			switch {
			case q.Route == "":
			case moved:
				q.Route = ""
				changed = true
			case q.Route == RouteOrchestrator && !b.withOrchestrator(it.ID, q):
				q.Route = RouteUser
				changed = true
			}
		}
		if changed {
			it.UpdatedAt = now
			ids = append(ids, it.ID)
		}
	}
	return ids
}

// OrchestratorDoneNote is the Inbox note af adds when a group's orchestrator
// marks an item done.
func OrchestratorDoneNote(pr string) string {
	if pr == "" {
		return "The effort session marked this done."
	}
	return "The effort session marked this done · " + pr
}

// noteOrchestratorDone adds an Inbox note on item id when sessionID, the
// session that marked it done, orchestrates its group. It reports whether it
// added one.
func (b *Backlog) noteOrchestratorDone(id int, sessionID string, now time.Time) bool {
	o := b.orchestratorFor(id)
	it := b.Item(id)
	if it == nil || o == nil || sessionID == "" || o.SessionID != sessionID || b.rootOf(id) == id {
		return false
	}
	it.appendMessage(Message{Kind: KindNote, Author: "agent:" + sessionID, Text: OrchestratorDoneNote(it.PR), Inbox: true}, now)
	return true
}

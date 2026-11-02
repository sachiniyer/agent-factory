package backlog

import (
	"slices"
	"strings"
	"time"
)

// Where an item's question goes. A question with no route goes to the user,
// as one in a group with no orchestrator always does.
const (
	RouteOrchestrator = "orchestrator"
	// RouteUser marks a question the orchestrator escalated to the user.
	RouteUser = "user"
)

// OrchStallAfter is how long an item's run may sit idle, with no done and no
// question, before af tells the group's orchestrator it stalled.
const OrchStallAfter = 30 * time.Minute

// HasOrchestrator is whether any group of b has an orchestrator.
func (b *Backlog) HasOrchestrator() bool {
	for i := range b.Items {
		if b.Items[i].Orchestrator != nil {
			return true
		}
	}
	return false
}

// queueOrchChanges compares b with prev, the same backlog before a write, and
// queues the events the write caused on the orchestrated groups: an agent's
// new question, an item that moved to review or got a new PR there, an escalated question the user
// answered, and a group whose children are all done or dropped. It returns
// the groups it queued on.
func (b *Backlog) queueOrchChanges(prev *Backlog, now time.Time) []int {
	if !b.HasOrchestrator() {
		return nil
	}
	var groups []int
	queue := func(ev OrchEvent) {
		if g := b.QueueOrchEvent(ev, now); g != 0 && !slices.Contains(groups, g) {
			groups = append(groups, g)
		}
	}
	for i := range b.Items {
		it := &b.Items[i]
		if it.Orchestrator != nil && b.groupFinished(it.ID) && !prev.groupFinished(it.ID) {
			queue(OrchEvent{Kind: EventFinished, Item: it.ID})
		}
		if b.IsParent(it.ID) {
			continue
		}
		was := prev.Item(it.ID)
		if it.Status == StatusReview && (was == nil || was.Status != StatusReview || (it.PR != "" && was.PR != it.PR)) {
			queue(OrchEvent{Kind: EventReview, Item: it.ID})
		}
		for _, m := range it.Thread {
			if m.Kind != KindQuestion {
				continue
			}
			if strings.HasPrefix(m.Author, "agent:") && (was == nil || was.message(m.ID) == nil) {
				queue(OrchEvent{Kind: EventQuestion, Item: it.ID, Message: m.ID})
			}
			if m.Route == RouteUser && (m.Closed == ClosedAnswered || m.Closed == ClosedAnsweredInSession) && was != nil && was.questionOpen(m.ID) {
				queue(OrchEvent{Kind: EventEscalationAnswered, Item: it.ID, Message: m.ID})
			}
		}
	}
	return groups
}

// groupFinished is whether group has children and every item below it that
// is not itself a parent is done or dropped.
func (b *Backlog) groupFinished(group int) bool {
	found := false
	for i := range b.Items {
		it := &b.Items[i]
		if !b.IsAncestor(group, it.ID) && it.Parent != group {
			continue
		}
		if b.IsParent(it.ID) {
			continue
		}
		if it.Status.Open() {
			return false
		}
		found = true
	}
	return found
}

// RunActivity is what the daemon knows about an item run's session for stall
// detection.
type RunActivity struct {
	// Working: the session is live and its run is active.
	Working bool
	// Stopped: the session is dead, archived, killed, gone, or lost with no
	// restore coming.
	Stopped bool
	// LastActive is the latest sign of work in the session: pane output or a
	// prompt sent to it. Zero when unknown.
	LastActive time.Time
}

// QueueOrchStalls queues a stalled event for each running item of an
// orchestrated group whose run sat idle OrchStallAfter with no done and no
// question, or whose session stopped. Each idle period gets one event: the
// run's StalledAt records it, and only work after it starts a new period.
// activity reports a session; false when the daemon cannot tell yet. It
// returns the groups it queued on.
func (b *Backlog) QueueOrchStalls(activity func(sessionID string) (RunActivity, bool), now time.Time) []int {
	if !b.HasOrchestrator() {
		return nil
	}
	var groups []int
	for i := range b.Items {
		it := &b.Items[i]
		run := it.lastRun()
		if it.Status != StatusRunning || run == nil || run.State != RunStarted || run.SessionID == "" ||
			it.OpenQuestion() != nil || b.IsParent(it.ID) {
			continue
		}
		g := b.Item(b.rootOf(it.ID))
		if g == nil || g.Orchestrator == nil {
			continue
		}
		a, ok := activity(run.SessionID)
		if !ok {
			continue
		}
		since := run.StartedAt
		if a.LastActive.After(since) {
			since = a.LastActive
		}
		switch {
		case a.Stopped:
		case a.Working && now.Sub(since) >= OrchStallAfter:
		default:
			continue
		}
		if !run.StalledAt.Before(since) {
			continue
		}
		run.StalledAt = now
		it.UpdatedAt = now
		if gid := b.QueueOrchEvent(OrchEvent{Kind: EventStalled, Item: it.ID}, now); gid != 0 && !slices.Contains(groups, gid) {
			groups = append(groups, gid)
		}
	}
	return groups
}

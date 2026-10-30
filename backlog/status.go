package backlog

import (
	"fmt"
	"time"
)

type Actor string

const (
	ActorUser  Actor = "user"
	ActorAgent Actor = "agent"
	ActorAF    Actor = "af"
)

// StatusChange is one requested status change on a leaf item.
type StatusChange struct {
	ID   int
	From Status
	To   Status
	By   Actor
	// RunActive is whether the item's session has an active backlog run.
	RunActive bool
	// StartFailed marks af putting a failed start back to PrevStatus.
	StartFailed bool
	PrevStatus  Status
}

// CheckStatusChange validates c against the status-change table. It does not
// know about parents; Backlog.SetStatus handles those.
func CheckStatusChange(c StatusChange) error {
	if !c.To.Valid() {
		return fmt.Errorf("unknown status %q", c.To)
	}
	if c.From == c.To && c.To != StatusRunning && !agentReview(c) {
		return fmt.Errorf("%s is already %s", FormatID(c.ID), c.To)
	}
	if c.StartFailed {
		if c.By != ActorAF || c.From != StatusRunning || c.To != c.PrevStatus || !c.PrevStatus.Valid() {
			return refuse(c)
		}
		return nil
	}
	ok := false
	switch c.To {
	case StatusQueued:
		ok = c.By == ActorUser && (c.From == StatusDraft || c.From == StatusReview)
	case StatusDraft:
		ok = c.By == ActorUser && (c.From == StatusQueued || c.From == StatusReview || c.From == StatusDone || c.From == StatusDropped)
	case StatusRunning:
		if c.By != ActorAF {
			return fmt.Errorf("only af sets %s to running; use \"af backlog start %d\"", FormatID(c.ID), c.ID)
		}
		if c.From == StatusRunning && c.RunActive {
			return fmt.Errorf("%s is already working", FormatID(c.ID))
		}
		ok = c.From == StatusDraft || c.From == StatusQueued || c.From == StatusReview || c.From == StatusRunning
	case StatusReview:
		ok = c.From == StatusRunning || agentReview(c)
	case StatusDone, StatusDropped:
		ok = c.By == ActorUser && c.From.Open()
	}
	if !ok {
		return refuse(c)
	}
	return nil
}

// agentReview is a repeated `done --self`: the item stays in review and the
// caller updates the PR link.
func agentReview(c StatusChange) bool {
	return c.By == ActorAgent && c.From == StatusReview && c.To == StatusReview
}

func refuse(c StatusChange) error {
	hint := ""
	switch {
	case !c.From.Open() && c.To != StatusDraft:
		hint = fmt.Sprintf(" — reopen it first with \"af backlog reopen %d\"", c.ID)
	case c.To == StatusReview:
		hint = " — only a running item goes to review"
	}
	return fmt.Errorf("%s is %s; %s cannot set it to %s%s", FormatID(c.ID), c.From, actorLabel(c.By), c.To, hint)
}

func actorLabel(a Actor) string {
	switch a {
	case ActorUser:
		return "you"
	case ActorAgent:
		return "an agent"
	case ActorAF:
		return "af"
	}
	return string(a)
}

// SetStatus applies a user or agent status change to id. A parent accepts
// only queued (queues its draft leaves), dropped (drops its open leaves) and
// draft (reopens its done and dropped leaves). Done and dropped close the item's open questions. runActive
// reports whether a session has an active backlog run; pass nil when no
// session can be running. It returns the ids it changed. On error b is
// unchanged.
func (b *Backlog) SetStatus(id int, to Status, by Actor, runActive func(*Item) bool, now time.Time) ([]int, error) {
	c := b.clone()
	it, err := c.get(id)
	if err != nil {
		return nil, err
	}
	if runActive == nil {
		runActive = func(*Item) bool { return false }
	}
	if !c.IsParent(id) {
		if err := CheckStatusChange(StatusChange{ID: id, From: it.Status, To: to, By: by, RunActive: runActive(it)}); err != nil {
			return nil, err
		}
		c.applyStatus(it, to, now)
		*b = *c
		return []int{id}, nil
	}

	var want func(Status) bool
	switch to {
	case StatusQueued:
		want = func(s Status) bool { return s == StatusDraft }
	case StatusDropped:
		want = Status.Open
	case StatusDraft:
		want = func(s Status) bool { return !s.Open() }
	case StatusRunning:
		return nil, fmt.Errorf("%s is a parent; parents run through their children — start a child", FormatID(id))
	default:
		return nil, fmt.Errorf("%s is a parent; only queue, drop and reopen apply to it", FormatID(id))
	}
	if by != ActorUser {
		return nil, refuse(StatusChange{ID: id, From: it.Status, To: to, By: by})
	}
	var changed []int
	for i := range c.Items {
		leaf := &c.Items[i]
		if !c.IsAncestor(id, leaf.ID) || c.IsParent(leaf.ID) || !want(leaf.Status) {
			continue
		}
		if err := CheckStatusChange(StatusChange{ID: leaf.ID, From: leaf.Status, To: to, By: by, RunActive: runActive(leaf)}); err != nil {
			return nil, err
		}
		c.applyStatus(leaf, to, now)
		changed = append(changed, leaf.ID)
	}
	if len(changed) == 0 {
		return nil, fmt.Errorf("%s has no children to %s", FormatID(id), verb(to))
	}
	if to == StatusDropped {
		// A dropped subtree has nothing left to ask: close the questions on the
		// target and on every intermediate parent too, or its roll-up keeps
		// reading needs_you and a retry finds no leaf left to drop.
		for i := range c.Items {
			parent := &c.Items[i]
			if (parent.ID != id && !c.IsAncestor(id, parent.ID)) || !c.IsParent(parent.ID) || parent.OpenQuestion() == nil {
				continue
			}
			parent.closeOpenQuestions(ClosedDropped)
			parent.UpdatedAt = now
			changed = append(changed, parent.ID)
		}
	}
	*b = *c
	return changed, nil
}

func verb(s Status) string {
	switch s {
	case StatusQueued:
		return "queue"
	case StatusDraft:
		return "reopen"
	}
	return "drop"
}

func (b *Backlog) applyStatus(it *Item, to Status, now time.Time) {
	if to != StatusRunning {
		it.LiveAnswers = 0
	}
	it.Status = to
	it.UpdatedAt = now
	switch to {
	case StatusDone:
		it.closeOpenQuestions(ClosedDone)
	case StatusDropped:
		it.closeOpenQuestions(ClosedDropped)
	case StatusReview:
		it.closeAFQuestions(ClosedSentToReview)
	}
}

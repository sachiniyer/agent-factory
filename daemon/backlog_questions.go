package daemon

import (
	"errors"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/session"
)

// restoreBacklogSession restores an archived, lost or dead item session before
// an answer is delivered. A package var so tests can stand in for a restore.
var restoreBacklogSession = func(m *Manager, req RestoreSessionRequest) (session.InstanceData, error) {
	_, restored, err := m.RestoreSession(req)
	return restored, err
}

// backlogAnswerWait blocks while a held answer waits for the user to detach or
// a usage limit to reset. It reports false when the daemon is stopping. A
// package var so tests can see the hold and end it.
var backlogAnswerWait = func(stop <-chan struct{}) bool {
	t := time.NewTimer(2 * time.Second)
	defer t.Stop()
	select {
	case <-stop:
		return false
	case <-t.C:
		return true
	}
}

var errBacklogAnswerHeld = errors.New("held")

func (h backlogHost) SetAttention(repoID, sessionID, reason string) {
	req := SetAttentionRequest{ID: sessionID, RepoID: repoID, Reason: reason, Clear: reason == ""}
	if _, _, err := h.m.SetAttention(req); err != nil {
		h.m.warn().Printf("backlog attention on session %s: %v", sessionID, err)
	}
}

func (h backlogHost) AnswerTarget(repoID, sessionID string) backlog.AnswerTarget {
	s, ok := h.m.itemSessions(repoID)[sessionID]
	if !ok || s.view.UserKilled || s.view.InFlightOp == session.OpKilling {
		return backlog.TargetGone
	}
	if backlogNeedsRestore(s) {
		return backlog.TargetRestore
	}
	return backlog.TargetLive
}

// backlogNeedsRestore is whether an answer must restore the session first. A
// package var so tests can stand in for an archived session.
var backlogNeedsRestore = func(s itemSession) bool {
	switch s.view.Liveness {
	case session.LiveArchived, session.LiveLost, session.LiveDead:
		return true
	}
	return false
}

func (h backlogHost) DeliverAnswer(repoID string, a backlog.PendingAnswer) {
	m := h.m
	m.backlog.reserve(repoID)
	launched := m.launchBackgroundMutation(func(stop <-chan struct{}) {
		m.deliverBacklogAnswer(stop, repoID, a)
	})
	if !launched {
		m.backlog.release(repoID)
		m.backlogAnswerFailed(repoID, a, "af is shutting down")
	}
}

// deliverBacklogAnswer finishes a pending answer. The caller reserved a slot
// for it; the slot passes to the run marker once it is armed. A restore comes
// first. The run is armed before any hold, so a held answer shows Working and
// keeps its slot, and the send takes the per-target lock task deliveries use.
// A client attached full screen or a usage limit holds the send.
func (m *Manager) deliverBacklogAnswer(stop <-chan struct{}, repoID string, a backlog.PendingAnswer) {
	reserved := true
	release := func() {
		if reserved {
			reserved = false
			m.backlog.release(repoID)
		}
	}
	defer release()
	s, ok := m.itemSessions(repoID)[a.SessionID]
	if !ok || s.view.UserKilled || s.view.InFlightOp == session.OpKilling {
		m.backlogAnswerFailed(repoID, a, "the session is gone")
		return
	}
	if backlogNeedsRestore(s) {
		restored, err := restoreBacklogSession(m, RestoreSessionRequest{ID: a.SessionID, RepoID: repoID})
		if err != nil {
			m.backlogAnswerFailed(repoID, a, "af could not restore the session: "+err.Error())
			return
		}
		m.publishEvent(agentproto.EventSessionRestored, restored)
		if s, ok = m.itemSessions(repoID)[a.SessionID]; !ok {
			m.backlogAnswerFailed(repoID, a, "the session is gone")
			return
		}
		m.syncBacklogAttention(repoID, a)
	}
	inst := s.instance
	if inst.TaskRunActive() {
		m.backlogAnswerFailed(repoID, a, "the session is running a task")
		return
	}
	if !inst.ArmBacklogRun(time.Now()) {
		m.backlogAnswerFailed(repoID, a, "the session is not an item session")
		return
	}
	m.persistAndPublishInstance(repoID, inst)
	release()
	for {
		err := m.sendBacklogAnswer(repoID, inst, a.Prompt)
		if err == nil {
			m.backlogAnswerDelivered(repoID, a)
			return
		}
		if !errors.Is(err, errBacklogAnswerHeld) {
			m.disarmBacklogRun(repoID, inst)
			m.backlogAnswerFailed(repoID, a, err.Error())
			return
		}
		if !backlogAnswerWait(stop) {
			m.disarmBacklogRun(repoID, inst)
			m.backlogAnswerFailed(repoID, a, "af stopped while the answer was held")
			return
		}
		if _, ok := m.itemSessions(repoID)[a.SessionID]; !ok {
			m.backlogAnswerFailed(repoID, a, "the session is gone")
			return
		}
	}
}

// sendBacklogAnswer sends under the per-target lock, or returns
// errBacklogAnswerHeld while a client is attached or the session is parked at
// a usage limit.
func (m *Manager) sendBacklogAnswer(repoID string, inst *session.Instance, prompt string) error {
	unlock := m.lockTarget(repoID, inst.Title)
	defer unlock()
	if m.isPollPaused(repoID, inst.Title, inst.ID) || inst.GetLiveness() == session.LiveLimitReached {
		return errBacklogAnswerHeld
	}
	return sendBacklogPrompt(m, SendPromptRequest{ID: inst.ID, Title: inst.Title, RepoID: repoID, Prompt: prompt})
}

// syncBacklogAttention mirrors the item's oldest open question on a session
// that was just restored, under the backlog lock.
func (m *Manager) syncBacklogAttention(repoID string, a backlog.PendingAnswer) {
	_, err := m.backlogService().Apply(backlog.ProjectRef{ID: repoID}, func(b *backlog.Backlog) ([]int, error) {
		if text := b.OpenQuestionText(a.ID); text != "" {
			backlogHost{m: m}.SetAttention(repoID, a.SessionID, text)
		}
		return []int{}, nil
	})
	if err != nil {
		m.warn().Printf("backlog attention after restoring session %s: %v", a.SessionID, err)
	}
}

func (m *Manager) backlogAnswerDelivered(repoID string, a backlog.PendingAnswer) {
	svc := m.backlogService()
	if _, err := svc.Apply(backlog.ProjectRef{ID: repoID}, func(b *backlog.Backlog) ([]int, error) {
		return []int{a.ID}, b.AnswerDelivered(a.ID, a.AnswerID, svc.Now())
	}); err != nil {
		m.warn().Printf("record delivered answer on %s: %v", backlog.FormatID(a.ID), err)
	}
}

func (m *Manager) backlogAnswerFailed(repoID string, a backlog.PendingAnswer, reason string) {
	svc := m.backlogService()
	if _, err := svc.Apply(backlog.ProjectRef{ID: repoID}, func(b *backlog.Backlog) ([]int, error) {
		return []int{a.ID}, b.AnswerFailed(a.ID, a.AnswerID, reason, svc.Now())
	}); err != nil {
		m.warn().Printf("record undelivered answer on %s: %v", backlog.FormatID(a.ID), err)
	}
}

// noteBacklogSubmitted closes the item's oldest open question as answered in
// session when the user submits a line in the agent pane, and arms the run if
// the item is running without one.
func (m *Manager) noteBacklogSubmitted(instance *session.Instance) {
	if instance == nil || instance.BacklogItem == 0 {
		return
	}
	repoID, ok := m.repoIDForInstance(instance)
	if !ok {
		return
	}
	sessionID := instance.ID
	m.launchBackgroundMutation(func(<-chan struct{}) {
		b, err := backlog.Load(repoID)
		if err != nil {
			return
		}
		if it := b.ItemForSession(sessionID); it == nil || it.OpenQuestion() == nil {
			return
		}
		svc := m.backlogService()
		running := false
		_, err = svc.Apply(backlog.ProjectRef{ID: repoID}, func(b *backlog.Backlog) ([]int, error) {
			id := b.AnsweredInSession(sessionID, svc.Now())
			if id == 0 {
				return []int{}, nil
			}
			running = b.Item(id).Status == backlog.StatusRunning
			backlogHost{m: m}.SetAttention(repoID, sessionID, b.OpenQuestionText(id))
			return []int{id}, nil
		})
		if err != nil {
			m.warn().Printf("close question answered in session %s: %v", sessionID, err)
			return
		}
		if running && !instance.BacklogRunActive() && instance.ArmBacklogRun(time.Now()) {
			m.persistAndPublishInstance(repoID, instance)
		}
	})
}

// backlogSubmitBinding reports a line the user submits in an item session's
// agent pane, so an open question there closes as answered in session. The fork
// hung this on its attention-clearing binding; master has no such binding (the
// attention port is theme A), so the wrapper stands alone and the two can be
// combined when that lands.
type backlogSubmitBinding struct {
	ptyTabBinding
	submitted func()
}

func (b backlogSubmitBinding) input(p []byte) error {
	err := b.ptyTabBinding.input(p)
	if err == nil && isSubmittedInput(p) {
		b.submitted()
	}
	return err
}

// isSubmittedInput reports whether a typed frame ends a line: the user sent
// what they typed. A frame that starts with ESC is a terminal's own reply to a
// focus, cursor or device query, not the user answering.
func isSubmittedInput(p []byte) bool {
	if len(p) == 0 || p[0] == 0x1b {
		return false
	}
	for _, c := range p {
		if c == '\r' || c == '\n' {
			return true
		}
	}
	return false
}

// redeliverBacklogAnswers finishes the answers a daemon restart left pending.
// It returns the sessions they go to, so recovery does not end their runs.
func (m *Manager) redeliverBacklogAnswers(repoID string, b *backlog.Backlog) map[string]bool {
	out := map[string]bool{}
	for _, a := range b.PendingAnswers() {
		if it := b.Item(a.ID); it == nil || !answeredBefore(it, a.AnswerID, daemonProcessStart) {
			continue
		}
		out[a.SessionID] = true
		backlogHost{m: m}.DeliverAnswer(repoID, a)
	}
	return out
}

func answeredBefore(it *backlog.Item, answerID int, t time.Time) bool {
	for _, msg := range it.Thread {
		if msg.ID == answerID {
			return msg.At.Before(t)
		}
	}
	return false
}

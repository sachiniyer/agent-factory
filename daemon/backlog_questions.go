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

func (h backlogHost) SetAttention(repoID, sessionID string, questionID int, reason string) {
	if reason == "" {
		h.m.backlog.asked.Delete(sessionID)
		h.m.clearArchivedForAnswer(repoID, sessionID)
	} else {
		h.m.backlog.asked.Store(sessionID, seenQuestion{ID: questionID, Text: session.NormalizeAttentionReason(reason)})
	}
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
	// One worker per answer: recovery can offer the same pending answer again on
	// every retry (an answer held behind an attached client stays pending), and
	// each worker would send it once the client detaches (Codex P2 on #6295).
	if !m.backlog.claimAnswer(repoID, a) {
		return
	}
	m.backlog.reserve(repoID)
	launched := m.launchBackgroundMutation(func(stop <-chan struct{}) {
		defer m.backlog.releaseAnswer(repoID, a)
		m.deliverBacklogAnswer(stop, repoID, a)
	})
	if !launched {
		m.backlog.releaseAnswer(repoID, a)
		m.backlog.release(repoID)
		m.backlogAnswerFailed(repoID, a, "af is shutting down", nil)
	}
}

// deliverBacklogAnswer finishes a pending answer. The caller reserved a slot
// for it, kept while the send is held and passed to the run marker when the
// run is armed. A restore comes first. The run is armed immediately before the
// send (sendBacklogAnswer), which takes the per-target lock task deliveries
// use; a client attached full screen or a usage limit holds the send, and a held
// answer keeps its reserved slot without opening the completion gate.
func (m *Manager) deliverBacklogAnswer(stop <-chan struct{}, repoID string, a backlog.PendingAnswer) {
	reserved := true
	release := func() {
		if reserved {
			reserved = false
			m.backlog.release(repoID)
		}
	}
	defer release()
	for waits := 0; ; waits++ {
		inst, why := m.backlogAnswerTarget(repoID, a)
		if why != "" {
			m.backlogAnswerFailed(repoID, a, why, stop)
			return
		}
		err := m.sendBacklogAnswerUntilSettled(stop, repoID, inst, a)
		if err == nil {
			release() // the armed run marker holds the slot from here
			m.backlogAnswerDelivered(repoID, a, stop)
			return
		}
		if !errors.Is(err, errBacklogAnswerTornDown) {
			m.backlogAnswerFailed(repoID, a, err.Error(), stop)
			return
		}
		// The session is being archived (af's archive after the agent asked, or
		// the user's) or deleted. Wait for that to settle, then restore the
		// session, or find it gone, and send again: the archive's guard checks
		// the question just before it shuts the session, so an answer recorded
		// after that check meets the archive here instead of failing.
		if waits >= backlogAnswerTeardownWaits {
			m.backlogAnswerFailed(repoID, a, "the session stayed busy being archived or deleted", stop)
			return
		}
		if !backlogAnswerWait(stop) {
			m.backlogAnswerFailed(repoID, a, "af stopped while the answer waited for the session's archive", stop)
			return
		}
	}
}

// backlogAnswerTeardownWaits bounds how many waits an answer spends on an
// archive or a kill of its session in progress (each backlogAnswerWait).
const backlogAnswerTeardownWaits = 150

// errBacklogAnswerTornDown marks a send that met an archive or a kill of the
// session in progress: nothing was sent, and the caller waits it out.
var errBacklogAnswerTornDown = errors.New("the session is being archived or deleted")

// backlogAnswerTarget returns the live session an answer goes to, restoring it
// first when it is archived, lost or dead, or why it cannot be had.
func (m *Manager) backlogAnswerTarget(repoID string, a backlog.PendingAnswer) (*session.Instance, string) {
	s, ok := m.itemSessions(repoID)[a.SessionID]
	if !ok || s.view.UserKilled || s.view.InFlightOp == session.OpKilling {
		return nil, "the session is gone"
	}
	// Restoration is serialized per session and rechecked under the lock: two
	// answers to one archived session (recovery launches them together) must not
	// both restore it, the second would be refused as already live and its answer
	// failed although the agent is there (Codex P2 on #6295).
	restoreMu := m.backlog.restoreLock(a.SessionID)
	restoreMu.Lock()
	if s, ok = m.itemSessions(repoID)[a.SessionID]; ok && backlogNeedsRestore(s) {
		restored, err := restoreBacklogSession(m, RestoreSessionRequest{ID: a.SessionID, RepoID: repoID})
		if err != nil {
			restoreMu.Unlock()
			return nil, "af could not restore the session: " + err.Error()
		}
		m.publishEvent(agentproto.EventSessionRestored, restored)
		m.syncBacklogAttention(repoID, a)
		s, ok = m.itemSessions(repoID)[a.SessionID]
	}
	restoreMu.Unlock()
	if !ok {
		return nil, "the session is gone"
	}
	if s.instance.TaskRunActive() {
		return nil, "the session is running a task"
	}
	return s.instance, ""
}

// sendBacklogAnswerUntilSettled sends the answer, holding it while a client is
// attached or the session is at a usage limit. It returns nil once delivered,
// errBacklogAnswerTornDown when an archive or a kill is in the way, or why the
// answer cannot be delivered.
func (m *Manager) sendBacklogAnswerUntilSettled(stop <-chan struct{}, repoID string, inst *session.Instance, a backlog.PendingAnswer) error {
	for {
		// The item may have been restarted in a new session while this answer waited
		// (to be restored, or behind an attached client): it is no longer the
		// item's latest run, and sending would leave two sessions working on it.
		if !backlogSessionIsLatest(repoID, a.ID, a.SessionID) {
			return errors.New("the item was started again in another session")
		}
		err := m.sendBacklogAnswer(repoID, inst, a.Prompt)
		if err == nil || !errors.Is(err, errBacklogAnswerHeld) {
			return err
		}
		if !backlogAnswerWait(stop) {
			return errors.New("af stopped while the answer was held")
		}
		if _, ok := m.itemSessions(repoID)[a.SessionID]; !ok {
			return errors.New("the session is gone")
		}
	}
}

// backlogTearingDown reports whether an archive or a kill of the session is in
// progress or done: its claim is held, its op is archiving or killing, or it is
// archived.
func (m *Manager) backlogTearingDown(repoID string, inst *session.Instance) bool {
	switch inst.GetInFlightOp() {
	case session.OpArchiving, session.OpKilling:
		return true
	}
	if inst.GetLiveness() == session.LiveArchived {
		return true
	}
	m.mu.Lock()
	_, claimed := m.killsInFlight[daemonInstanceKey(repoID, inst.Title)]
	m.mu.Unlock()
	return claimed
}

// sendBacklogAnswer sends under the per-target lock, or returns
// errBacklogAnswerHeld while a client is attached or the session is parked at
// a usage limit. The run is armed here, immediately before the submission and
// disarmed again if it fails: a held answer has no open completion gate (an
// attached idle pane could otherwise satisfy the silent fallback and end a run
// whose prompt was never sent), and its slot stays reserved by the caller.
func (m *Manager) sendBacklogAnswer(repoID string, inst *session.Instance, prompt string) error {
	unlock := m.lockTarget(repoID, inst.Title)
	defer unlock()
	if m.isPollPaused(repoID, inst.Title, inst.ID) || inst.GetLiveness() == session.LiveLimitReached {
		return errBacklogAnswerHeld
	}
	if m.backlogTearingDown(repoID, inst) {
		return errBacklogAnswerTornDown
	}
	// A second answer can arrive while the first is still being worked on: the
	// run that was already active is not ours to close if this send fails.
	wasActive := inst.BacklogRunActive()
	if !inst.ArmBacklogRun(time.Now()) {
		return errors.New("the session is not an item session")
	}
	m.persistAndPublishInstance(repoID, inst)
	if err := sendBacklogPrompt(m, SendPromptRequest{ID: inst.ID, Title: inst.Title, RepoID: repoID, Prompt: prompt, BacklogOrigin: true}); err != nil {
		if errors.Is(err, errBacklogDeliveryUncertain) {
			// It may have landed: keep the run marker (the agent may be working on
			// the answer) and treat the answer as delivered so a restart does not
			// send it again; the thread says it was not confirmed.
			if ref, rerr := m.backlogService().Resolve(repoID, ""); rerr == nil {
				if it := backlogItemOfSession(repoID, inst.ID); it != 0 {
					m.noteBacklogUncertainDelivery(ref, it, err)
				}
			}
			return nil
		}
		if !wasActive {
			m.disarmBacklogRun(repoID, inst)
		}
		if errors.Is(err, errBacklogTargetLimited) {
			return errBacklogAnswerHeld // a limit published since the check: hold the answer
		}
		if errors.Is(err, errBacklogTargetTearingDown) || m.backlogTearingDown(repoID, inst) {
			return errBacklogAnswerTornDown // an archive or a kill since the check: wait it out
		}
		return err
	}
	return nil
}

// syncBacklogAttention mirrors the item's oldest open question on a session
// that was just restored, under the backlog lock.
func (m *Manager) syncBacklogAttention(repoID string, a backlog.PendingAnswer) {
	_, err := m.backlogService().Apply(backlog.ProjectRef{ID: repoID}, func(b *backlog.Backlog) ([]int, error) {
		if qid, text := b.OpenQuestionRef(a.ID); text != "" {
			backlogHost{m: m}.SetAttention(repoID, a.SessionID, qid, text)
		}
		return []int{}, nil
	})
	if err != nil {
		m.warn().Printf("backlog attention after restoring session %s: %v", a.SessionID, err)
	}
}

// backlogAnswerDelivered records that the prompt reached the agent. The write is
// retried while the backlog is busy (like finishBacklogStart and settleRunEnd):
// left pending, the answer would be sent again by the next restart's
// redelivery and restart work the agent already did. A closed stop gives up.
func (m *Manager) backlogAnswerDelivered(repoID string, a backlog.PendingAnswer, stop <-chan struct{}) {
	svc := m.backlogService()
	if err := m.applyRetrying(repoID, stop, func(b *backlog.Backlog) ([]int, error) {
		return []int{a.ID}, b.AnswerDelivered(a.ID, a.AnswerID, svc.Now())
	}); err != nil {
		m.warn().Printf("record delivered answer on %s: %v", backlog.FormatID(a.ID), err)
		return
	}
	// A pending answer holds back the run's end (RunEnded), so an agent that
	// finished the answer before this write landed had its idle edge dropped and
	// no later edge will come: settle it now.
	m.reconcileIdleRun(repoID, a.SessionID, stop)
}

func (m *Manager) backlogAnswerFailed(repoID string, a backlog.PendingAnswer, reason string, stop <-chan struct{}) {
	svc := m.backlogService()
	if err := m.applyRetrying(repoID, stop, func(b *backlog.Backlog) ([]int, error) {
		return []int{a.ID}, b.AnswerFailed(a.ID, a.AnswerID, reason, svc.Now())
	}); err != nil {
		m.warn().Printf("record undelivered answer on %s: %v", backlog.FormatID(a.ID), err)
		return
	}
	m.reconcileIdleRun(repoID, a.SessionID, stop)
}

// reconcileIdleRun settles the session's run end when it is idle with the marker
// clear: a pending answer held the previous run's end back (RunEnded), and the
// answer's outcome (delivered or failed) is what releases it. Generation-fenced
// like every completion.
func (m *Manager) reconcileIdleRun(repoID, sessionID string, stop <-chan struct{}) {
	if s, ok := m.itemSessions(repoID)[sessionID]; ok && !s.instance.BacklogRunActive() &&
		s.view.Liveness == session.LiveReady {
		m.settleRunEnd(repoID, sessionID, s.gen, stop)
	}
}

// backlogSeenQuestion is the text of the question a line submitted now would
// answer: what the session's attention shows. It is read from memory, never from
// backlog.json, so the terminal's input path cannot stall behind the backlog
// lock; the question is then closed on a worker (noteBacklogSubmitted).
func (m *Manager) backlogSeenQuestion(instance *session.Instance) seenQuestion {
	if instance == nil || instance.BacklogItem == 0 {
		return seenQuestion{}
	}
	remembered, have := seenQuestion{}, false
	if q, ok := m.backlog.asked.Load(instance.ID); ok {
		remembered, have = q.(seenQuestion), true
	}
	if a := instance.Attention(); a != nil {
		// What the chip shows. The remembered question supplies the identity when it
		// is the same one; an attention the backlog did not set has text only.
		if have && remembered.Text == a.Reason {
			return remembered
		}
		return seenQuestion{Text: a.Reason}
	}
	// The chip was dismissed, but the backlog question is still open: it is
	// remembered apart from the display (Codex P2 on #6295).
	return remembered
}

// seenQuestion is the backlog question a session showed: its message id (zero when
// only the display text is known) and the attention-normalized text. The id is the
// identity; two questions can share their first 500 runes.
type seenQuestion struct {
	ID   int
	Text string
}

func (q seenQuestion) empty() bool { return q.ID == 0 && q.Text == "" }

// noteBacklogSubmitted closes the question the user saw (questionText, captured
// before the line was forwarded) as answered in session, and arms the run if the
// item is running without one, in the same transaction so an old completion
// cannot observe the gap.
func (m *Manager) noteBacklogSubmitted(instance *session.Instance, question seenQuestion) {
	if instance == nil || instance.BacklogItem == 0 || question.empty() {
		return
	}
	repoID, ok := m.repoIDForInstance(instance)
	if !ok {
		return
	}
	sessionID := instance.ID
	m.launchBackgroundMutation(func(stop <-chan struct{}) {
		svc := m.backlogService()
		armed := false
		err := m.applyRetrying(repoID, stop, func(b *backlog.Backlog) ([]int, error) {
			id := b.AnsweredInSessionText(sessionID, question.ID, question.Text, session.NormalizeAttentionReason, svc.Now())
			if id == 0 {
				return []int{}, nil
			}
			nextID, nextText := b.OpenQuestionRef(id)
			backlogHost{m: m}.SetAttention(repoID, sessionID, nextID, nextText)
			// The answer typed in the pane is new work: an open item (in review after
			// the agent asked, or queued or drafted since) goes back to running, counts
			// as a live answer for rollbacks of other pending ones, and gets its run
			// armed here, in the same transaction (Codex P2 on #6295).
			if it := b.Item(id); it.Status.Open() {
				// The marker may still be active from the run that asked (the item
				// went to review before its idle poll): the answer is still new work,
				// so the item runs and counts it; only an inactive marker is armed.
				active := instance.BacklogRunActive()
				if !active {
					armed = instance.ArmBacklogRun(time.Now())
				}
				if armed || (active && it.Status != backlog.StatusRunning) {
					it.Status = backlog.StatusRunning
					it.LiveAnswers++
				}
			}
			return []int{id}, nil
		})
		if err != nil {
			if armed {
				m.disarmBacklogRun(repoID, instance)
			}
			m.warn().Printf("close question answered in session %s: %v", sessionID, err)
			return
		}
		if armed {
			m.persistAndPublishInstance(repoID, instance)
		}
	})
}

// backlogSubmitBinding reports a line the user submits in an item session's
// agent pane, so the open question it answers closes as answered in session. The
// question is captured BEFORE the line is forwarded, like the attention-clearing
// binding captures its request: one the agent raises while the input is in
// flight is a different question the user never saw. The fork hung this on the
// attention binding; here it wraps that binding (#5581) and shares its
// isSubmittedInput.
type backlogSubmitBinding struct {
	ptyTabBinding
	openQuestion func() seenQuestion
	submitted    func(question seenQuestion)
	// submit carries bracketed-paste state across the connection's frames.
	submit *submitDetector
}

func (b backlogSubmitBinding) input(p []byte) error {
	submit := b.submit.Submitted(p) // every frame, so paste state stays current
	var seen seenQuestion
	if submit {
		seen = b.openQuestion()
	}
	err := b.ptyTabBinding.input(p)
	if err == nil && submit && !seen.empty() {
		b.submitted(seen)
	}
	return err
}

// redeliverBacklogAnswers finishes the answers a daemon restart left pending.
// It returns the sessions they go to, so recovery does not end their runs.
func (m *Manager) redeliverBacklogAnswers(repoID string, b *backlog.Backlog) map[string]bool {
	out := map[string]bool{}
	for _, a := range b.PendingAnswers() {
		if it := b.Item(a.ID); it == nil || !answeredBefore(it, a.AnswerID, daemonProcessStart) {
			continue
		}
		if a.Superseded {
			// The run that accepted it is gone: recording the failure is all that is
			// left, and it must not mark the new run's session as answering.
			m.launchBackgroundMutation(func(stop <-chan struct{}) {
				m.backlogAnswerFailed(repoID, a, "the item was started again in another session", stop)
			})
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

// backlogItemOfSession is the id of the item whose latest run is the session, or 0.
func backlogItemOfSession(repoID, sessionID string) int {
	b, err := backlog.Load(repoID)
	if err != nil {
		return 0
	}
	if it := b.ItemForSession(sessionID); it != nil {
		return it.ID
	}
	return 0
}

// rebuildAskedQuestions refills the remembered question of each item session from
// the backlog after a restart, so a terminal answer still reaches an open question
// whose attention chip was dismissed. It sets no attention: a dismissed chip stays
// dismissed (Codex P2 on #6299).
func (m *Manager) rebuildAskedQuestions(b *backlog.Backlog) {
	for _, it := range b.Items {
		qid, q := b.OpenQuestionRef(it.ID)
		r := b.LatestRun(it.ID)
		if q == "" || r == nil || r.SessionID == "" {
			continue
		}
		m.backlog.asked.Store(r.SessionID, seenQuestion{ID: qid, Text: session.NormalizeAttentionReason(q)})
	}
}

// backlogSessionIsLatest reports whether sessionID is still the session of item
// id's latest run. An unreadable backlog counts as yes: the answer is not failed
// on a transient read error.
func backlogSessionIsLatest(repoID string, id int, sessionID string) bool {
	b, err := backlog.Load(repoID)
	if err != nil {
		return true
	}
	r := b.LatestRun(id)
	return r == nil || r.SessionID == sessionID
}

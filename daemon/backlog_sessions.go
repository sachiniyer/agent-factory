package daemon

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// errBacklogNoChange aborts a backlog write that has nothing to record.
var errBacklogNoChange = errors.New("backlog: nothing to change")

// itemSession is one item session of a project, read under one lock.
type itemSession struct {
	instance *session.Instance
	view     session.LifecycleView
	resetAt  string
	// gen is the session's run generation in the same snapshot as view.
	gen int
}

// itemSessions returns the project's item sessions by session id.
func (m *Manager) itemSessions(repoID string) map[string]itemSession {
	out := map[string]itemSession{}
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, inst := range m.instances {
		if inst == nil || inst.BacklogItem == 0 {
			continue
		}
		if rid, _ := splitDaemonInstanceKey(key); rid != repoID {
			continue
		}
		s := itemSession{instance: inst, view: inst.LifecycleView(), gen: inst.BacklogRunGeneration()}
		if at, ok := inst.LimitResetAt(); ok && !at.IsZero() {
			s.resetAt = at.Local().Format("15:04")
		}
		out[inst.ID] = s
	}
	return out
}

// backlogSessionFacts is what Derive needs to know about one item session.
// An unknown session was killed: its record is gone.
func backlogSessionFacts(s itemSession, ok bool) backlog.SessionFacts {
	if !ok {
		return backlog.SessionFacts{Reason: "session killed"}
	}
	v := s.view
	switch {
	case v.UserKilled || v.InFlightOp == session.OpKilling:
		return backlog.SessionFacts{Reason: "session killed"}
	case v.StartupStateUnknown:
		return backlog.SessionFacts{Reason: "session did not start"}
	}
	switch v.Liveness {
	case session.LiveArchived:
		return backlog.SessionFacts{Reason: "session archived"}
	case session.LiveDead:
		return backlog.SessionFacts{Reason: "session dead"}
	case session.LiveLost:
		return backlog.SessionFacts{Lost: true, AutoRestore: v.BacklogRunActive && canAutoRestoreLostSession(v), Reason: "session lost"}
	case session.LiveLimitReached:
		reason := "usage limit"
		if s.resetAt != "" {
			reason += " until " + s.resetAt
		}
		return backlog.SessionFacts{Limited: true, Reason: reason}
	}
	return backlog.SessionFacts{Live: true, RunActive: v.BacklogRunActive}
}

// holdsBacklogSlot is whether an item session counts against max_running: its
// run is active on a live session, or it is lost mid-run and af will restore
// it. The same facts drive the Working and Stopped states.
func holdsBacklogSlot(s itemSession) bool {
	f := backlogSessionFacts(s, true)
	return f.RunActive || (f.Lost && f.AutoRestore)
}

// backlogSlotsUsed counts starts in flight plus the item sessions that hold a
// slot and are their item's latest started run. A start is counted by its
// reservation until its run records the session, then by the session.
func (m *Manager) backlogSlotsUsed(repoID string) int {
	used := m.backlog.reserved(repoID)
	var latest map[string]int
	if b, err := backlog.Load(repoID); err == nil {
		latest = b.LatestSessions()
	}
	for id, s := range m.itemSessions(repoID) {
		if latest != nil {
			if _, ok := latest[id]; !ok {
				continue
			}
		}
		if holdsBacklogSlot(s) {
			used++
		}
	}
	return used
}

// noteBacklogRunEnd applies the end of a backlog run when this poll is the idle
// edge that ended it: the marker went true→false and the session settled
// Ready. A teardown that clears the marker lands elsewhere and fires nothing.
func (m *Manager) noteBacklogRunEnd(repoID string, instance *session.Instance, wasActive bool) {
	if !wasActive || instance.BacklogItem == 0 || instance.BacklogRunActive() ||
		instance.StartupStateUnknown() || instance.GetLiveness() != session.LiveReady {
		return
	}
	sessionID, gen := instance.ID, instance.BacklogRunGeneration()
	m.launchBackgroundMutation(func(stop <-chan struct{}) {
		m.settleRunEnd(repoID, sessionID, gen, stop)
	})
}

// backlogRunEnded settles the end of whatever run the session has, with no
// generation fence. Tests and callers that hold no observation use it.
func (m *Manager) backlogRunEnded(repoID, sessionID string) {
	m.settleRunEnd(repoID, sessionID, -1, nil)
}

// backlogRunEndedGen settles the end of the run of generation gen (-1: any).
func (m *Manager) backlogRunEndedGen(repoID, sessionID string, gen int) {
	m.settleRunEnd(repoID, sessionID, gen, nil)
}

// applyBacklog runs a backlog transaction. A package var so a test can make the
// lock busy.
var applyBacklog = func(svc *backlog.Service, ref backlog.ProjectRef, fn func(*backlog.Backlog) ([]int, error)) (*backlog.Backlog, error) {
	return svc.Apply(ref, fn)
}

// applyRetrying runs a backlog transaction, retrying a busy backlog with backoff
// until it lands or stop closes (a nil stop retries until it lands).
func (m *Manager) applyRetrying(repoID string, stop <-chan struct{}, fn func(*backlog.Backlog) ([]int, error)) error {
	delay := finishStartBackoff
	for {
		_, err := applyBacklog(m.backlogService(), backlog.ProjectRef{ID: repoID}, fn)
		if !errors.Is(err, backlog.ErrBusy) {
			return err
		}
		select {
		case <-stop:
			return err
		case <-time.After(delay):
		}
		delay = min(delay*2, 5*time.Second)
	}
}

// settleRunEnd moves a silent run's item to review, and archives the session
// when the user already marked the item done and on_done is archive, or the run
// ended on questions asked with then archive. It is the
// one place a run's end is written, and keeps the lifecycle model:
//
//   - fenced: gen is the generation observed at the idle edge; the transaction
//     applies only while the session's marker is inactive and still at gen, so a
//     continue or an answer accepted since is never finished by an old edge;
//   - not lost: a busy backlog is retried with backoff until it lands or stop
//     closes (a nil stop retries until it lands), and an edge observed before the
//     session's start record is committed is held and replayed by
//     finishBacklogStart.
func (m *Manager) settleRunEnd(repoID, sessionID string, gen int, stop <-chan struct{}) {
	svc := m.backlogService()
	delay := finishStartBackoff
	for {
		var end backlog.RunEnd
		_, err := applyBacklog(svc, backlog.ProjectRef{ID: repoID}, func(b *backlog.Backlog) ([]int, error) {
			if gen >= 0 {
				if s, ok := m.itemSessions(repoID)[sessionID]; ok &&
					(s.instance.BacklogRunActive() || s.instance.BacklogRunGeneration() != gen) {
					return nil, errBacklogNoChange
				}
			}
			if b.ItemForSession(sessionID) == nil {
				// Not linked yet: inside the transaction, so finishBacklogStart's
				// own transaction (which links it) is ordered after this hold.
				m.backlog.holdEnd(sessionID, gen)
				return nil, errBacklogNoChange
			}
			end = b.RunEnded(sessionID, svc.Now())
			if !end.Changed {
				return nil, errBacklogNoChange
			}
			return []int{end.ID}, nil
		})
		switch {
		case err == nil || errors.Is(err, errBacklogNoChange):
			if end.Archive {
				m.archiveItemSession(repoID, sessionID, end.Reason)
			}
			return
		case errors.Is(err, backlog.ErrBusy):
			select {
			case <-stop:
				m.warn().Printf("backlog run end for session %s: %v; recovery will settle it", sessionID, err)
				return
			case <-time.After(delay):
			}
			delay = min(delay*2, 5*time.Second)
		default:
			m.warn().Printf("backlog run end for session %s: %v", sessionID, err)
			return
		}
	}
}

// backlogArchiveCheck refuses an item-session archive unless the session is still
// the same idle item session with nothing done in it since its run ended: no
// new run, no delivery (deliveries is the count read with the adoption fence
// shut, or the live count for a pre-check), and no pane change.
func backlogArchiveCheck(sessionID string, current *session.Instance, deliveries uint64) error {
	switch {
	case current == nil || current.ID != sessionID:
		return fmt.Errorf("session %s was replaced", sessionID)
	case current.BacklogRunActive():
		return errors.New("a new run started")
	case current.GetLiveness() != session.LiveReady:
		return errors.New("the session is not idle")
	case deliveries != current.AdoptionDeliveriesAtRunEnd():
		return errors.New("a prompt reached the session after its run ended")
	case current.LastPaneChurnAt().After(current.BacklogRunEndedAt()):
		return errors.New("the session changed after its run ended")
	}
	// The delivery counter above is memory-only, so after a daemon restart it says
	// nothing. The persisted prompt attempt does: a prompt that reached (or may
	// have reached) the session after the run ended is work the archive would
	// interrupt. A proven non-delivery is not.
	if d := current.ToInstanceData(); d.LastPromptAttemptAt.After(current.BacklogRunEndedAt()) &&
		d.LastPromptDeliveryStatus != session.PromptNotDelivered {
		return errors.New("a prompt was sent to the session after its run ended")
	}
	return nil
}

// backlogArchiveGuard is backlogArchiveCheck as the teardown's guard: it shuts
// the adoption fence and reads the delivery count in one critical section, so
// input that arrives after the check cannot start work the archive then
// interrupts. A refusal reopens the fence at once; otherwise the caller reopens
// it when the archive is over (fenced reports the instance to reopen).
func backlogArchiveGuard(sessionID string, fenced func(*session.Instance)) sessionTeardownGuard {
	return func(current *session.Instance) error {
		if current == nil || current.ID != sessionID {
			return backlogArchiveCheck(sessionID, current, 0)
		}
		deliveries := current.CloseAdoptionFence()
		fenced(current)
		if err := backlogArchiveCheck(sessionID, current, deliveries); err != nil {
			current.ReopenAdoptionFence()
			return err
		}
		return nil
	}
}

// daemonProcessStart bounds restart recovery to starts made by an earlier
// daemon. A var so tests can move it.
var daemonProcessStart = time.Now()

// RecoverBacklogStarts settles what an earlier daemon left behind in each
// project's backlog: runs still starting are matched to the session created
// for them or failed, and a running item whose session went idle while no
// daemon watched goes to review. A project whose instances failed to load is
// retried on a later poll, once its sessions are known.
func (m *Manager) RecoverBacklogStarts() {
	if !m.backlog.beginRecovery() {
		return
	}
	if !m.launchBackgroundMutation(func(stop <-chan struct{}) { m.recoverBacklogStarts(stop) }) {
		m.backlog.endRecovery(true)
	}
}

func (m *Manager) recoverBacklogStarts(stop <-chan struct{}) {
	pending := false
	defer func() { m.backlog.endRecovery(pending) }()
	home, err := config.GetConfigDir()
	if err != nil {
		pending = true
		return
	}
	skipped := map[string]bool{}
	m.mu.Lock()
	for _, s := range m.skippedRepos {
		skipped[s.RepoID] = true
	}
	m.mu.Unlock()
	paths, _ := filepath.Glob(filepath.Join(home, "repos", "*", "backlog.json"))
	for _, p := range paths {
		repoID := filepath.Base(filepath.Dir(p))
		if m.backlog.isRecovered(repoID) {
			continue
		}
		if skipped[repoID] {
			pending = true
			continue
		}
		if m.recoverBacklog(repoID, stop) {
			m.backlog.markRecovered(repoID)
		} else {
			pending = true
		}
	}
}

// recoverBacklog reports whether the project's backlog was read and settled.
func (m *Manager) recoverBacklog(repoID string, stop <-chan struct{}) bool {
	b, err := backlog.Load(repoID)
	if err != nil {
		m.warnBacklogUnreadable("backlog recovery", repoID, err)
		return false
	}
	sessions := m.itemSessions(repoID)
	var list []backlog.ItemSession
	for id, s := range sessions {
		list = append(list, backlog.ItemSession{ID: id, Title: s.instance.Title, Item: s.instance.BacklogItem, CreatedAt: s.instance.CreatedAt})
	}
	m.rebuildAskedQuestions(b)
	answering := m.redeliverBacklogAnswers(repoID, b)
	starting := false
	for _, it := range b.Items {
		for _, r := range it.Runs {
			starting = starting || (r.State == backlog.RunStarting && r.StartedAt.Before(daemonProcessStart))
		}
	}
	if starting {
		svc := m.backlogService()
		if _, err := svc.Apply(backlog.ProjectRef{ID: repoID}, func(b *backlog.Backlog) ([]int, error) {
			return b.RecoverStarts(list, daemonProcessStart, svc.Now()), nil
		}); err != nil {
			m.warn().Printf("backlog recovery for project %s: %v", repoID, err)
			return false
		}
	}
	// Computed from the backlog AS RECOVERED: a start whose session link was just
	// repaired above can already have gone idle, and only the repaired run
	// (state started) qualifies as ended. Reading it before the repair would
	// leave that item running with no marker edge left to move it to review.
	if starting {
		if b, err = backlog.Load(repoID); err != nil {
			m.warn().Printf("backlog recovery for project %s: %v", repoID, err)
			return false
		}
	}
	settled := map[string]bool{}
	for _, sessionID := range idleRunEnds(b, sessions) {
		if answering[sessionID] {
			continue
		}
		// The generation observed in the snapshot: a continue accepted since is
		// not finished by this stale observation.
		m.settleRunEnd(repoID, sessionID, sessions[sessionID].gen, stop)
		settled[sessionID] = true
	}
	// An archive the earlier daemon decided on and did not finish (filed on the
	// session before it ran): finished now if it is still owed, else cleared (an
	// answer on its way cancels an ask archive).
	m.redriveOwedArchives(repoID, sessions, settled)
	return true
}

// idleRunEnds lists the sessions whose run ended unobserved: the item is still
// running (or done, with on_done archive) on its latest started run, and the
// session is live with no run. A running item whose run ended on questions
// asked with then archive is among them; its run end archives the session.
func idleRunEnds(b *backlog.Backlog, sessions map[string]itemSession) []string {
	var out []string
	for _, it := range b.Items {
		// A done item is settled too when on_done is archive: the user marked it
		// done during the run, and the archive waits for the run's idle edge, which
		// a crash between the marker clearing and the completion running loses.
		// archiveItemSession's guards decide whether it can still be archived.
		settle := it.Status == backlog.StatusRunning ||
			(it.Status == backlog.StatusDone && b.Settings.OnDone == backlog.OnDoneArchive)
		if !settle || len(it.Runs) == 0 {
			continue
		}
		r := it.Runs[len(it.Runs)-1]
		s, ok := sessions[r.SessionID]
		if r.State != backlog.RunStarted || !ok {
			continue
		}
		f := backlogSessionFacts(s, true)
		if f.Live && !f.RunActive && s.view.Liveness == session.LiveReady {
			out = append(out, r.SessionID)
		}
	}
	return out
}

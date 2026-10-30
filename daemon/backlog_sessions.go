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
		s := itemSession{instance: inst, view: inst.LifecycleView()}
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
	sessionID := instance.ID
	m.launchBackgroundMutation(func(<-chan struct{}) {
		m.backlogRunEnded(repoID, sessionID)
	})
}

// backlogRunEnded moves a silent run's item to review, and archives the
// session when the user already marked the item done and on_done is archive.
func (m *Manager) backlogRunEnded(repoID, sessionID string) {
	svc := m.backlogService()
	var end backlog.RunEnd
	_, err := svc.Apply(backlog.ProjectRef{ID: repoID}, func(b *backlog.Backlog) ([]int, error) {
		end = b.RunEnded(sessionID, svc.Now())
		if !end.Changed {
			return nil, errBacklogNoChange
		}
		return []int{end.ID}, nil
	})
	if err != nil && !errors.Is(err, errBacklogNoChange) {
		m.warn().Printf("backlog run end for session %s: %v", sessionID, err)
		return
	}
	if end.Archive {
		m.archiveDoneItemSession(repoID, sessionID)
	}
}

// archiveDoneItemSession applies on_done archive to an idle item session whose
// item the user marked done. While a client is attached the archive waits for
// the next unpaused poll of the session.
func (m *Manager) archiveDoneItemSession(repoID, sessionID string) {
	b, err := backlog.Load(repoID)
	if err != nil {
		m.warn().Printf("backlog on_done for session %s: %v", sessionID, err)
		return
	}
	it := b.ItemForSession(sessionID)
	if it == nil || it.Status != backlog.StatusDone || b.Settings.OnDone != backlog.OnDoneArchive {
		return
	}
	if r := b.LatestRun(it.ID); r == nil || r.SessionID != sessionID {
		return
	}
	s, ok := m.itemSessions(repoID)[sessionID]
	if !ok {
		return
	}
	if m.isPollPaused(repoID, s.instance.Title, sessionID) {
		m.backlog.deferArchive(sessionID)
		return
	}
	guard := backlogArchiveGuard(sessionID)
	if err := guard(s.instance); err != nil {
		m.info().Printf("backlog on_done: keeping session %q: %v", s.instance.Title, err)
		return
	}
	if err := archiveSessionForLifecycle(m, ArchiveSessionRequest{ID: sessionID, RepoID: repoID}, guard); err != nil {
		m.warn().Printf("backlog on_done archive of session %q: %v", s.instance.Title, err)
	}
}

// backlogArchiveGuard refuses the on_done archive unless the session is still
// the same idle item session with nothing done in it since its run ended: no
// new run, no delivery, and no pane change.
func backlogArchiveGuard(sessionID string) sessionTeardownGuard {
	return func(current *session.Instance) error {
		switch {
		case current == nil || current.ID != sessionID:
			return fmt.Errorf("session %s was replaced", sessionID)
		case current.BacklogRunActive():
			return errors.New("a new run started")
		case current.GetLiveness() != session.LiveReady:
			return errors.New("the session is not idle")
		case current.AdoptionDeliveries() != current.AdoptionDeliveriesAtRunEnd():
			return errors.New("a prompt reached the session after its run ended")
		case current.LastPaneChurnAt().After(current.BacklogRunEndedAt()):
			return errors.New("the session changed after its run ended")
		}
		return nil
	}
}

// drainBacklogDeferredArchive retries an on_done archive parked while a client
// was attached, now that the session is polled normally again.
func (m *Manager) drainBacklogDeferredArchive(repoID string, instance *session.Instance) {
	if instance.BacklogItem == 0 || !m.backlog.takeDeferredArchive(instance.ID) {
		return
	}
	sessionID := instance.ID
	m.launchBackgroundMutation(func(<-chan struct{}) {
		m.archiveDoneItemSession(repoID, sessionID)
	})
}

// onBacklogItemDone applies on_done archive when the user marks an item done:
// now if its session is idle; at the run's idle edge if a run is active.
func (m *Manager) onBacklogItemDone(repoID string, it backlog.Item) {
	if it.Status != backlog.StatusDone || len(it.Runs) == 0 {
		return
	}
	r := it.Runs[len(it.Runs)-1]
	s, ok := m.itemSessions(repoID)[r.SessionID]
	if r.State != backlog.RunStarted || !ok || s.instance.BacklogRunActive() {
		return
	}
	sessionID := r.SessionID
	m.launchBackgroundMutation(func(<-chan struct{}) {
		m.archiveDoneItemSession(repoID, sessionID)
	})
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
	if !m.launchBackgroundMutation(func(<-chan struct{}) { m.recoverBacklogStarts() }) {
		m.backlog.endRecovery(true)
	}
}

func (m *Manager) recoverBacklogStarts() {
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
		if m.recoverBacklog(repoID) {
			m.backlog.markRecovered(repoID)
		} else {
			pending = true
		}
	}
}

// recoverBacklog reports whether the project's backlog was read and settled.
func (m *Manager) recoverBacklog(repoID string) bool {
	b, err := backlog.Load(repoID)
	if err != nil {
		m.warn().Printf("backlog recovery for project %s: %v", repoID, err)
		return false
	}
	sessions := m.itemSessions(repoID)
	var list []backlog.ItemSession
	for id, s := range sessions {
		list = append(list, backlog.ItemSession{ID: id, Title: s.instance.Title, Item: s.instance.BacklogItem, CreatedAt: s.instance.CreatedAt})
	}
	answering := m.redeliverBacklogAnswers(repoID, b)
	ended := idleRunEnds(b, sessions)
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
	for _, sessionID := range ended {
		if answering[sessionID] {
			continue
		}
		m.backlogRunEnded(repoID, sessionID)
	}
	return true
}

// idleRunEnds lists the sessions whose run ended unobserved: the item is still
// running on its latest started run, and the session is live with no run.
func idleRunEnds(b *backlog.Backlog, sessions map[string]itemSession) []string {
	var out []string
	for _, it := range b.Items {
		if it.Status != backlog.StatusRunning || len(it.Runs) == 0 {
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

package daemon

import (
	"errors"

	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/session"
)

// errArchiveNoLongerOwed refuses an item-session archive whose reason went away
// between the decision and the teardown: an answer came, or a new run started.
var errArchiveNoLongerOwed = errors.New("the archive is no longer owed")

// archiveItemSession archives an idle item session for reason while af still
// owes it: backlog.ArchiveDone (the user marked the item done and on_done is
// archive) or backlog.ArchiveAsk (the run ended on questions asked with then
// archive). The archive is filed on the session first, so a daemon that stops
// before it finishes it on restart. While a client is attached, or the session
// is not idle yet, it waits for a later poll of the session. When the session
// changed since its run ended, it stays live and the item gets a note.
func (m *Manager) archiveItemSession(repoID, sessionID, reason string) {
	b, err := backlog.Load(repoID)
	if err != nil {
		m.warn().Printf("backlog archive (%s) of session %s: %v", reason, sessionID, err)
		return
	}
	s, ok := m.itemSessions(repoID)[sessionID]
	if !ok {
		return
	}
	inst := s.instance
	if !b.ArchiveOwed(sessionID, reason) || s.view.Liveness == session.LiveArchived {
		m.settleOwedArchive(repoID, inst)
		return
	}
	m.fileOwedArchive(repoID, inst, reason)
	if m.isPollPaused(repoID, inst.Title, sessionID) || inst.GetLiveness() != session.LiveReady {
		m.backlog.deferArchive(sessionID, reason)
		return
	}
	// The pre-check only reads; the guard handed to the teardown closes the
	// adoption fence (browser terminal input takes no manager lock and serializes
	// against a teardown only through it) and reopens it however the archive
	// ends, as the task-session lifecycle does.
	if err := backlogArchiveCheck(sessionID, inst, inst.AdoptionDeliveries()); err != nil {
		m.keepItemSessionLive(repoID, inst, reason, err)
		return
	}
	var fenced *session.Instance
	defer func() {
		if fenced != nil {
			fenced.ReopenAdoptionFence()
		}
	}()
	check := backlogArchiveGuard(sessionID, func(cur *session.Instance) { fenced = cur })
	var refused error
	guard := func(cur *session.Instance) error {
		if refused = check(cur); refused != nil {
			return refused
		}
		// Read again with the fence shut: an answer recorded since the decision
		// cancels the archive. One recorded after this read meets the archive
		// on its way and restores the session (deliverBacklogAnswer).
		if b, err := backlog.Load(repoID); err != nil || !b.ArchiveOwed(sessionID, reason) {
			refused = errArchiveNoLongerOwed
			fenced.ReopenAdoptionFence()
			fenced = nil
			return refused
		}
		return nil
	}
	if err := archiveSessionForLifecycle(m, ArchiveSessionRequest{ID: sessionID, RepoID: repoID}, guard); err != nil {
		if refused != nil {
			m.keepItemSessionLive(repoID, inst, reason, refused)
			return
		}
		m.warn().Printf("backlog archive (%s) of session %q: %v", reason, inst.Title, err)
		return
	}
	m.settleOwedArchive(repoID, inst)
}

// keepItemSessionLive settles a refused archive: the session stays. A refused
// ask archive also notes why on the item and turns its questions to then wait,
// unless the archive is no longer owed (an answer came first), so nothing
// tries again: a change means someone is using the session. A refused on_done
// archive is only logged, as before.
func (m *Manager) keepItemSessionLive(repoID string, inst *session.Instance, reason string, why error) {
	m.info().Printf("backlog archive (%s): keeping session %q: %v", reason, inst.Title, why)
	m.settleOwedArchive(repoID, inst)
	if reason != backlog.ArchiveAsk {
		return
	}
	svc := m.backlogService()
	if _, err := svc.Apply(backlog.ProjectRef{ID: repoID}, func(b *backlog.Backlog) ([]int, error) {
		id, err := b.KeptLive(inst.ID, reason, why.Error(), svc.Now())
		if err != nil || id == 0 {
			return nil, err
		}
		return []int{id}, nil
	}); err != nil {
		m.warn().Printf("backlog note on session %q: %v", inst.Title, err)
	}
}

// fileOwedArchive records on the session that af owes it an archive for
// reason, keeping the first filing time when it is already owed.
func (m *Manager) fileOwedArchive(repoID string, inst *session.Instance, reason string) {
	owed := inst.OwedArchive()
	if owed != nil && owed.Reason == reason {
		return
	}
	inst.SetOwedArchive(&session.OwedArchiveData{Reason: reason, FiledAt: nowFunc()})
	if err := m.persistAndPublishInstanceErr(repoID, inst); err != nil {
		m.warn().Printf("the owed archive of session %q could not be written to disk (a restart before the archive would not finish it): %v",
			inst.Title, err)
	}
}

// settleOwedArchive clears an owed archive that is done or no longer owed.
func (m *Manager) settleOwedArchive(repoID string, inst *session.Instance) {
	if inst.OwedArchive() == nil {
		return
	}
	inst.SetOwedArchive(nil)
	m.persistAndPublishInstance(repoID, inst)
}

// drainBacklogDeferredArchive retries an archive parked while a client was
// attached or the session was not idle, once the session is polled normally
// again and idle.
func (m *Manager) drainBacklogDeferredArchive(repoID string, instance *session.Instance) {
	if instance.BacklogItem == 0 {
		return
	}
	reason, ok := m.backlog.takeDeferredArchive(instance.ID)
	if !ok {
		return
	}
	if instance.GetLiveness() != session.LiveReady {
		m.backlog.deferArchive(instance.ID, reason)
		return
	}
	sessionID := instance.ID
	m.launchBackgroundMutation(func(<-chan struct{}) {
		m.archiveItemSession(repoID, sessionID, reason)
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
		m.archiveItemSession(repoID, sessionID, backlog.ArchiveDone)
	})
}

// redriveOwedArchives finishes the archives an earlier daemon filed and did not
// settle, skipping the sessions whose run end recovery already applied.
func (m *Manager) redriveOwedArchives(repoID string, sessions map[string]itemSession, settled map[string]bool) {
	for id, s := range sessions {
		if settled[id] {
			continue
		}
		if owed := s.instance.OwedArchive(); owed != nil {
			m.archiveItemSession(repoID, id, owed.Reason)
		}
	}
}

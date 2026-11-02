package daemon

import (
	"errors"
	"time"

	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/session"
)

// orchRunActivity is what stall detection knows about an item session; false
// when af cannot tell yet, or a restore or usage limit will bring it back.
func orchRunActivity(s itemSession, ok bool) (backlog.RunActivity, bool) {
	if !ok {
		return backlog.RunActivity{Stopped: true}, true
	}
	v := s.view
	last := s.instance.LastActivityAt()
	switch {
	case v.UserKilled || v.InFlightOp == session.OpKilling:
		return backlog.RunActivity{Stopped: true, LastActive: last}, true
	case v.StartupStateUnknown || v.InFlightOp != session.OpNone:
		return backlog.RunActivity{}, false
	}
	switch v.Liveness {
	case session.LiveArchived, session.LiveDead:
		return backlog.RunActivity{Stopped: true, LastActive: last}, true
	case session.LiveLost:
		if v.BacklogRunActive && canAutoRestoreLostSession(v) {
			return backlog.RunActivity{}, false
		}
		return backlog.RunActivity{Stopped: true, LastActive: last}, true
	case session.LiveLimitReached:
		return backlog.RunActivity{}, false
	}
	if !v.BacklogRunActive {
		return backlog.RunActivity{}, false
	}
	// Pane facts live in memory, so a restart counts as activity: a run is
	// not called stalled for quiet this process did not see.
	if daemonProcessStart.After(last) {
		last = daemonProcessStart
	}
	return backlog.RunActivity{Working: true, LastActive: last}, true
}

// noteOrchStalls queues stalled events for the project's orchestrated groups
// and returns the backlog to deliver from.
func (m *Manager) noteOrchStalls(repoID string, b *backlog.Backlog, now time.Time) *backlog.Backlog {
	if !b.HasOrchestrator() || !m.orchestratorReady(repoID) {
		return b
	}
	sessions := m.itemSessions(repoID)
	activity := func(id string) (backlog.RunActivity, bool) {
		s, ok := sessions[id]
		return orchRunActivity(s, ok)
	}
	if len(b.QueueOrchStalls(activity, now)) == 0 {
		return b
	}
	out, err := m.backlogService().Apply(backlog.ProjectRef{ID: repoID}, func(b *backlog.Backlog) ([]int, error) {
		groups := b.QueueOrchStalls(activity, now)
		if len(groups) == 0 {
			return nil, errBacklogNoChange
		}
		return groups, nil
	})
	if errors.Is(err, errBacklogNoChange) {
		out, err = backlog.Load(repoID)
	}
	if err != nil {
		m.warn().Printf("orchestrator stall check for project %s: %v", repoID, err)
		return b
	}
	return out
}

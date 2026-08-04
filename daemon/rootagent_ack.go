package daemon

import (
	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// acknowledgeRootRecreate clears a re-created root's one-shot "fresh context"
// note now that a client has the session's pane open, and makes the clear
// durable and visible everywhere (#2629).
//
// Seeing the pane IS the acknowledgement. The note exists to tell a user
// something they would otherwise learn only by grepping agent-factory.log, and
// once the agent is on screen they are in the one place that answers the
// question for real. Every surface reaches this through the same PTY stream — a
// TUI/CLI attach and a web pane render alike — so one call site covers all three
// rails, which is also why no new user-facing "acknowledge" verb exists to get
// out of sync with them.
//
// Cheap on the hot path: the first thing it does is a single guarded read that
// is false for every session that is not a freshly healed root, which is all of
// them almost always. Only that read costs anything on an ordinary attach.
//
// Best-effort by design. A failed persist logs and leaves the note set, so the
// user sees it again after a restart — showing a stale note is a nuisance, while
// swallowing the write and dropping it in memory would lose the notice for good.
func (m *Manager) acknowledgeRootRecreate(instance *session.Instance) {
	if instance == nil || instance.RootRecreateContext() == session.RootRecreateContextNone {
		return
	}
	repoID, ok := m.repoIDForInstance(instance)
	if !ok {
		// Not the tracked instance for any key — a throwaway built from disk, or a
		// session removed while this stream was being set up. Nothing durable to
		// clear, and mutating it would only affect an object about to be dropped.
		return
	}
	// Captured BEFORE the clear so a failed persist can put it back. Reading it
	// afterwards would only ever return the cleared value.
	previous := instance.RootRecreateContext()
	if !instance.AcknowledgeRootRecreateContext() {
		// Another stream got here first, or the value is one this binary does not
		// render and therefore has not shown anybody. Exactly one acknowledger
		// persists and announces; the losers must not re-announce an unchanged row.
		return
	}

	// Serialize the mutate+persist against other writers for this repo, the same
	// discipline CreateTab/CloseTab follow for a targeted record update.
	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	defer repoStartLock.Unlock()

	data := instance.ToInstanceData()
	if err := persistInstanceData(repoID, data); err != nil {
		// Put the note back in memory, the way SetPRInfo rolls its value back: the
		// clear above is not durable, so leaving it applied would make THIS daemon's
		// snapshots drop the notice while disk still carries it — and every later
		// stream open would take the early return above and never retry the write.
		// The user would lose the warning until a restart reloaded the stale record.
		// Restored, the next pane open tries the whole acknowledgement again.
		instance.ReconcileRootRecreateContext(previous)
		log.WarningLog.Printf("could not clear the re-create notice on session %q; leaving it set so the next pane open retries: %v", instance.Title, err)
		return
	}
	// Announce the cleared row so every OTHER open client drops the note too — a
	// notice that stays on a second rail after the user has already read the pane
	// is the same staleness the one-shot design exists to avoid.
	m.publishEvent(agentproto.EventSessionUpdated, data)
}

// streamShowsAgentPane reports whether a PTY stream request addresses the
// session's AGENT tab — the only pane that answers "did this root keep its
// context?" (#2629).
//
// It mirrors bindTab's addressing precedence: a supplied ?tab_id= is the
// authority (matched against the roster's own agent tab, which is Tabs[0] by
// construction), and only a request with no id falls back to the ordinal, where
// the agent tab is slot 0 and an absent ?tab= defaults there.
//
// Both unknown answers resolve to false — an id that matches no tab, or an
// instance with no roster. Not acknowledging costs the user one extra look at a
// notice; acknowledging on a pane that may not be the agent's silently retires
// the warning, and only one of those is recoverable.
func streamShowsAgentPane(instance *session.Instance, tabID string, tab int) bool {
	if instance == nil {
		return false
	}
	if tabID == "" {
		return tab == 0
	}
	tabs := instance.GetTabs()
	return len(tabs) > 0 && tabs[0].ID == tabID
}

// repoIDForInstance returns the repo this instance is tracked under, or false
// when it is not the daemon's tracked instance for any key.
//
// It resolves by POINTER identity rather than by title: a kill/recreate cycle
// replaces the tracked instance under the same title, and a caller holding the
// old object must not be able to write through the new one's record (the #1723
// class). Not-tracked is a real answer here, not a failure — the stream resolver
// can hand back a throwaway instance built from disk mid-restore.
func (m *Manager) repoIDForInstance(instance *session.Instance) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, tracked := range m.instances {
		if tracked == instance {
			repoID, _ := splitDaemonInstanceKey(key)
			return repoID, true
		}
	}
	return "", false
}

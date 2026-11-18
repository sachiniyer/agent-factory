package session

import (
	"errors"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// deferToLostRestore loads a row whose restore was refused for later
// (restoreDeferred) as Lost instead of dropping it: started, so SaveInstances
// keeps the record and lostSessionWantsRestore admits it. The daemon's
// lost-restore loop owns what comes next, rebuilding a missing worktree before
// it respawns (#5172) or retrying once the socket answers (#4135), and it can
// only reach a row that materialized.
//
// The loader is also the one place that knows why the row is Lost. It records
// whether the agent's own process is proven gone (LostAgentGone), and it enrolls
// the load checkpoint so that verdict is on disk before the daemon installs the
// restored map. That is before its boot replay of a pending cron fire reads the
// row (#7518), which would otherwise read the pre-load record, and that record
// still names a live agent.
func (i *Instance) deferToLostRestore(err error) *Instance {
	i.mu.Lock()
	i.assignLivenessLocked(LiveLost)
	i.lostAgentGone = agentProvenGone(err)
	i.started = true
	i.dropAttentionIfStoppedLocked()
	i.touchLocked()
	i.mu.Unlock()
	i.markLoadRuntimeReplaced()
	return i
}

// agentRestoreRefusal is the agent tab's own restore refusal on load. It is
// kept apart from a sibling tab's (setupTabs) because only the agent's own says
// anything about the process its prompt went to: a sibling's spawn-dir refusal
// can land beside an agent that was just reattached alive. It changes no
// message.
type agentRestoreRefusal struct{ err error }

func (r *agentRestoreRefusal) Error() string { return r.err.Error() }
func (r *agentRestoreRefusal) Unwrap() error { return r.err }

// agentProvenGone reports whether a deferred load proves the agent's own tmux
// session gone. Both sentinels are raised only on Restore's confirmed-absent
// branch: tmux answered that the session does not exist, and the respawn was
// then refused because the spawn directory is missing or the replacement
// conversation could not be made durable. ErrSessionSocketUnverified proves
// nothing of the kind, because a live server may still be running the agent.
func agentProvenGone(err error) bool {
	var refusal *agentRestoreRefusal
	return errors.As(err, &refusal) &&
		(errors.Is(refusal.err, tmux.ErrSpawnDirMissing) || errors.Is(refusal.err, ErrReplacementNotDurable))
}

package daemon

import (
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// sweepStartupTabCleanup retries the tmux teardown of tabs whose close was
// durably committed by a previous daemon but never confirmed (#2669), and
// persists each record whose handles it managed to retire.
//
// It runs in the same window as the orphan-container sweep: after instance
// restore, before the readiness barrier opens. That placement is what makes the
// pass safe to run without per-session op-locks — no RPC can be closing a tab,
// spawning a sibling, or killing a session concurrently, so every handle the
// sweep sees belongs to the previous daemon's unfinished work rather than to an
// in-flight close of this one.
//
// Failures are logged and never fatal. An unconfirmed retry keeps its handle for
// the next start, which is strictly better than the pre-#2669 behavior of
// forgetting the session existed; a failed persist keeps a handle whose retry is
// idempotent. Neither justifies refusing to open the daemon.
func sweepStartupTabCleanup(manager *Manager) {
	for _, instance := range manager.InstancesSnapshot() {
		if len(instance.PendingTabCleanup()) == 0 {
			continue
		}
		retired, remaining := instance.RetryPendingTabCleanup()
		if retired == 0 {
			continue
		}
		// Persist through the targeted per-repo writer, the single-writer direction
		// of #960 — a whole-list SaveInstances here would rewrite every sibling
		// record to retire one session's handles.
		root := instance.GetRepoPath()
		if root == "" {
			root = instance.Path
		}
		data := instance.ToInstanceData()
		if err := persistInstanceData(config.RepoIDFromRoot(root), data); err != nil {
			log.WarningLog.Printf(
				"tab cleanup sweep: reaped %d leftover tmux session(s) of session %q but could not record it; the next start will retry the finished teardown: %v",
				retired, instance.Title, err)
			continue
		}
		log.InfoLog.Printf("tab cleanup sweep: session %q reaped %d leftover tmux session(s), %d still unconfirmed",
			instance.Title, retired, remaining)
	}
}

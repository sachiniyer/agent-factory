package daemon

import (
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// startInstancePollLoop runs the daemon's status poll for the lifetime of the
// daemon: the tick that refreshes instances, computes liveness, and drives the
// self-healing passes that depend on it. Extracted verbatim from RunDaemon
// (#2212) — the ordering comments below are the contract between the passes, so
// the sequence is load-bearing, not incidental.
//
// pollInterval seeds the ticker; a daemon_poll_interval change re-arms it in
// place through manager.pollReloadCh. The loop body runs once before the first
// ticker wait, so the first pass happens right after the restore.
func startInstancePollLoop(manager *Manager, pollInterval time.Duration, stopCh <-chan struct{}, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for {
			if err := manager.RefreshInstances(); err != nil {
				log.WarningLog.Printf("failed to refresh daemon instances: %v", err)
			}

			// Compute and persist each session's status (Ready/Dead/Running). The
			// daemon is the sole
			// owner of status now (#935/#960 PR 5): it computes the liveness here
			// and the TUI renders it from Snapshot instead of computing its own.
			manager.RefreshStatuses()

			// Always-ensure the root agent for repos opted in via root_agents
			// (#1106). Runs after RefreshStatuses so a root whose tmux died is
			// marked Dead and healed in the same tick; the loop body runs once
			// before the first ticker wait, so the first ensure happens right
			// after the restore. A (re-)create blocks this poll briefly while
			// the session starts — acceptable for a rare, backoff-throttled
			// event. root_agents is read from the daemon's startup config;
			// changing it takes effect on the next daemon restart.
			manager.EnsureRootAgents()

			// Best-effort restore of Lost sessions (#1108): the general form
			// of the root self-heal, for every session whose tmux vanished
			// with no kill on record. Runs after RefreshStatuses (which marks
			// them Lost) and after EnsureRootAgents (which owns the reserved
			// root title). Backoff-throttled per session, like root-ensure.
			manager.RestoreLostSessions()

			// Complete a handoff mission whose post-swap checkpoint survived a
			// daemon restart before delivery was confirmed. This runs after status
			// and Lost recovery so it acts only on a positively ready pane, and
			// before limit resume so a newly observed incoming limit can inherit the
			// exact rendered mission in the same tick.
			manager.ResumePendingHandoffs()

			// Opt-in auto-resume of usage-limit-blocked sessions (#1146 PR3):
			// re-prompt a LiveLimitReached row once its limit window elapsed. A
			// no-op unless limit_auto_resume is set, so a default install keeps
			// a limit surface-only. Runs after RestoreLostSessions because a
			// session must be settled onto its liveness first; it borrows the
			// same per-session op-lock discipline.
			manager.ResumeLimitedSessions()

			// Handle stop before ticker.
			select {
			case <-stopCh:
				return
			case <-manager.pollReloadCh:
				// daemon_poll_interval changed via ApplyConfig (#2480): reset the
				// ticker to the new cadence in place — no dropped poll goroutine, no
				// session touched. Validated positive at config-set time; guard
				// anyway since Reset panics on a non-positive duration.
				if next := time.Duration(manager.Config().DaemonPollInterval) * time.Millisecond; next > 0 {
					ticker.Reset(next)
				}
			case <-ticker.C:
			}
		}
	}()
}

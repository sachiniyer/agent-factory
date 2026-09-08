package git

import (
	"context"
	"time"

	"github.com/sachiniyer/agent-factory/internal/systemdunit"
	"github.com/sachiniyer/agent-factory/log"
)

// The runner keeps HooksDone open while a scope-stop result is inconclusive.
// The initial deadline only changes the log message. A permanently unavailable
// user manager must leave the journal pending rather than report completion;
// cancellation is the only way this wait ends without resuming the suffix.
var runningHookPrefixesForResume = systemdunit.RunningHookPrefixes

func waitForHookScopeGone(ctx context.Context, prefix string) bool {
	if prefix == "" {
		return true
	}
	deadline := time.NewTimer(hookStopTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(hookAdoptionPollInterval)
	defer ticker.Stop()
	var lastError string
	deadlineLogged := false
	for {
		live, err := runningHookPrefixesForResume(prefix)
		if err == nil && len(live) == 0 {
			return true
		}
		if err != nil {
			if message := err.Error(); message != lastError {
				log.WarningLog.Printf("waiting for post-worktree hook scope %s to stop: %v", prefix, err)
				lastError = message
			}
		} else if lastError != "" {
			log.InfoLog.Printf("post-worktree hook scope %s stop probe recovered", prefix)
			lastError = ""
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			if !deadlineLogged {
				log.WarningLog.Printf("post-worktree hook scope %s is still waiting to stop; keeping HooksDone open for recovery", prefix)
				deadlineLogged = true
			}
		case <-ticker.C:
		}
	}
}

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

// Nested recovery completed its command loop, but its durable finished marker
// can briefly be unreadable. Retry that third-answer storage result rather than
// turning it into either completion or permanent in-flight state.
func waitForHookProgressFinished(ctx context.Context, p *hookProgress) bool {
	ticker := time.NewTicker(hookAdoptionPollInterval)
	defer ticker.Stop()
	var lastError string
	for {
		finished, err := p.finishedState()
		if finished {
			return true
		}
		if err != nil && err.Error() != lastError {
			log.WarningLog.Printf("waiting to verify completed post-worktree hook journal for %s: %v", p.Worktree, err)
			lastError = err.Error()
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

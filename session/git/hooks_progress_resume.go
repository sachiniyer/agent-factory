package git

import (
	"context"
	"time"

	"github.com/sachiniyer/agent-factory/internal/systemdunit"
	"github.com/sachiniyer/agent-factory/log"
)

// The runner keeps HooksDone open while a scope-stop result is inconclusive.
// This is deliberately bounded: a permanently unavailable user manager leaves
// the journal for the next daemon generation instead of allowing teardown to
// mistake an unfinished list for a completed one.
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
			log.WarningLog.Printf("post-worktree hook scope %s did not become conclusively stopped; leaving journal for adoption", prefix)
			return false
		case <-ticker.C:
		}
	}
}

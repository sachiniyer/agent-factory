package git

import (
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// Cancellation is terminal intent, not permission to forget an unreadable
// journal. Keep the existing watcher pending until we can record that intent
// or prove there is no resumable owned journal. The teardown join has its own
// bounded timeout and refuses worktree mutation while this retry is pending.
func waitForCancelledHookProgress(ticks <-chan time.Time, worktreePath, sessionID string) {
	var lastError string
	for {
		p, _, err := readOwnedHookProgress(worktreePath, sessionID)
		if noResumableHookProgress(err) {
			return
		}
		if err == nil {
			err = p.markFinished()
		}
		if err == nil {
			if lastError != "" {
				log.InfoLog.Printf("cancelled hook journal terminalized for %s", worktreePath)
			}
			return
		}
		if message := err.Error(); message != lastError {
			log.WarningLog.Printf("waiting to terminalize cancelled hook journal for %s: %v", worktreePath, err)
			lastError = message
		}
		<-ticks
	}
}

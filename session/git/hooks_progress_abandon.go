package git

import (
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

func abandonHookProgress(worktreePath, sessionID string) error {
	p, _, err := readOwnedHookProgress(worktreePath, sessionID)
	if noResumableHookProgress(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return p.markFinished()
}

func retryHookProgressAbandonment(worktreePath, sessionID string) {
	delay := 100 * time.Millisecond
	var lastError string
	for attempt := 0; attempt < 8; attempt++ {
		time.Sleep(delay)
		if err := abandonHookProgress(worktreePath, sessionID); err != nil {
			if message := err.Error(); message != lastError {
				log.WarningLog.Printf("cannot retry hook progress abandonment for %s: %v; continuing bounded retry", worktreePath, err)
				lastError = message
			}
			delay = min(2*delay, 2*time.Second)
			continue
		}
		return
	}
	log.WarningLog.Printf("hook progress abandonment for %s remains deferred to pruning", worktreePath)
}

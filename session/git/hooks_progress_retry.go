package git

import (
	"os"
	"path/filepath"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// A teardown has already joined the runner and proved its scope stopped.
// Snapshot identity checks under the publication lock protect a newer journal
// at the same path, including a same-session rebuild.
func retireHookProgressSnapshot(p *hookProgress, path string) (bool, error) {
	return config.TryWithFileLock(filepath.Join(filepath.Dir(path), ".progress"), func() error {
		current, err := readHookProgress(path)
		if os.IsNotExist(err) {
			retired := filepath.Join(filepath.Dir(path), "retired-"+filepath.Base(p.Directory)+".json")
			if _, statErr := BoundedLstat(retired); statErr == nil {
				// A prior attempt may have renamed the resumable journal before
				// failing to remove its receipts. Continue the same retirement
				// against the non-resumable name.
				return hookProgressRemove(retired, p)
			} else if !os.IsNotExist(statErr) {
				return statErr
			}
			return nil
		}
		if err != nil {
			return err
		}
		if current.Directory != p.Directory || current.SessionID != p.SessionID || current.Generation != p.Generation {
			return nil
		}
		return hookProgressRemove(path, p)
	})
}

// Test seam for a transient filesystem failure after the progress lock has
// been acquired. Production always uses removeHookProgress.
var hookProgressRemove = removeHookProgress

// Bound the retry lifetime and load: 100ms exponential backoff, capped at 2s,
// eight attempts. Durable terminal state is also eligible for creation-time GC
// if the daemon exits or storage remains busy beyond this retry budget.
func retryHookProgressRetirement(p *hookProgress, path string) {
	delay := 100 * time.Millisecond
	var lastError string
	for attempt := 0; attempt < 8; attempt++ {
		time.Sleep(delay)
		acquired, err := retireHookProgressSnapshot(p, path)
		if err != nil {
			if message := err.Error(); message != lastError {
				log.WarningLog.Printf("cannot retry hook progress reclamation for %s: %v; continuing bounded retry", p.Worktree, err)
				lastError = message
			}
			delay = min(2*delay, 2*time.Second)
			continue
		}
		if acquired {
			return
		}
		delay = min(2*delay, 2*time.Second)
	}
	log.WarningLog.Printf("hook progress reclamation for %s remains deferred to pruning", p.Worktree)
}

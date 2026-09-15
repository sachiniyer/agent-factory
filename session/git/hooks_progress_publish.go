package git

import (
	"context"
	"fmt"
	"time"
)

// enableResume writes the positive recovery commit only after the journal's
// shared name has passed its directory durability barrier. The update is one
// byte on the already-synced journal inode, so a crash can yield only the
// fail-closed zero, the recovery-authorizing one, or invalid JSON. A timeout
// downgrades only restart recovery; the operator's current hook run proceeds.
func (p *preparedHookProgress) enableResume() error {
	if p == nil || p.journalFile == nil {
		return fmt.Errorf("hook journal publication file is unavailable")
	}
	file := p.journalFile
	p.journalFile = nil
	if p.progress.ResumeDisabled {
		closeHookProgressFile(file)
		return nil
	}
	done := make(chan error, 1)
	readyAt := p.resumeReadyAt
	go func() {
		_, err := file.WriteAt([]byte("1"), readyAt)
		if err == nil {
			err = file.Sync()
		}
		done <- err
		closeHookProgressFile(file)
	}()
	timer := time.NewTimer(relocationIdentityTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if err == nil {
			p.progress.ResumeReady = 1
		}
		return err
	case <-timer.C:
		return fmt.Errorf("timed out after %s while committing hook journal recovery: %w", relocationIdentityTimeout, context.DeadlineExceeded)
	}
}

package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

var errHookProgressResumeDisabled = errors.New("hook journal was published without a safe resume identity")

func noResumableHookProgress(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, errInvalidHookProgress) || errors.Is(err, errHookProgressResumeDisabled)
}

// Absence, invalid ownership/shape and a finished marker are conclusive.
// Storage failures (including the completion-marker probe) must remain pending.
func readPendingHookProgress(worktreePath, sessionID string) (*hookProgress, error) {
	p, _, err := readOwnedHookProgress(worktreePath, sessionID)
	if err != nil {
		return nil, err
	}
	info, err := BoundedLstat(filepath.Join(p.Directory, "finished"))
	if os.IsNotExist(err) {
		if p.ResumeDisabled {
			return p, errHookProgressResumeDisabled
		}
		return p, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: finished marker is not regular", errInvalidHookProgress)
	}
	return nil, os.ErrNotExist
}

func waitForHookProgressRead(ctx context.Context, ticks <-chan time.Time, worktreePath, sessionID string, err error) (*hookProgress, error) {
	var lastError string
	for {
		if message := err.Error(); message != lastError {
			log.WarningLog.Printf("waiting to read post-worktree hook journal for %s: %v", worktreePath, err)
			lastError = message
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticks:
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var p *hookProgress
		p, err = readPendingHookProgress(worktreePath, sessionID)
		if err == nil {
			log.InfoLog.Printf("hook journal read recovered for %s", worktreePath)
			return p, nil
		}
		if noResumableHookProgress(err) {
			return p, err
		}
	}
}

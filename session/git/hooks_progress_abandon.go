package git

import (
	"path/filepath"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// abandonHookProgress reads and terminalizes the owned journal for the
// worktree/session. It returns the snapshot it read (nil when no resumable
// journal exists) so a caller scheduling a bounded retry can thread the
// snapshot's immutable identity and refuse to terminalize a replacement
// journal published at the same path while the retry was pending.
func abandonHookProgress(worktreePath, sessionID string) (*hookProgress, error) {
	p, _, err := readOwnedHookProgress(worktreePath, sessionID)
	if noResumableHookProgress(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := p.markFinished(); err != nil {
		return p, err
	}
	return p, nil
}

// abandonHookProgressSnapshot re-reads the owned journal for worktreePath/
// sessionID under the .progress publication lock and terminalizes it only when
// its immutable identity still matches snapshot. The lock and identity check
// protect against a newer journal at the same path, including a same-session
// rebuild that reuses SessionID/Worktree/Prefix while publishing a new
// Directory and Generation — the hazard retireHookProgressSnapshot already
// defends against on the retirement path.
//
// When snapshot is nil the caller had no durable snapshot — the shared restore
// read missed its budget before decoding a journal. The first attempt then
// captures the identity of the owned journal it reads under the lock and only
// that identity is terminalized on this and later attempts; a replacement
// published after that first read is left to its own runner.
//
// Returns (done, captured, err):
//   - done is true when the abandonment reached a conclusive state — the
//     journal is absent, conclusively invalid, replaced by a newer identity,
//     or terminalized — and the caller must stop retrying.
//   - captured is the journal decoded on this attempt (nil until a journal is
//     read under the lock), so the caller can thread its identity into later
//     attempts.
//   - err is the storage error that left the attempt inconclusive.
func abandonHookProgressSnapshot(worktreePath, sessionID string, snapshot *hookProgress) (bool, *hookProgress, error) {
	path, err := hookProgressPath(worktreePath)
	if err != nil {
		return false, nil, err
	}
	var captured *hookProgress
	acquired, err := tryWithBoundedHookProgressFileLock(filepath.Join(filepath.Dir(path), ".progress"), relocationIdentityTimeout.Load(), func() error {
		current, _, readErr := readOwnedHookProgress(worktreePath, sessionID)
		if noResumableHookProgress(readErr) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
		captured = current
		if snapshot != nil && (current.Directory != snapshot.Directory || current.SessionID != snapshot.SessionID || current.Generation != snapshot.Generation) {
			return nil
		}
		return current.markFinished()
	})
	if err != nil {
		return false, captured, err
	}
	return acquired, captured, nil
}

// Test seam for the pause before each abandonment retry attempt, so a test can
// publish a rebuild in front of a given attempt. Production always sleeps.
var hookProgressAbandonmentRetryWait = time.Sleep

// retryHookProgressAbandonment retries terminalizing the owned journal for
// worktreePath/sessionID after a synchronous abandonment failed. snapshot
// carries the immutable identity of the journal the synchronous abandonment
// read; when nil, the first locked attempt captures the identity of the owned
// journal it reads and re-validates against it on every later attempt. A
// replacement journal published at the same path by a same-session rebuild —
// same SessionID/Worktree/Prefix, new Directory/Generation — is left to its
// own runner instead of being terminalized.
func retryHookProgressAbandonment(worktreePath, sessionID string, snapshot *hookProgress) {
	delay := 100 * time.Millisecond
	var lastError string
	for attempt := 0; attempt < 8; attempt++ {
		hookProgressAbandonmentRetryWait(delay)
		done, captured, err := abandonHookProgressSnapshot(worktreePath, sessionID, snapshot)
		if err != nil {
			if message := err.Error(); message != lastError {
				log.WarningLog.Printf("cannot retry hook progress abandonment for %s: %v; continuing bounded retry", worktreePath, err)
				lastError = message
			}
			if captured != nil {
				snapshot = captured
			}
			delay = min(2*delay, 2*time.Second)
			continue
		}
		if done {
			return
		}
		if captured != nil {
			snapshot = captured
		}
		delay = min(2*delay, 2*time.Second)
	}
	log.WarningLog.Printf("hook progress abandonment for %s remains deferred to pruning", worktreePath)
}

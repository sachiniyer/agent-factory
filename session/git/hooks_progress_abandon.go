package git

import (
	"path/filepath"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// abandonHookProgress reads and terminalizes the owned journal for the
// worktree/session. It returns the snapshot it read (nil when no resumable
// journal exists or the read failed) so a caller scheduling a bounded retry
// can thread the snapshot's immutable identity and refuse to terminalize a
// replacement journal published at the same path while the retry was pending.
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
// it is provably the journal the abandonment was armed for. The lock and that
// proof protect a newer journal at the same path, including a same-session
// rebuild that reuses SessionID/Worktree/Prefix while publishing a new
// Directory and Generation — the hazard retireHookProgressSnapshot already
// defends against on the retirement path. Publication holds the same lock.
//
// The proof is the snapshot's immutable identity (Directory/SessionID/
// Generation) when a read before arming decoded one. When none did — the
// shared restore read missed its budget, or the synchronous read failed — the
// proof is that this process published nothing at the path since arming:
// armed must still equal hookProgressPublicationCount. A journal that fails
// its proof belongs to a live run and is left to its own runner. Without
// either proof a retry would finish whichever journal it first found.
//
// Returns (done, captured, err):
//   - done is true when the abandonment reached a conclusive state — the
//     journal is absent, conclusively invalid, not the armed journal, or
//     terminalized — and the caller must stop retrying.
//   - captured is the journal this attempt proved is the armed one, so a
//     caller that started without a snapshot can check later attempts by its
//     identity. Nil when no journal was proved.
//   - err is the storage error that left the attempt inconclusive.
func abandonHookProgressSnapshot(worktreePath, sessionID string, snapshot *hookProgress, armed uint64) (bool, *hookProgress, error) {
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
		if snapshot != nil {
			if current.Directory != snapshot.Directory || current.SessionID != snapshot.SessionID || current.Generation != snapshot.Generation {
				return nil
			}
		} else if hookProgressPublicationCount(worktreePath) != armed {
			return nil
		}
		captured = current
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
// read; armed is the publication count taken when the abandonment was armed,
// before that read. When snapshot is nil, the first attempt that proves the
// journal by armed captures its identity, and later attempts check that. A
// replacement journal published at the same path by a same-session rebuild —
// same SessionID/Worktree/Prefix, new Directory/Generation — is left to its
// own runner instead of being terminalized.
func retryHookProgressAbandonment(worktreePath, sessionID string, snapshot *hookProgress, armed uint64) {
	delay := 100 * time.Millisecond
	var lastError string
	for attempt := 0; attempt < 8; attempt++ {
		hookProgressAbandonmentRetryWait(delay)
		done, captured, err := abandonHookProgressSnapshot(worktreePath, sessionID, snapshot, armed)
		if captured != nil {
			snapshot = captured
		}
		if err == nil && done {
			return
		}
		if err != nil {
			if message := err.Error(); message != lastError {
				log.WarningLog.Printf("cannot retry hook progress abandonment for %s: %v; continuing bounded retry", worktreePath, err)
				lastError = message
			}
		}
		delay = min(2*delay, 2*time.Second)
	}
	log.WarningLog.Printf("hook progress abandonment for %s remains deferred to pruning", worktreePath)
}

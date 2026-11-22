package daemon

import (
	"fmt"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// singleFlightArchive makes a second /v1/agent/archive request JOIN the archive
// already running instead of starting another one (#2997 finding 2).
//
// THE PROBLEM IT SOLVES, which is a property of the wire and not of this server:
// the mutating RPCs are registered through rpcHandler, which discards the request
// context, so a client that gives up ends the REQUEST and not the server-side
// mutation. The daemon gives up on its own schedule — AgentArchiveCallTimeout is
// 3 minutes and the caller's outer bound sits 30s ABOVE it — and then
// preserveSandboxBeforeReap refuses to reap and says, in its own error text, that
// "the daemon keeps retrying". That retry is the second actor: it issues another
// /v1/agent/archive while the first is still inside git add/commit/push on the
// same worktree.
//
// Two concurrent archives on one worktree is the harm — contending on index.lock
// at best, interleaving a snapshot commit with another's staging at worst.
//
// WHY JOIN RATHER THAN REFUSE. A refusal would also stop the overlap, and it was
// the obvious answer, but it throws away the thing the retry actually needs. The
// first archive pushes a branch and returns its name to a client that has already
// hung up, so the daemon never learns it — and preserveSandboxBeforeReap treats an
// unknown branch as a refusal to reap, so the session stays stuck exactly as it
// was. Joining turns the retry into the mechanism that RECOVERS the answer: it
// waits out the in-flight push and returns the same branch, so the loop converges
// instead of restarting a three-minute operation it will abandon again.
//
// WHY NOT CANCELLATION. Threading the request context into the git commands is the
// other option the issue names, and it requires deciding what cancelling between
// `add` and `commit` means — a half-staged index is not a state this code can
// safely leave behind. Joining needs no such decision, because nothing is
// interrupted.
//
// SCOPE: archive only, deliberately. Kill mutates the same workspace and is not
// fenced here, because blocking a teardown behind a three-minute push is a worse
// failure than the one being fixed. It is also not reachable from the path above:
// preserveSandboxBeforeReap refuses to reap when the archive result is unknown, so
// the retry loop re-archives rather than killing.
type singleFlightArchive struct {
	mu     sync.Mutex
	active *archiveAttempt
}

// archiveAttempt is one in-flight archive and its eventual result. done is closed
// once branch/err are written, which is what orders a joiner's read after the
// leader's write.
type archiveAttempt struct {
	done   chan struct{}
	branch string
	err    error
}

// archiveJoinTimeout bounds how long a joining request waits for the archive
// already running.
//
// Set to the CLIENT's own budget for this call, deliberately: a joiner that
// outwaited its caller would be holding a handler goroutine for an answer nobody
// is listening for any more, and since every refused reap produces another retry,
// those would accumulate one per retry for as long as a stalled push lasts —
// runGitCommand has no timeout of its own, so "as long as" can be unbounded.
//
// Expiring is not a regression on today's behaviour: the caller has given up by
// then either way. What it costs is the joiner's chance to recover the branch,
// which is only lost in the case where there was no answer to recover.
var archiveJoinTimeout = session.AgentArchiveCallTimeout

// do runs archive, or joins the one already running and returns its result.
func (s *singleFlightArchive) do(archive func() (string, error)) (string, error) {
	s.mu.Lock()
	if joined := s.active; joined != nil {
		s.mu.Unlock()
		timer := time.NewTimer(archiveJoinTimeout)
		defer timer.Stop()
		select {
		case <-joined.done:
			return joined.branch, joined.err
		case <-timer.C:
			// An error, not an empty branch. The caller must read this as "we do not
			// know", which is what makes it refuse to reap rather than reap onto a
			// branch that may not exist — the same rule archiveWithin states.
			return "", fmt.Errorf("an archive of this workspace has been running for over %s and has not finished; "+
				"not starting a second one against the same worktree", archiveJoinTimeout)
		}
	}
	attempt := &archiveAttempt{done: make(chan struct{})}
	s.active = attempt
	s.mu.Unlock()

	attempt.branch, attempt.err = archive()

	// Closed BEFORE the slot is cleared, so a request that grabbed this attempt
	// microseconds ago gets its result rather than blocking on a channel whose
	// writer has moved on. Clearing after also means the next caller starts a fresh
	// archive, which is correct: a completed archive is a snapshot of a moment, and
	// a later caller wants the state as it is then, not a cached branch name.
	close(attempt.done)
	s.mu.Lock()
	s.active = nil
	s.mu.Unlock()
	return attempt.branch, attempt.err
}

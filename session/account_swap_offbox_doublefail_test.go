package session

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Regression coverage for the off-box account swap's double-failure arm in
// scopeRotation.rotate(): when the incoming rename (next -> live) AND its
// fallback restore rename (prev -> live) both fail, no live scope exists on the
// host, A is stranded in .prev and B in .next. The error must carry the
// accountScopeIndeterminate marker so the recorder/daemon reconcile the swap as
// unknown (fence the session, leave both directories intact, write nothing back)
// instead of misclassifying it as a definite rollback (discard B's staged
// credentials and assert the old account is live).

// scopeRenameFailer makes scopeRename fail with failErr whenever its source is
// one of failSrc, leaving every other rename on the real syscall. Restored on
// cleanup.
func scopeRenameFailer(t *testing.T, failSrc []string, failErr error) {
	t.Helper()
	original := scopeRename
	scopeRename = func(src, dst string) error {
		for _, s := range failSrc {
			if src == s {
				return failErr
			}
		}
		return original(src, dst)
	}
	t.Cleanup(func() { scopeRename = original })
}

// TestScopeRotationDoubleFailureIsIndeterminate reproduces the bug report's core
// white-box case: the very arm of rotate() whose error had to carry the marker.
func TestScopeRotationDoubleFailureIsIndeterminate(t *testing.T) {
	live := rotationHost(t)
	rot, err := newScopeRotation(tmux.ProgramClaude)
	require.NoError(t, err)

	// Make both the incoming rename and its fallback restore rename fail, the
	// post-double-failure layout: no live scope, A in .prev, B in .next.
	scopeRenameFailer(t, []string{rot.next, rot.prev}, errors.New("simulated EIO"))

	err = rot.rotate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), accountScopeIndeterminate,
		"the double-failure arm must carry the indeterminate marker so a broken layout is not reconciled as a definite rollback")
	assert.False(t, rot.rotated, "rotated stays false: nothing was put back")

	assert.NoDirExists(t, live, "the live scope is gone")
	assert.Equal(t, "A", readOrEmpty(filepath.Join(live+accountPrevSuffix, credFile)),
		"the outgoing account A is stranded in .prev")
	assert.Equal(t, "B", readOrEmpty(filepath.Join(live+accountNextSuffix, credFile)),
		"the incoming account B is still staged in .next")

	// restore() no-ops when !rotated and must neither disturb the broken layout
	// nor hide the indeterminate state by succeeding.
	require.NoError(t, rot.restore())
	assert.NoDirExists(t, live)
	assert.DirExists(t, live+accountPrevSuffix)
	assert.DirExists(t, live+accountNextSuffix)
}

// TestScopeRotationSingleFailureRollsBack is the contrast arm that must stay
// truthful: only the incoming rename fails and the fallback restore succeeds,
// so the host really holds A again and the error carries no marker.
func TestScopeRotationSingleFailureRollsBack(t *testing.T) {
	live := rotationHost(t)
	rot, err := newScopeRotation(tmux.ProgramClaude)
	require.NoError(t, err)

	scopeRenameFailer(t, []string{rot.next}, errors.New("simulated EIO"))

	err = rot.rotate()
	require.Error(t, err)
	assert.NotContains(t, err.Error(), accountScopeIndeterminate,
		"no marker: the scope really was put back, so rolled_back is truthful")
	assert.False(t, rot.rotated)

	assert.Equal(t, "A", readOrEmpty(filepath.Join(live, credFile)), "A is live again")
	assert.NoDirExists(t, live+accountPrevSuffix, "A was moved back out of .prev")
	assert.DirExists(t, live+accountNextSuffix, "B is still staged")
}

// TestSwapOffBoxAccountDoubleFailureIsIndeterminate is the end-to-end guarantee:
// the agent-server rotates into the double-failure arm and the daemon must fence
// the session as startup-unknown, keep B staged in .next, strand A in .prev, and
// return ErrAccountSwapIndeterminate instead of discarding .next and asserting
// the old account is live.
func TestSwapOffBoxAccountDoubleFailureIsIndeterminate(t *testing.T) {
	f := newSwapFixture(t)
	f.refreshOnHost(t, "work-refreshed-on-host") // A's token was rotated before the swap
	// The real remote transport wraps an agent-server refusal as
	// *agentAnsweredError, so swapOutcomeIndeterminate resolves it on the marker;
	// exercise that exact path rather than the lost-call reconciliation arm.
	f.srv.wrapAnswered = true

	live := f.w.AccountDir()
	scopeRenameFailer(t, []string{f.w.NextAccountDir(), f.w.PrevAccountDir()}, errors.New("simulated EIO"))

	_, err := f.inst.SwapOffBoxAccount("alt", false)
	require.ErrorIs(t, err, ErrAccountSwapIndeterminate)
	assert.True(t, f.inst.StartupStateUnknown(),
		"the session is fenced, not presented as healthy on an account whose scope is missing")

	// B's staged credentials are not destroyed: the daemon must NOT discard .next.
	assert.DirExists(t, f.w.NextAccountDir(), ".next is left intact so B's staged credentials survive")
	assert.Equal(t, secretAlt, f.hostCred(t, "account"+accountNextSuffix), "B's credential is still staged in .next")

	// A's refreshed credentials are stranded in .prev and never written back.
	assert.Equal(t, "work-refreshed-on-host", f.hostCred(t, "account"+accountPrevSuffix),
		"A's refresh is stranded in .prev")
	assert.Equal(t, secretWork, readOrEmpty(f.workReg),
		"nothing is written back from a scope of unknown identity")

	// The live scope is gone and the row still names the recorded account.
	assert.NoDirExists(t, live, "no live scope exists on the host")
	acct, _ := f.inst.AccountSelection()
	assert.Equal(t, "work", acct, "the row is not moved to an account the host was not shown to run")
}

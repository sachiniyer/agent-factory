package session

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7788: follow-ups to the #7784 reconcile binding. The stage's swap binding
// is re-proven at the rename itself, a fenced session admits no new swap, the
// staged credentials come off an old agent-server wherever no request can be
// in flight, a restored stage is bound before it appears at .next, and a
// marker the discard cannot read is a failure rather than a silent "not
// mine".

// newScopeRotation's binding check runs before the agent stops, while a
// later swap's re-stage stays unordered with the rotation that follows — so
// the binding is re-read at the rename itself, inside the lock the rename
// holds. A stage re-staged under another swap's marker is refused there,
// never rotated in under this request's id.
func TestScopeRotationRechecksTheBindingAtTheRename(t *testing.T) {
	live := rotationHost(t)
	next := live + accountNextSuffix
	require.NoError(t, os.WriteFile(filepath.Join(next, accountSwapMarkerRel), []byte("swap-1"), 0o600))
	rot, err := newScopeRotation(tmux.ProgramClaude, "swap-1")
	require.NoError(t, err, "the stage names this swap at the early check")

	// A later swap stages over it before the rename: new marker, new creds.
	require.NoError(t, os.RemoveAll(next))
	require.NoError(t, os.MkdirAll(next, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(next, credFile), []byte("C"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(next, accountSwapMarkerRel), []byte("swap-2"), 0o600))

	err = rot.rotate()
	require.ErrorIs(t, err, errStageNotMine, "the binding is re-proven at the rename, not only at admission")
	assert.Equal(t, "A", readOrEmpty(filepath.Join(live, credFile)), "the outgoing scope was put back")
	assert.Equal(t, "C", readOrEmpty(filepath.Join(next, credFile)), "the later swap's stage is untouched")
	assert.Equal(t, "swap-2", readOrEmpty(filepath.Join(next, accountSwapMarkerRel)))
	assert.NoDirExists(t, live+accountPrevSuffix)

	// A stage removed mid-rotation is refused the same way: the rename never
	// rotates in an anonymous or absent stage under a named request.
	rot, err = newScopeRotation(tmux.ProgramClaude, "swap-2")
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(next))
	err = rot.rotate()
	require.ErrorIs(t, err, errStageNotMine)
	assert.Equal(t, "A", readOrEmpty(filepath.Join(live, credFile)))
	assert.NoDirExists(t, live+accountPrevSuffix)
}

// A row whose earlier swap reconciled indeterminate admits no new swap: the
// request that swap sent may still be queued on the agent-server, and a scope
// staged beside it could be rotated in under that request's id or emptied
// under its rotation. Admission refuses — before anything is staged or
// stopped — and names how the fence resolves. A definite verdict never
// strands the row.
func TestSwapOffBoxAccountFencedSessionAdmitsNoNewSwap(t *testing.T) {
	f := newSwapFixture(t)
	f.srv.loseResponse = errors.New("remote agent-server: context deadline exceeded")
	f.srv.statusErr = errors.New("remote agent-server: connection reset")

	_, err := f.inst.SwapOffBoxAccount("alt", false)
	require.ErrorIs(t, err, ErrAccountSwapIndeterminate)
	require.True(t, f.inst.StartupStateUnknown())
	calls, shells := f.srv.calls, len(*f.shellCalls)

	f.registerThird(t)
	_, err = f.inst.SwapOffBoxAccount("third", false)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrAccountSwapIndeterminate, "the refusal names the fence, not a new indeterminate verdict")
	assert.Contains(t, err.Error(), "fenc", "an actionable refusal, not a bare no")
	assert.Contains(t, err.Error(), "queued", "it says why the fence matters to a swap")
	assert.Equal(t, calls, f.srv.calls, "a fenced session never reaches the agent-server")
	assert.Len(t, *f.shellCalls, shells, "and never stages a scope on the host")
	assert.Equal(t, secretAlt, f.hostCred(t, "account"),
		"the lost swap may have rotated — the fence asserts neither account")
	require.Error(t, f.inst.ValidateOffBoxAccountSwap("third"),
		"the scheduler's per-candidate admission refuses it the same way")

	// Once the fence resolves — the pane proved live or the row was rebuilt —
	// the swap is admitted again; a fenced session is never stranded.
	f.inst.ResolveStartupState()
	f.srv.loseResponse, f.srv.statusErr = nil, nil
	_, err = f.inst.SwapOffBoxAccount("third", false)
	require.NoError(t, err)
	acct, _ := f.inst.AccountSelection()
	assert.Equal(t, "third", acct)
}

// On a pre-identity agent-server there is no serialized discard — but a swap
// it answered is terminal: nothing can be in flight, so the staged
// credentials it leaves behind come off the host through an ordered remote
// rm rather than sitting for teardown (#7788).
func TestSwapOffBoxAccountOldServerDefiniteRefusalRemovesTheStage(t *testing.T) {
	f := newSwapFixture(t)
	f.srv.anonymous = true
	f.srv.wrapAnswered = true
	f.srv.failAfter = errors.New("the new agent did not start")

	_, err := f.inst.SwapOffBoxAccount("alt", false)
	require.ErrorContains(t, err, "did not start")
	require.NotErrorIs(t, err, ErrAccountSwapIndeterminate, "the server answered: a definite refusal, not a guess")

	assert.NoDirExists(t, f.w.NextAccountDir(), "the restored stage's credentials do not stay on the host")
	assert.Equal(t, secretWork, f.hostCred(t, "account"))
	assert.True(t, slices.ContainsFunc(*f.shellCalls, func(c string) bool {
		return strings.HasPrefix(c, "rm -rf ") && strings.Contains(c, accountNextSuffix)
	}), "an ordered remote rm removes the stage when no request can be in flight")
}

// The same ordered removal covers a stage whose request was never sent: the
// staging itself failed part-way, leaving the incoming account's credentials
// beside the live scope on a server with no discard route.
func TestSwapOffBoxAccountOldServerStageFailureRemovesTheStage(t *testing.T) {
	f := newSwapFixture(t)
	f.srv.anonymous = true
	nextDir := f.w.NextAccountDir()
	require.NoError(t, os.MkdirAll(nextDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(nextDir, credFile), []byte("half-written"), 0o600))
	p := f.inst.currentBackend().(*sshBackend).provisioner
	real := p.runCommandFn
	p.runCommandFn = func(d time.Duration, script string, in io.Reader, c bool) ([]byte, error) {
		if strings.Contains(script, "tar -xf") {
			return nil, errors.New("ssh: connection reset")
		}
		return real(d, script, in, c)
	}

	_, err := f.inst.SwapOffBoxAccount("alt", false)
	require.ErrorContains(t, err, "staging account")
	assert.NoDirExists(t, nextDir, "the half-written stage comes off the host — no request was ever sent")
}

// A restore interrupted between moving the incoming scope back to staging and
// returning the outgoing one must leave the stage bound the whole time: the
// binding is written before the scope ever appears at .next, so a crash in
// that window can never leave an anonymous stage holding credentials the
// discard would then refuse to remove (#7788).
func TestScopeRotationRestoreBindsTheStageBeforeItAppears(t *testing.T) {
	live := rotationHost(t)
	require.NoError(t, os.WriteFile(
		filepath.Join(live+accountNextSuffix, accountSwapMarkerRel), []byte("swap-1"), 0o600))
	rot, err := newScopeRotation(tmux.ProgramClaude, "swap-1")
	require.NoError(t, err)
	require.NoError(t, rot.rotate())

	// Fail the restore's last rename: the incoming scope already sits at
	// .next — bound, because the marker was written while it was still live.
	scopeRenameFailer(t, []string{rot.prev}, errors.New("simulated EIO"))
	require.Error(t, rot.restore())

	marker, err := os.ReadFile(filepath.Join(live+accountNextSuffix, accountSwapMarkerRel))
	require.NoError(t, err, "the stage carries its binding from the moment it exists at .next")
	assert.Equal(t, "swap-1", string(marker))
	inst := &Instance{Title: "s", Program: tmux.ProgramClaude}
	require.NoError(t, inst.discardAccountNextInPlace("swap-1"),
		"and the bound stage is still this swap's to remove")
	assert.NoDirExists(t, live+accountNextSuffix)
}

// The discard's verdict on the stage is honest about its own read: success
// only when the stage is gone or a readable marker names a different swap —
// never on a marker it could not read, which may be this swap's stage (#7788).
func TestDiscardAccountNextInPlaceMarkerReadErrorsAreFailures(t *testing.T) {
	live := rotationHost(t)
	nextDir := live + accountNextSuffix
	inst := &Instance{Title: "s", Program: tmux.ProgramClaude}

	// A marker that exists but cannot be read is not "a different id": the
	// discard fails rather than report the stage gone.
	require.NoError(t, os.MkdirAll(filepath.Join(nextDir, accountSwapMarkerRel), 0o700))
	require.Error(t, inst.discardAccountNextInPlace("swap-1"), "an unreadable binding is a failure, not a pass")
	assert.DirExists(t, nextDir, "the stage stays for whatever inspects it")

	// An anonymous stage belongs to no named swap — a stage always carries
	// its marker before it holds credentials — so the discard removes it.
	require.NoError(t, os.RemoveAll(nextDir))
	require.NoError(t, os.MkdirAll(nextDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(nextDir, credFile), []byte("B"), 0o600))
	require.NoError(t, inst.discardAccountNextInPlace("swap-1"))
	assert.NoDirExists(t, nextDir)
}

// A fence against a pre-identity server is only actionable if it names the
// route the server lacks (#7788): the indeterminate error says the
// agent-server predates account-swap-discard and must be upgraded.
func TestSwapOffBoxAccountOldServerFenceNamesTheUpgrade(t *testing.T) {
	f := newSwapFixture(t)
	f.srv.anonymous = true
	f.srv.loseBefore = errors.New("remote agent-server: connection reset")

	_, err := f.inst.SwapOffBoxAccount("alt", false)
	require.ErrorIs(t, err, ErrAccountSwapIndeterminate)
	assert.Contains(t, err.Error(), "predates the "+accountSwapDiscardFeature+" route")
	assert.Contains(t, err.Error(), "upgrade")
}

// DiscardAccountNext and SwapAccountInPlace serialize on the server's
// operation lock — this drives the REAL methods rather than a fake's private
// mutex, so dropping agentOpMu from either fails it (#7788). With the lock
// held, neither call can enter its critical section; a method that stopped
// taking it would answer immediately.
func TestLocalAgentServerDiscardAndSwapShareTheOperationLock(t *testing.T) {
	s := &localAgentServer{inst: &Instance{Title: "s"}}

	s.agentOpMu.Lock()
	swapDone := make(chan error, 1)
	discardDone := make(chan error, 1)
	go func() { _, err := s.SwapAccountInPlace("swap-1"); swapDone <- err }()
	go func() { discardDone <- s.DiscardAccountNext("swap-1") }()
	select {
	case err := <-swapDone:
		t.Fatalf("SwapAccountInPlace ran to completion while agentOpMu was held: %v", err)
	case err := <-discardDone:
		t.Fatalf("DiscardAccountNext ran to completion while agentOpMu was held: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	s.agentOpMu.Unlock()
	require.Error(t, <-swapDone, "the bare fixture has no scope to rotate — the lock, not the outcome, is under test")
	require.Error(t, <-discardDone)
}

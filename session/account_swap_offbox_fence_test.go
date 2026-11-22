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

	"github.com/sachiniyer/agent-factory/session/git"
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
	next := stagedScopeDir(live, "swap-1")
	require.NoError(t, os.MkdirAll(next, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(next, credFile), []byte("B"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(next, accountSwapMarkerRel), []byte("swap-1"), 0o600))
	rot, err := newScopeRotation(tmux.ProgramClaude, "swap-1", true)
	require.NoError(t, err, "the stage names this swap at the early check")

	// The stage is swapped out before the rename: new marker, new creds — the
	// unordered re-stage the rename-time check exists for, at this swap's own
	// path (#7793).
	require.NoError(t, os.RemoveAll(next))
	require.NoError(t, os.MkdirAll(next, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(next, credFile), []byte("C"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(next, accountSwapMarkerRel), []byte("swap-2"), 0o600))

	err = rot.rotate()
	require.ErrorIs(t, err, errStageRebound, "the binding is re-proven at the rename, not only at admission")
	assert.Equal(t, "A", readOrEmpty(filepath.Join(live, credFile)), "the outgoing scope was put back")
	assert.Equal(t, "C", readOrEmpty(filepath.Join(next, credFile)), "the later swap's stage is untouched")
	assert.Equal(t, "swap-2", readOrEmpty(filepath.Join(next, accountSwapMarkerRel)))
	assert.NoDirExists(t, live+accountPrevSuffix)

	// A stage removed mid-rotation is refused the same way: the rename never
	// rotates in an anonymous or absent stage under a named request.
	next = stagedScopeDir(live, "swap-2")
	require.NoError(t, os.MkdirAll(next, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(next, credFile), []byte("C"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(next, accountSwapMarkerRel), []byte("swap-2"), 0o600))
	rot, err = newScopeRotation(tmux.ProgramClaude, "swap-2", true)
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(next))
	err = rot.rotate()
	require.ErrorIs(t, err, errStageRebound)
	assert.Equal(t, "A", readOrEmpty(filepath.Join(live, credFile)))
	assert.NoDirExists(t, live+accountPrevSuffix)
}

// A row whose earlier swap reconciled indeterminate admits no new swap: the
// request that swap sent may still be queued on the agent-server, and a scope
// staged beside it could be rotated in under that request's id or emptied
// under its rotation. Admission refuses — before anything is staged or
// stopped — and names the way out: a daemon restart re-asks the host for THIS
// swap's verdict, `af sessions kill` removes the row (#7793).
func TestSwapOffBoxAccountFencedSessionAdmitsNoNewSwap(t *testing.T) {
	f := newSwapFixture(t)
	f.srv.loseResponse = errors.New("remote agent-server: context deadline exceeded")
	f.srv.statusErr = errors.New("remote agent-server: connection reset")

	_, err := f.inst.SwapOffBoxAccount("alt", false)
	require.ErrorIs(t, err, ErrAccountSwapIndeterminate)
	require.True(t, f.inst.StartupStateUnknown())
	require.NotNil(t, f.inst.InFlightOffBoxSwap(), "the record the verb reconciles is kept")
	calls, shells := f.srv.calls, len(*f.shellCalls)

	f.registerThird(t)
	_, err = f.inst.SwapOffBoxAccount("third", false)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrAccountSwapIndeterminate, "the refusal names the fence, not a new indeterminate verdict")
	assert.Contains(t, err.Error(), "fenc", "an actionable refusal, not a bare no")
	assert.Contains(t, err.Error(), "queued", "it says why the fence matters to a swap")
	assert.Contains(t, err.Error(), "daemon restart", "it names the automatic reconcile a restart drives")
	assert.Contains(t, err.Error(), "af sessions kill", "and the row removal that always works")
	assert.Equal(t, calls, f.srv.calls, "a fenced session never reaches the agent-server")
	assert.Len(t, *f.shellCalls, shells, "and never stages a scope on the host")
	assert.Equal(t, secretAlt, f.hostCred(t, "account"),
		"the lost swap may have rotated — the fence asserts neither account")
	require.Error(t, f.inst.ValidateOffBoxAccountSwap("third"),
		"the scheduler's per-candidate admission refuses it the same way")
}

// The session-level verdict reconcile (#7793): the fenced row re-asks the
// host for THIS swap's terminal record — never re-sending the request — and
// lifts the fence exactly when the answer is definite. This is the machinery
// the daemon's restart reconcile reaches through ReattachSandbox; an
// indeterminate answer keeps the fence and the record, and pane liveness
// alone never clears either.
func TestSwapOffBoxAccountReconcileSwapUnfencesOnAVerdict(t *testing.T) {
	f := newSwapFixture(t)
	f.srv.loseResponse = errors.New("remote agent-server: context deadline exceeded")
	f.srv.statusErr = errors.New("remote agent-server: connection reset")

	_, err := f.inst.SwapOffBoxAccount("alt", false)
	require.ErrorIs(t, err, ErrAccountSwapIndeterminate)
	require.True(t, f.inst.StartupStateUnknown())
	require.False(t, f.inst.Started(), "the fence lifts the running claim with it")
	require.NotNil(t, f.inst.InFlightOffBoxSwap())
	f.inst.remoteClient = &remoteAgentClient{} // the transport is bound; only the host's answer is missing

	// While the host cannot say how the swap ended, the verb refuses and the
	// fence — and the record it reconciles — stays.
	settled, err := f.inst.ReconcileOffBoxSwapFence()
	require.Error(t, err)
	require.False(t, settled)
	assert.ErrorIs(t, err, ErrOffBoxSwapVerdictIndeterminate)
	require.True(t, f.inst.StartupStateUnknown())
	require.NotNil(t, f.inst.InFlightOffBoxSwap())

	// The host answers: this swap committed. The fence lifts, the row records
	// the account the host provably runs, and the record retires.
	f.srv.statusErr = nil
	settled, err = f.inst.ReconcileOffBoxSwapFence()
	require.NoError(t, err)
	assert.True(t, settled)
	acct, _ := f.inst.AccountSelection()
	assert.Equal(t, "alt", acct, "the committed verdict settles the row onto the account the host runs")
	assert.Equal(t, secretAlt, f.hostCred(t, "account"))
	assert.False(t, f.inst.StartupStateUnknown())
	assert.True(t, f.inst.Started())
	assert.Nil(t, f.inst.InFlightOffBoxSwap())
	assert.NoDirExists(t, f.w.PrevAccountDir(), "the outgoing scope came home and was removed")
}

// KNOWN GAP (#7797): there is no operator-invoked unfence verb. The row's
// only exits while the host cannot say how the recorded swap ended are a
// daemon restart — whose load path re-runs the same session-level reconcile —
// or `af sessions kill`. Whether a public verb should exist is the owner's
// decision in #7797; the fence must not pretend one exists in the meantime.
func TestSwapOffBoxAccountFencedSessionHasNoUnfenceVerb_KnownGap7797(t *testing.T) {
	f := newSwapFixture(t)
	f.srv.loseResponse = errors.New("remote agent-server: context deadline exceeded")
	f.srv.statusErr = errors.New("remote agent-server: connection reset")

	_, err := f.inst.SwapOffBoxAccount("alt", false)
	require.ErrorIs(t, err, ErrAccountSwapIndeterminate)
	require.True(t, f.inst.StartupStateUnknown())
	require.False(t, f.inst.Started(), "the fence lifts the running claim with it")

	f.registerThird(t)
	_, err = f.inst.SwapOffBoxAccount("third", false)
	require.Error(t, err, "the fence does not decay: a later attempt is refused the same way")
	require.True(t, f.inst.StartupStateUnknown(),
		"and nothing in the swap path clears it — a daemon restart re-runs the reconcile and `af sessions kill` removes the row; #7797 tracks whether an operator verb should exist")
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

	assert.NoDirExists(t, f.stagedDir(), "the restored stage's credentials do not stay on the host")
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
	nextDir := f.stagedDir()
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
	next := stagedScopeDir(live, "swap-1")
	require.NoError(t, os.MkdirAll(next, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(next, credFile), []byte("B"), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(next, accountSwapMarkerRel), []byte("swap-1"), 0o600))
	rot, err := newScopeRotation(tmux.ProgramClaude, "swap-1", true)
	require.NoError(t, err)
	require.NoError(t, rot.rotate())

	// Fail the restore's last rename: the incoming scope already sits at
	// its staging path — bound, because the marker was written while it was
	// still live.
	scopeRenameFailer(t, []string{rot.prev}, errors.New("simulated EIO"))
	require.Error(t, rot.restore())

	marker, err := os.ReadFile(filepath.Join(next, accountSwapMarkerRel))
	require.NoError(t, err, "the stage carries its binding from the moment it exists at its staging path")
	assert.Equal(t, "swap-1", string(marker))
	inst := &Instance{Title: "s", Program: tmux.ProgramClaude}
	require.NoError(t, inst.discardAccountNextInPlace("swap-1"),
		"and the bound stage is still this swap's to remove")
	assert.NoDirExists(t, next)
}

// The discard's verdict on the stage is honest about its own read: success
// only when the stage is gone or a readable marker names a different swap —
// never on a marker it could not read, which may be this swap's stage (#7788).
func TestDiscardAccountNextInPlaceMarkerReadErrorsAreFailures(t *testing.T) {
	live := rotationHost(t)
	nextDir := stagedScopeDir(live, "swap-1")
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
	go func() { _, err := s.SwapAccountInPlace("swap-1", true); swapDone <- err }()
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

// rebindBackend is a FakeBackend whose sibling-stop re-stages this swap's
// staging path under another swap's marker — the unordered re-stage the
// rename-time binding re-check exists to refuse — and records whether the
// swap's start half ran,
// standing in for the outgoing agent's restart on the unchanged scope.
type rebindBackend struct {
	*FakeBackend
	restage   func()
	restarted bool
}

func (b *rebindBackend) stopForAccountSwap(*Instance, bool) error {
	b.restage()
	return nil
}

func (b *rebindBackend) SwapAgent(_ *Instance, plan AgentSwapPlan) error {
	if err := plan.CaptureAfterStop(); err != nil {
		return err
	}
	if err := plan.RunBeforeStart(); err != nil {
		return err
	}
	b.restarted = true
	return nil
}

// A stage re-staged between the agent's stop and the rename is refused at the
// rename itself — but the refusal lands AFTER the outgoing agent is dead, so
// answering it like a pre-stop straggler strands a live row's agent (#7792
// R5). The unchanged scope is put back by rotate, the same command restarts
// on it, and the refusal lands rolled_back under THIS swap's id: restoring a
// previous swap's committed record would let a lost reconcile claim committed
// for a host that kept the old account.
func TestSwapAccountInPlaceReboundStageRestartsTheOutgoingAgent(t *testing.T) {
	live := rotationHost(t)
	next := stagedScopeDir(live, "swap-1")
	require.NoError(t, os.MkdirAll(next, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(next, credFile), []byte("B"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(next, accountSwapMarkerRel), []byte("swap-1"), 0o600))

	bin := t.TempDir()
	claude := filepath.Join(bin, "claude")
	require.NoError(t, os.WriteFile(claude, []byte("#!/bin/sh\n"), 0o755))
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	gw, err := git.NewGitWorktreeFromStorage(
		initTempGitRepo(t), existingDir(t, filepath.Join(t.TempDir(), "wt")),
		"rebound", "af/rebound", "", false, true)
	require.NoError(t, err)

	be := &rebindBackend{FakeBackend: NewFakeBackend(), restage: func() {
		// The agent is already stopped: a later swap re-stages .next under
		// its own marker before this swap's rename runs.
		if err := os.RemoveAll(next); err != nil {
			return
		}
		_ = os.MkdirAll(next, 0o700)
		_ = os.WriteFile(filepath.Join(next, credFile), []byte("C"), 0o600)
		_ = os.WriteFile(filepath.Join(next, accountSwapMarkerRel), []byte("swap-2"), 0o600)
	}}
	inst, err := NewInstance(InstanceOptions{Title: "rebound", Path: gw.GetWorktreePath(), Program: tmux.ProgramClaude})
	require.NoError(t, err)
	inst.SetBackend(be)
	inst.SetStartedForTest(true)
	inst.gitWorktree = gw
	inst.Tabs = []*Tab{newAgentTab(tmux.NewTmuxSession("rebound", tmux.ProgramClaude))}
	inst.runtimeProgram = claude

	s := &localAgentServer{inst: inst}
	// A previous swap's committed record sits in the slot: a post-stop refusal
	// must not be mistaken for a pre-stop straggler restoring it.
	s.acctSwap.id, s.acctSwap.outcome, s.acctSwap.program = "swap-0", swapOutcomeCommitted, "claude"

	_, err = s.SwapAccountInPlace("swap-1", true)
	require.Error(t, err)
	assert.True(t, be.restarted,
		"the outgoing agent must be restarted on the unchanged scope, not left dead")
	outcome, program, id := s.acctSwap.status()
	assert.Equal(t, swapOutcomeRolledBack, outcome,
		"the refusal is THIS swap's own rolled_back — not a previous record's restore")
	assert.Equal(t, "swap-1", id)
	assert.Empty(t, program)
	assert.Equal(t, "A", readOrEmpty(filepath.Join(live, credFile)),
		"the outgoing scope was put back and stays live")
	assert.Equal(t, "C", readOrEmpty(filepath.Join(next, credFile)),
		"the later swap's stage is untouched")
	assert.Equal(t, "swap-2", readOrEmpty(filepath.Join(next, accountSwapMarkerRel)))
}

package session

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Account handoff for a session on a listed host (#5571 W7.6).

const (
	secretWork = "token-for-work"
	secretAlt  = "token-for-alt"
	credFile   = ".credentials.json"
)

// acctSwapServer is the sandbox's agent-server, as far as the rotation goes: it
// runs the REAL scope rotation (newScopeRotation / rotate) on the host directory,
// and records what the host looked like at the moment the agent would have been
// stopped.
type acctSwapServer struct {
	AgentServer
	live, next string
	can        bool
	canErr     error
	failAfter  error // returned after a successful rotation, to exercise restore
	// loseResponse is a transport failure AFTER the host rotated: the work is done
	// and the answer is lost. loseBefore is one before anything ran. afterRotate
	// lets a test damage the host between the two.
	loseResponse, loseBefore error
	afterRotate              func()
	statusErr                error
	rec                      accountSwapRecorder
	calls                    int
	atCall                   struct{ live, next string }
}

func (s *acctSwapServer) CanSwapAccount() (bool, error) { return s.can, s.canErr }

func (s *acctSwapServer) AccountSwapStatus() (string, string, error) {
	if s.statusErr != nil {
		return "", "", s.statusErr
	}
	o, p := s.rec.status()
	return o, p, nil
}

// SwapAccountInPlace records the terminal outcome the way the real server does,
// and may then lose the response.
func (s *acctSwapServer) SwapAccountInPlace() (string, error) {
	if s.loseBefore != nil {
		s.calls++
		return "", s.loseBefore
	}
	prog, err := s.rec.run(s.swap)
	if err == nil && s.loseResponse != nil {
		return "", s.loseResponse
	}
	return prog, err
}

func (s *acctSwapServer) swap() (string, error) {
	s.calls++
	s.atCall.live = readOrEmpty(filepath.Join(s.live, credFile))
	s.atCall.next = readOrEmpty(filepath.Join(s.next, credFile))
	if s.loseBefore != nil {
		return "", s.loseBefore
	}
	rot, err := newScopeRotation(tmux.ProgramClaude)
	if err != nil {
		return "", err
	}
	if err := rot.rotate(); err != nil {
		return "", err
	}
	if s.afterRotate != nil {
		s.afterRotate()
	}
	if s.failAfter != nil {
		if rbErr := rot.restore(); rbErr != nil {
			return "", errors.Join(s.failAfter, rbErr)
		}
		return "", s.failAfter
	}
	return "claude", nil
}

func readOrEmpty(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return string(b)
}

type swapFixture struct {
	inst             *Instance
	w                *sandboxWorkspace
	srv              *acctSwapServer
	dir              string // the session dir on the "host"
	workReg, altReg  string // registry credential files
	shellCalls       *[]string
	stagedSecretSeen *string
}

func newSwapFixture(t *testing.T) *swapFixture {
	t.Helper()
	f := newListedFixture(t, oneListedHost)
	home := os.Getenv("AGENT_FACTORY_HOME")
	altDir, err := agentaccount.Register(home, tmux.ProgramClaude, "alt")
	require.NoError(t, err)
	workReg, altReg := filepath.Join(f.accountDir, credFile), filepath.Join(altDir, credFile)
	require.NoError(t, os.WriteFile(workReg, []byte(secretWork), 0o600))
	require.NoError(t, os.WriteFile(altReg, []byte(secretAlt), 0o600))

	dir := t.TempDir()
	var calls []string
	p := &sandboxProvisioner{sessionDir: dir}
	p.runCommandFn = func(d time.Duration, script string, stdin io.Reader, combined bool) ([]byte, error) {
		calls = append(calls, script)
		return localShell{}.Run(d, script, stdin, combined)
	}
	acct := sessionenv.Account{Agent: tmux.ProgramClaude, Name: "work", Dir: f.accountDir}
	w := &sandboxWorkspace{shell: p, SessionDir: dir, spec: ProvisionSpec{Account: acct, AccountSyncHost: "box1", Program: tmux.ProgramClaude}}
	require.NoError(t, w.syncAccountIn(time.Minute))
	p.account = w
	calls = nil

	srv := &acctSwapServer{live: w.AccountDir(), next: w.NextAccountDir(), can: true}
	t.Setenv("CLAUDE_CONFIG_DIR", w.AccountDir())
	inst := &Instance{
		Title: "swap", Path: f.repo, Program: tmux.ProgramClaude, Host: "box1",
		Account: "work", accountAgent: tmux.ProgramClaude,
		backend: &sshBackend{provisioner: p, cleanup: &SSHRuntimeCleanupData{SessionDir: dir}},
		started: true, Tabs: []*Tab{newRemoteAgentTab()},
	}
	inst.agentSrv = srv
	return &swapFixture{inst: inst, w: w, srv: srv, dir: dir, workReg: workReg, altReg: altReg, shellCalls: &calls}
}

func (f *swapFixture) hostCred(t *testing.T, scope string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, scope, credFile))
	require.NoError(t, err)
	return string(b)
}

func (f *swapFixture) refreshOnHost(t *testing.T, token string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(f.w.AccountDir(), credFile), []byte(token), 0o600))
}

func TestSwapOffBoxAccountRotatesTheScopeAndWritesTheOldAccountBack(t *testing.T) {
	f := newSwapFixture(t)
	f.refreshOnHost(t, "work-refreshed-on-host") // the agent rotated A's token before the swap

	res, err := f.inst.SwapOffBoxAccount("alt", false)
	require.NoError(t, err)

	// B was staged beside the live scope while A was still the live one: the agent
	// is stopped between the two, never with B under it.
	assert.Equal(t, "work-refreshed-on-host", f.srv.atCall.live, "A is still live when the agent-server is asked")
	assert.Equal(t, secretAlt, f.srv.atCall.next, "B is staged before the agent is stopped")

	assert.Equal(t, secretAlt, f.hostCred(t, "account"), "the live scope now holds B")
	assert.NoDirExists(t, f.w.NextAccountDir())
	assert.NoDirExists(t, f.w.PrevAccountDir(), "nothing of A is left on the host")
	assert.Equal(t, "work-refreshed-on-host", readOrEmpty(f.workReg), "A's refresh came home before A was removed")
	assert.Equal(t, 1, res.WrittenBack)
	assert.NoError(t, res.CleanupErr)

	acct, auto := f.inst.AccountSelection()
	assert.Equal(t, "alt", acct)
	assert.False(t, auto)
	assert.Equal(t, "alt", f.w.spec.Account.Name)
	assert.Equal(t, "alt", f.inst.currentBackend().(*sshBackend).provisioner.spec.Account.Name)

	// The baseline is now B's: a refresh of B writes back, an unchanged B does not.
	f.refreshOnHost(t, "alt-refreshed")
	back, err := f.w.writeBackAccount(time.Minute)
	require.NoError(t, err)
	assert.Equal(t, 1, back.Written)
	assert.Equal(t, "alt-refreshed", readOrEmpty(f.altReg))
	assert.Equal(t, "work-refreshed-on-host", readOrEmpty(f.workReg), "and A's registry copy is untouched by B's session")
}

func TestSwapOffBoxAccountAutomaticSelectionIsRecorded(t *testing.T) {
	f := newSwapFixture(t)
	_, err := f.inst.SwapOffBoxAccount("alt", true)
	require.NoError(t, err)
	_, auto := f.inst.AccountSelection()
	assert.True(t, auto, "a scheduler choice stays distinguishable from an operator's pin")
}

// Every refusal is decided before anything is staged or stopped, and each leaves
// the session exactly as it was.
func TestSwapOffBoxAccountRefusalsCostNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		to   string
		prep func(t *testing.T, f *swapFixture)
		want string
	}{
		{"the same account", "work", nil, "already uses"},
		{"an unregistered account", "ghost", nil, "ghost"},
		{"no account named", "", nil, "needs the account"},
		{"a host that is not listed", "alt", func(t *testing.T, f *swapFixture) { f.w.spec.AccountSyncHost = "" }, "global host list"},
		{"a command that sets its own identity", "alt", func(t *testing.T, f *swapFixture) {
			f.w.spec.Program = "CLAUDE_CONFIG_DIR=/elsewhere claude"
		}, "sets an identity variable itself"},
		{"an account of another agent", "other", func(t *testing.T, f *swapFixture) {
			home := os.Getenv("AGENT_FACTORY_HOME")
			dir, err := agentaccount.Register(home, tmux.ProgramCodex, "other")
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "auth.json"), []byte("x"), 0o600))
		}, "other"},
		{"an agent-server that cannot rotate", "alt", func(t *testing.T, f *swapFixture) { f.srv.can = false }, "cannot swap accounts"},
		{"an agent-server that cannot be asked", "alt", func(t *testing.T, f *swapFixture) { f.srv.canErr = errors.New("unreachable") }, "unreachable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwapFixture(t)
			if tc.prep != nil {
				tc.prep(t, f)
			}
			_, err := f.inst.SwapOffBoxAccount(tc.to, false)
			require.ErrorContains(t, err, tc.want)
			assert.Zero(t, f.srv.calls, "the agent-server is never asked to stop anything")
			assert.Empty(t, *f.shellCalls, "and nothing is sent to the host")
			assert.NoDirExists(t, f.w.NextAccountDir())
			assert.Equal(t, secretWork, f.hostCred(t, "account"))
			acct, _ := f.inst.AccountSelection()
			assert.Equal(t, "work", acct)
		})
	}
}

func TestSwapOffBoxAccountFailureInsideTheHostLeavesTheOldAccount(t *testing.T) {
	f := newSwapFixture(t)
	f.srv.failAfter = errors.New("the new agent did not start")

	_, err := f.inst.SwapOffBoxAccount("alt", false)
	require.ErrorContains(t, err, "did not start")

	assert.Equal(t, secretWork, f.hostCred(t, "account"), "the scope is put back as A")
	assert.NoDirExists(t, f.w.NextAccountDir(), "B's credentials do not stay on the host")
	assert.NoDirExists(t, f.w.PrevAccountDir())
	acct, _ := f.inst.AccountSelection()
	assert.Equal(t, "work", acct, "the row names an account the host still holds")
	assert.Equal(t, "work", f.w.spec.Account.Name)
}

func TestSwapOffBoxAccountStageFailureStopsNothing(t *testing.T) {
	f := newSwapFixture(t)
	p := f.inst.currentBackend().(*sshBackend).provisioner
	p.runCommandFn = func(time.Duration, string, io.Reader, bool) ([]byte, error) {
		return nil, errors.New("ssh: connection refused")
	}
	_, err := f.inst.SwapOffBoxAccount("alt", false)
	require.ErrorContains(t, err, "staging account")
	assert.Zero(t, f.srv.calls, "an account that did not arrive is never swapped to")
	acct, _ := f.inst.AccountSelection()
	assert.Equal(t, "work", acct)
}

// A failed cleanup of the outgoing account is reported, never swallowed — the
// credentials are still on the host — and does not undo the completed swap.
func TestSwapOffBoxAccountReportsCredentialsLeftOnTheHost(t *testing.T) {
	f := newSwapFixture(t)
	p := f.inst.currentBackend().(*sshBackend).provisioner
	real := p.runCommandFn
	p.runCommandFn = func(d time.Duration, script string, stdin io.Reader, combined bool) ([]byte, error) {
		if stdin == nil && len(script) > 5 && script[:5] == "rm -r" && containsStr(script, accountPrevSuffix) {
			return []byte("busy"), errors.New("rm failed")
		}
		return real(d, script, stdin, combined)
	}
	res, err := f.inst.SwapOffBoxAccount("alt", false)
	require.NoError(t, err, "the swap itself completed")
	require.Error(t, res.CleanupErr)
	assert.Contains(t, res.CleanupErr.Error(), "previous account")
	acct, _ := f.inst.AccountSelection()
	assert.Equal(t, "alt", acct)
}

func containsStr(s, sub string) bool {
	return len(sub) > 0 && (len(s) >= len(sub)) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestOffBoxAccountHandoffAppliesOnlyToASyncedAccountOnAHost(t *testing.T) {
	f := newSwapFixture(t)
	assert.True(t, f.inst.OffBoxAccountHandoffApplies())
	assert.False(t, (&Instance{Title: "local"}).OffBoxAccountHandoffApplies(), "a local session keeps its own path")
	f.inst.currentBackend().(*sshBackend).provisioner.account = nil
	assert.False(t, f.inst.OffBoxAccountHandoffApplies(), "a sandbox session without an account has nothing to rotate")
}

// --- the host-side rotation ---

func rotationHost(t *testing.T) (live string) {
	t.Helper()
	root := t.TempDir()
	live = filepath.Join(root, "account")
	for dir, secret := range map[string]string{live: "A", live + accountNextSuffix: "B"} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, credFile), []byte(secret), 0o600))
	}
	t.Setenv("CLAUDE_CONFIG_DIR", live)
	return live
}

func TestScopeRotationRoundTrip(t *testing.T) {
	live := rotationHost(t)
	rot, err := newScopeRotation(tmux.ProgramClaude)
	require.NoError(t, err)

	require.NoError(t, rot.rotate())
	assert.Equal(t, "B", readOrEmpty(filepath.Join(live, credFile)))
	assert.Equal(t, "A", readOrEmpty(filepath.Join(live+accountPrevSuffix, credFile)))
	assert.NoDirExists(t, live+accountNextSuffix)

	require.NoError(t, rot.restore())
	assert.Equal(t, "A", readOrEmpty(filepath.Join(live, credFile)))
	assert.Equal(t, "B", readOrEmpty(filepath.Join(live+accountNextSuffix, credFile)))
	assert.NoDirExists(t, live+accountPrevSuffix)
	assert.NoError(t, rot.restore(), "restoring what was not rotated is a no-op")
}

// The scope is derived from the agent-server's own environment, and only a path
// af made (".../account") is ever touched, so a request cannot redirect it.
func TestScopeRotationRefusesAScopeAfDidNotMake(t *testing.T) {
	t.Run("not named account", func(t *testing.T) {
		rotationHost(t)
		other := filepath.Join(t.TempDir(), "home-dot-claude")
		require.NoError(t, os.MkdirAll(other+accountNextSuffix, 0o700))
		t.Setenv("CLAUDE_CONFIG_DIR", other)
		_, err := newScopeRotation(tmux.ProgramClaude)
		require.ErrorContains(t, err, "not a session account scope")
	})
	t.Run("unset", func(t *testing.T) {
		t.Setenv("CLAUDE_CONFIG_DIR", "")
		_, err := newScopeRotation(tmux.ProgramClaude)
		require.Error(t, err)
	})
	t.Run("nothing staged", func(t *testing.T) {
		live := rotationHost(t)
		require.NoError(t, os.RemoveAll(live+accountNextSuffix))
		_, err := newScopeRotation(tmux.ProgramClaude)
		require.ErrorContains(t, err, "no staged account")
	})
	t.Run("staged scope holds no credential", func(t *testing.T) {
		live := rotationHost(t)
		require.NoError(t, os.Remove(filepath.Join(live+accountNextSuffix, credFile)))
		_, err := newScopeRotation(tmux.ProgramClaude)
		require.ErrorContains(t, err, "no credential file")
	})
	t.Run("staged scope is a symlink", func(t *testing.T) {
		live := rotationHost(t)
		require.NoError(t, os.RemoveAll(live+accountNextSuffix))
		require.NoError(t, os.Symlink(t.TempDir(), live+accountNextSuffix))
		_, err := newScopeRotation(tmux.ProgramClaude)
		require.Error(t, err)
	})
	t.Run("an agent with no scope variable", func(t *testing.T) {
		_, err := newScopeRotation("aider")
		require.Error(t, err)
	})
}

// --- transport failure mid-swap (reconciliation) ---

// The call's response is lost AFTER the host rotated. The session must come to
// record the account the host actually runs, not "still the old one".
func TestSwapOffBoxAccountLostResponseAfterRotationRecordsTheNewAccount(t *testing.T) {
	f := newSwapFixture(t)
	f.refreshOnHost(t, "work-refreshed-on-host")
	f.srv.loseResponse = errors.New("remote agent-server: POST /v1/agent/swap-account: context deadline exceeded")

	res, err := f.inst.SwapOffBoxAccount("alt", false)
	require.NoError(t, err)
	assert.True(t, res.Reconciled, "the outcome was learned from the host, not assumed")

	acct, _ := f.inst.AccountSelection()
	assert.Equal(t, "alt", acct, "recorded identity matches the running one")
	assert.Equal(t, secretAlt, f.hostCred(t, "account"))
	assert.NoDirExists(t, f.w.PrevAccountDir())
	assert.Equal(t, "work-refreshed-on-host", readOrEmpty(f.workReg), "A's refresh still came home")

	// Teardown must not push B's credentials over A's registry copy.
	require.NoError(t, f.w.teardownAccount(time.Minute))
	assert.Equal(t, "work-refreshed-on-host", readOrEmpty(f.workReg))
}

func TestSwapOffBoxAccountLostResponseBeforeRotationKeepsTheOldAccount(t *testing.T) {
	f := newSwapFixture(t)
	f.srv.loseBefore = errors.New("remote agent-server: POST /v1/agent/swap-account: connection reset")

	_, err := f.inst.SwapOffBoxAccount("alt", false)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrAccountSwapIndeterminate, "the host showed the old account live: that is a fact, not a guess")
	acct, _ := f.inst.AccountSelection()
	assert.Equal(t, "work", acct)
	assert.Equal(t, secretWork, f.hostCred(t, "account"))
	assert.NoDirExists(t, f.w.NextAccountDir())
	assert.False(t, f.inst.StartupStateUnknown())
}

// When the host cannot say which account it runs, none is asserted: the session is
// fenced, and nothing is written back from a scope of unknown identity.
func TestSwapOffBoxAccountUnknownHostStateAssertsNeitherAccount(t *testing.T) {
	f := newSwapFixture(t)
	f.refreshOnHost(t, "work-refreshed-on-host")
	f.srv.loseResponse = errors.New("transport: connection reset")
	f.srv.statusErr = errors.New("transport: connection reset") // the terminal record cannot be read either

	_, err := f.inst.SwapOffBoxAccount("alt", false)
	require.ErrorIs(t, err, ErrAccountSwapIndeterminate)
	acct, _ := f.inst.AccountSelection()
	assert.Equal(t, "work", acct, "the row is not moved to an account the host was not shown to run")
	assert.True(t, f.inst.StartupStateUnknown(), "and it is not presented as healthy on the old one")
	require.NoError(t, f.w.teardownAccount(time.Minute))
	assert.Equal(t, secretWork, readOrEmpty(f.workReg), "no write-back from an unknown scope")
}

// A server that never ran the request vouches for nothing by itself: the host's
// markers must agree, or the state is unknown.
func TestSwapOffBoxAccountNoRecordNeedsTheHostToAgree(t *testing.T) {
	f := newSwapFixture(t)
	f.srv.loseBefore = errors.New("transport: connection reset")
	require.NoError(t, os.RemoveAll(f.w.NextAccountDir())) // contradicts "never ran, staged"
	// stageAccountNext re-stages, so damage the probe instead.
	p := f.inst.currentBackend().(*sshBackend).provisioner
	real := p.runCommandFn
	p.runCommandFn = func(d time.Duration, script string, stdin io.Reader, c bool) ([]byte, error) {
		if indexOf(script, "echo outgoing") >= 0 {
			return nil, errors.New("ssh: connection refused")
		}
		return real(d, script, stdin, c)
	}
	_, err := f.inst.SwapOffBoxAccount("alt", false)
	require.ErrorIs(t, err, ErrAccountSwapIndeterminate)
	assert.True(t, f.inst.StartupStateUnknown())
}

// The server's own record can say it could not restore the scope.
func TestSwapOffBoxAccountServerThatCouldNotRestoreIsUnknown(t *testing.T) {
	f := newSwapFixture(t)
	f.srv.failAfter = errors.New("start failed (" + accountScopeIndeterminate + ")")
	_, err := f.inst.SwapOffBoxAccount("alt", false)
	require.ErrorIs(t, err, ErrAccountSwapIndeterminate)
	assert.True(t, f.inst.StartupStateUnknown())
	acct, _ := f.inst.AccountSelection()
	assert.Equal(t, "work", acct)
}

// A status request made while the swap is mid-flight waits for its terminal
// result: it can never observe the intermediate layout and let the daemon delete
// the rollback state under a swap that may still fail.
func TestAccountSwapStatusWaitsForTheRunningSwap(t *testing.T) {
	var r accountSwapRecorder
	started, release, got := make(chan struct{}), make(chan struct{}), make(chan [2]string, 1)
	go func() {
		_, _ = r.run(func() (string, error) {
			close(started)
			<-release
			return "claude", nil
		})
	}()
	<-started
	go func() { o, p := r.status(); got <- [2]string{o, p} }()
	select {
	case v := <-got:
		t.Fatalf("status answered %v while the swap was still running", v)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	assert.Equal(t, [2]string{swapOutcomeCommitted, "claude"}, <-got)

	var fresh accountSwapRecorder
	o, _ := fresh.status()
	assert.Equal(t, swapOutcomeNone, o)
	_, _ = fresh.run(func() (string, error) { return "", errors.New("refused") })
	o, _ = fresh.status()
	assert.Equal(t, swapOutcomeRolledBack, o)
	_, _ = fresh.run(func() (string, error) { return "", errors.New(accountScopeIndeterminate) })
	o, _ = fresh.status()
	assert.Equal(t, swapOutcomeUnknown, o)
}

// Siblings are stopped before the scope moves, and one that cannot be confirmed
// stopped aborts the rotation untouched.
func TestScopeRotationStopsSiblingsFirst(t *testing.T) {
	live := rotationHost(t)
	rot, err := newScopeRotation(tmux.ProgramClaude)
	require.NoError(t, err)

	var order []string
	require.NoError(t, rot.rotateAfter(func() error {
		order = append(order, "siblings")
		assert.Equal(t, "A", readOrEmpty(filepath.Join(live, credFile)), "the scope has not moved yet")
		return nil
	})())
	assert.Equal(t, []string{"siblings"}, order)
	assert.Equal(t, "B", readOrEmpty(filepath.Join(live, credFile)))

	live = rotationHost(t)
	rot, err = newScopeRotation(tmux.ProgramClaude)
	require.NoError(t, err)
	err = rot.rotateAfter(func() error { return errors.New("cannot confirm tab \"shell\" stopped") })()
	require.ErrorContains(t, err, "cannot confirm")
	assert.Equal(t, "A", readOrEmpty(filepath.Join(live, credFile)), "no rotation under a live sibling")
	assert.DirExists(t, live+accountNextSuffix)
}

// The restart re-executes the recorded, attributed launch command — never a
// re-resolution of the agent's name, and never a command af cannot attribute.
func TestProvenRestartCommand(t *testing.T) {
	bin := t.TempDir()
	claude := filepath.Join(bin, "claude")
	require.NoError(t, os.WriteFile(claude, []byte("#!/bin/sh\n"), 0o755))
	notClaude := filepath.Join(bin, "codex")
	require.NoError(t, os.WriteFile(notClaude, []byte("#!/bin/sh\n"), 0o755))

	inst := &Instance{Title: "s"}
	_, err := inst.provenRestartCommand("claude")
	require.ErrorContains(t, err, "no recorded launch command")

	inst.runtimeProgram = claude
	got, err := inst.provenRestartCommand("claude")
	require.NoError(t, err)
	assert.Equal(t, claude, got, "exactly the recorded absolute command")

	inst.runtimeProgram = "claude" // bare: a PATH lookup the workspace could answer
	_, err = inst.provenRestartCommand("claude")
	require.Error(t, err, "a bare command is not attributable")

	inst.runtimeProgram = notClaude // overrides pointed claude's slot at codex
	_, err = inst.provenRestartCommand("claude")
	require.ErrorContains(t, err, "refusing to restart a different agent")
}

// A refusal the agent-server itself returned is a definite answer; one that says
// it could not restore the scope is not.
func TestSwapOffBoxAccountServerAnswersAreClassified(t *testing.T) {
	assert.False(t, swapOutcomeIndeterminate(&agentAnsweredError{err: errors.New("no staged account")}))
	assert.True(t, swapOutcomeIndeterminate(&agentAnsweredError{err: errors.New("x; " + accountScopeIndeterminate)}))
	assert.True(t, swapOutcomeIndeterminate(errors.New("context deadline exceeded")))
}

// The in-sandbox swap restarts the SAME agent, which the handoff validation
// refuses everywhere unless the account swap says that is what it is doing.
func TestSameAgentRestartIsAdmittedOnlyForTheAccountSwap(t *testing.T) {
	inst := &Instance{Title: "s", Program: tmux.ProgramClaude}
	require.ErrorContains(t, inst.ValidateHandoffTarget(tmux.ProgramClaude), "already running")
	inst.restartingSameAgent = true
	assert.NoError(t, inst.ValidateHandoffTarget(tmux.ProgramClaude))
	assert.NoError(t, func() error {
		inst.mu.RLock()
		defer inst.mu.RUnlock()
		return inst.validateHandoffTargetLocked(tmux.ProgramClaude, tmux.ProgramClaude)
	}(), "the record and swap boundaries read the same allowance")
	inst.restartingSameAgent = false
	require.ErrorContains(t, inst.ValidateHandoffTarget(tmux.ProgramClaude), "already running", "and it is not left open")
}

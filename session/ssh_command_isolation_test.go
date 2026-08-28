package session

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// `backend = "ssh"` reads NO ssh configuration file, and these are the tests that
// say so (#3052).
//
// The first version of the convergence let ssh_config apply. Review returned
// eight findings against that one decision — RemoteCommand, SendEnv, ProxyJump,
// ControlMaster, RequestTTY, ForwardAgent, IdentityAgent, and a host-key-name
// rewrite that broke the legacy-tombstone probe. Seven were the same class
// arriving in two batches, which is the argument against fixing them one by one:
// each pin is a claim to have enumerated a set OpenSSH keeps extending.
//
// So the guarantee under test is the REDUCTION, not any individual directive. A
// test per directive would rot the same way the pins would.

func TestSSHCommandReadsNoConfigurationFile(t *testing.T) {
	cmd := sshCmdFor(t, config.SSHConfig{Host: "h.example.com"}, config.SSHHostKeyStrict)

	assert.Contains(t, cmd, "-F none",
		"ssh_config injects BEHAVIOURS (RemoteCommand, SendEnv, ProxyJump, ForwardAgent, ControlMaster) "+
			"that no -o pin can mask; -F none is what makes the pins sufficient rather than a best-effort list")
}

// The reduction has to hold for every posture, because the postures are exactly
// what an external file must not be able to relax.
func TestSSHCommandReadsNoConfigurationFileUnderEveryPosture(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	for _, posture := range []string{
		config.SSHHostKeyStrict, config.SSHHostKeyAcceptNew, config.SSHHostKeyInsecure, "some-future-value",
	} {
		t.Run(posture, func(t *testing.T) {
			assert.Contains(t, sshCmdFor(t, config.SSHConfig{Host: "h.example.com"}, posture), "-F none")
		})
	}
}

// THE TWO ENDS OF THE IDENTITY-FILE SPLIT, asserted through the paths that
// actually run rather than through the validator alone. A check on the right
// function called from the wrong place is how this guarantee was lost once already
// (#3092): it sat inside command composition, which the teardown-restore path also
// calls, and turned a rotated key into an unreapable session.

// The REAP end. restoreRuntimeCleanup reports an unusable handle by REJECTING it,
// and instance_data.go then captures that rejection in unavailableRuntimeCleanup
// for the daemon's whole lifetime — so a key that is missing, rotated, or on a home
// directory not mounted yet must not be consulted here at all. The remote workspace
// outlives the local key file, and a rejected handle leaks it with nothing left
// pointing at it.
func TestRestoredSSHTeardownSurvivesAMissingIdentityFile(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	data := &RuntimeCleanupData{SSH: &SSHRuntimeCleanupData{
		Config: config.SSHConfig{
			Host:         "h.example.com",
			IdentityFile: filepath.Join(t.TempDir(), "key-rotated-away"),
		},
		SessionDir:          "/home/af/.af-sessions/x.AbCdEf",
		HostKeyVerification: config.SSHHostKeyStrict,
	}}

	backend, teardown, err := restoreRuntimeCleanup("missing-identity-restore", "ssh", data)
	require.NoError(t, err,
		"an ssh.identity_file that no longer exists locally must NOT make a persisted session "+
			"unreapable — refusing a teardown protects nothing and leaks the workspace")
	require.NotNil(t, teardown)
	require.NotNil(t, backend)
}

// The CREATE end, driven through Provision rather than by calling the validator:
// a validator nothing calls is precisely how this comes back. Hermetic — the
// refusal lands before any dial, so no sshd is involved.
func TestSSHProvisionRefusesAnUnreadableIdentityFile(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	repo := initTempGitRepo(t)
	missing := filepath.Join(t.TempDir(), "typo-in-the-path")
	writeInRepoConfig(t, repo, map[string]any{
		"backend": "ssh",
		"ssh":     map[string]any{"host": "build-box", "identity_file": missing},
	})

	_, err := sshRuntime{}.Provision(ProvisionSpec{RepoRoot: repo, Title: "s", CloneURL: "file:///x"})

	require.Error(t, err, "a create with an unusable ssh.identity_file must not go on to authenticate as something else")
	assert.Contains(t, err.Error(), missing,
		"the error must NAME the path — the whole failure mode is a typo nobody can see")
	assert.Contains(t, err.Error(), "identity_file",
		"and name the setting, so the operator knows which key to fix")
}

// Composition must be a pure function of its arguments. restoreRuntimeCleanup
// calls it while persisted instances are being LOADED, under a contract that I/O
// happens only inside the returned teardown closure.
func TestSSHCommandCompositionCreatesNothing(t *testing.T) {
	afHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", afHome)
	store := filepath.Join(afHome, sshKnownHostsFileName)

	_ = sshCmdFor(t, config.SSHConfig{Host: "h.example.com"}, config.SSHHostKeyAcceptNew)

	_, err := os.Stat(store)
	assert.True(t, os.IsNotExist(err), "composing a command must not touch the filesystem, got stat err %v", err)

	// …and the separate, explicit step is what creates it.
	require.NoError(t, prepareSSHHostKeyStore(config.SSHConfig{Host: "h.example.com"}, config.SSHHostKeyAcceptNew))
	_, err = os.Stat(store)
	assert.NoError(t, err, "prepareSSHHostKeyStore is what must create the accept-new store")
}

// The other two postures have no store to prepare: strict reads a file the
// operator owns, and insecure reads /dev/null. Preparing one would create a file
// af has no business creating.
func TestPrepareSSHHostKeyStoreOnlyTouchesAcceptNew(t *testing.T) {
	afHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", afHome)

	for _, posture := range []string{config.SSHHostKeyStrict, config.SSHHostKeyInsecure, ""} {
		require.NoError(t, prepareSSHHostKeyStore(config.SSHConfig{Host: "h.example.com"}, posture))
	}
	_, err := os.Stat(filepath.Join(afHome, sshKnownHostsFileName))
	assert.True(t, os.IsNotExist(err), "no posture but accept-new may create the af store")
}

// A transiently unwritable AF home must cost ONE attempt, not the record. The
// old shape prepared the store while composing the teardown, so a bad moment at
// load time captured a permanently dead closure and the remote leaked until the
// daemon restarted.
func TestRestoredSSHTeardownPreparesTheStorePerAttempt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so an unwritable AF home cannot be simulated")
	}
	afHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", afHome)
	blocked := filepath.Join(afHome, "locked")
	require.NoError(t, os.Mkdir(blocked, 0o500)) // no write bit: MkdirAll/create below fails
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(blocked, "home"))

	data := &RuntimeCleanupData{SSH: &SSHRuntimeCleanupData{
		Config:              config.SSHConfig{Host: "h.example.com"},
		SessionDir:          "/home/af/.af-sessions/x.AbCdEf",
		HostKeyVerification: config.SSHHostKeyAcceptNew,
	}}
	backend, teardown, err := restoreRuntimeCleanup("accept-new-restore", "ssh", data)
	require.NoError(t, err, "restore must SUCCEED even when the store cannot be created yet")
	require.NotNil(t, teardown)

	sb, ok := backend.(*sshBackend)
	require.True(t, ok)
	reached := 0
	sb.provisioner.runCommandFn = func(time.Duration, string, io.Reader, bool) ([]byte, error) {
		reached++
		return nil, errors.New("exit status 255")
	}

	// Attempt 1, still unwritable: the record is RETAINED, and the reap is not
	// even attempted because the store it would append to cannot exist.
	first := teardown()
	require.Error(t, first)
	assert.True(t, errors.Is(first, ErrWorkspaceStateUnknown),
		"a store that cannot be prepared says nothing about the remote; retain, got %v", first)
	assert.Equal(t, 0, reached)

	// The operator fixes the filesystem. NO daemon restart.
	require.NoError(t, os.Chmod(blocked, 0o700))

	// Attempt 2 gets past preparation and makes a real attempt — which is the
	// whole point: the closure was never permanently dead.
	second := teardown()
	require.Error(t, second, "the stub fails the remote command, so this is still an error")
	assert.Equal(t, 1, reached, "the second attempt must actually reach the transport")
	_, statErr := os.Stat(filepath.Join(blocked, "home", sshKnownHostsFileName))
	assert.NoError(t, statErr, "and the store must have been created by the attempt, not by restore")
}

// TestBackendUnusableReasonSSHRequiresTheSSHBinary — the picker is a promise. The
// in-process client needed no binary, so this check did not exist; now every step
// is an `ssh` invocation and a host without it must not be offered the backend.
func TestBackendUnusableReasonSSHRequiresTheSSHBinary(t *testing.T) {
	repo := repoWithOriginForTest(t)
	cfg := &config.ResolvedConfig{SSH: &config.SSHConfig{Host: "h.example.com"}}

	restore := SetLookPathForTest(func(name string) (string, error) {
		if name == "ssh" {
			return "", exec.ErrNotFound
		}
		return "/usr/bin/" + name, nil
	})
	err := BackendUnusableReason(BackendSSH, cfg, repo)
	restore()

	require.Error(t, err, "a daemon host with no ssh binary must not be offered backend=ssh")
	assert.Contains(t, err.Error(), "backend=ssh")
	assert.Contains(t, err.Error(), "`ssh` CLI", "the message must name the missing binary")

	// With ssh present the backend is usable again, so the check is the only thing
	// this test moved.
	restore = SetLookPathForTest(func(name string) (string, error) { return "/usr/bin/" + name, nil })
	defer restore()
	assert.NoError(t, BackendUnusableReason(BackendSSH, cfg, repo))
}

// Errors from the shared transport must name the backend the operator selected.
// Sending an `ssh.host` user to look up `sandbox_ssh` is a wrong answer, not a
// cosmetic one.
func TestSharedTransportErrorsCarryTheOwningBackend(t *testing.T) {
	sshP := newSSHSandboxProvisioner(ProvisionSpec{Title: "t"}, "ssh -F none host", "", "")
	assert.Contains(t, sshP.sandboxErr(errors.New("clone failed")).Error(), "backend=ssh:")

	sandboxP := &sandboxProvisioner{spec: ProvisionSpec{Title: "t"}, sshCmd: "ssh host"}
	assert.Contains(t, sandboxP.sandboxErr(errors.New("clone failed")).Error(), "backend=sandbox:",
		"the zero value must keep meaning sandbox, so no existing construction site changes meaning")

	assert.Contains(t, sandboxTransportConfigKey(string(BackendSandbox)), "sandbox.ssh")
	assert.Contains(t, sandboxTransportConfigKey(string(BackendSSH)), "ssh.host")
}

// THE ANSWERED-FAILURE LATCH. `rm && respond` conflates "the far side executed"
// with "and it succeeded", so a real permission or filesystem error from rm
// arrived with no proof and was read as "we never reached the sandbox" —
// retained and retried forever, for a cause that answers identically every time.
// Driven through a REAL shell, because the defect is in shell semantics.
func TestSandboxReapScriptProvesExecutionEvenWhenRemovalFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can remove a directory inside a read-only parent, so rm cannot be made to fail this way")
	}
	parent := t.TempDir()
	victim := filepath.Join(parent, "session")
	require.NoError(t, os.Mkdir(victim, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(victim, "f"), []byte("x"), 0o600))
	require.NoError(t, os.Chmod(parent, 0o500)) // rm -rf must fail with a DEFINITE status
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	p := &sandboxProvisioner{sessionDir: victim}
	script, expect := p.reapScript("a1b2c3d4")

	out, err := exec.Command("sh", "-c", script).CombinedOutput()

	require.Error(t, err, "premise: rm must fail definitively here, or this test proves nothing")
	var exitErr *exec.ExitError
	require.True(t, errors.As(err, &exitErr))
	require.NotEqual(t, sshBinaryExitAmbiguous, exitErr.ExitCode(),
		"premise: the status must be a remote answer, not ssh's own ambiguous 255")
	assert.Contains(t, string(out), expect,
		"an ANSWERED removal failure must still prove the far side ran, or reap can never latch it")
	assert.DirExists(t, victim, "premise: the directory really did survive")
}

// And the success path must keep both properties: the proof is emitted and the
// script exits 0, so a clean reap still latches as a success.
func TestSandboxReapScriptStillSucceedsCleanly(t *testing.T) {
	victim := filepath.Join(t.TempDir(), "session")
	require.NoError(t, os.Mkdir(victim, 0o700))

	p := &sandboxProvisioner{sessionDir: victim}
	script, expect := p.reapScript("a1b2c3d4")

	out, err := exec.Command("sh", "-c", script).CombinedOutput()

	require.NoError(t, err, "a clean removal must still exit 0: %s", out)
	assert.Contains(t, string(out), expect)
	assert.NoDirExists(t, victim)
	assert.False(t, strings.Contains(script, expect), "the script must never carry its own answer")
}

// repoWithOriginForTest is a real git repo carrying an `origin`, so the origin
// precondition passes and this file's assertions are about the ssh check alone.
func repoWithOriginForTest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", "https://example.invalid/repo.git"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git is unavailable for the origin fixture: %v: %s", err, out)
		}
	}
	return dir
}

//go:build !windows

package sessionenv

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/shellquote"
)

// Tagged !windows because it drives claimedExecInvocation and processExec, which
// live in exec_unix.go. The windows stub has no shim protocol at all.

// shimRun is what a claimed shim handed to exec.
type shimRun struct {
	argv    []string
	environ []string
	err     error
}

// runShim mints c on a real broker, runs the shim's claim path against it with
// argvCommand as the (ignored) trailing operand, and reports what reached exec.
// Every claim comes from THIS process, which the lookup names as the pane root.
func runShim(t *testing.T, b *CapabilityBroker, sock, token string, environmentOnly bool, argvCommand string) shimRun {
	t.Helper()
	t.Setenv("TMUX_PANE", "%1")
	accountDir := t.TempDir()
	prevLookup, prevExec := AccountLookup, processExec
	prevWrite, prevRead := paneBookkeepingWrite, paneStampRead
	AccountLookup = func(agent, name string) (Account, error) {
		return Account{Agent: agent, Name: name, Dir: accountDir}, nil
	}
	paneBookkeepingWrite = func(context.Context, string) error { return nil }
	paneStampRead = func(context.Context, string) (string, error) { return "", nil }
	var run shimRun
	sentinel := errors.New("stop before exec")
	processExec = func(_ string, argv []string, environ []string) error {
		run.argv = append([]string(nil), argv...)
		run.environ = append([]string(nil), environ...)
		return sentinel
	}
	t.Cleanup(func() {
		AccountLookup, processExec = prevLookup, prevExec
		paneBookkeepingWrite, paneStampRead = prevWrite, prevRead
	})
	err := claimedExecInvocation([]string{sock, token, argvCommand}, environmentOnly)
	if errors.Is(err, sentinel) {
		err = nil
	}
	run.err = err
	return run
}

func mintFor(t *testing.T, c Capability) (*CapabilityBroker, string, string) {
	t.Helper()
	b, sock := startBroker(t, paneOf(c.Session, os.Getpid()))
	token, err := b.Mint(c)
	require.NoError(t, err)
	return b, sock, token
}

// The shim must scope the pane from the CAPABILITY, carrying the launcher's
// declaration (the account, its exact command with a quoted path, its proof)
// intact, and the command operand must reach /bin/sh verbatim (#3083).
func TestClaimedShim_RunsTheAdmittedLaunch(t *testing.T) {
	pluginDir := "/plugins/with a space"
	trusted := "/opt/af detected/claude"
	command := shellquote.Quote(trusted) + " --session-id abc-123 --plugin-dir " + shellquote.Quote(pluginDir)
	c := Capability{
		Session: "s", Agent: "claude", Account: "work", Command: command,
		Proof: AccountLaunchProof{TrustedExecutable: trusted, GeneratedArgs: []string{"--session-id", "abc-123", "--plugin-dir", pluginDir}},
	}
	b, sock, token := mintFor(t, c)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ambient-must-not-survive")
	run := runShim(t, b, sock, token, false, command)
	require.NoError(t, run.err)
	require.Equal(t, command, run.argv[2])
	_, present := envValue(run.environ, "CLAUDE_CONFIG_DIR")
	require.True(t, present, "the account's credential root must be injected")
	_, leaked := envValue(run.environ, "ANTHROPIC_API_KEY")
	require.False(t, leaked, "the ambient key outranks the config directory and must be gone")
}

// Required by #4731: a replayed invocation is refused. The program the shim exec'd
// keeps the pane root pid, so a second claim comes from the right process and only
// the single-use record stops it.
func TestClaimedShim_ReplayIsRefused(t *testing.T) {
	c := Capability{Session: "s", Agent: "claude", Account: "work", Command: "claude"}
	b, sock, token := mintFor(t, c)
	require.NoError(t, runShim(t, b, sock, token, false, "claude").err)
	run := runShim(t, b, sock, token, false, "claude")
	require.ErrorContains(t, run.err, "already used")
	require.Nil(t, run.argv, "a replayed claim must never reach exec")
}

// Required by #4731: an environment smuggled in alongside the claim is not
// authority. LD_PRELOAD in the shim's own environment is filtered like any
// other name, and listing it in the SHIM's argv or environment as a passthrough
// has no effect because the passthrough list is the capability's.
func TestClaimedShim_LDPreloadIsRefused(t *testing.T) {
	c := Capability{Session: "s", Agent: "claude", Account: "work", Command: "claude"}
	b, sock, token := mintFor(t, c)
	t.Setenv("LD_PRELOAD", "/tmp/evil.so")
	t.Setenv("AF_SESSION_ENV_PASSTHROUGH", "LD_PRELOAD")
	run := runShim(t, b, sock, token, false, "claude")
	require.NoError(t, run.err)
	_, present := envValue(run.environ, "LD_PRELOAD")
	require.False(t, present, "LD_PRELOAD must not reach the launched program")

	// And a loader hijack written into the admitted COMMAND (an assignment prefix)
	// is refused by the account boundary, never executed under the account.
	bad := Capability{Session: "s", Agent: "claude", Account: "work", Command: "LD_PRELOAD=./steal.so claude"}
	b2, sock2, token2 := mintFor(t, bad)
	run = runShim(t, b2, sock2, token2, false, "claude")
	require.Error(t, run.err)
	require.Nil(t, run.argv)
}

// Required by #4731: the launched program is the one the launcher admitted. A
// config edit after admission (an account swap rewriting the program between
// mint and launch) changes what a re-derivation would see, but the shim never
// re-derives: it runs the frozen command, whatever its argv says.
func TestClaimedShim_AccountSwapLaunchesTheFrozenProgram(t *testing.T) {
	c := Capability{
		Session: "s", Agent: "claude", Account: "work", Command: "claude --admitted",
		Proof: AccountLaunchProof{GeneratedArgs: []string{"--admitted"}},
	}
	b, sock, token := mintFor(t, c)
	run := runShim(t, b, sock, token, false, "claude --rewritten-after-admission")
	require.NoError(t, run.err)
	require.Equal(t, "claude --admitted", run.argv[2])
}

// The shim claims for the marker it was started under: a capability minted for a
// sibling environment pane cannot be redeemed by the agent marker, or the reverse,
// and an agent capability whose command is not that agent is refused.
func TestClaimedShim_RefusesAMismatchedCapability(t *testing.T) {
	envCap := Capability{Session: "s", Agent: "claude", Account: "work", Command: "/bin/sh -i", EnvironmentOnly: true}
	b, sock, token := mintFor(t, envCap)
	run := runShim(t, b, sock, token, false, "/bin/sh -i")
	require.ErrorContains(t, run.err, "does not describe this launch")

	wrong := Capability{Session: "s", Agent: "claude", Account: "work", Command: "./not-claude"}
	b, sock, token = mintFor(t, wrong)
	run = runShim(t, b, sock, token, false, "./not-claude")
	require.ErrorContains(t, run.err, "operator-attributable")
	require.Nil(t, run.argv)
}

// A malformed argv refuses rather than guessing, and a missing claim is fatal:
// there is no fallback to the ambient identity.
func TestClaimedShim_RefusesMalformedAndUnclaimable(t *testing.T) {
	prev := claimCapability
	t.Cleanup(func() { claimCapability = prev })
	claimCapability = func(string, string) (Capability, error) {
		return Capability{}, errors.New("launch capability refused: no endpoint")
	}
	paneBookkeepingWrite = func(context.Context, string) error { return nil }
	for _, args := range [][]string{{}, {"sock"}, {"sock", "tok"}, {"sock", "tok", "cmd", "extra"}} {
		require.Error(t, claimedExecInvocation(args, false), "argv %v", args)
	}
	require.ErrorContains(t, claimedExecInvocation([]string{"sock", "tok", "claude"}, false), "no endpoint")
}

func TestWrapAccountCommand_CarriesOnlyTheClaimCoordinates(t *testing.T) {
	wrapped, err := WrapAccountCommand("/usr/local/bin/af", "/tmp/af-claim/claim.sock", "tok123", "claude --x 'a b'")
	require.NoError(t, err)
	call, ok := singleSimpleCall(wrapped)
	require.True(t, ok)
	words, ok := literalCommandArgs(call.Args)
	require.True(t, ok)
	require.Equal(t, []string{"/usr/local/bin/af", AccountExecMarker, "/tmp/af-claim/claim.sock", "tok123", "claude --x 'a b'"}, words)
	_, err = WrapAccountCommand("/usr/local/bin/af", "", "tok", "claude")
	require.Error(t, err)
	_, err = WrapAccountEnvironmentCommand("/usr/local/bin/af", "/s", "", "sh")
	require.Error(t, err)
}

// Required by #4731: an `--here` launch in a linked worktree of a bare repo
// succeeds. The launcher decided at mint whether the agent executable is
// operator-attributable, against the directory it launches in. The shim used to
// ask the same workdir-containment question again from ITS cwd, so the answer
// depended on where the pane happened to start: with the agent on a PATH entry
// beneath the shim's cwd (a bare repo checked out beside its worktrees) it was
// refused, even though the launcher had admitted it. The shim must not consult
// its cwd at all.
func TestClaimedShim_DoesNotDependOnItsWorkingDirectory(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	bare := filepath.Join(root, "proj.git")
	seed := filepath.Join(root, "seed")
	gitIn := func(dir string, args ...string) {
		t.Helper()
		c := exec.Command(git, args...)
		c.Dir = dir
		out, err := c.CombinedOutput()
		require.NoError(t, err, string(out))
	}
	gitIn(root, "init", "-q", "--bare", bare)
	gitIn(root, "init", "-q", seed)
	gitIn(seed, "-c", "user.email=a@b", "-c", "user.name=n", "commit", "-q", "--allow-empty", "-m", "x")
	gitIn(seed, "push", "-q", bare, "HEAD:refs/heads/main")
	worktree := filepath.Join(bare, "worktrees-here", "w")
	gitIn(bare, "worktree", "add", "-q", worktree, "main")
	bin := filepath.Join(bare, "bin")
	require.NoError(t, os.MkdirAll(bin, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\n"), 0o755))
	t.Setenv("PATH", bin)

	// The shape the old shim refused: its cwd is an ancestor of the PATH entry.
	require.Empty(t, GrantAgentForCommand("claude", os.Environ(), root),
		"precondition: re-deriving the grant from this cwd answers differently than the launcher's")
	require.Equal(t, "claude", GrantAgentForCommand("claude", os.Environ(), worktree))

	for _, cwd := range []string{worktree, root} {
		t.Chdir(cwd)
		c := Capability{Session: "s", Agent: "claude", Account: "work", Command: "claude"}
		b, sock, token := mintFor(t, c)
		run := runShim(t, b, sock, token, false, "claude")
		require.NoError(t, run.err, "the shim launched from %s must not be refused over its cwd", cwd)
		require.NotNil(t, run.argv)
	}
}

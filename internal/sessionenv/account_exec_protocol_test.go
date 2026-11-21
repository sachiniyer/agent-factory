//go:build !windows

package sessionenv

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// After an upgrade, a session launched by the previous af still names the old
// argv in its default-command. The new binary must refuse it as a refused
// capability, because HandleInternalExec prints the "run af daemon restart" hint
// only for that class of error (#6384).
func TestClaimedShim_OldProtocolArgvTakesTheRestartHintPath(t *testing.T) {
	paneBookkeepingWrite = func(context.Context, string) error { return nil }
	old := []string{"claude", "0", "work", "", "0", "/bin/sh -i"}
	err := claimedExecInvocation(old, true)
	require.Error(t, err)
	require.True(t, strings.HasPrefix(err.Error(), "launch capability refused"), "got %q", err)
}

// Required by #7555: the keep-if-set claim is decided at the in-pane boundary,
// where the forwarded value actually lives. A name the launch admitted in the
// capability's passthrough extras reaches the shim's filtered env and is the
// operator's claim — kept. A name the capability does not forward is filtered
// out before the scope, and applyAccount installs it defined-empty: the tmux
// -e half never stamped it, so absence here is honest.
//
// GIT_CONFIG_COUNT is whole: a forwarded count resolves to N>0 and git then
// requires GIT_CONFIG_KEY_0..N-1 / GIT_CONFIG_VALUE_0..N-1 to travel with it,
// or every git invocation fails fatally. The complete-triple forward keeps the
// count's value (it is the operator's claim); a count-only forward is
// neutralized to empty at the exec shim, keeping git usable. The prior test
// pinned the count-only forward as "the claim must reach the agent" — exactly
// the half-forward the boundary now refuses to deliver live.
func TestClaimedShim_LoaderNamesKeepIfSetAtThePane(t *testing.T) {
	// Complete forward: the count and its pairs are all the operator's claim
	// and must reach the agent verbatim.
	complete := Capability{
		Session: "s", Agent: "gemini", Account: "work", Command: "gemini",
		Extras: []string{"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0"},
	}
	b, sock, token := mintFor(t, complete)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "safe.directory")
	t.Setenv("GIT_CONFIG_VALUE_0", "/work")
	t.Setenv("CLOUD_SHELL", "repo-or-ambient-value")
	run := runShim(t, b, sock, token, false, "gemini")
	require.NoError(t, run.err)
	if v, ok := envValue(run.environ, "GIT_CONFIG_COUNT"); !ok || v != "1" {
		t.Errorf("GIT_CONFIG_COUNT = %q (present %v): the forwarded value is the operator's claim and must reach the agent", v, ok)
	}
	if v, ok := envValue(run.environ, "GIT_CONFIG_KEY_0"); !ok || v != "safe.directory" {
		t.Errorf("GIT_CONFIG_KEY_0 = %q (present %v): the forwarded pair rides the claim unexamined", v, ok)
	}
	if v, ok := envValue(run.environ, "GIT_CONFIG_VALUE_0"); !ok || v != "/work" {
		t.Errorf("GIT_CONFIG_VALUE_0 = %q (present %v): the forwarded pair rides the claim unexamined", v, ok)
	}
	if v, ok := envValue(run.environ, "CLOUD_SHELL"); !ok || v != "" {
		t.Errorf("CLOUD_SHELL = %q (present %v): an ambient value on an unforwarded name is not a claim — pinned empty", v, ok)
	}

	// Incomplete forward: GIT_CONFIG_COUNT forwarded and set to N>0 WITHOUT
	// its pairs. Delivering the live count would make git fail fatally, so
	// the exec shim neutralizes it to empty (the same shape an unforwarded
	// count takes) and writes the operator-facing diagnostic.
	b2, sock2, token2 := mintFor(t, Capability{
		Session: "s", Agent: "gemini", Account: "work", Command: "gemini",
		Extras: []string{"GIT_CONFIG_COUNT"},
	})
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "")
	t.Setenv("GIT_CONFIG_VALUE_0", "")
	run = runShim(t, b2, sock2, token2, false, "gemini")
	require.NoError(t, run.err)
	if v, ok := envValue(run.environ, "GIT_CONFIG_COUNT"); !ok || v != "" {
		t.Errorf("GIT_CONFIG_COUNT = %q (present %v): a count forwarded without its pairs is neutralized empty so git does not fail", v, ok)
	}

	// The same ambient value, with the name absent from the capability's
	// extras: the claim channel never carried it, so the pin answers instead.
	b3, sock3, token3 := mintFor(t, Capability{Session: "s", Agent: "gemini", Account: "work", Command: "gemini"})
	t.Setenv("GIT_CONFIG_COUNT", "1")
	run = runShim(t, b3, sock3, token3, false, "gemini")
	require.NoError(t, run.err)
	if v, ok := envValue(run.environ, "GIT_CONFIG_COUNT"); !ok || v != "" {
		t.Errorf("GIT_CONFIG_COUNT = %q (present %v): unforwarded, the ambient value must be pinned empty — presence here is the claim", v, ok)
	}
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

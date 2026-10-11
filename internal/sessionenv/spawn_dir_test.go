//go:build !windows

package sessionenv

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/shellquote"

	"github.com/stretchr/testify/require"
)

// TestSpawnDirPrefix pins the launch-line contract: the directory travels as a
// leading NAME=value assignment — the one channel every supported tmux
// carries — resolved by the daemon's cwd (filepath.Abs) so the pane-side
// shim's chdir names the directory af admitted, not whatever the pane's
// fallback cwd would resolve a relative path against (#5174 review).
func TestSpawnDirPrefix(t *testing.T) {
	require.Empty(t, SpawnDirPrefix(""), "no directory, no assignment")

	prefix := SpawnDirPrefix("rel dir")
	abs, err := filepath.Abs("rel dir")
	require.NoError(t, err)
	require.Equal(t, SpawnDirEnv+"="+shellquote.Quote(abs)+" ", prefix,
		"absolute, shell-quoted, trailing-space-separated")
}

// TestEnterSpawnDirChdir binds the shim's behavior to the variable: unset —
// a launcher that predates the prefix — does nothing; set — the pane must be
// moved into exactly that directory before the command execs; a chdir failure
// is reported so the caller can stamp the refusal verdict.
func TestEnterSpawnDirChdir(t *testing.T) {
	require.NoError(t, os.Unsetenv(SpawnDirEnv))
	require.NoError(t, enterSpawnDir(), "an unset SpawnDirEnv enforces nothing")

	want := t.TempDir()
	t.Setenv(SpawnDirEnv, want)

	var got string
	prev := spawnDirChdir
	spawnDirChdir = func(dir string) error {
		got = dir
		return nil
	}
	t.Cleanup(func() { spawnDirChdir = prev })

	require.NoError(t, enterSpawnDir())
	require.Equal(t, want, got, "the shim chdirs to the recorded directory")

	failure := errors.New("permission denied")
	spawnDirChdir = func(string) error { return failure }
	require.ErrorIs(t, enterSpawnDir(), failure,
		"a failed chdir reports — the caller exits with the refusal verdict")
}

// TestExecInvocationStripsSpawnDirEnv pins the doc-comment contract on
// SpawnDirEnv: the name rides the launch line so the SHIM can enforce
// placement, but it is an af-owned marker and must never reach the exec'd
// agent's environment. Driven through the processExec seam with the variable
// live in os.Environ across the three launch shapes — a supported agent, an
// unattributed command, and an account-scoped launch — because each takes a
// different filtering path and the name must survive none of them.
func TestExecInvocationStripsSpawnDirEnv(t *testing.T) {
	sentinel := errors.New("stop before exec")
	spawnDir := t.TempDir()
	t.Setenv(SpawnDirEnv, spawnDir)

	var chdirTo string
	var gotEnviron []string
	prevChdir, prevExec := spawnDirChdir, processExec
	spawnDirChdir = func(dir string) error {
		chdirTo = dir
		return nil
	}
	processExec = func(_ string, _ []string, environ []string) error {
		gotEnviron = append([]string(nil), environ...)
		return sentinel
	}
	accountDir := t.TempDir()
	prevLookup := AccountLookup
	AccountLookup = func(agent, name string) (Account, error) {
		return Account{Agent: agent, Name: name, Dir: accountDir}, nil
	}
	t.Cleanup(func() { spawnDirChdir, processExec, AccountLookup = prevChdir, prevExec, prevLookup })

	// An account-scoped launch through the real wrapper: the proof declaration
	// must survive argv the same way production builds it, or ApplyAccount
	// refuses and the environ assertion never runs.
	trustedExecutable := "/opt/af detected/claude"
	scopedCommand := shellquote.Quote(trustedExecutable) + " --session-id abc-123"
	wrapped, err := WrapAccountCommand("/usr/local/bin/af", "claude", "work",
		AccountLaunchProof{TrustedExecutable: trustedExecutable, GeneratedArgs: []string{"--session-id", "abc-123"}},
		nil, scopedCommand)
	require.NoError(t, err)
	call, ok := singleSimpleCall(wrapped)
	require.True(t, ok, "the wrapped launch must be a single simple command")
	scopedWords, ok := literalCommandArgs(call.Args)
	require.True(t, ok, "every scoped word must survive quoting as a literal")

	for _, tc := range []struct {
		name   string
		args   []string
		scoped bool
	}{
		{"supported agent", []string{"claude", "0", "claude"}, false},
		{"unattributed command", []string{"codex", "0", "echo not-an-agent"}, false},
		{"account-scoped launch", scopedWords[2:], true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chdirTo, gotEnviron = "", nil
			err := execInvocation(tc.args, tc.scoped)
			require.ErrorIs(t, err, sentinel,
				"reaching the exec seam proves the launch was not refused")
			require.Equal(t, spawnDir, chdirTo,
				"the shim must have consumed AF_SPAWN_DIR itself before exec")
			_, leaked := envValue(gotEnviron, SpawnDirEnv)
			require.False(t, leaked,
				"AF_SPAWN_DIR reached the exec'd environment — it names an af-owned marker the agent must never see")
		})
	}
}

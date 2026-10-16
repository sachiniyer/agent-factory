//go:build !windows

package sessionenv

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/shellquote"

	"github.com/stretchr/testify/require"
)

// TestSpawnDirPrefix pins the launch-line contract: the directory travels as a
// leading NAME=value assignment — the one channel every supported tmux
// carries — resolved by the daemon's cwd (filepath.Abs) so the pane-side
// shim's chdir names the directory af admitted, not whatever the pane's
// fallback cwd would resolve a relative path against; and the admitted
// FileInfo's inode pair rides beside it so the shim can prove the pathname
// landed on the admitted directory, not a replacement (#5174 review).
func TestSpawnDirPrefix(t *testing.T) {
	require.Empty(t, SpawnDirPrefix("", nil), "no directory, no assignment")

	dir := t.TempDir()
	info, err := os.Stat(dir)
	require.NoError(t, err)
	prefix := SpawnDirPrefix(dir, info)
	abs, err := filepath.Abs(dir)
	require.NoError(t, err)
	dev, ino, ok := spawnDirInodeID(info)
	require.True(t, ok, "a real directory must expose its inode pair")
	require.Equal(t,
		SpawnDirEnv+"="+shellquote.Quote(abs)+" "+SpawnDirInodeEnv+"="+
			strconv.FormatUint(dev, 10)+":"+strconv.FormatUint(ino, 10)+" ",
		prefix, "absolute dir, shell-quoted, inode pair, trailing-space-separated")

	// A FileInfo carrying no inode pair — every non-unix Sys() shape — emits
	// only the directory assignment: the shim falls back to chdir-only
	// enforcement, exactly like a launcher that predates the pair.
	require.Equal(t, SpawnDirEnv+"="+shellquote.Quote(abs)+" ",
		SpawnDirPrefix(dir, nil), "no inode pair, no second assignment")
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

// TestEnterSpawnDirVerifiesInode binds the second half of the pane-side
// enforcement: the chdir landing on the recorded PATHNAME is not enough —
// between the daemon's admission stat and the shim's chdir a rename-and-
// replace or a retargeted worktree symlink can make that path resolve to a
// directory af never approved. When the launch line carries the admitted
// inode pair the shim must prove the entered directory IS that inode before
// exec, refuse on a mismatch, a malformed claim, or an undeterminable
// identity — and skip the check only when the pair is absent, the
// old-launcher compatibility case (#5174 review).
func TestEnterSpawnDirVerifiesInode(t *testing.T) {
	want := t.TempDir()
	info, err := os.Stat(want)
	require.NoError(t, err)
	dev, ino, ok := spawnDirInodeID(info)
	require.True(t, ok)
	t.Setenv(SpawnDirEnv, want)

	prevChdir, prevStat := spawnDirChdir, spawnDirStat
	spawnDirChdir = func(string) error { return nil }
	spawnDirStat = func(string) (os.FileInfo, error) { return info, nil }
	t.Cleanup(func() { spawnDirChdir, spawnDirStat = prevChdir, prevStat })

	pair := strconv.FormatUint(dev, 10) + ":" + strconv.FormatUint(ino, 10)

	require.NoError(t, os.Unsetenv(SpawnDirInodeEnv))
	require.NoError(t, enterSpawnDir(),
		"an unset inode pair is an old launcher — chdir-only enforcement")

	t.Setenv(SpawnDirInodeEnv, pair)
	require.NoError(t, enterSpawnDir(), "the admitted inode accepts")

	// A different directory the path now resolves to: SameFile's two
	// coordinates differ, either coordinate convicts.
	other := t.TempDir()
	otherInfo, err := os.Stat(other)
	require.NoError(t, err)
	spawnDirStat = func(string) (os.FileInfo, error) { return otherInfo, nil }
	require.Error(t, enterSpawnDir(),
		"a chdir that landed on a different inode refuses before exec")

	// Restore the admitted answer for the remaining malformed/unverifiable
	// claims — each must refuse the same way.
	spawnDirStat = func(string) (os.FileInfo, error) { return info, nil }
	for _, malformed := range []string{"abc", "1", "1:2:3", "1:x"} {
		t.Setenv(SpawnDirInodeEnv, malformed)
		require.Error(t, enterSpawnDir(),
			"a malformed identity claim refuses rather than skip its own check: %q", malformed)
	}

	t.Setenv(SpawnDirInodeEnv, pair)
	statErr := errors.New("i/o error")
	spawnDirStat = func(string) (os.FileInfo, error) { return nil, statErr }
	err = enterSpawnDir()
	require.Error(t, err, "an unverifiable entered directory refuses")
	require.ErrorIs(t, err, statErr)
}

// TestExecInvocationStripsSpawnDirEnv pins the doc-comment contract on
// SpawnDirEnv: the name rides the launch line so the SHIM can enforce
// placement, but it is an af-owned marker and must never reach the exec'd
// agent's environment. Driven through the processExec seam with the variable
// live in os.Environ across the three launch shapes — a supported agent, an
// unattributed command, and an account-scoped launch — because each takes a
// different filtering path and the name must survive none of them. The
// admitted-inode pair rides beside it under the same rule.
func TestExecInvocationStripsSpawnDirEnv(t *testing.T) {
	sentinel := errors.New("stop before exec")
	spawnDir := t.TempDir()
	info, err := os.Stat(spawnDir)
	require.NoError(t, err)
	dev, ino, ok := spawnDirInodeID(info)
	require.True(t, ok)
	t.Setenv(SpawnDirEnv, spawnDir)
	t.Setenv(SpawnDirInodeEnv, strconv.FormatUint(dev, 10)+":"+strconv.FormatUint(ino, 10))

	var chdirTo string
	var gotEnviron []string
	prevChdir, prevStat, prevExec := spawnDirChdir, spawnDirStat, processExec
	spawnDirChdir = func(dir string) error {
		chdirTo = dir
		return nil
	}
	spawnDirStat = func(string) (os.FileInfo, error) { return info, nil }
	processExec = func(_ string, _ []string, environ []string) error {
		gotEnviron = append([]string(nil), environ...)
		return sentinel
	}
	accountDir := t.TempDir()
	prevLookup := AccountLookup
	AccountLookup = func(agent, name string) (Account, error) {
		return Account{Agent: agent, Name: name, Dir: accountDir}, nil
	}
	t.Cleanup(func() {
		spawnDirChdir, spawnDirStat, processExec, AccountLookup = prevChdir, prevStat, prevExec, prevLookup
	})

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
			for _, name := range []string{SpawnDirEnv, SpawnDirInodeEnv} {
				_, leaked := envValue(gotEnviron, name)
				require.False(t, leaked,
					"%s reached the exec'd environment — it names an af-owned marker the agent must never see", name)
			}
		})
	}
}

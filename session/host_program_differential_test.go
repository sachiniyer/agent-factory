package session

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The property remote resolution has to hold (#5780–#5783): it picks exactly
// the executable the remote shell would execute, as the session's agent user.
// The Go PATH walk is only trustworthy if it is checked against a real shell,
// so every case here runs the command twice under a real `sh -c` — as written,
// and as resolved — in a fixture tree built from the shapes that broke the
// earlier models, and the two must run the same file. Each fixture executable
// prints its own identity, so "same file" is observable rather than inferred.
func TestResolveProgramOnHostMatchesARealShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	sh, err := exec.LookPath("sh")
	require.NoError(t, err)

	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	// Every file prints its own name; a deliberate decoy sits in each place a
	// wrong model would look.
	file := func(rel string, mode os.FileMode) string {
		path := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\necho "+rel+"\n"), mode))
		require.NoError(t, os.Chmod(path, mode))
		return path
	}
	file("plain/bin/claude", 0o755)
	file("first/claude", 0o644)     // no exec bit at all
	file("second/claude", 0o755)    // the usable later entry
	file("groupx/claude", 0o010)    // exec bit for a group this user is not in
	file("real/bin/claude", 0o755)  // reached through a symlinked parent
	file("up/claude", 0o755)        // what "link/../claude" means physically
	file("other/claude", 0o755)     // what "link/../claude" means lexically
	file("env/bin/claude", 0o755)   // an env-assigned PATH target
	file("space dir/claude", 0o755) // a path that needs quoting
	require.NoError(t, os.MkdirAll(filepath.Join(root, "adir", "claude"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join(root, "real", "bin"), filepath.Join(root, "link")))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "dead"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join(root, "nowhere"), filepath.Join(root, "dead", "claude")))
	// "link/.." is real/, not the lexical root; put the decoy where Clean looks.
	file("real/claude", 0o755)

	p := func(rel string) string { return filepath.Join(root, rel) }
	envDir := realEnvDir(t, root)
	cases := []struct{ name, program, path string }{
		{"plain lookup", "claude", p("plain/bin")},
		{"arguments kept", "claude --x --y", p("plain/bin")},
		{"non-executable entry is skipped, not pinned (#5782)", "claude", p("first") + ":" + p("second")},
		{"foreign exec bit is skipped (#5782)", "claude", p("groupx") + ":" + p("second")},
		{"directory of that name is skipped", "claude", p("adir") + ":" + p("second")},
		{"dangling symlink is skipped", "claude", p("dead") + ":" + p("second")},
		{"symlinked entry resolves through the link (#5783)", "claude", p("link")},
		{"dotdot after a symlink is physical (#5783)", "claude", p("link") + "/.."},
		{"trailing slash entry", "claude", p("plain/bin") + "/"},
		{"name with a space in the dir", "claude", p("space dir")},
		{"command PATH assignment", "PATH=" + p("env/bin") + " claude", p("plain/bin")},
		{"quoted PATH assignment", `PATH="` + p("space dir") + `" claude`, p("plain/bin")},
		{"env PATH assignment", "env PATH=" + p("env/bin") + " claude", p("plain/bin")},
		{"exec builtin", "exec claude", p("plain/bin")},
		{"quoted env assignment before the program (#5780)", `env "VAR=$HOME/x" claude`, p("plain/bin")},
		{"quoted env assignment, PATH after", `env "VAR=$HOME/x" PATH=` + p("env/bin") + ` claude`, p("plain/bin")},
		{"computed PATH with an explicit executable (#5781)", `PATH=$HOME/bin:$PATH ` + p("plain/bin/claude"), p("second")},
		{"computed PATH, quoted env, explicit executable", `env "PATH=$X" ` + p("plain/bin/claude"), p("second")},
		{"relative entry first leaves it to the shell", "claude", ".:" + p("plain/bin")},
		{"empty entry first leaves it to the shell", "claude", ":" + p("plain/bin")},
	}
	for _, tc := range cases {
		// A real env sits last on every PATH, so env-wrapper cases execute
		// instead of agreeing on "env: not found" (#5873). It holds no
		// `claude`, so it can never be the answer to a lookup.
		tc.path += ":" + envDir
		t.Run(tc.name, func(t *testing.T) {
			// The shell's cwd holds a decoy `claude` so a wrongly cwd-relative
			// answer would show; the pane's cwd is the worktree.
			cwd := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(cwd, "claude"), []byte("#!/bin/sh\necho CWD\n"), 0o755))
			// The nonexistent-cwd entries above make the decoy the shell's
			// answer for ".": that case is verbatim and still equal.
			prober := hostProgramProber{
				shell: sh, host: "fixture", processPATH: "",
				run: func(_ context.Context, _ string, argv []string, _ string) ([]byte, error) {
					return []byte("\n" + argv[len(argv)-1] + "\n" + tc.path + "\n"), nil
				},
				canExecute: canExecuteFile,
				nonce:      func() string { return "N" },
			}
			resolved, adopted, err := resolveProgramOnHost(context.Background(), tc.program, prober)
			require.NoError(t, err)

			want, err := runOnShell(sh, cwd, root, tc.program, tc.path)
			require.NoError(t, err, "the oracle shell failed: %s", want)
			// A fixture executable always prints its own identity; anything else
			// (a diagnostic, nothing) means the command never ran an agent.
			require.Regexp(t, `(^|/)claude$|^CWD$`, want, "the oracle must have executed an agent, not just produced output")
			got, err := runOnShell(sh, cwd, root, resolved, adopted)
			require.NoError(t, err, "the resolved command failed: %s", got)
			assert.Equal(t, want, got, "resolved %q", resolved)
		})
	}
}

// Anything the shell cannot run is a not-found here too, never a guess.
func TestResolveProgramOnHostRefusesWhatNoShellCouldRun(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "claude"), []byte("#!/bin/sh\n"), 0o644))
	prober := loginPATHProber("", root)
	_, _, err := resolveProgramOnHost(context.Background(), "claude", prober)
	require.Error(t, err)
}

// runOnShell runs command under a real sh with only PATH and HOME set, and
// returns its trimmed combined output. A failing command is an error: the
// oracle may not treat a diagnostic as an answer (#5873).
func runOnShell(sh, cwd, home, command, pathEnv string) (string, error) {
	cmd := exec.Command(sh, "-c", command)
	cmd.Dir = cwd
	cmd.Env = []string{"PATH=" + pathEnv, "HOME=" + home}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// realEnvDir is a directory whose only entry is the host's real env.
func realEnvDir(t *testing.T, root string) string {
	t.Helper()
	env, err := exec.LookPath("env")
	require.NoError(t, err, "the differential oracle needs a real env")
	dir := filepath.Join(root, "realenv")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.Symlink(env, filepath.Join(dir, "env")))
	return dir
}

// The control for the env cases (#5873): without env on PATH the same command
// must fail on the shell, and with it must run. If the fixture ever stops
// providing env the table above would agree on a diagnostic; this fails first.
func TestDifferentialOracleActuallyExecutesEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	sh, err := exec.LookPath("sh")
	require.NoError(t, err)
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	bin := filepath.Join(root, "bin")
	require.NoError(t, os.MkdirAll(bin, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\necho bin/claude\n"), 0o755))
	envDir := realEnvDir(t, root)

	out, err := runOnShell(sh, root, root, "env V=1 claude", bin)
	require.Error(t, err, "env must be absent from this PATH: %s", out)
	assert.Contains(t, out, "not found")
	out, err = runOnShell(sh, root, root, "env V=1 claude", bin+":"+envDir)
	require.NoError(t, err, out)
	assert.Equal(t, "bin/claude", out)
}

// A quoted prefix followed by an unquoted expansion is field-split by the shell
// (#5872): X='v PATH=<dir>' gives env a second, real PATH assignment, so the
// agent that runs is not the one on the inherited PATH. The resolver must
// refuse rather than pin the wrong one.
func TestResolveProgramOnHostRefusesFieldSplitEnvAssignments(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	sh, err := exec.LookPath("sh")
	require.NoError(t, err)
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	for _, rel := range []string{"inherited", "smuggled"} {
		dir := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\necho "+rel+"\n"), 0o755))
	}
	pathEnv := filepath.Join(root, "inherited") + ":" + realEnvDir(t, root)

	for _, program := range []string{`env "VAR="$X claude`, `env VAR=$X claude`, `env VAR=${X} claude`, `env VAR=$(echo v) claude`} {
		t.Run(program, func(t *testing.T) {
			// Control: the real shell does run the smuggled agent.
			cmd := exec.Command(sh, "-c", program)
			cmd.Env = []string{"PATH=" + pathEnv, "HOME=" + root, "X=v PATH=" + filepath.Join(root, "smuggled")}
			if strings.Contains(program, "echo") {
				cmd.Env[2] = "X=unused"
			} else {
				out, err := cmd.CombinedOutput()
				require.NoError(t, err, string(out))
				require.Equal(t, "smuggled", strings.TrimSpace(string(out)), "the control must show the smuggled PATH winning")
			}
			prober := hostProgramProber{
				shell: sh, host: "fixture",
				run: func(_ context.Context, _ string, argv []string, _ string) ([]byte, error) {
					return []byte("\n" + argv[len(argv)-1] + "\n" + pathEnv + "\n"), nil
				},
				canExecute: canExecuteFile,
				nonce:      func() string { return "N" },
			}
			_, _, err := resolveProgramOnHost(context.Background(), program, prober)
			require.Error(t, err, "a field-splittable env operand must be refused")
		})
	}
}

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
		t.Run(tc.name, func(t *testing.T) {
			// The shell's cwd holds a decoy `claude` so a wrongly cwd-relative
			// answer would show; the pane's cwd is the worktree.
			cwd := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(cwd, "claude"), []byte("#!/bin/sh\necho CWD\n"), 0o755))
			// The nonexistent-cwd entries above make the decoy the shell's
			// answer for ".": that case is verbatim and still equal.
			prober := hostProgramProber{
				shell: sh, host: "fixture", processPATH: "",
				run: func(_ context.Context, argv []string) ([]byte, error) {
					return []byte("\n" + argv[5] + "\n" + tc.path + "\n"), nil
				},
				canExecute: canExecuteFile,
				nonce:      func() string { return "N" },
			}
			resolved, adopted, err := resolveProgramOnHost(context.Background(), tc.program, prober)
			require.NoError(t, err)

			run := func(program, pathEnv string) string {
				cmd := exec.Command(sh, "-c", program)
				cmd.Dir = cwd
				cmd.Env = []string{"PATH=" + pathEnv, "HOME=" + root}
				out, _ := cmd.CombinedOutput()
				return strings.TrimSpace(string(out))
			}
			want := run(tc.program, tc.path)
			got := run(resolved, adopted)
			require.NotEmpty(t, want, "the oracle shell ran nothing — a broken fixture, not a resolver result")
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

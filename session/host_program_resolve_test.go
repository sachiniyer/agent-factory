package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// probeReply renders what the real probe prints: whatever the login profile
// wrote, then the nonce fence, then the login PATH. The nonce is always the
// trailing argv element across all shell families (POSIX/fish keep an af-probe
// $0 placeholder; csh/tcsh do not), so it is read from the last slot.
func probeReply(argv []string, profile, loginPATH string) []byte {
	return []byte(profile + "\n" + argv[len(argv)-1] + "\n" + loginPATH + "\n")
}

// executableFile makes a real executable under dir and returns its path.
func executableFile(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755))
	return path
}

// loginPATHProber answers the PATH probe with loginPATH after profile noise.
func loginPATHProber(profile, loginPATH string) hostProgramProber {
	return newTestProber(func(_ context.Context, _ string, argv []string, _ string) ([]byte, error) {
		return probeReply(argv, profile, loginPATH), nil
	})
}

func newTestProber(run hostProgramRunner) hostProgramProber {
	return hostProgramProber{
		shell:       "/bin/sh",
		host:        "sandbox.example",
		processPATH: "/spawn/bin",
		run:         run,
		canExecute:  canExecuteFile,
		nonce:       func() string { return "NONCE" },
	}
}

// The four merged-#5578 findings (#5725–#5728) and the plain command, in one
// table: the executable word comes from the shared command reader, the lookup
// honours the command's own PATH, profile noise never becomes the answer, and
// only the executable word is rewritten. The argument cases are #5767: what
// follows the executable may be computed without losing the resolution.
func TestResolveProgramOnHostTable(t *testing.T) {
	dir := t.TempDir()
	loginDir := filepath.Join(dir, "login")
	customDir := filepath.Join(dir, "custom")
	login := executableFile(t, loginDir, "claude")
	custom := executableFile(t, customDir, "claude")
	codex := executableFile(t, loginDir, "codex")

	for _, tc := range []struct {
		name        string
		program     string
		profile     string // login-profile output printed before the probe
		wantCommand string
	}{
		{name: "plain command", program: "claude --model opus", wantCommand: login + " --model opus"},
		{name: "#5725 exec prefix resolves the agent, not exec", program: "exec claude --x", wantCommand: "exec " + login + " --x"},
		{
			name: "#5726 command-local PATH is the lookup path", program: "PATH=" + customDir + " claude",
			wantCommand: "PATH=" + customDir + " " + custom,
		},
		{
			name: "#5726 env PATH is the lookup path", program: "env PATH=" + customDir + " claude -p",
			wantCommand: "env PATH=" + customDir + " " + custom + " -p",
		},
		{
			name: "#5727 login profile output is not the answer", program: "codex -c x=1",
			profile: "/home/operator\nwelcome back", wantCommand: codex + " -c x=1",
		},
		{
			name: "#5728 env wrapper and assignments survive", program: "env CLAUDE_CONFIG_DIR=/opt/profile claude --dangerously-skip-permissions",
			wantCommand: "env CLAUDE_CONFIG_DIR=/opt/profile " + login + " --dangerously-skip-permissions",
		},
		{name: "assignments and exec together", program: "FOO=1 exec claude", wantCommand: "FOO=1 exec " + login},
		{
			name: "#5767 computed argument", program: `claude --add-dir "$HOME/project"`,
			wantCommand: login + ` --add-dir "$HOME/project"`,
		},
		{
			name: "#5767 command substitution argument", program: "exec claude --append-system-prompt \"$(cat /etc/motd)\"",
			wantCommand: "exec " + login + " --append-system-prompt \"$(cat /etc/motd)\"",
		},
		{
			name: "#5767 computed env assignment value", program: `env CLAUDE_CONFIG_DIR="$HOME/.claude" claude --x`,
			wantCommand: `env CLAUDE_CONFIG_DIR="$HOME/.claude" ` + login + " --x",
		},
		{
			name: "#5767 computed non-PATH assignment", program: `FOO="$HOME" claude`,
			wantCommand: `FOO="$HOME" ` + login,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var argvs [][]string
			prober := newTestProber(func(_ context.Context, _ string, argv []string, _ string) ([]byte, error) {
				argvs = append(argvs, append([]string(nil), argv...))
				return probeReply(argv, tc.profile, loginDir), nil
			})
			command, path, err := resolveProgramOnHost(context.Background(), tc.program, prober)
			require.NoError(t, err)
			assert.Equal(t, tc.wantCommand, command)
			assert.Equal(t, loginDir+":/spawn/bin", path)
			for _, argv := range argvs {
				assert.Equal(t, hostProbePathOnly, argv[3])
				assert.Len(t, argv, 6, "the only thing asked of the shell is its PATH — never the program name")
			}
		})
	}
}

// The pane's sh finds files, not shell functions: a login profile that defines
// a `claude` function (or prints anything else) cannot change the answer
// (#5768). Real shell, real function, env-wrapped PATH.
func TestResolveProgramOnHostIgnoresShellFunctions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := executableFile(t, filepath.Join(home, "opt", "agent", "bin"), "claude")
	require.NoError(t, os.WriteFile(filepath.Join(home, ".profile"),
		[]byte("claude() { echo shadowed; }\nexport -f claude 2>/dev/null\n"), 0o644))

	prober := newTestProber(execProbeRunner)
	prober.nonce = randomProbeNonce
	prober.processPATH = "/usr/bin:/bin"

	program := "env PATH=" + filepath.Dir(bin) + " claude --x"
	command, _, err := resolveProgramOnHost(context.Background(), program, prober)
	require.NoError(t, err)
	assert.Equal(t, "env PATH="+filepath.Dir(bin)+" "+bin+" --x", command)
}

// A computed executable word or PATH is refused with an actionable error — it
// is never sent on as a daemon-local path (#5767). A computed word that still
// contains a slash is a path, which runs as written.
func TestResolveProgramOnHostComputedExecutable(t *testing.T) {
	for _, program := range []string{
		"$AGENT --x",
		`"$(command -v claude)" --x`,
		"exec $AGENT",
		"env FOO=1 $AGENT",
		`env -u "$NAME" claude`,
	} {
		t.Run(program, func(t *testing.T) {
			prober := newTestProber(func(context.Context, string, []string, string) ([]byte, error) {
				t.Fatal("a refused command must not probe")
				return nil, nil
			})
			_, _, err := resolveProgramOnHost(context.Background(), program, prober)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "literal name or an absolute path")
			assert.Contains(t, err.Error(), "sandbox.example")
		})
	}

	// A computed PATH is refused only for a bare name it would resolve; a
	// slash word never consults PATH (#5781).
	for _, program := range []string{`PATH="$HOME/bin" claude`, `env PATH="$HOME/bin" claude`, `env "PATH=$HOME/bin" claude`} {
		_, _, err := resolveProgramOnHost(context.Background(), program, loginPATHProber("", "/login/bin"))
		require.Error(t, err, program)
		assert.Contains(t, err.Error(), "computes its PATH at run time", program)
	}
	for _, program := range []string{`PATH=$HOME/bin:$PATH /abs/claude --x`, `env "PATH=$X" /abs/claude`} {
		command, _, err := resolveProgramOnHost(context.Background(), program, loginPATHProber("", "/login/bin"))
		require.NoError(t, err, program)
		assert.Equal(t, program, command)
	}

	for _, program := range []string{`"$HOME/bin/claude" --x`, `exec $HOME/.local/bin/claude`} {
		t.Run("path-shaped "+program, func(t *testing.T) {
			command, _, err := resolveProgramOnHost(context.Background(), program, loginPATHProber("", "/login/bin"))
			require.NoError(t, err)
			assert.Equal(t, program, command)
		})
	}
}

// Whatever is not exactly an absolute path to an executable regular file on
// the searched PATH is a failed resolution — never repaired into a guess — and
// a command that names its own PATH never falls back to the process PATH.
func TestResolveProgramOnHostRejectsAnythingButAnExecutableFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "plain"), []byte("x"), 0o644))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "adir"), 0o755))

	for _, name := range []string{"plain", "adir", "gone"} {
		t.Run(name, func(t *testing.T) {
			_, _, err := resolveProgramOnHost(context.Background(), name, loginPATHProber("/home/operator", dir))
			require.Error(t, err)
			assert.Contains(t, err.Error(), `"`+name+`"`)
		})
	}

	t.Run("a relative PATH entry leaves the lookup to the shell", func(t *testing.T) {
		executableFile(t, dir, "claude")
		for _, program := range []string{"PATH=rel:" + dir + " claude", "PATH=:" + dir + " claude"} {
			command, _, err := resolveProgramOnHost(context.Background(), program, loginPATHProber("", dir))
			require.NoError(t, err, program)
			assert.Equal(t, program, command, "a cwd-relative entry precedes the absolute one")
		}
	})

	t.Run("own PATH never falls back to the process PATH", func(t *testing.T) {
		good := executableFile(t, dir, "claude")
		prober := loginPATHProber("", dir)
		prober.processPATH = filepath.Dir(good)
		_, _, err := resolveProgramOnHost(context.Background(), "PATH=/opt/nowhere claude", prober)
		require.Error(t, err, "claude exists on the login and process PATH, but the command named a different PATH")
		assert.Contains(t, err.Error(), "/opt/nowhere")
	})
}

// A bare name found only on the process PATH (a docker image's ENV PATH) still
// resolves, and must be a real executable.
func TestResolveProgramOnHostProcessPATHFallback(t *testing.T) {
	bin := executableFile(t, t.TempDir(), "my-agent")
	prober := loginPATHProber("", "/login/bin")
	prober.processPATH = filepath.Dir(bin)
	command, path, err := resolveProgramOnHost(context.Background(), "my-agent", prober)
	require.NoError(t, err)
	assert.Equal(t, bin, command)
	assert.Equal(t, "/login/bin:"+filepath.Dir(bin), path)
}

// Not found names the program, the host and the PATH searched.
func TestResolveProgramOnHostNotFoundNamesProgramPathAndHost(t *testing.T) {
	prober := loginPATHProber("", "/login/a:/login/b")
	prober.host = "sandbox-42.example"
	_, _, err := resolveProgramOnHost(context.Background(), "my-agent", prober)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"my-agent"`)
	assert.Contains(t, err.Error(), "sandbox-42.example")
	assert.Contains(t, err.Error(), "/login/a:/login/b")
}

// Commands af cannot resolve honestly run as written; none of them is searched.
func TestResolveProgramOnHostRunsVerbatimWhatItCannotResolve(t *testing.T) {
	for _, program := range []string{
		"/opt/company/agent --x",   // path-qualified: the operator's explicit choice
		"env -i claude",            // environment cleared: lookup unknowable
		"env -u PATH claude",       // PATH removed
		"claude | tee /tmp/log",    // not one simple command
		"FOO=1 /opt/company/agent", // assignment before an explicit path
	} {
		t.Run(program, func(t *testing.T) {
			command, path, err := resolveProgramOnHost(context.Background(), program, loginPATHProber("", "/login/bin"))
			require.NoError(t, err)
			assert.Equal(t, program, command)
			assert.Equal(t, "/login/bin:/spawn/bin", path)
		})
	}
}

// A command with no executable word fails like an empty program.
func TestResolveProgramOnHostAllAssignmentsIsNoExecutable(t *testing.T) {
	prober := newTestProber(func(context.Context, string, []string, string) ([]byte, error) {
		t.Fatal("no executable word — no probe may run")
		return nil, nil
	})
	_, _, err := resolveProgramOnHost(context.Background(), "FOO=1 BAR=two", prober)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no executable word")
}

// The program name never reaches a shell at all. The REAL shell runs: if the
// name were ever interpolated the sentinels would exist.
func TestResolveProgramOnHostShellMetacharactersAreInert(t *testing.T) {
	dir := t.TempDir()
	sentinels := []string{"pwned-subshell", "pwned-backtick"}
	name := "'-rf;$(touch " + sentinels[0] + ")`touch " + sentinels[1] + "`\"z'"

	t.Setenv("HOME", dir) // `sh -l` reads ~/.profile — keep the probe hermetic.
	t.Chdir(dir)
	prober := newTestProber(execProbeRunner)
	prober.processPATH = "/nonexistent"
	prober.nonce = randomProbeNonce

	_, _, err := resolveProgramOnHost(context.Background(), name, prober)
	require.Error(t, err, "the metacharacter name resolves to nothing")
	for _, sentinel := range sentinels {
		_, serr := os.Stat(filepath.Join(dir, sentinel))
		assert.True(t, os.IsNotExist(serr), "the probe name reached a shell as code: %s exists", sentinel)
	}
}

// End to end through a real shell: a profile that prints an absolute path, and
// a command-local PATH, both behave (#5726, #5727).
func TestResolveProgramOnHostRealShellProfileNoiseAndLocalPATH(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := executableFile(t, filepath.Join(home, "opt", "bin"), "myagent")
	require.NoError(t, os.WriteFile(filepath.Join(home, ".profile"), []byte("echo /home/operator\n"), 0o644))

	prober := newTestProber(execProbeRunner)
	prober.nonce = randomProbeNonce
	prober.processPATH = "/usr/bin:/bin"

	command, _, err := resolveProgramOnHost(context.Background(), "PATH="+filepath.Dir(bin)+" myagent --x", prober)
	require.NoError(t, err)
	assert.Equal(t, "PATH="+filepath.Dir(bin)+" "+bin+" --x", command,
		"the profile's /home/operator line is not the executable, and the command's own PATH found it")
}

// #5728: a Claude alias detected as `env NAME=… claude` is stored whole as the
// built-in override. Stripping the detected path must replace only the
// executable word — the env wrapper and its assignment stay in front of it.
func TestRemoteProgramForHostKeepsTheEnvWrapperAroundADetectedAlias(t *testing.T) {
	alias := "env CLAUDE_CONFIG_DIR=/opt/profile /home/u/.local/bin/claude --dangerously-skip-permissions"
	resolved := resolvedConfigWithBuiltIn(t, "claude", alias, alias)
	assert.Equal(t, "env CLAUDE_CONFIG_DIR=/opt/profile claude --dangerously-skip-permissions",
		remoteProgramForHost(resolved, "claude"))

	// A bare-name built-in behind the env wrapper is already host-neutral.
	bare := "env CLAUDE_CONFIG_DIR=/opt/profile claude"
	resolved = resolvedConfigWithBuiltIn(t, "claude", bare, bare)
	assert.Equal(t, bare, remoteProgramForHost(resolved, "claude"))

	// `exec` in front of a detected path keeps the exec.
	exec := "exec /home/u/.local/bin/claude --x"
	resolved = resolvedConfigWithBuiltIn(t, "claude", exec, exec)
	assert.Equal(t, "exec claude --x", remoteProgramForHost(resolved, "claude"))
}

// #5767 on the daemon side: a detected path followed by a computed argument is
// still stripped to the bare agent name.
func TestRemoteProgramForHostStripsTheDetectedPathBeforeComputedArguments(t *testing.T) {
	cmd := `/home/u/.local/bin/claude --add-dir "$HOME/project" --dangerously-skip-permissions`
	resolved := resolvedConfigWithBuiltIn(t, "claude", cmd, cmd)
	assert.Equal(t, `claude --add-dir "$HOME/project" --dangerously-skip-permissions`,
		remoteProgramForHost(resolved, "claude"))
}

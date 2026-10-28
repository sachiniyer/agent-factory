package session

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// probeReply renders what the real probe script prints: whatever the login
// profile wrote, then the fenced report, then the login PATH. The fence nonce
// is the probe's own argument ($2 for a lookup, $1 for the PATH-only probe).
func probeReply(argv []string, profile, report, loginPATH string) []byte {
	if len(argv) == 6 {
		return []byte(profile + "\n" + argv[5] + "\n" + loginPATH + "\n")
	}
	return []byte(profile + "\n" + argv[6] + "\n" + report + "\n" + argv[6] + "\n" + loginPATH + "\n")
}

// executableFile makes a real executable under dir and returns its path.
func executableFile(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755))
	return path
}

func newTestProber(run hostProgramRunner) hostProgramProber {
	return hostProgramProber{
		shell:       "/bin/sh",
		host:        "sandbox.example",
		processPATH: "/spawn/bin",
		lookPath:    func(string) (string, error) { return "", errors.New("not on the process PATH") },
		run:         run,
		statFile:    os.Stat,
		nonce:       func() string { return "NONCE" },
	}
}

// The four merged-#5578 findings (#5725–#5728) and the plain command, in one
// table: the executable word comes from the shared command reader, the lookup
// honours the command's own PATH, profile noise never becomes the answer, and
// only the executable word is rewritten.
func TestResolveProgramOnHostTable(t *testing.T) {
	dir := t.TempDir()
	login := executableFile(t, filepath.Join(dir, "login"), "claude")
	custom := executableFile(t, filepath.Join(dir, "custom"), "claude")
	codex := executableFile(t, filepath.Join(dir, "login"), "codex")

	for _, tc := range []struct {
		name        string
		program     string
		profile     string // login-profile output printed before the probe
		report      string // what `command -v` printed
		wantName    string // the $1 the probe must receive
		wantLocal   string // the $3 command-local PATH the probe must receive
		wantCommand string
	}{
		{
			name: "plain command", program: "claude --model opus",
			report: login, wantName: "claude",
			wantCommand: login + " --model opus",
		},
		{
			name: "#5725 exec prefix probes the agent, not exec", program: "exec claude --x",
			report: login, wantName: "claude",
			wantCommand: "exec " + login + " --x",
		},
		{
			name: "#5726 command-local PATH is the lookup path", program: "PATH=" + filepath.Dir(custom) + " claude",
			report: custom, wantName: "claude", wantLocal: filepath.Dir(custom),
			wantCommand: "PATH=" + filepath.Dir(custom) + " " + custom,
		},
		{
			name: "#5727 login profile output is not the answer", program: "codex -c x=1",
			profile: "/home/operator\nwelcome back", report: codex, wantName: "codex",
			wantCommand: codex + " -c x=1",
		},
		{
			name: "#5728 env wrapper and assignments survive", program: "env CLAUDE_CONFIG_DIR=/opt/profile claude --dangerously-skip-permissions",
			report: login, wantName: "claude",
			wantCommand: "env CLAUDE_CONFIG_DIR=/opt/profile " + login + " --dangerously-skip-permissions",
		},
		{
			name: "assignments and exec together", program: "FOO=1 exec claude",
			report: login, wantName: "claude",
			wantCommand: "FOO=1 exec " + login,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			prober := newTestProber(func(_ context.Context, argv []string) ([]byte, error) {
				if len(argv) > 6 {
					got = append([]string(nil), argv...)
				}
				return probeReply(argv, tc.profile, tc.report, "/login/bin"), nil
			})
			command, path, err := resolveProgramOnHost(context.Background(), tc.program, prober)
			require.NoError(t, err)
			assert.Equal(t, tc.wantCommand, command)
			assert.Equal(t, "/login/bin:/spawn/bin", path)
			require.Len(t, got, 8)
			assert.Equal(t, tc.wantName, got[5], "$1 is the executable word, not exec/env/an assignment")
			assert.Equal(t, tc.wantLocal, got[7], "$3 is the command's own PATH, or empty for the login PATH")
			assert.NotContains(t, got[3], tc.wantName, "the -c script never carries the program text")
		})
	}
}

// Whatever is not exactly one absolute path to an executable regular file is a
// failed resolution — it is never repaired into a guess, and a command that
// names its own PATH never falls back to the process PATH (#5726).
func TestResolveProgramOnHostRejectsAnythingButOneExecutablePath(t *testing.T) {
	dir := t.TempDir()
	good := executableFile(t, dir, "claude")
	notExec := filepath.Join(dir, "plain")
	require.NoError(t, os.WriteFile(notExec, []byte("x"), 0o644))

	for _, tc := range []struct {
		name   string
		report string
	}{
		{"nothing", ""},
		{"alias text", "alias claude='claude --x'"},
		{"relative path", "bin/claude"},
		{"directory", dir},
		{"not executable", notExec},
		{"two answers", good + "\n" + good},
		{"missing file", filepath.Join(dir, "gone")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prober := newTestProber(func(_ context.Context, argv []string) ([]byte, error) {
				return probeReply(argv, "/home/operator", tc.report, "/login/bin"), nil
			})
			_, _, err := resolveProgramOnHost(context.Background(), "claude", prober)
			require.Error(t, err)
			assert.Contains(t, err.Error(), `"claude"`)
		})
	}

	t.Run("own PATH never falls back to the process PATH", func(t *testing.T) {
		prober := newTestProber(func(_ context.Context, argv []string) ([]byte, error) {
			return probeReply(argv, "", "", "/login/bin"), nil
		})
		prober.lookPath = func(string) (string, error) { return good, nil }
		_, _, err := resolveProgramOnHost(context.Background(), "PATH=/opt/nowhere claude", prober)
		require.Error(t, err, "claude exists on the process PATH, but the command named a different PATH")
		assert.Contains(t, err.Error(), "/opt/nowhere")
	})
}

// A bare name resolved only by the process PATH (a docker image's ENV PATH)
// still works, and must also be a real executable.
func TestResolveProgramOnHostProcessPATHFallback(t *testing.T) {
	bin := executableFile(t, t.TempDir(), "my-agent")
	prober := newTestProber(func(_ context.Context, argv []string) ([]byte, error) {
		return probeReply(argv, "", "", "/login/bin"), nil
	})
	prober.processPATH = filepath.Dir(bin)
	prober.lookPath = func(string) (string, error) { return bin, nil }
	command, path, err := resolveProgramOnHost(context.Background(), "my-agent", prober)
	require.NoError(t, err)
	assert.Equal(t, bin, command)
	assert.Equal(t, filepath.Dir(bin), path)
}

// Not found names the program, the host and the PATH searched.
func TestResolveProgramOnHostNotFoundNamesProgramPathAndHost(t *testing.T) {
	prober := newTestProber(func(_ context.Context, argv []string) ([]byte, error) {
		return probeReply(argv, "", "", "/login/a:/login/b"), nil
	})
	prober.host = "sandbox-42.example"
	_, _, err := resolveProgramOnHost(context.Background(), "my-agent", prober)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"my-agent"`)
	assert.Contains(t, err.Error(), "sandbox-42.example")
	assert.Contains(t, err.Error(), "/login/a:/login/b")
}

// Commands af cannot resolve honestly run as written; none of them is probed
// for an executable.
func TestResolveProgramOnHostRunsVerbatimWhatItCannotResolve(t *testing.T) {
	for _, program := range []string{
		"/opt/company/agent --x",   // path-qualified: the operator's explicit choice
		"env -i claude",            // environment cleared: lookup unknowable
		"env -u PATH claude",       // PATH removed
		"claude | tee /tmp/log",    // not one simple command
		"FOO=1 /opt/company/agent", // assignment before an explicit path
	} {
		t.Run(program, func(t *testing.T) {
			var scripts []string
			prober := newTestProber(func(_ context.Context, argv []string) ([]byte, error) {
				scripts = append(scripts, argv[3])
				return probeReply(argv, "", "", "/login/bin"), nil
			})
			command, path, err := resolveProgramOnHost(context.Background(), program, prober)
			require.NoError(t, err)
			assert.Equal(t, program, command)
			assert.Equal(t, "/login/bin:/spawn/bin", path)
			assert.Equal(t, []string{hostProbePathOnly}, scripts)
		})
	}
}

// A command with no executable word fails like an empty program.
func TestResolveProgramOnHostAllAssignmentsIsNoExecutable(t *testing.T) {
	prober := newTestProber(func(context.Context, []string) ([]byte, error) {
		t.Fatal("no executable word — no probe may run")
		return nil, nil
	})
	_, _, err := resolveProgramOnHost(context.Background(), "FOO=1 BAR=two", prober)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no executable word")
}

// The program name reaches the shell only as a positional argument. The REAL
// shell runs: if the name were ever interpolated the sentinels would exist.
func TestResolveProgramOnHostShellMetacharactersAreInert(t *testing.T) {
	dir := t.TempDir()
	sentinels := []string{"pwned-subshell", "pwned-backtick"}
	// One single-quoted literal word: a real shell would never run it, so any
	// sentinel that appears means the name was interpolated into a script.
	name := "'-rf;$(touch " + sentinels[0] + ")`touch " + sentinels[1] + "`\"z'"

	t.Setenv("HOME", dir) // `sh -l` reads ~/.profile — keep the probe hermetic.
	t.Chdir(dir)
	prober := newTestProber(execProbeRunner)
	prober.processPATH = "/nonexistent"
	prober.lookPath = exec.LookPath
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

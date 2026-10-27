package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resolvedConfigWithBuiltIn builds the minimal ResolvedConfig whose
// program_overrides.<agent> leaf reports one built-in candidate — the shape
// DefaultConfig's detected claude probe produces.
func resolvedConfigWithBuiltIn(t *testing.T, agent, builtIn, effective string) *config.ResolvedConfig {
	t.Helper()
	return &config.ResolvedConfig{
		Config: config.Config{
			ProgramOverrides: map[string]string{agent: effective},
		},
		Resolution: []config.ResolvedValue{{
			Key:   "program_overrides",
			Value: map[string]any{agent: effective},
			Origins: map[string]config.SourceRef{
				agent: {Layer: config.SourceGlobal.String()},
			},
			Candidates: []config.CandidateTrace{
				{Layer: config.SourceBuiltIn.String(), Present: true, Value: map[string]any{agent: builtIn}},
				{Layer: config.SourceGlobal.String(), Present: true, Value: map[string]any{agent: effective}},
			},
			Precedence: []string{config.SourceBuiltIn.String(), config.SourceGlobal.String()},
		}},
	}
}

// af's own auto-detected executable is meaningless on another filesystem —
// the wire program must carry the bare agent name with every flag preserved.
func TestRemoteProgramForHostStripsDetectedPath(t *testing.T) {
	detected := "/home/siyer/.local/bin/claude --dangerously-skip-permissions"
	resolved := resolvedConfigWithBuiltIn(t, "claude", detected, detected)

	got := remoteProgramForHost(resolved, "claude")
	assert.Equal(t, "claude --dangerously-skip-permissions", got,
		"the detected daemon path must be rewritten to the bare agent name")
}

// A program the operator extended with flags still carries the detected
// executable — strip it and keep every flag.
func TestRemoteProgramForHostPreservesFlagsWhenStripping(t *testing.T) {
	detected := "/home/siyer/.local/bin/claude --dangerously-skip-permissions"
	effective := detected + " --model opus"
	resolved := resolvedConfigWithBuiltIn(t, "claude", detected, effective)

	got := remoteProgramForHost(resolved, "claude")
	assert.Equal(t, "claude --dangerously-skip-permissions --model opus", got,
		"an operator-added flag rides the rewrite — only the detected path is dropped")
}

// An operator-authored override is untrusted text sent verbatim: a different
// absolute path, even with the same flags, is never rewritten to a bare name
// the host might resolve to a different binary.
func TestRemoteProgramForHostKeepsOperatorPathVerbatim(t *testing.T) {
	detected := "/home/siyer/.local/bin/claude --dangerously-skip-permissions"
	operator := "/opt/company/bin/claude --dangerously-skip-permissions"
	resolved := resolvedConfigWithBuiltIn(t, "claude", detected, operator)

	assert.Equal(t, operator, remoteProgramForHost(resolved, "claude"))
}

// A bare non-detected name needs no rewrite — the host probe resolves it.
func TestRemoteProgramForHostKeepsBareName(t *testing.T) {
	detected := "/home/siyer/.local/bin/claude --dangerously-skip-permissions"
	resolved := resolvedConfigWithBuiltIn(t, "claude", detected, "claude-custom")
	assert.Equal(t, "claude-custom", remoteProgramForHost(resolved, "claude"))
}

// A quoted detected path compares on the unquoted token, and the rewrite keeps
// the original suffix byte-for-byte.
func TestRemoteProgramForHostQuotedDetectedPath(t *testing.T) {
	detected := "'/opt/dir with space/claude' --dangerously-skip-permissions"
	resolved := resolvedConfigWithBuiltIn(t, "claude", detected, detected)
	assert.Equal(t, "claude --dangerously-skip-permissions", remoteProgramForHost(resolved, "claude"))
}

func TestRemoteProgramForHostNilConfig(t *testing.T) {
	assert.Equal(t, "claude", remoteProgramForHost(nil, "claude"))
}

func TestRemoteProgramForHostEmptyProgram(t *testing.T) {
	resolved := resolvedConfigWithBuiltIn(t, "claude", "/x/claude", "/x/claude")
	assert.Equal(t, "", remoteProgramForHost(resolved, ""))
}

// The probe must pass the program name as a positional argument — the argv
// shape `"$SHELL" -l -c 'command -v -- "$1"' af-probe "$name"` is the contract
// (#5571 W2.1): the untrusted override text never enters the -c script.
func TestResolveProgramOnHostPassesNameAsPositionalArg(t *testing.T) {
	var calls [][]string
	prober := hostProgramProber{
		shell:       "/bin/sh",
		host:        "sandbox.example",
		processPATH: "/spawn/bin",
		lookPath:    func(string) (string, error) { return "", errors.New("not on process PATH") },
		run: func(_ context.Context, argv []string) ([]byte, error) {
			calls = append(calls, append([]string(nil), argv...))
			return []byte("/login/bin/my-agent\n__af_path__\n/login/bin\n"), nil
		},
	}

	command, path, err := resolveProgramOnHost(context.Background(), "my-agent --flag", prober)
	require.NoError(t, err)
	assert.Equal(t, "/login/bin/my-agent --flag", command)
	assert.Equal(t, "/login/bin:/spawn/bin", path,
		"the adopted PATH is the login PATH first, then spawn-PATH extras")

	require.Len(t, calls, 1, "one command -v probe answered")
	assert.Equal(t, []string{"/bin/sh", "-l", "-c", hostProbeCommandV, "af-probe", "my-agent"}, calls[0],
		"the probe is a login shell taking the name as $1 — the script must not contain it")
	assert.NotContains(t, calls[0][3], "my-agent",
		"the -c script carries no interpolated program name")
}

// A name built from shell metacharacters — semicolon, $(…), backticks, quotes,
// a leading dash — reaches the probe only as $1. The REAL shell run is the
// point: if the name were ever interpolated, the sentinels would be created.
func TestResolveProgramOnHostShellMetacharactersAreInert(t *testing.T) {
	dir := t.TempDir()
	// Relative sentinel names keep the metacharacter word free of "/", or the
	// executable would be classified path-qualified and skip the probes — the
	// very paths this test exists to exercise.
	sentinels := []string{"pwned-subshell", "pwned-backtick"}
	// `;$(touch x)` `touch y` 'quote' and a leading dash — every injection
	// vector the review asked for, kept inside one shell word so the program's
	// executable is the metacharacter name itself.
	name := "-rf\";$(touch " + sentinels[0] + ")\"'`touch " + sentinels[1] + "`'\"'z"
	// The tokenizer strips the quoting, so the probe receives the bare word.
	wantExe := "-rf;$(touch " + sentinels[0] + ")`touch " + sentinels[1] + "`'z"

	t.Setenv("HOME", dir) // `sh -l` reads ~/.profile — keep the probe hermetic.
	t.Chdir(dir)          // any touch the name smuggles in lands in the temp dir
	var calls [][]string
	prober := hostProgramProber{
		shell:       "/bin/sh",
		host:        "sandbox.example",
		processPATH: "/nonexistent",
		lookPath:    exec.LookPath,
		run: func(ctx context.Context, argv []string) ([]byte, error) {
			calls = append(calls, append([]string(nil), argv...))
			return execProbeRunner(ctx, argv) // the REAL shell, not a fake
		},
	}

	_, _, err := resolveProgramOnHost(context.Background(), name, prober)
	require.Error(t, err, "the metacharacter name resolves to nothing")

	for _, sentinel := range sentinels {
		_, serr := os.Stat(filepath.Join(dir, sentinel))
		assert.True(t, os.IsNotExist(serr), "the probe name reached a shell as code: %s exists", sentinel)
	}
	for _, call := range calls {
		require.GreaterOrEqual(t, len(call), 5)
		assert.Equal(t, "-c", call[2])
		assert.NotContains(t, call[3], "touch",
			"the -c script must never carry the program name; it arrives as $1")
		if len(call) > 5 { // the PATH-only probe carries no positional name
			assert.Equal(t, wantExe, call[len(call)-1],
				"the name is the final positional argument, byte-for-byte")
		}
	}
	// The name was probed — command -v AND the which fallback both saw it.
	var scripts []string
	for _, call := range calls {
		scripts = append(scripts, call[3])
	}
	assert.Contains(t, scripts, hostProbeCommandV)
	assert.Contains(t, scripts, hostProbeWhich)
}

// command -v that reports something other than a path (an alias value, a
// function name) is not an answer — the which fallback gets the same
// positional treatment.
func TestResolveProgramOnHostWhichFallback(t *testing.T) {
	var calls [][]string
	prober := hostProgramProber{
		shell:       "/bin/sh",
		host:        "h",
		processPATH: "/proc/bin",
		lookPath:    func(string) (string, error) { return "", errors.New("no") },
		run: func(_ context.Context, argv []string) ([]byte, error) {
			calls = append(calls, append([]string(nil), argv...))
			if argv[3] == hostProbeWhich {
				// GNU which answers one line; BSD which may interleave noise —
				// only a /-anchored line is an answer.
				return []byte("no my-agent in /usr/bin\n/login/bin/my-agent\n__af_path__\n/login/bin\n"), nil
			}
			return []byte("alias my-agent='my-agent --x'\n__af_path__\n/login/bin\n"), nil
		},
	}

	command, _, err := resolveProgramOnHost(context.Background(), "my-agent --x", prober)
	require.NoError(t, err)
	assert.Equal(t, "/login/bin/my-agent --x", command)
	require.Len(t, calls, 2)
	assert.Equal(t, "my-agent", calls[1][len(calls[1])-1],
		"the which fallback takes the name positionally too")
}

// A program no probe can find fails create with the name, the host, and the
// PATH it searched — the three facts the diagnosis needs (#5571 W2.1).
func TestResolveProgramOnHostNotFoundNamesProgramPathAndHost(t *testing.T) {
	prober := hostProgramProber{
		shell:       "/bin/sh",
		host:        "sandbox-42.example",
		processPATH: "/proc/bin",
		lookPath:    func(string) (string, error) { return "", errors.New("no") },
		run: func(_ context.Context, argv []string) ([]byte, error) {
			return []byte("\n__af_path__\n/login/a:/login/b\n"), nil
		},
	}
	_, _, err := resolveProgramOnHost(context.Background(), "my-agent", prober)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"my-agent"`)
	assert.Contains(t, err.Error(), "sandbox-42.example")
	assert.Contains(t, err.Error(), "/login/a:/login/b")
}

// The process's own PATH is the last resort — a docker image's ENV PATH is
// invisible to every login profile, and a bare name the image provides must
// still resolve.
func TestResolveProgramOnHostProcessPATHFallback(t *testing.T) {
	prober := hostProgramProber{
		shell:       "/bin/sh",
		host:        "h",
		processPATH: "/image/bin",
		lookPath:    func(name string) (string, error) { return "/image/bin/" + name, nil },
		run: func(_ context.Context, argv []string) ([]byte, error) {
			return []byte("\n__af_path__\n/login/bin\n"), nil
		},
	}
	command, path, err := resolveProgramOnHost(context.Background(), "my-agent", prober)
	require.NoError(t, err)
	assert.Equal(t, "/image/bin/my-agent", command)
	assert.Equal(t, "/image/bin", path,
		"a process-PATH resolution keeps the process PATH — the login PATH never saw it")
}

// An operator's absolute path runs verbatim — no executable probe at all — and
// the login PATH is still adopted for the pane.
func TestResolveProgramOnHostAbsolutePathVerbatim(t *testing.T) {
	var scripts []string
	prober := hostProgramProber{
		shell:       "/bin/sh",
		host:        "h",
		processPATH: "/proc/bin",
		lookPath:    func(string) (string, error) { return "", errors.New("no") },
		run: func(_ context.Context, argv []string) ([]byte, error) {
			scripts = append(scripts, argv[3])
			return []byte("__af_path__\n/login/bin\n"), nil
		},
	}
	command, path, err := resolveProgramOnHost(context.Background(), "/opt/company/agent --x", prober)
	require.NoError(t, err)
	assert.Equal(t, "/opt/company/agent --x", command)
	assert.Equal(t, "/login/bin:/proc/bin", path)
	assert.Equal(t, []string{hostProbePathOnly}, scripts,
		"a path-qualified executable needs no command -v — only the PATH probe runs")
}

func TestMergeHostPATHDedupesAndPreservesOrder(t *testing.T) {
	assert.Equal(t, "/a:/b:/c:/d", mergeHostPATH("/a:/b", "/b:/c:/d"))
	assert.Equal(t, "/proc/bin", mergeHostPATH("", "/proc/bin"))
	assert.Equal(t, "/login/bin", mergeHostPATH("/login/bin", ""))
	assert.Equal(t, "", mergeHostPATH("", ""))
}

// readBanner must fail the moment the remote agent-server is gone — a program
// the host could not resolve exits before printing a banner, and only the
// kill -0 liveness half of the poll sees it (#5571 W2.1).
func TestReadBannerFailsFastOnDeadRemoteProcess(t *testing.T) {
	p := &sandboxProvisioner{}
	w := &sandboxWorkspace{shell: p, spec: ProvisionSpec{Title: "t"}, SessionDir: "/remote/sess", RemotePID: "4321"}
	var scripts []string
	p.runCommandFn = func(_ time.Duration, script string, _ io.Reader, _ bool) ([]byte, error) {
		scripts = append(scripts, script)
		if strings.Contains(script, "kill -0") {
			return []byte(remoteProcDead + "\n"), nil
		}
		return []byte("af agent-server: agent program \"claude\" not found on sandbox-1 (login-shell PATH \"/x\")\n"), nil
	}

	_, err := w.readBanner(time.Minute, time.Millisecond, time.Second)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pid 4321")
	assert.Contains(t, err.Error(), `agent program "claude" not found`)
	require.NotEmpty(t, scripts)
	assert.Contains(t, scripts[0], "kill -0 4321",
		"the banner poll folds the liveness check into the same remote command")
	assert.Contains(t, scripts[len(scripts)-1], "cat",
		"a dead process pulls the remote log into the error")
}

// An alive-but-slow server still polls to the banner — the liveness marker is
// stripped before the JSON parse.
func TestReadBannerParsesProgramFromBanner(t *testing.T) {
	p := &sandboxProvisioner{}
	w := &sandboxWorkspace{shell: p, spec: ProvisionSpec{Title: "t"}, SessionDir: "/remote/sess", RemotePID: "4321"}
	p.runCommandFn = func(_ time.Duration, script string, _ io.Reader, _ bool) ([]byte, error) {
		if strings.Contains(script, "kill -0") {
			return []byte(`{"addr":"127.0.0.1:4141","token":"tok","program":"/host/bin/claude --flag"}` + "\n" + remoteProcAlive + "\n"), nil
		}
		return nil, fmt.Errorf("unexpected")
	}
	banner, err := w.readBanner(time.Minute, time.Millisecond, time.Second)
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:4141", banner.Addr)
	assert.Equal(t, "/host/bin/claude --flag", banner.Program)
}

// The docker twin of the ssh fast-fail: a dead in-container server must not
// outwait the banner timeout.
func TestDockerReadBannerFailsFastOnDeadProcess(t *testing.T) {
	p := &dockerProvisioner{containerID: "cid123"}
	defer SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "kill -0"):
			return []byte(remoteProcDead + "\n"), nil
		case strings.Contains(joined, "cat "+dockerLogPath):
			return []byte("af agent-server: not found\n"), nil
		default:
			return nil, fmt.Errorf("unexpected docker call: %v", args)
		}
	})()

	_, err := p.readBanner()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exited before reporting a startup banner")
	assert.Contains(t, err.Error(), "not found")
}

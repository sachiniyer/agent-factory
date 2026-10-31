package session

import (
	"context"
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

// An env-assignment prefix does not hide the detected executable — the
// rewrite keeps the assignments and still swaps the daemon-local path for the
// bare agent name (#5578 review).
func TestRemoteProgramForHostStripsDetectedPathAfterEnvPrefix(t *testing.T) {
	detected := "/home/siyer/.local/bin/claude --dangerously-skip-permissions"
	effective := "CLAUDE_CODE_USE_BEDROCK=1 " + detected
	resolved := resolvedConfigWithBuiltIn(t, "claude", detected, effective)

	got := remoteProgramForHost(resolved, "claude")
	assert.Equal(t, "CLAUDE_CODE_USE_BEDROCK=1 claude --dangerously-skip-permissions", got,
		"the assignment prefix survives while the detected path becomes the bare name")
}

func TestRemoteProgramForHostNilConfig(t *testing.T) {
	assert.Equal(t, "claude", remoteProgramForHost(nil, "claude"))
}

func TestRemoteProgramForHostEmptyProgram(t *testing.T) {
	resolved := resolvedConfigWithBuiltIn(t, "claude", "/x/claude", "/x/claude")
	assert.Equal(t, "", remoteProgramForHost(resolved, ""))
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

// #5877: after login-PATH adoption the pane must still resolve the af it was
// provisioned with, not an older install the login profile puts first. Checked
// against a real shell's lookup, with the unpinned PATH as the control.
func TestPinPATHDirKeepsTheStagedAfFirst(t *testing.T) {
	root := t.TempDir()
	write := func(dir, id string) string {
		d := filepath.Join(root, dir)
		require.NoError(t, os.MkdirAll(d, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(d, "af"), []byte("#!/bin/sh\necho "+id+"\n"), 0o755))
		return d
	}
	sessionDir, installed := write("sess", "staged"), write("usr-local-bin", "old-install")
	login := installed + ":/usr/bin:/bin"
	lookup := func(path string) string {
		cmd := exec.Command("/bin/sh", "-c", "af")
		cmd.Env = []string{"PATH=" + path}
		out, err := cmd.Output()
		require.NoError(t, err)
		return strings.TrimSpace(string(out))
	}
	merged := mergeHostPATH(login, sessionDir+":/usr/bin")
	require.Equal(t, "old-install", lookup(merged), "control: adoption alone lets the older install win")
	pinned := PinPATHDir(merged, sessionDir)
	assert.Equal(t, "staged", lookup(pinned))
	assert.Equal(t, 1, strings.Count(pinned, sessionDir), "no duplicate entry")

	assert.Equal(t, "/a:/b", PinPATHDir("/a:/b", ""))
	assert.Equal(t, "/s:/a::/a", PinPATHDir("/a::/s:/a:/s", "/s"), "only the dir itself moves; other entries keep their order")
}

func TestSessionBinDirOnlyForTheStagedAf(t *testing.T) {
	assert.Equal(t, "/home/u/.af-sessions/s", SessionBinDir("/home/u/.af-sessions/s/af"))
	for _, argv0 := range []string{"af", "./af", "/usr/bin/other", ""} {
		assert.Empty(t, SessionBinDir(argv0), argv0)
	}
}

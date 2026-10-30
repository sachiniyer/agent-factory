package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/log/logtest"
)

// forceNewSessionEnvMarkers pins the `-e` support probe for the duration of
// a test so assertions on the exact new-session argv are deterministic
// regardless of which tmux (if any) is installed on the box.
func forceNewSessionEnvMarkers(t *testing.T, supported bool) {
	t.Helper()
	newSessionEnvSupportedOverride = &supported
	t.Cleanup(func() { newSessionEnvSupportedOverride = nil })
}

func TestVersionSupportsNewSessionEnv(t *testing.T) {
	cases := []struct {
		version string
		want    bool
	}{
		{"tmux 3.2", true},
		{"tmux 3.2a", true},
		{"tmux 3.4", true},
		{"tmux 3.5-rc2", true},
		{"tmux 4.0", true},
		{"tmux next-3.6", true},
		{"tmux master", true},
		{"tmux 3.1c", false},
		{"tmux 3.0", false},
		{"tmux 2.9a", false},
		{"tmux 1.8", false},
		{"tmux openbsd-7.4", false},
		{"garbage", false},
		{"", false},
	}
	for _, c := range cases {
		if got := versionSupportsNewSessionEnv(c.version); got != c.want {
			t.Errorf("versionSupportsNewSessionEnv(%q) = %v, want %v", c.version, got, c.want)
		}
	}
}

func TestSessionEnvFlagsIncludeMarkers(t *testing.T) {
	forceNewSessionEnvMarkers(t, true)
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv(envMarkerTestguardRun, "test-run-1")

	flags := sessionEnvFlags("af_abc_mysession", "generation-1", "")
	require.Equal(t, []string{
		"-e", "AF_SESSION=af_abc_mysession",
		"-e", "AF_SESSION_GEN=generation-1",
		"-e", "AF_HOME=" + home,
		"-e", "AF_TESTGUARD_RUN=test-run-1",
	}, flags)
}

// #6258: the create attempt's instance id rides next to the other markers, so a
// later probe of the live session can prove which attempt spawned it. It is
// omitted when the session is not an instance's agent.
func TestSessionEnvFlagsCarryTheInstanceID(t *testing.T) {
	forceNewSessionEnvMarkers(t, true)
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv(envMarkerTestguardRun, "test-run-1")

	require.Equal(t, []string{
		"-e", "AF_SESSION=af_abc_mysession",
		"-e", "AF_SESSION_GEN=generation-1",
		"-e", "AF_HOME=" + home,
		"-e", "AF_INSTANCE_ID=inst-1",
		"-e", "AF_TESTGUARD_RUN=test-run-1",
	}, sessionEnvFlags("af_abc_mysession", "generation-1", "inst-1"))
	require.NotContains(t, sessionEnvFlags("af_abc_mysession", "generation-1", ""), "AF_INSTANCE_ID=",
		"a session with no instance id stamps none")
}

// #6258: SetInstanceID reaches the actual new-session command line.
func TestStartStampsTheInstanceID(t *testing.T) {
	forceNewSessionEnvMarkers(t, true)
	forceSessionEnvExecutable(t, "/test/af")
	forceAgentOnSearchPath(t, "claude")
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	oldNewSessionGeneration := newSessionGeneration
	newSessionGeneration = func() string { return "generation-1" }
	t.Cleanup(func() { newSessionGeneration = oldNewSessionGeneration })

	ptyFactory := NewMockPtyFactory(t)
	created := false
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "has-session") && !created {
				created = true
				return fmt.Errorf("session not found")
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			if len(cmd.Args) >= 2 && cmd.Args[1] == "show-options" {
				return nil, fmt.Errorf("no server running")
			}
			return []byte("output"), nil
		},
	}

	session := newTmuxSession(toTmuxName("stamped", ""), "claude", ptyFactory, cmdExec)
	session.SetInstanceID("inst-from-launch-options")
	require.NoError(t, session.Start(t.TempDir()))
	require.NotEmpty(t, ptyFactory.cmds)
	require.Contains(t, strings.Join(ptyFactory.cmds[0].Args, " "), " -e AF_INSTANCE_ID=inst-from-launch-options ")
}

func TestSessionEnvFlagsEmptyWhenUnsupported(t *testing.T) {
	forceNewSessionEnvMarkers(t, false)
	require.Nil(t, sessionEnvFlags("af_abc_mysession", "generation-1", ""))
}

// TestStartInjectsEnvMarkers verifies the markers reach the actual
// new-session command line (#1104): they are what lets `af doctor` trace an
// orphaned pane descendant back to its session after teardown.
func TestStartInjectsEnvMarkers(t *testing.T) {
	forceNewSessionEnvMarkers(t, true)
	forceSessionEnvExecutable(t, "/test/af")
	// The expected argv carries the claude marker; a controlled PATH fixture
	// keeps the launcher's grant from depending on the host's PATH (#5265).
	forceAgentOnSearchPath(t, "claude")
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv(envMarkerTestguardRun, "test-run-1")
	oldNewSessionGeneration := newSessionGeneration
	newSessionGeneration = func() string { return "generation-1" }
	t.Cleanup(func() { newSessionGeneration = oldNewSessionGeneration })

	ptyFactory := NewMockPtyFactory(t)
	created := false
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "has-session") && !created {
				created = true
				return fmt.Errorf("session not found")
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			if len(cmd.Args) >= 2 && cmd.Args[1] == "show-options" {
				return nil, fmt.Errorf("no server running")
			}
			return []byte("output"), nil
		},
	}

	workdir := t.TempDir()
	session := newTmuxSession(toTmuxName("marked", ""), "claude", ptyFactory, cmdExec)
	require.NoError(t, session.Start(workdir))
	require.NotEmpty(t, ptyFactory.cmds)
	require.Equal(t,
		fmt.Sprintf("tmux new-session -d -s af_marked -c %s -e AF_SESSION=af_marked -e AF_SESSION_GEN=generation-1 -e AF_HOME=%s -e AF_TESTGUARD_RUN=test-run-1 %s",
			workdir, home, wrappedProgramForTest(t, "/test/af", "claude")),
		strings.Join(ptyFactory.cmds[0].Args, " "))
}

// isolateNewSessionEnvProbe ensures the cached `-e` support probe starts
// unprobed and stays free of any test override, so a shim on PATH is the only
// thing that can answer `tmux -V`. The cache is process-global, so without this
// a prior test's successful probe (or a leftover override) would short-circuit
// the shim.
func isolateNewSessionEnvProbe(t *testing.T) {
	t.Helper()
	prevOverride := newSessionEnvSupportedOverride
	newSessionEnvSupportedOverride = nil
	resetNewSessionEnvProbeCache()
	t.Cleanup(func() {
		newSessionEnvSupportedOverride = prevOverride
		resetNewSessionEnvProbeCache()
	})
}

// TestNewSessionEnvProbeFailureDoesNotDowngradeAndIsRetried is the envmarker
// counterpart of TestNoStartProbeFailureDoesNotDowngradeAndIsRetried: a
// transient `tmux -V` failure must NOT be cached. The first call reports false
// (the probe could not answer), but the second call re-probes and recovers,
// rather than latching false for the process lifetime as the old sync.Once did.
// The latch permanently blinded cleanup, adoption, orphan reaping, and doctor
// to every session created after a single hiccup.
func TestNewSessionEnvProbeFailureDoesNotDowngradeAndIsRetried(t *testing.T) {
	dir := t.TempDir()
	attemptsPath := filepath.Join(dir, "attempts")
	tmuxShim := filepath.Join(dir, "tmux")
	script := `#!/bin/sh
set -eu
attempt=0
if [ -f "$AF_TEST_TMUX_PROBE_ATTEMPTS" ]; then
    attempt=$(cat "$AF_TEST_TMUX_PROBE_ATTEMPTS")
fi
attempt=$((attempt + 1))
printf '%s\n' "$attempt" >"$AF_TEST_TMUX_PROBE_ATTEMPTS"
if [ "$attempt" -eq 1 ]; then
    exit 75
fi
printf '%s\n' 'tmux 3.4'
`
	if err := os.WriteFile(tmuxShim, []byte(script), 0o700); err != nil {
		t.Fatalf("write tmux probe shim: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AF_TEST_TMUX_PROBE_ATTEMPTS", attemptsPath)
	isolateNewSessionEnvProbe(t)

	var warnings logtest.Buffer
	oldWarningOut, oldWarningFlags := log.WarningLog.Writer(), log.WarningLog.Flags()
	log.WarningLog.SetOutput(&warnings)
	log.WarningLog.SetFlags(0)
	t.Cleanup(func() {
		log.WarningLog.SetOutput(oldWarningOut)
		log.WarningLog.SetFlags(oldWarningFlags)
	})

	if got := tmuxSupportsNewSessionEnv(); got != false {
		t.Fatalf("first tmuxSupportsNewSessionEnv() = %v, want false (probe failed)", got)
	}
	for _, want := range []string{"tmux -V probe failed", "exit status 75"} {
		if !strings.Contains(warnings.String(), want) {
			t.Errorf("failed-probe warning %q does not contain %q", warnings.String(), want)
		}
	}

	if got := tmuxSupportsNewSessionEnv(); got != true {
		t.Fatalf("second tmuxSupportsNewSessionEnv() = %v, want true (probe retried and succeeded)", got)
	}
	attempts, err := os.ReadFile(attemptsPath)
	if err != nil {
		t.Fatalf("read tmux probe attempts: %v", err)
	}
	if got := strings.TrimSpace(string(attempts)); got != "2" {
		t.Fatalf("tmux -V probe attempts = %q, want failure plus one retry", got)
	}
}

// TestNewSessionEnvProbeCachesSupportedVerdict verifies that a successful probe
// is cached: the shim is invoked exactly once across two calls, and the second
// call returns the cached supported verdict without re-probing.
func TestNewSessionEnvProbeCachesSupportedVerdict(t *testing.T) {
	dir := t.TempDir()
	attemptsPath := filepath.Join(dir, "attempts")
	tmuxShim := filepath.Join(dir, "tmux")
	script := `#!/bin/sh
set -eu
attempt=0
if [ -f "$AF_TEST_TMUX_PROBE_ATTEMPTS" ]; then
    attempt=$(cat "$AF_TEST_TMUX_PROBE_ATTEMPTS")
fi
printf '%s\n' "$((attempt + 1))" >"$AF_TEST_TMUX_PROBE_ATTEMPTS"
printf '%s\n' 'tmux 3.4'
`
	if err := os.WriteFile(tmuxShim, []byte(script), 0o700); err != nil {
		t.Fatalf("write tmux probe shim: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AF_TEST_TMUX_PROBE_ATTEMPTS", attemptsPath)
	isolateNewSessionEnvProbe(t)

	for i := 0; i < 2; i++ {
		if got := tmuxSupportsNewSessionEnv(); got != true {
			t.Fatalf("call %d: tmuxSupportsNewSessionEnv() = %v, want true", i+1, got)
		}
	}
	attempts, err := os.ReadFile(attemptsPath)
	if err != nil {
		t.Fatalf("read tmux probe attempts: %v", err)
	}
	if got := strings.TrimSpace(string(attempts)); got != "1" {
		t.Fatalf("tmux -V probe attempts = %q, want one cached supported verdict", got)
	}
}

// TestNewSessionEnvProbeCachesUnsupportedVerdict verifies that a successful
// probe of an old tmux is cached as false: the shim is invoked exactly once
// across two calls, and the second call returns the cached false without
// re-probing. A false from an unparseable or too-old version is a deterministic
// answer, not a transient failure, so it must be cached just like a true.
func TestNewSessionEnvProbeCachesUnsupportedVerdict(t *testing.T) {
	dir := t.TempDir()
	attemptsPath := filepath.Join(dir, "attempts")
	tmuxShim := filepath.Join(dir, "tmux")
	script := `#!/bin/sh
set -eu
attempt=0
if [ -f "$AF_TEST_TMUX_PROBE_ATTEMPTS" ]; then
    attempt=$(cat "$AF_TEST_TMUX_PROBE_ATTEMPTS")
fi
attempt=$((attempt + 1))
printf '%s\n' "$attempt" >"$AF_TEST_TMUX_PROBE_ATTEMPTS"
if [ "$attempt" -eq 1 ]; then
    printf '%s\n' 'tmux 3.1c'
else
    printf '%s\n' 'tmux 3.4'
fi
`
	if err := os.WriteFile(tmuxShim, []byte(script), 0o700); err != nil {
		t.Fatalf("write tmux probe shim: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AF_TEST_TMUX_PROBE_ATTEMPTS", attemptsPath)
	isolateNewSessionEnvProbe(t)

	for i := 0; i < 2; i++ {
		if got := tmuxSupportsNewSessionEnv(); got != false {
			t.Fatalf("call %d: tmuxSupportsNewSessionEnv() = %v, want false (cached unsupported)", i+1, got)
		}
	}
	attempts, err := os.ReadFile(attemptsPath)
	if err != nil {
		t.Fatalf("read tmux probe attempts: %v", err)
	}
	if got := strings.TrimSpace(string(attempts)); got != "1" {
		t.Fatalf("tmux -V probe attempts = %q, want one cached unsupported verdict", got)
	}
}

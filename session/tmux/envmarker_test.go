package tmux

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	cmd2 "github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/testguard"
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

	flags := sessionEnvFlags("af_abc_mysession")
	require.Equal(t, []string{
		"-e", "AF_SESSION=af_abc_mysession",
		"-e", "AF_HOME=" + home,
	}, flags)
}

func TestSessionEnvFlagsEmptyWhenUnsupported(t *testing.T) {
	forceNewSessionEnvMarkers(t, false)
	require.Nil(t, sessionEnvFlags("af_abc_mysession"))
}

// TestStartInjectsEnvMarkers verifies the markers reach the actual
// new-session command line (#1104): they are what lets `af doctor` trace an
// orphaned pane descendant back to its session after teardown.
func TestStartInjectsEnvMarkers(t *testing.T) {
	forceNewSessionEnvMarkers(t, true)
	forceSessionEnvExecutable(t, "/test/af")
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

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
		fmt.Sprintf("tmux new-session -d -s af_marked -c %s -e AF_SESSION=af_marked -e AF_HOME=%s %s",
			workdir, home, wrappedProgramForTest(t, "/test/af", "claude")),
		cmd2.ToString(ptyFactory.cmds[0]))
}

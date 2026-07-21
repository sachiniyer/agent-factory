package session

import (
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

type forbiddenStartupPty struct{ t *testing.T }

func (p forbiddenStartupPty) Start(*exec.Cmd) (*os.File, error) {
	p.t.Fatal("loading a startup-unknown record attempted to open a tmux PTY")
	return nil, fmt.Errorf("unreachable")
}

func (forbiddenStartupPty) Close() {}

func TestFromInstanceData_StartupUnknownLoadsInert(t *testing.T) {
	data := deadInstanceData(t, Running, "af_uncertain", "af_uncertain__shell")
	data.StartupStateUnknown = true

	forbiddenExec := cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error {
			t.Fatal("loading a startup-unknown record attempted a tmux command")
			return fmt.Errorf("unreachable")
		},
		OutputFunc: func(*exec.Cmd) ([]byte, error) {
			t.Fatal("loading a startup-unknown record attempted a tmux query")
			return nil, fmt.Errorf("unreachable")
		},
	}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, forbiddenStartupPty{t}, forbiddenExec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })

	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	require.True(t, restored.StartupStateUnknown())
	require.False(t, restored.Started(), "an uncertain runtime must remain inert after a daemon restart")
	require.True(t, restored.ToInstanceData().StartupStateUnknown,
		"the startup-unknown classification must survive another persistence round-trip")
}

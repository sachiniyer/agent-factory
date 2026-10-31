//go:build !windows

package tmux

import (
	"errors"
	"os/exec"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

var lookupFails atomic.Bool

// accountSessionForCapabilityTest is an account-scoped codex session whose
// prepare step mints a per-pane (default-command) capability.
func accountSessionForCapabilityTest(t *testing.T, name string, exec cmd_test.MockCmdExec, pty PtyFactory) *TmuxSession {
	t.Helper()
	forceAgentOnSearchPath(t, "codex")
	forceSessionEnvExecutable(t, "/test/af")
	forceNewSessionEnvMarkers(t, true)
	accountDir := t.TempDir()
	prev := sessionenv.AccountLookup
	sessionenv.AccountLookup = func(agent, account string) (sessionenv.Account, error) {
		if lookupFails.Load() {
			return sessionenv.Account{}, errors.New("account store unavailable")
		}
		return sessionenv.Account{Agent: agent, Name: account, Dir: accountDir}, nil
	}
	lookupFails.Store(false)
	t.Cleanup(func() { sessionenv.AccountLookup = prev })
	s := NewTmuxSessionFromSanitizedNameWithDeps(name, "codex", pty, exec)
	s.SetAccountForAgent("codex", "work")
	return s
}

// A PerPane capability has no clock expiry, so it lives exactly as long as
// someone revokes it (#4731). Every way a session can end must do so.
func TestPerPaneLaunchCapabilityDoesNotOutliveItsSession(t *testing.T) {
	t.Run("prepare supersedes the previous capability", func(t *testing.T) {
		name := "af_cap_prepare"
		s := accountSessionForCapabilityTest(t, name, cmd_test.MockCmdExec{}, nil)
		for i := 0; i < 3; i++ { // a restore refresh re-prepares the session
			_, _, _, _, _, err := s.prepareLaunchEnvironment("")
			require.NoError(t, err)
		}
		total, perPane := launchCapabilities(name)
		require.Equal(t, 2, total, "the initial pane's single-use capability and the default-command's")
		require.Equal(t, 1, perPane, "a re-prepared session must not accumulate a per-pane capability per prepare")
		revokeLaunchCapabilities(name)
		total, _ = launchCapabilities(name)
		require.Zero(t, total)
	})

	t.Run("a failed prepare leaves nothing behind", func(t *testing.T) {
		name := "af_cap_failed_prepare"
		s := accountSessionForCapabilityTest(t, name, cmd_test.MockCmdExec{}, nil)
		// The launch command's capability is minted before the account's
		// environment is resolved, so a failure there strands it unless revoked.
		lookupFails.Store(true)
		_, _, _, _, _, err := s.prepareLaunchEnvironment("")
		require.Error(t, err)
		total, _ := launchCapabilities(name)
		require.Zero(t, total)
	})

	t.Run("a Start that fails after prepare", func(t *testing.T) {
		name := "af_cap_start_fails"
		pty := &captureLaunchEnvPty{}
		s := accountSessionForCapabilityTest(t, name, cmd_test.MockCmdExec{
			RunFunc:    func(*exec.Cmd) error { return errors.New("session not found") },
			OutputFunc: func(*exec.Cmd) ([]byte, error) { return nil, nil },
		}, pty)
		require.Error(t, s.Start(t.TempDir()), "the pty factory stops the launch after prepare")
		total, _ := launchCapabilities(name)
		require.Zero(t, total, "a session that never started must not keep a launch capability")
	})

	t.Run("Close", func(t *testing.T) {
		name := "af_cap_close"
		var kills int
		s := accountSessionForCapabilityTest(t, name, killSessionProbeExec(false, &kills), NewMockPtyFactory(t))
		_, _, _, _, _, err := s.prepareLaunchEnvironment("")
		require.NoError(t, err)
		_, perPane := launchCapabilities(name)
		require.Equal(t, 1, perPane)
		_, err = s.Close()
		require.NoError(t, err)
		total, _ := launchCapabilities(name)
		require.Zero(t, total, "teardown (archive and kill both go through Close) must revoke")
	})

	t.Run("Close that leaves the session alive keeps it", func(t *testing.T) {
		name := "af_cap_close_survives"
		var kills int
		s := accountSessionForCapabilityTest(t, name, killSessionProbeExec(true, &kills), NewMockPtyFactory(t))
		_, _, _, _, _, err := s.prepareLaunchEnvironment("")
		require.NoError(t, err)
		_, err = s.Close()
		require.Error(t, err)
		_, perPane := launchCapabilities(name)
		require.Equal(t, 1, perPane,
			"a session tmux says is still alive still needs the capability its new windows claim")
		revokeLaunchCapabilities(name)
	})
}

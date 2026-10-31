//go:build !windows

package tmux

import (
	"errors"
	"os/exec"
	"strings"
	"sync"
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
	forceLaunchCapabilityOwner(t, true)
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
	t.Run("a restore refresh replaces the previous capability, never accumulates", func(t *testing.T) {
		name := "af_cap_refresh"
		exec, _, _ := recordingExec()
		s := accountSessionForCapabilityTest(t, name, exec, nil)
		for i := 0; i < 3; i++ { // each daemon restore re-establishes the session
			require.NoError(t, s.refreshRestoredAccountEnvironment())
		}
		total, perPane := launchCapabilities(name)
		require.Equal(t, 1, perPane, "a re-established session must hold one per-pane capability, not one per restore")
		require.Equal(t, 1, total, "and no single-use capability: a refresh launches no pane")
		revokeLaunchCapabilities(name)
		total, _ = launchCapabilities(name)
		require.Zero(t, total)
	})

	t.Run("a refresh that fails keeps the capability the installed default-command names", func(t *testing.T) {
		name := "af_cap_refresh_fails"
		exec, _, _ := recordingExec()
		s := accountSessionForCapabilityTest(t, name, exec, nil)
		require.NoError(t, s.refreshRestoredAccountEnvironment()) // installs the first one
		before := sessionenv.LaunchBrokerIfStarted()
		require.NotNil(t, before)
		_, perPaneBefore := launchCapabilities(name)
		require.Equal(t, 1, perPaneBefore)

		lookupFails.Store(true) // the next refresh fails after minting
		require.Error(t, s.refreshRestoredAccountEnvironment())
		total, perPane := launchCapabilities(name)
		require.Equal(t, 1, perPane, "a failed refresh must not revoke the capability a live window would claim")
		require.Equal(t, 1, total, "and must revoke only what it minted")
		revokeLaunchCapabilities(name)
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

// defaultCommandWrites runs a restore's account-environment refresh and returns
// every default-command value it wrote to tmux.
func defaultCommandWrites(t *testing.T, s *TmuxSession, mu *sync.Mutex, got *[]string) []string {
	t.Helper()
	require.NoError(t, s.refreshRestoredAccountEnvironment())
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), *got...)
}

func recordingExec() (cmd_test.MockCmdExec, *sync.Mutex, *[]string) {
	var mu sync.Mutex
	var writes []string
	return cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			args := strings.Join(c.Args, " ")
			if strings.Contains(args, "set-option") && strings.Contains(args, "default-command") {
				mu.Lock()
				writes = append(writes, args)
				mu.Unlock()
			}
			return nil
		},
		OutputFunc: func(*exec.Cmd) ([]byte, error) { return nil, nil },
	}, &mu, &writes
}

// Any process that restores a session runs the account-environment refresh (af
// sessions preview and attach, the TUI). Only the daemon may leave a capability
// behind default-command: a process that is about to exit would hand the
// session's every later window to nobody (#4731).
func TestRestoreRefreshWritesDefaultCommandOnlyForTheOwner(t *testing.T) {
	t.Run("a process that is not the owner leaves it alone", func(t *testing.T) {
		exec, mu, got := recordingExec()
		s := accountSessionForCapabilityTest(t, "af_cap_nonowner", exec, nil)
		forceLaunchCapabilityOwner(t, false)
		require.Empty(t, defaultCommandWrites(t, s, mu, got),
			"a non-owner restore replaced the daemon's default-command with a capability it cannot keep alive")
		_, perPane := launchCapabilities("af_cap_nonowner")
		require.Zero(t, perPane)
	})

	t.Run("the owner re-establishes it and keeps only the per-pane capability", func(t *testing.T) {
		exec, mu, got := recordingExec()
		s := accountSessionForCapabilityTest(t, "af_cap_owner", exec, nil)
		writes := defaultCommandWrites(t, s, mu, got)
		require.Len(t, writes, 1)
		require.Contains(t, writes[0], sessionenv.AccountEnvironmentExecMarker)
		total, perPane := launchCapabilities("af_cap_owner")
		require.Equal(t, 1, perPane)
		require.Equal(t, 1, total, "the refresh launches nothing, so no single-use capability may linger")
		revokeLaunchCapabilities("af_cap_owner")
	})
}

// A session created by a process that is not the owner must not be left with tmux's
// own login shell for new windows, which would run outside the account boundary.
func TestNonOwnerPrepareInstallsARefusingDefaultCommandAndMintsNoPerPaneCapability(t *testing.T) {
	s := accountSessionForCapabilityTest(t, "af_cap_nonowner_start", cmd_test.MockCmdExec{}, nil)
	forceLaunchCapabilityOwner(t, false)
	_, _, _, _, defaultCommand, err := s.prepareLaunchEnvironment("")
	require.NoError(t, err)
	require.Equal(t, nonOwnerDefaultCommand, defaultCommand)
	_, perPane := launchCapabilities("af_cap_nonowner_start")
	require.Zero(t, perPane)
	revokeLaunchCapabilities("af_cap_nonowner_start")
}

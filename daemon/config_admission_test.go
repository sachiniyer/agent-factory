package daemon

import (
	"context"
	"net"
	"net/rpc"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/internal/upgradetxn"
)

// TestConfigMutationsRefusedWhileQuiescing is the quiescing companion to the
// probation admission ledger (#3231): both halves of a config change — the
// write (SetConfigValue) and the live apply (ApplyConfig) — must be refused
// while the daemon is handing off to a validated upgrade candidate, because the
// apply mutates the very posture the hand-off depends on (listeners, auth) and
// the write's confirmation would be a lie to the successor daemon, which read
// config before it.
func TestConfigMutationsRefusedWhileQuiescing(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	manager, err := newManagerShellForDaemon(config.DefaultConfig(), "")
	require.NoError(t, err)
	require.NoError(t, manager.RestoreInstances())
	manager.lifecycle.markQuiescing()
	server := &controlServer{manager: manager}

	var applyResp ApplyConfigResponse
	applyErr := server.ApplyConfig(ApplyConfigRequest{}, &applyResp)
	require.True(t, IsDaemonQuiescingErr(applyErr),
		"ApplyConfig while quiescing reached handler logic instead of the admission gate: %v", applyErr)

	var setResp SetConfigValueResponse
	setErr := server.SetConfigValue(SetConfigValueRequest{Key: "auto_update", Value: "false"}, &setResp)
	require.True(t, IsDaemonQuiescingErr(setErr),
		"SetConfigValue while quiescing reached handler logic instead of the admission gate: %v", setErr)
}

// configClientHome sandboxes both halves of the client under test: the config
// file it may write and the control socket it dials resolve into the same
// throwaway, socket-length-safe home.
func configClientHome(t *testing.T) string {
	t.Helper()
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	return home
}

// serveControlStub answers the control socket with svc registered under the
// real control service name, so SetGlobalConfigValue dials it exactly as it
// would a daemon.
func serveControlStub(t *testing.T, svc any) {
	t.Helper()
	socketPath, err := DaemonSocketPath()
	require.NoError(t, err)
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	server := rpc.NewServer()
	require.NoError(t, server.RegisterName(controlServiceName, svc))
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go server.ServeConn(conn)
		}
	}()
}

// quiescingConfigControl refuses the write the way a quiescing daemon does.
type quiescingConfigControl struct{}

func (s *quiescingConfigControl) SetConfigValue(_ SetConfigValueRequest, _ *SetConfigValueResponse) error {
	return errDaemonQuiescing()
}

// refusingThemeControl is reachable and answers Ping, but refuses the palette
// mutation. RequestApplyTheme must not flatten this into the same benign outcome
// as a failed dial or an old daemon without the method.
type refusingThemeControl struct{}

func (s *refusingThemeControl) ApplyTheme(_ ApplyThemeRequest, _ *ApplyThemeResponse) error {
	return errDaemonQuiescing()
}

func (s *refusingThemeControl) Ping(_ PingRequest, resp *PingResponse) error {
	resp.Phase = DaemonPhaseQuiescing
	return nil
}

func TestRequestApplyThemeDistinguishesAbsenceSkewAndRefusal(t *testing.T) {
	t.Run("no daemon is a later-start apply", func(t *testing.T) {
		configClientHome(t)
		_, err := RequestApplyTheme()
		require.NoError(t, err)
	})

	t.Run("old daemon is a later-restart apply", func(t *testing.T) {
		configClientHome(t)
		serveControlStub(t, &legacyConfigControl{applied: make(chan struct{})})
		_, err := RequestApplyTheme()
		require.NoError(t, err)
	})

	t.Run("reachable refusal is final", func(t *testing.T) {
		configClientHome(t)
		serveControlStub(t, &refusingThemeControl{})
		_, err := RequestApplyTheme()
		require.True(t, IsDaemonQuiescingErr(err), "want the daemon's own refusal, got: %v", err)
	})
}

// TestSetGlobalConfigValueRefusalIsFinal pins the client half of #3231: once a
// daemon has answered the dial, its refusal is the outcome — the client must
// not fall back to the local write it would have used with no daemon, because
// that fallback is exactly the admission split this closes.
func TestSetGlobalConfigValueRefusalIsFinal(t *testing.T) {
	home := configClientHome(t)
	tomlPath := filepath.Join(home, config.TomlConfigFileName)
	orig := "default_program = 'claude'\n"
	require.NoError(t, os.WriteFile(tomlPath, []byte(orig), 0o644))
	serveControlStub(t, &quiescingConfigControl{})

	_, err := SetGlobalConfigValue("default_program", "codex")
	require.True(t, IsDaemonQuiescingErr(err), "want the daemon's own refusal, got: %v", err)

	written, readErr := os.ReadFile(tomlPath)
	require.NoError(t, readErr)
	require.Equal(t, orig, string(written), "a daemon refusal must leave config.toml untouched")
}

// TestSetGlobalConfigValueRefusesFallbackDuringLiveUpgrade pins the hand-off
// window the socket cannot witness: the quiescing daemon has exited, the
// candidate has not bound yet, so the dial fails — but the durable upgrade
// journal still proves a live transaction, and the local-write fallback must
// refuse rather than mutate config the candidate already validated against.
func TestSetGlobalConfigValueRefusesFallbackDuringLiveUpgrade(t *testing.T) {
	prev := runEntrypointGate
	t.Cleanup(func() { runEntrypointGate = prev })

	tests := []struct {
		name      string
		phase     upgradetxn.Phase
		wantCause string
		wantRetry string
	}{
		{
			name:      "forward upgrade",
			phase:     upgradetxn.PhaseCandidateValidating,
			wantCause: "config change refused during daemon upgrade handoff",
			wantRetry: "retry after the upgrade finishes",
		},
		{
			name:      "rollback restore",
			phase:     upgradetxn.PhasePreviousValidating,
			wantCause: "config change refused while rollback restores the previous daemon",
			wantRetry: "retry after rollback recovery finishes",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := configClientHome(t)
			inProgress := &upgradetxn.UpgradeInProgressError{
				TransactionID: "txn-cfg", ToVersion: "1.0.256", Phase: tc.phase,
				Deadline: time.Now().Add(time.Minute),
			}
			runEntrypointGate = func(context.Context, string, bool) error { return inProgress }

			_, err := SetGlobalConfigValue("default_program", "codex")
			var typed *upgradetxn.UpgradeInProgressError
			require.ErrorAs(t, err, &typed, "a live upgrade must refuse the daemonless fallback")
			require.ErrorContains(t, err, tc.wantCause)
			require.ErrorContains(t, err, tc.wantRetry)

			_, statErr := os.Stat(filepath.Join(home, config.TomlConfigFileName))
			require.True(t, os.IsNotExist(statErr), "the refused fallback must write nothing")
		})
	}
}

// TestSetGlobalConfigValueNoDaemonWritesLocally pins the fallback: with nothing
// answering the control socket, `af config set` keeps working — the write lands
// through config.SetGlobalConfigValue and the effect notice is the honest
// not-applied one.
func TestSetGlobalConfigValueNoDaemonWritesLocally(t *testing.T) {
	home := configClientHome(t)

	resp, err := SetGlobalConfigValue("default_program", "codex")
	require.NoError(t, err)
	require.NotNil(t, resp.Result)
	require.Equal(t, "default_program", resp.Result.Key)
	require.Equal(t, config.EffectNotice("default_program", false), resp.RestartNotice)

	written, readErr := os.ReadFile(filepath.Join(home, config.TomlConfigFileName))
	require.NoError(t, readErr)
	require.Contains(t, string(written), "default_program = 'codex'")
}

// legacyConfigControl models a pre-#1960 daemon: no SetConfigValue method at
// all, only the ApplyConfig poke such a daemon expects its clients to send
// after writing the file themselves.
type legacyConfigControl struct {
	applied chan struct{}
}

func (s *legacyConfigControl) ApplyConfig(_ ApplyConfigRequest, _ *ApplyConfigResponse) error {
	close(s.applied)
	return nil
}

// TestSetGlobalConfigValueLegacyDaemonFallsBack pins the version-skew path: a
// daemon that does not register SetConfigValue answers "can't find method",
// which is absence, not refusal — the client falls back to the legacy sequence
// (local write, then the ApplyConfig poke) instead of failing the set.
func TestSetGlobalConfigValueLegacyDaemonFallsBack(t *testing.T) {
	home := configClientHome(t)
	stub := &legacyConfigControl{applied: make(chan struct{})}
	serveControlStub(t, stub)

	resp, err := SetGlobalConfigValue("default_program", "codex")
	require.NoError(t, err)
	require.Equal(t, config.EffectNotice("default_program", true), resp.RestartNotice)
	select {
	case <-stub.applied:
	default:
		t.Fatal("the legacy fallback must poke ApplyConfig after the local write")
	}

	written, readErr := os.ReadFile(filepath.Join(home, config.TomlConfigFileName))
	require.NoError(t, readErr)
	require.Contains(t, string(written), "default_program = 'codex'")
}

// answeringConfigControl accepts the write and reports its own outcome, the way
// a running daemon does (write + in-place apply, server-side).
type answeringConfigControl struct{}

func (s *answeringConfigControl) SetConfigValue(req SetConfigValueRequest, resp *SetConfigValueResponse) error {
	resp.Result = &config.SetResult{Key: req.Key, Value: req.Value, Path: "/daemon/config.toml"}
	resp.RestartNotice = "stub notice"
	resp.Applied = []string{req.Key}
	return nil
}

// TestSetGlobalConfigValueDaemonAnswerIsVerbatim pins that with a daemon
// answering, the daemon's SetConfigValue is the entire write path: the client
// echoes the daemon's response and performs no local write of its own — the
// file is the daemon's to write, under its file lock.
func TestSetGlobalConfigValueDaemonAnswerIsVerbatim(t *testing.T) {
	home := configClientHome(t)
	serveControlStub(t, &answeringConfigControl{})

	resp, err := SetGlobalConfigValue("default_program", "codex")
	require.NoError(t, err)
	require.NotNil(t, resp.Result)
	require.Equal(t, "stub notice", resp.RestartNotice)
	require.Equal(t, []string{"default_program"}, resp.Applied)

	entries, readErr := os.ReadDir(home)
	require.NoError(t, readErr)
	for _, entry := range entries {
		require.False(t, strings.HasSuffix(entry.Name(), ".toml"),
			"the client wrote %s locally; the accepted write belongs to the daemon", entry.Name())
	}
}

package daemon

import (
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/stretchr/testify/require"
)

func TestDaemonLifecycleNormalReadinessBarrier(t *testing.T) {
	lifecycle, err := newDaemonLifecycle("", "127.0.0.1:8443")
	require.NoError(t, err)

	initial := lifecycle.snapshot()
	require.Equal(t, DaemonPhaseWarming, initial.phase)
	require.Len(t, initial.bootID, daemonBootIDBytes*2)
	require.Empty(t, initial.transactionID)
	require.True(t, initial.listeners.TCPConfigured)
	require.Equal(t, "127.0.0.1:8443", initial.listeners.TCPListenAddr)
	require.Error(t, lifecycle.markReady(), "liveness must not become readiness before restore")

	lifecycle.markRestoreComplete()
	require.Equal(t, DaemonPhaseWarming, lifecycle.snapshot().phase,
		"restore alone is not the daemon's full operational barrier")
	require.NoError(t, lifecycle.markReady())
	require.Equal(t, DaemonPhaseReady, lifecycle.snapshot().phase)
}

func TestUpgradeProbationErrorIsTypedAndRetryable(t *testing.T) {
	err := errDaemonUpgradeProbation("transaction-2212")
	require.True(t, IsDaemonUpgradeProbationErr(err))
	require.True(t, isDaemonAdmissionRetryable(err))
	require.Contains(t, err.Error(), "transaction-2212")
	require.False(t, IsDaemonUpgradeProbationErr(errDaemonStarting()))
}

func TestDaemonLifecycleProbationCannotSelfAdmit(t *testing.T) {
	const transactionID = "transaction-2212"
	lifecycle, err := newDaemonLifecycle(transactionID, "")
	require.NoError(t, err)

	require.True(t, IsDaemonUpgradeProbationErr(lifecycle.mutationAdmissionError()))

	lifecycle.markRestoreComplete()
	require.Equal(t, DaemonPhaseUpgradeProbation, lifecycle.snapshot().phase)
	require.Error(t, lifecycle.markReady(),
		"a candidate must not grade itself ready while admission is closed")
	require.True(t, IsDaemonUpgradeProbationErr(lifecycle.mutationAdmissionError()))
	require.Equal(t, transactionID, lifecycle.snapshot().transactionID)
}

func TestRunDaemonReportsReadyAtOperationalBarrier(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	cfg := config.DefaultConfig()
	cfg.ListenAddr = ""

	runDone := make(chan error, 1)
	go func() { runDone <- RunDaemon(cfg) }()
	stopped := false
	defer func() {
		if stopped {
			return
		}
		var resp ShutdownResponse
		_ = callDaemonNoEnsure("Shutdown", ShutdownRequest{}, &resp)
		select {
		case <-runDone:
		case <-time.After(5 * time.Second):
			t.Errorf("daemon did not stop during cleanup")
		}
	}()

	ready := waitForDaemonPhase(t, DaemonPhaseReady)
	require.Empty(t, ready.TransactionID)
	require.NotEmpty(t, ready.BootID)
	require.True(t, ready.Listeners.HTTPUnixBound)
	require.False(t, ready.Listeners.TCPConfigured)

	var shutdown ShutdownResponse
	require.NoError(t, callDaemonNoEnsure("Shutdown", ShutdownRequest{}, &shutdown))
	select {
	case err := <-runDone:
		require.NoError(t, err)
		stopped = true
	case <-time.After(5 * time.Second):
		t.Fatal("ready daemon did not stop after Shutdown")
	}
}

func TestRunDaemonUpgradeProbationEndToEnd(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "" // keep this lifecycle test on AF-owned Unix sockets only
	const transactionID = "transaction-e2e-2212"

	runDone := make(chan error, 1)
	go func() { runDone <- runDaemon(cfg, transactionID) }()
	stopped := false
	defer func() {
		if stopped {
			return
		}
		var resp ShutdownResponse
		_ = callDaemonNoEnsure("Shutdown", ShutdownRequest{}, &resp)
		select {
		case <-runDone:
		case <-time.After(5 * time.Second):
			t.Errorf("probationary daemon did not stop during cleanup")
		}
	}()

	probation := waitForDaemonPhase(t, DaemonPhaseUpgradeProbation)
	require.Equal(t, transactionID, probation.TransactionID)
	require.NotEmpty(t, probation.BootID)
	require.True(t, probation.Listeners.HTTPUnixBound)
	require.False(t, probation.Listeners.TCPConfigured)

	// Read-only restored state remains available, while a mutation is rejected
	// before it can validate or touch its zero-valued request.
	var snapshot SnapshotResponse
	require.NoError(t, callDaemonNoEnsure("Snapshot", SnapshotRequest{}, &snapshot))
	var create CreateSessionResponse
	err := callDaemonNoEnsure("CreateSession", CreateSessionRequest{}, &create)
	require.True(t, IsDaemonUpgradeProbationErr(err), "mutation error = %v", err)

	health := Health()
	require.NoError(t, health.PingErr)
	require.Equal(t, DaemonPhaseUpgradeProbation, health.Phase)
	require.Equal(t, transactionID, health.TransactionID)

	var shutdown ShutdownResponse
	require.NoError(t, callDaemonNoEnsure("Shutdown", ShutdownRequest{}, &shutdown))
	select {
	case err := <-runDone:
		require.NoError(t, err)
		stopped = true
	case <-time.After(5 * time.Second):
		t.Fatal("probationary daemon did not stop after Shutdown")
	}
}

func waitForDaemonPhase(t *testing.T, want DaemonPhase) PingResponse {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last PingResponse
	var lastErr error
	for time.Now().Before(deadline) {
		last, lastErr = pingDaemonResponse()
		if lastErr == nil && last.Phase == want {
			return last
		}
		select {
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatalf("daemon never reached phase %q (last phase %q, error %v)", want, last.Phase, lastErr)
	return PingResponse{}
}

func TestDaemonListenerHealthRecordsBindOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name         string
		occupyTCP    bool
		wantTCPBound bool
	}{
		{name: "configured TCP binds", wantTCPBound: true},
		{name: "configured TCP bind fails", occupyTCP: true, wantTCPBound: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
			cfg := config.DefaultConfig()
			cfg.ListenAddr = "127.0.0.1:0"
			var occupied net.Listener
			if tc.occupyTCP {
				var err error
				occupied, err = net.Listen("tcp", cfg.ListenAddr)
				require.NoError(t, err)
				t.Cleanup(func() { _ = occupied.Close() })
				cfg.ListenAddr = occupied.Addr().String()
			}

			manager, err := NewManager(cfg)
			require.NoError(t, err)
			closeHTTP, err := startHTTPServer(manager, newTaskScheduler(), newWatcherSupervisor())
			require.NoError(t, err, "the Unix HTTP surface remains available when TCP fails")

			var ping PingResponse
			require.NoError(t, (&controlServer{manager: manager}).Ping(PingRequest{}, &ping))
			require.True(t, ping.Listeners.HTTPUnixBound)
			require.True(t, ping.Listeners.TCPConfigured)
			require.Equal(t, cfg.ListenAddr, ping.Listeners.TCPListenAddr)
			require.Equal(t, tc.wantTCPBound, ping.Listeners.TCPBound)
			if tc.wantTCPBound {
				require.NotEmpty(t, ping.Listeners.TCPBoundAddr)
			} else {
				require.Empty(t, ping.Listeners.TCPBoundAddr)
			}

			require.NoError(t, closeHTTP())
			require.NoError(t, (&controlServer{manager: manager}).Ping(PingRequest{}, &ping))
			require.False(t, ping.Listeners.HTTPUnixBound)
			require.False(t, ping.Listeners.TCPBound)
		})
	}
}

// controlMethodPolicies is an exhaustive parity ledger for the net/rpc method
// set. Reflection makes a newly exported handler fail this test until its
// admission class is chosen; the mutation loop then invokes every classified
// mutation under probation, so a handler that forgets the gate fails before it
// can become a production bypass.
type probationPolicy uint8

const (
	allowedDuringProbation probationPolicy = iota
	blockedDuringProbation
)

var controlMethodPolicies = map[string]probationPolicy{
	"AddTask":          blockedDuringProbation,
	"ArchiveSession":   blockedDuringProbation,
	"CloseTab":         blockedDuringProbation,
	"CreateSession":    blockedDuringProbation,
	"CreateTab":        blockedDuringProbation,
	"DeleteProject":    blockedDuringProbation,
	"DeliverPrompt":    blockedDuringProbation,
	"HandoffSession":   blockedDuringProbation,
	"KillSession":      blockedDuringProbation,
	"PauseStatusPoll":  blockedDuringProbation,
	"ReapConfigAgent":  blockedDuringProbation,
	"ReloadTasks":      blockedDuringProbation,
	"RemoveTask":       blockedDuringProbation,
	"RenameTab":        blockedDuringProbation,
	"ReorderTab":       blockedDuringProbation,
	"RestoreArchived":  blockedDuringProbation,
	"RestoreSession":   blockedDuringProbation,
	"ResumeFromLimit":  blockedDuringProbation,
	"ResumeStatusPoll": blockedDuringProbation,
	"SendPrompt":       blockedDuringProbation,
	"SetConfigValue":   blockedDuringProbation,
	"SetPRInfo":        blockedDuringProbation,
	"SpawnConfigAgent": blockedDuringProbation,
	"TriggerTask":      blockedDuringProbation,
	"UpdateTask":       blockedDuringProbation,
	"GetConfig":        allowedDuringProbation,
	"ListBackends":     allowedDuringProbation,
	"ListPrograms":     allowedDuringProbation,
	"ListTasks":        allowedDuringProbation,
	"Ping":             allowedDuringProbation,
	"Preview":          allowedDuringProbation,
	"Shutdown":         allowedDuringProbation,
	"Snapshot":         allowedDuringProbation,
}

func TestControlServerProbationAdmissionLedger(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	manager, err := newManagerShellForDaemon(config.DefaultConfig(), "ledger-transaction")
	require.NoError(t, err)
	require.NoError(t, manager.RestoreInstances())
	server := &controlServer{
		manager: manager, scheduler: newTaskScheduler(), watchers: newWatcherSupervisor(),
	}

	typ := reflect.TypeOf(server)
	seen := make(map[string]struct{}, typ.NumMethod())
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		seen[name] = struct{}{}
		_, classified := controlMethodPolicies[name]
		require.Truef(t, classified, "control method %s has no probation admission policy", name)
	}
	for name := range controlMethodPolicies {
		_, exists := seen[name]
		require.Truef(t, exists, "stale probation admission policy for removed method %s", name)
	}

	value := reflect.ValueOf(server)
	for name, policy := range controlMethodPolicies {
		if policy != blockedDuringProbation {
			continue
		}
		t.Run(name, func(t *testing.T) {
			method := value.MethodByName(name)
			require.Equal(t, 2, method.Type().NumIn(), "control RPC signature changed")
			request := reflect.Zero(method.Type().In(0))
			response := reflect.New(method.Type().In(1).Elem())
			result := method.Call([]reflect.Value{request, response})
			require.Len(t, result, 1)
			callErr, _ := result[0].Interface().(error)
			require.True(t, IsDaemonUpgradeProbationErr(callErr),
				"%s reached handler logic instead of the probation gate: %v", name, callErr)
		})
	}
}

func TestHTTPMutationAndInteractiveStreamReturnRetryableProbation(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	manager, err := newManagerShellForDaemon(config.DefaultConfig(), "http-transaction")
	require.NoError(t, err)
	require.NoError(t, manager.RestoreInstances())
	server := &controlServer{manager: manager, scheduler: newTaskScheduler()}
	mux := newHTTPMux(server)

	request := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code)
	var health PingResponse
	dataInto(t, decodeEnvelope(t, recorder), &health)
	require.Equal(t, DaemonPhaseUpgradeProbation, health.Phase)
	require.Equal(t, "http-transaction", health.TransactionID)
	require.NotEmpty(t, health.BootID)

	request = httptest.NewRequest(http.MethodPost, "/v1/SetConfigValue",
		strings.NewReader(`{"key":"auto_update","value":"false"}`))
	recorder = httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.Contains(t, recorder.Body.String(), daemonUpgradeProbationErrText)

	request = httptest.NewRequest(http.MethodGet, "/v1/sessions/session-id/stream", nil)
	recorder = httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.Contains(t, recorder.Body.String(), daemonUpgradeProbationErrText)

	request = httptest.NewRequest(http.MethodGet, webtabPathPrefix+"session-id/tab-id/", nil)
	recorder = httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.Contains(t, recorder.Body.String(), "Validating upgrade")
}

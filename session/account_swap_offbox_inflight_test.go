package session

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/runtimetoken"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7793: the durable in-flight swap record — written before the request
// crosses, fencing admission while it exists, reconciled for the recorded id
// before anything is admitted on daemon load, and cleared only by a definite
// verdict — plus the operator's reconcile verb and the per-swap staging path.

// The record exists at the moment the request crosses: a daemon that dies
// queued or in transit reloads a row that knows WHICH swap to ask the host
// about.
func TestSwapOffBoxAccountRecordsTheInFlightSwapBeforeSending(t *testing.T) {
	f := newSwapFixture(t)
	var atSend *InFlightOffBoxSwapData
	f.srv.atSend = func(id string) {
		atSend = f.inst.InFlightOffBoxSwap()
		require.NotNil(t, atSend, "the durable record exists before the request crosses")
		assert.Equal(t, id, atSend.SwapID, "and names this swap's id")
		assert.Equal(t, "alt", atSend.To)
	}

	_, err := f.inst.SwapOffBoxAccount("alt", false)
	require.NoError(t, err)
	require.NotNil(t, atSend)

	// The durable write ran for the record's transitions: set before the
	// request, cleared by the settle.
	var sawSet bool
	for _, rec := range *f.persisted {
		if rec != nil && rec.SwapID == atSend.SwapID {
			sawSet = true
		}
	}
	assert.True(t, sawSet, "the record was durably written")
	assert.Nil(t, f.inst.InFlightOffBoxSwap(), "a definite verdict retires it")
}

// A session with no durable write behind it refuses to send at all — an
// unrecorded request is exactly the hole the record exists to close.
func TestSwapOffBoxAccountRefusesToSendUnrecorded(t *testing.T) {
	f := newSwapFixture(t)
	f.inst.SetOffBoxSwapPersist(nil)

	_, err := f.inst.SwapOffBoxAccount("alt", false)
	require.ErrorContains(t, err, "no durable write")
	assert.Zero(t, f.srv.calls, "the request never crossed")
	assert.Equal(t, secretWork, f.hostCred(t, "account"))
}

// The record's own reconcile, reached on a row whose transport is bound: the
// same protocol the lost-response path runs — a status read, then the
// serialized discard when the first read cannot attribute the answer.
func TestInFlightSwapReconcileSettlesTheHeldRequest(t *testing.T) {
	f := newSwapFixture(t)
	f.srv.holdNext = 1 // the request crossed but is still queued server-side

	_, err := f.inst.SwapOffBoxAccount("alt", false)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrAccountSwapIndeterminate, "the discard bounded it: a definite rollback")
	require.Nil(t, f.inst.InFlightOffBoxSwap(), "the verdict retired the record")

	// The queued request then lands — tombstoned, it can only roll back.
	_, err = f.srv.runHeldSwap(0)
	require.Error(t, err)
	assert.Equal(t, secretWork, f.hostCred(t, "account"), "the live scope never moved")
}

// A row restored mid-flight reconciles the RECORDED id before anything is
// admitted against it. Here the swap committed on the host while the daemon
// was dead: the reload must record the account the host provably runs — never
// silently reload the old account while the host runs the new one.
func TestReattachSandboxReconcilesTheInFlightSwapBeforeBinding(t *testing.T) {
	newListedFixture(t, oneListedHost)
	home := os.Getenv("AGENT_FACTORY_HOME")
	workDir, err := agentaccount.Register(home, tmux.ProgramClaude, "work")
	require.NoError(t, err)
	altDir, err := agentaccount.Register(home, tmux.ProgramClaude, "alt")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(workDir, credFile), []byte(secretWork), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(altDir, credFile), []byte(secretAlt), 0o600))

	// The host after the lost swap committed: the live scope holds the new
	// account, the outgoing one waits at .prev.
	dir := t.TempDir()
	live := filepath.Join(dir, "account")
	require.NoError(t, os.MkdirAll(live, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(live, credFile), []byte(secretAlt), 0o600))
	prev := live + accountPrevSuffix
	require.NoError(t, os.MkdirAll(prev, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(prev, credFile), []byte(secretWork), 0o600))
	t.Setenv("CLAUDE_CONFIG_DIR", live)

	key, err := runtimetoken.EnsureKey()
	require.NoError(t, err)
	ref, err := runtimetoken.NewTokenRef()
	require.NoError(t, err)
	srvToken, err := runtimetoken.DeriveAgentServerToken(key, ref)
	require.NoError(t, err)

	// The stand-in agent-server: alive, and it answers the status read with
	// THIS swap's committed terminal record.
	var statusCalls int
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/agent/alive", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+srvToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": agentAliveResp{Alive: true}})
	})
	mux.HandleFunc("/v1/agent/swap-account-status", func(w http.ResponseWriter, r *http.Request) {
		statusCalls++
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"ok": true, "outcome": swapOutcomeCommitted, "program": "claude", "swap_id": "swap-1",
		}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	backend, teardown, err := restoreRuntimeCleanup("load", "sandbox", &RuntimeCleanupData{Sandbox: &SandboxRuntimeCleanupData{
		SSHCommand: "ssh fake-target", SessionDir: dir, RemotePID: "4242",
		RemoteAddr: "127.0.0.1:" + port, AgentServerVersion: "1.0.0", TokenRef: ref,
	}})
	require.NoError(t, err)
	p := backend.(*sandboxBackend).provisioner
	p.sessionDir = dir
	p.runCommandFn = localShell{}.Run
	p.startTunnelFn = func(string) (*AgentServerEndpoint, error) {
		return &AgentServerEndpoint{URL: "http://" + srv.Listener.Addr().String()}, nil
	}

	inst := &Instance{
		Title: "load", liveness: LiveLost, backend: backend, runtimeTeardown: teardown,
		Account: "work", accountAgent: tmux.ProgramClaude,
		startupStateUnknown: true, // the loader fenced it
		offBoxSwapInFlight:  &InFlightOffBoxSwapData{SwapID: "swap-1", From: "work", To: "alt"},
		offBoxSwapPersist:   func(*Instance) error { return nil },
	}

	require.NoError(t, inst.ReattachSandbox())
	assert.Equal(t, 1, statusCalls, "the host was asked about THIS swap")
	acct, _ := inst.AccountSelection()
	assert.Equal(t, "alt", acct, "the reconciled commit records the account the host provably runs")
	assert.Nil(t, inst.InFlightOffBoxSwap())
	assert.False(t, inst.StartupStateUnknown())
	assert.True(t, inst.started)
	assert.NoDirExists(t, prev, "the outgoing scope was removed under the commit's own terminal record")
	assert.NotNil(t, inst.remoteClient, "the row binds only after the record settles")
}

// The same reload where the host cannot say how the swap ended keeps the
// fence: the record stays, the row stays inert and unbound, and nothing is
// admitted — the reconcile verb or `af sessions kill` is the way out.
func TestReattachSandboxKeepsTheFenceWhileTheVerdictIsIndeterminate(t *testing.T) {
	key, err := runtimetoken.EnsureKey()
	require.NoError(t, err)
	ref, err := runtimetoken.NewTokenRef()
	require.NoError(t, err)
	srvToken, err := runtimetoken.DeriveAgentServerToken(key, ref)
	require.NoError(t, err)

	dir := t.TempDir()
	live := filepath.Join(dir, "account")
	require.NoError(t, os.MkdirAll(live, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(live, credFile), []byte(secretWork), 0o600))
	t.Setenv("CLAUDE_CONFIG_DIR", live)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/agent/alive", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": agentAliveResp{Alive: true}})
	})
	mux.HandleFunc("/v1/agent/swap-account-status", func(w http.ResponseWriter, r *http.Request) {
		// An anonymous answer proves nothing about this swap: the shape a
		// pre-identity agent-server speaks.
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"ok": true, "outcome": swapOutcomeCommitted, "program": "claude", "swap_id": "",
		}})
	})
	mux.HandleFunc("/v1/agent/swap-account-discard", func(w http.ResponseWriter, r *http.Request) {
		// And a pre-identity server has no serialized discard to bound the
		// lost request with.
		http.Error(w, "not found", http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	backend, teardown, err := restoreRuntimeCleanup("load", "sandbox", &RuntimeCleanupData{Sandbox: &SandboxRuntimeCleanupData{
		SSHCommand: "ssh fake-target", SessionDir: dir, RemotePID: "4242",
		RemoteAddr: "127.0.0.1:" + port, AgentServerVersion: "1.0.0", TokenRef: ref,
	}})
	require.NoError(t, err)
	p := backend.(*sandboxBackend).provisioner
	p.sessionDir = dir
	p.runCommandFn = localShell{}.Run
	p.startTunnelFn = func(string) (*AgentServerEndpoint, error) {
		return &AgentServerEndpoint{URL: "http://" + srv.Listener.Addr().String()}, nil
	}
	_ = srvToken

	inst := &Instance{
		Title: "load", liveness: LiveLost, backend: backend, runtimeTeardown: teardown,
		Account: "work", accountAgent: tmux.ProgramClaude,
		startupStateUnknown: true,
		offBoxSwapInFlight:  &InFlightOffBoxSwapData{SwapID: "swap-1", From: "work", To: "alt"},
		offBoxSwapPersist:   func(*Instance) error { return nil },
	}

	err = inst.ReattachSandbox()
	require.ErrorIs(t, err, ErrOffBoxSwapVerdictIndeterminate)
	assert.NotNil(t, inst.InFlightOffBoxSwap(), "an indeterminate verdict keeps the record")
	assert.True(t, inst.StartupStateUnknown(), "and the fence")
	assert.False(t, inst.started)
	assert.Nil(t, inst.remoteClient, "nothing binds while the verdict is unknown")
}

// A fenced row carrying NO record — one written before the record existed —
// keeps the old inert path: the daemon cannot name a swap to reconcile, so it
// remains kill-only rather than reattached to guess.
func TestReattachSandboxKeepsARecordlessFencedRowInert(t *testing.T) {
	inst, _ := reattachFixture(t, "")
	inst.startupStateUnknown = true
	assert.ErrorIs(t, inst.ReattachSandbox(), ErrReattachIneligible)
	assert.False(t, inst.started)
	assert.Nil(t, inst.remoteClient)
}

// The storage contract (#7793): the record persists on the row, and ForStorage
// projects the rollback fence so a previous release loads it inert rather
// than silently unfenced. A current reader restores the real flag.
func TestInFlightSwapRecordPersistsWithTheRollbackFence(t *testing.T) {
	inst := &Instance{Title: "s", startupStateUnknown: false,
		offBoxSwapInFlight: &InFlightOffBoxSwapData{SwapID: "swap-1", From: "work", To: "alt"}}
	data := inst.ToInstanceData().ForStorage()
	require.NotNil(t, data.OffBoxSwapInFlight)
	assert.Equal(t, "swap-1", data.OffBoxSwapInFlight.SwapID)
	assert.True(t, data.StartupStateUnknown, "the fence is projected for a previous release")
	require.NotNil(t, data.OffBoxSwapInFlight.OriginalStartupStateUnknown)
	assert.False(t, *data.OffBoxSwapInFlight.OriginalStartupStateUnknown,
		"the row's real flag is preserved for a current reader")

	restored := data.RestoreOffBoxSwapInFlightRollbackFence()
	assert.False(t, restored.StartupStateUnknown, "a current reader restores the real flag")
	assert.NotNil(t, restored.OffBoxSwapInFlight)
	assert.Nil(t, restored.OffBoxSwapInFlight.OriginalStartupStateUnknown,
		"and the projection marker comes off")

	// The projection is idempotent over an already-projected row.
	again := data.ForStorage()
	assert.False(t, *again.OffBoxSwapInFlight.OriginalStartupStateUnknown,
		"a second projection does not capture the projected true as the original")
}

// An identified swap stages at ITS OWN path only — the anonymous .next is
// never produced, so nothing unordered can create the path the queued
// request's rename later consumes (#7793 P3).
func TestSwapOffBoxAccountStagesAtTheSwapsOwnPath(t *testing.T) {
	f := newSwapFixture(t)

	_, err := f.inst.SwapOffBoxAccount("alt", false)
	require.NoError(t, err)

	assert.NoDirExists(t, f.w.NextAccountDir(), "the anonymous stage path is never written")
	assert.NoDirExists(t, f.stagedDir(), "the swap's own stage was rotated in")
	assert.Equal(t, secretAlt, f.hostCred(t, "account"))
	staged := false
	for _, c := range *f.shellCalls {
		if containsStr(c, accountNextSuffix+".test-swap-1") {
			staged = true
		}
	}
	assert.True(t, staged, "staging wrote the swap's own path")
}

// The rotation reads only the stage at THIS swap's own path: an anonymous
// stage and another swap's stage beside the live scope are both invisible to
// it — the rename's source can never be produced by anything but this swap's
// own staging (#7793 P3).
func TestScopeRotationConsumesOnlyTheSwapsOwnStage(t *testing.T) {
	live := rotationHost(t) // anonymous .next with "B" — never this swap's source
	for _, tc := range []struct{ id, cred string }{{"swap-2", "C"}} {
		next := stagedScopeDir(live, tc.id)
		require.NoError(t, os.MkdirAll(next, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(next, credFile), []byte(tc.cred), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(next, accountSwapMarkerRel), []byte(tc.id), 0o600))
	}
	next := stagedScopeDir(live, "swap-1")
	require.NoError(t, os.MkdirAll(next, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(next, credFile), []byte("B1"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(next, accountSwapMarkerRel), []byte("swap-1"), 0o600))

	rot, err := newScopeRotation(tmux.ProgramClaude, "swap-1")
	require.NoError(t, err)
	require.NoError(t, rot.rotate())

	assert.Equal(t, "B1", readOrEmpty(filepath.Join(live, credFile)),
		"this swap's own stage rotated in")
	assert.DirExists(t, live+accountNextSuffix, "the anonymous stage was never the rename's source")
	assert.Equal(t, "B", readOrEmpty(filepath.Join(live+accountNextSuffix, credFile)))
	assert.Equal(t, "C", readOrEmpty(filepath.Join(stagedScopeDir(live, "swap-2"), credFile)),
		"nor was another swap's")
}

// The verb settles only what the host answers definitely for the RECORDED id:
// a committed record belonging to a different swap is not this swap's commit —
// the staged scope is discarded and the row keeps its account.
func TestReconcileSwapSettlesOnlyTheRecordedID(t *testing.T) {
	f := newSwapFixture(t)
	f.srv.holdNext = 1
	_, err := f.inst.SwapOffBoxAccount("alt", false)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrAccountSwapIndeterminate)

	// A different swap's committed record sits in the slot while this swap's
	// request is still queued: the reconcile cannot credit it to this swap,
	// so the discard bounds it and the verdict is not-committed.
	f.srv.rec.id, f.srv.rec.outcome, f.srv.rec.program = "someone-elses", swapOutcomeCommitted, "claude"
	rec := &InFlightOffBoxSwapData{SwapID: "test-swap-1", From: "work", To: "alt"}
	f.inst.offBoxSwapInFlight = rec
	f.inst.remoteClient = &remoteAgentClient{}

	settled, err := f.inst.ReconcileOffBoxSwapFence()
	require.NoError(t, err)
	assert.True(t, settled)
	acct, _ := f.inst.AccountSelection()
	assert.Equal(t, "work", acct, "another swap's commit is not this swap's — the row keeps its account")
	assert.Nil(t, f.inst.InFlightOffBoxSwap())
	assert.NoDirExists(t, f.stagedDir(), "the staged scope was discarded")
	o, _, id := f.srv.rec.status()
	assert.Equal(t, swapOutcomeCommitted, o)
	assert.Equal(t, "someone-elses", id, "the other swap's record is never displaced")

	// And the still-queued request lands to a tombstone: rolled back only.
	_, err = f.srv.runHeldSwap(0)
	require.Error(t, err)
	assert.Equal(t, secretWork, f.hostCred(t, "account"))
}

// The tombstone half on a rolled-back verdict: the host provably kept the old
// account, so the fence lifts, the stage is discarded through the serialized
// path, and a late request can only roll back.
func TestReconcileSwapUnfencesOnARollbackVerdict(t *testing.T) {
	f := newSwapFixture(t)
	f.srv.loseResponse = errors.New("remote agent-server: context deadline exceeded")
	f.srv.statusErr = errors.New("remote agent-server: connection reset")

	_, err := f.inst.SwapOffBoxAccount("alt", false)
	require.ErrorIs(t, err, ErrAccountSwapIndeterminate)
	require.NotNil(t, f.inst.InFlightOffBoxSwap())
	f.inst.remoteClient = &remoteAgentClient{}

	// The host answers: this swap's stage was discarded under it — a definite
	// rollback (the discard's tombstone made commit impossible).
	f.srv.statusErr = nil
	f.srv.rec.id, f.srv.rec.outcome, f.srv.rec.program = "test-swap-1", swapOutcomeRolledBack, ""
	settled, err := f.inst.ReconcileOffBoxSwapFence()
	require.NoError(t, err)
	assert.True(t, settled)
	acct, _ := f.inst.AccountSelection()
	assert.Equal(t, "work", acct)
	assert.False(t, f.inst.StartupStateUnknown())
	assert.Nil(t, f.inst.InFlightOffBoxSwap())
}

var _ = time.Second // the file's fixture shares the package's helpers

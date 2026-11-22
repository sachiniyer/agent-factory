package session

import (
	"encoding/json"
	"errors"
	"io"
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
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7793: the durable in-flight swap record — written before the request
// crosses, fencing every lifecycle gate while it exists, reconciled for the
// recorded id before anything is admitted on daemon load, and cleared only by
// a definite verdict — plus the negotiated per-swap staging path.

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
// admitted — a later daemon restart re-runs the reconcile, or `af sessions
// kill` removes the row.
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
// than silently unfenced. A current reader keeps the flag UP for as long as
// the record exists — the record itself is this build's fence (#7793 R1): a
// row whose recorded request may still be queued on the host is startup-
// unknown at every gate, not only at swap admission and reattach, until the
// record's own definite verdict resolves it.
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
	assert.True(t, restored.StartupStateUnknown,
		"the record itself is the fence: while it exists the row is startup-unknown at EVERY gate — restoring the projected-away false would unfence recovery and reprovision before any verdict (#7793 R1)")
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
	assert.True(t, f.srv.lastScoped, "the request carried the negotiated scoped flag")
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

	rot, err := newScopeRotation(tmux.ProgramClaude, "swap-1", true)
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

// The older wire spoke only the shared stage: an agent-server already running
// from the previous release advertises no "account-swap-scoped-stage" feature,
// so the daemon stages at account.next (the path that server reads) and still
// sends the scoped flag it ignores. Skew in THIS direction must not break a
// swap that was safe before (#7793 review).
func TestSwapOffBoxAccountStagesAtTheSharedPathOnALegacyServer(t *testing.T) {
	f := newSwapFixture(t)
	f.srv.scoped = false // a previous-release server

	_, err := f.inst.SwapOffBoxAccount("alt", false)
	require.NoError(t, err)
	assert.False(t, f.srv.lastScoped, "the request never claims the scoped protocol to a legacy server")
	assert.NoDirExists(t, f.stagedDir(), "no suffixed stage is left")
	assert.Equal(t, secretAlt, f.hostCred(t, "account"), "the shared-path swap still committed")
	assert.Nil(t, f.inst.InFlightOffBoxSwap())
}

// The other skew direction refuses cleanly: a daemon that cannot read the
// server's advertised features cannot know which stage path the server will
// consume, so it refuses BEFORE staging rather than drop a scope where the
// server might not look — with an actionable upgrade hint. A silent
// shared-path guess against a scoped-only future server would orphan
// credentials the swap then cannot reach.
func TestSwapOffBoxAccountRefusesWhenTheStagingProtocolIsUnreadable(t *testing.T) {
	f := newSwapFixture(t)
	f.srv.scopedErr = errors.New("remote agent-server: connection refused")

	_, err := f.inst.SwapOffBoxAccount("alt", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "upgrade")
	assert.Zero(t, f.srv.calls, "the request never crossed")
	assert.Nil(t, f.inst.InFlightOffBoxSwap(), "nothing was sent, so nothing is owed a verdict")
	assert.Equal(t, secretWork, f.hostCred(t, "account"))
	// And nothing was staged for either protocol spelling.
	for _, c := range *f.shellCalls {
		assert.NotContains(t, c, "account.next", "no staging ran before the refusal")
	}
}

// A row restored with an unresolved swap record AND a dead agent-server —
// the pane it carried is gone too — must not restart the server, must not
// Provision or Launch an agent, and must keep every fence: recovery,
// relaunch, reprovision all refuse while the verdict is unknown, and kill
// still quarantines the workspace (#7793 review: reconcile before ANY of
// those, on a row the loader already fenced).
func TestReattachSandboxFencesARowWhoseServerCannotReconcile(t *testing.T) {
	newListedFixture(t, oneListedHost)
	_, err := runtimetoken.EnsureKey()
	require.NoError(t, err)
	ref, err := runtimetoken.NewTokenRef()
	require.NoError(t, err)

	// The agent-server is dead: no listener answers the recorded address.
	closed := httptest.NewServer(http.NewServeMux())
	port := closed.Listener.Addr().String()
	closed.Close()
	_, portNum, _ := net.SplitHostPort(port)

	var runCalls int
	dir := t.TempDir()
	backend, teardown, err := restoreRuntimeCleanup("load", "sandbox", &RuntimeCleanupData{Sandbox: &SandboxRuntimeCleanupData{
		SSHCommand: "ssh fake-target", SessionDir: dir, RemotePID: "4242",
		RemoteAddr: "127.0.0.1:" + portNum, AgentServerVersion: "1.0.0", TokenRef: ref,
	}})
	require.NoError(t, err)
	p := backend.(*sandboxBackend).provisioner
	p.sessionDir = dir
	// The row predates pane re-spawn only in shape: it WOULD be restartable
	// (a launch script exists), so any restart the record's gate fails to
	// stop shows up as a host call.
	p.launchScript = "echo 4242"
	p.runCommandFn = func(time.Duration, string, io.Reader, bool) ([]byte, error) {
		runCalls++
		return nil, errors.New("no host")
	}
	p.startTunnelFn = func(string) (*AgentServerEndpoint, error) {
		return &AgentServerEndpoint{URL: "http://" + port}, nil
	}

	inst := &Instance{
		Title: "load", liveness: LiveLost, backend: backend, runtimeTeardown: teardown,
		Account: "work", accountAgent: tmux.ProgramClaude,
		startupStateUnknown: true, // the loader fenced it
		offBoxSwapInFlight:  &InFlightOffBoxSwapData{SwapID: "swap-1", From: "work", To: "alt"},
		offBoxSwapPersist:   func(*Instance) error { return nil },
		gitWorktree:         &git.GitWorktree{},
		// Tabs is nil: the pane the row carried did not survive either.
	}

	err = inst.ReattachSandbox()
	require.Error(t, err, "the dead server can produce no verdict, so the row stays fenced")
	assert.Zero(t, runCalls, "no restart was attempted before the reconcile")
	assert.NotNil(t, inst.InFlightOffBoxSwap(), "the record survives")
	assert.True(t, inst.StartupStateUnknown(), "the fence survives")
	assert.False(t, inst.started)
	assert.Nil(t, inst.remoteClient)

	// Every gate the record fences refuses on the loaded row.
	assert.Error(t, inst.LifecycleView().ValidateRuntimeAction(RuntimeActionRecoverLost), "recover is fenced")
	mode, _, err := inst.prepareKillTeardown(false)
	require.NoError(t, err)
	assert.True(t, mode.keepWorkspace, "kill quarantines the workspace while the verdict is unknown")
}

// max_concurrent_runs: a run whose row still carries the swap record must
// hold its task slot through BOTH projections the cap reads — the live
// LifecycleView.Activity() arm and the storage-only LoadedActivity arm
// (#7793 review). The loaded arm is the ghost case: a sandbox row that
// failed to materialize is rewritten inert LiveLost, which classifies
// terminal — releasing the slot — unless the record reaches the classifier.
func TestInFlightSwapRecordHoldsItsSlotAcrossBothProjections(t *testing.T) {
	rec := &InFlightOffBoxSwapData{SwapID: "swap-1", From: "work", To: "alt"}

	// The live arm: a materialized row mid-task whose record has not settled.
	inst := &Instance{
		taskRunActive: true, liveness: LiveLost, backend: &sandboxBackend{},
		startupStateUnknown: true, offBoxSwapInFlight: rec,
	}
	assert.Equal(t, ActivityPending, inst.LifecycleView().Activity(),
		"holdsTaskRunSlot's Activity() arm must keep the run pending")

	// The ghost arm: the same row as it lies on disk (ForStorage projects the
	// startup fence for older readers), failed to materialize.
	row := InstanceData{
		TaskID: "task1", TaskRunActive: true, BackendType: "sandbox",
		Liveness: LiveLost, StartupStateUnknown: true, OffBoxSwapInFlight: rec,
	}
	activity, _ := LoadedActivity(row)
	assert.Equal(t, ActivityPending, activity,
		"rawTaskRunHoldsSlot's LoadedActivity arm must keep the ghost pending")
}

// A reconciled COMMIT is a replacement the run never saw (#7793 review): the
// host relaunched the agent on the new account while the daemon was dead, so
// the task it was serving was never replayed to the fresh conversation. The
// run ends interrupted — never completable by the replacement's first idle,
// which would fire on_complete — and the staging's skipped-config evidence
// rides the record back onto the row rather than vanishing.
func TestReconciledCommitEndsTheUnreplayedRunAndRestoresTheSkipList(t *testing.T) {
	f := newSwapFixture(t)
	f.srv.loseResponse = errors.New("remote agent-server: context deadline exceeded")
	f.srv.statusErr = errors.New("remote agent-server: connection reset")
	f.inst.taskRunActive = true

	_, err := f.inst.SwapOffBoxAccount("alt", false)
	require.ErrorIs(t, err, ErrAccountSwapIndeterminate)
	assert.Equal(t, secretAlt, f.hostCred(t, "account"), "the host committed while the response was lost")
	rec := f.inst.InFlightOffBoxSwap()
	require.NotNil(t, rec)

	// The record carries the staging evidence the live path would have
	// recorded — a recovered commit is not a clean placement.
	rec.AgentConfigSkipped = []string{"claude:CLAUDE.md: declared file is missing"}

	// The daemon's restart reconciles the committed verdict on the bound row:
	// the run the replacement never saw ends interrupted, not completed.
	f.inst.remoteClient = &remoteAgentClient{}
	f.srv.statusErr = nil
	settled, err := f.inst.ReconcileOffBoxSwapFence()
	require.NoError(t, err)
	require.True(t, settled)
	acct, _ := f.inst.AccountSelection()
	assert.Equal(t, "alt", acct)
	assert.False(t, f.inst.taskRunActive, "the unreplayed run ended")
	assert.True(t, f.inst.TaskRunInterrupted(), "as interrupted, never completed")
	assert.True(t, f.inst.ConsumeTaskRunInterrupted(), "the daemon stamps the owning task once")
	assert.False(t, f.inst.ConsumeTaskRunInterrupted())
	assert.Equal(t, []string{"claude:CLAUDE.md: declared file is missing"}, f.inst.agentConfigSkipped,
		"the skipped declared file survives the recovered commit")
	assert.Nil(t, f.inst.InFlightOffBoxSwap())
}

// A discard's tombstone is itself this swap's terminal verdict (#7793 review):
// when the first reconcile proves a never-sent request by discarding its stage
// and the record-clear checkpoint is lost, a restart replays the SAME question.
// The replayed status must answer this swap's rolled_back — not an anonymous
// "none" that a stage-less, prev-less host can never tell from "still queued",
// which would fence the row through every restart forever.
func TestDiscardVerdictReplaysAcrossALostCheckpoint(t *testing.T) {
	f := newSwapFixture(t)
	f.srv.holdNext = 1 // the request crossed but is still queued server-side

	_, err := f.inst.SwapOffBoxAccount("alt", false)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrAccountSwapIndeterminate, "the first reconcile settled: discard -> none -> rollback")
	require.Nil(t, f.inst.InFlightOffBoxSwap())

	// The clear's checkpoint never landed: a restart reloads a row that still
	// carries the record and re-runs the same reconcile cold.
	f.inst.offBoxSwapInFlight = &InFlightOffBoxSwapData{SwapID: "test-swap-1", From: "work", To: "alt"}
	f.inst.startupStateUnknown = true
	f.inst.remoteClient = &remoteAgentClient{}

	settled, err := f.inst.ReconcileOffBoxSwapFence()
	require.NoError(t, err)
	assert.True(t, settled, "the discard's tombstone replays as this swap's own rolled_back")
	assert.Nil(t, f.inst.InFlightOffBoxSwap())
	assert.False(t, f.inst.StartupStateUnknown())

	// And the queued request still lands to the tombstone: rolled back only.
	_, err = f.srv.runHeldSwap(0)
	require.Error(t, err)
	assert.Equal(t, secretWork, f.hostCred(t, "account"))
}

// A task-backed session whose swap cannot be settled keeps its run: the
// indeterminate fence is NOT a terminal startup failure, so taskRunActive —
// and the max_concurrent_runs slot it pays for — survives until the record's
// definite verdict or the operator's kill. MarkStartupStateUnknown's
// terminal-startup clear would release the slot while the request may still
// commit on the host (#7793 review).
func TestIndeterminateSwapFenceKeepsTheActiveTaskRun(t *testing.T) {
	f := newSwapFixture(t)
	f.srv.loseResponse = errors.New("remote agent-server: context deadline exceeded")
	f.srv.statusErr = errors.New("remote agent-server: connection reset")
	f.inst.taskRunActive = true

	_, err := f.inst.SwapOffBoxAccount("alt", false)
	require.ErrorIs(t, err, ErrAccountSwapIndeterminate)
	assert.True(t, f.inst.StartupStateUnknown())
	assert.True(t, f.inst.taskRunActive,
		"the run is not terminal — the swap may still commit on the host")
	assert.Equal(t, ActivityPending, f.inst.LifecycleView().Activity())

	// The reconcile path to the same indeterminate verdict keeps it too:
	// the host is re-asked, still cannot say, and the marker persists.
	f.inst.remoteClient = &remoteAgentClient{}
	settled, err := f.inst.ReconcileOffBoxSwapFence()
	require.Error(t, err)
	assert.False(t, settled)
	assert.True(t, f.inst.taskRunActive)
	assert.NotNil(t, f.inst.InFlightOffBoxSwap())
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

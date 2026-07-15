package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// remoteWorkspaceBackend is the faithful stand-in for a docker/ssh backend: an
// off-box workspace that ALSO advertises Recover, exactly as backend_docker.go
// and backend_ssh.go do. That pairing is the whole #1794 hazard — Workspace
// says "no local worktree", Recover says "the restore loop may re-provision me"
// — so a double that reports only one of the two cannot exercise it. (The older
// recoverFakeBackend "remote" variant reports Recover=false, which is why the
// automatic re-provision path went untested.)
type remoteWorkspaceBackend struct {
	*session.FakeBackend
	mu       sync.Mutex
	recovers int
}

func (b *remoteWorkspaceBackend) Type() string { return "docker" }

func (b *remoteWorkspaceBackend) Capabilities() session.Capabilities {
	return session.Capabilities{
		Workspace:        session.WorkspaceRemote,
		Attach:           true,
		Archive:          true,
		Recover:          true,
		InteractiveInput: true,
	}
}

// Recover stands in for recoverSandbox: the destructive re-provision whose
// firing against a live sandbox is the data loss #1794 exists to prevent. The
// tests assert on the COUNT, so a re-provision that should never have happened
// fails loudly rather than silently succeeding.
func (b *remoteWorkspaceBackend) Recover(inst *session.Instance) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recovers++
	inst.SetStatusForTest(session.Running)
	return nil
}

func (b *remoteWorkspaceBackend) recoverCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.recovers
}

// registerStartedRemote registers a started instance whose agent-server is the
// REAL remoteAgentServer client (#1592 Phase 4) pointed at `url`, so the daemon
// poll drives it over actual HTTP exactly as it does a docker/ssh session. Its
// backend reports an off-box workspace to match, so the poll's remote-vs-local
// branching (#1794) sees the same shape production does; liveness still comes
// only from the REST probes, never from a tmux stand-in.
func registerStartedRemote(t *testing.T, m *Manager, repoID, repoPath, title, url string, status session.Status) (*session.Instance, *remoteWorkspaceBackend) {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:             title,
		Path:              repoPath,
		Program:           "claude",
		RemoteAgentServer: &session.AgentServerEndpoint{URL: url, Token: "test-token"},
	})
	if err != nil {
		t.Fatalf("NewInstance(remote): %v", err)
	}
	backend := &remoteWorkspaceBackend{FakeBackend: session.NewFakeBackend()}
	inst.SetBackend(backend)
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(status)
	seedDiskInstance(t, repoID, title, repoPath)
	m.mu.Lock()
	m.instances[daemonInstanceKey(repoID, title)] = inst
	m.mu.Unlock()
	return inst, backend
}

// withRemoteLossThresholds shrinks the #1794 debounce knobs for one test and
// restores them after. Tests set them explicitly rather than inheriting the
// production values so an assertion reads against numbers on the same screen.
func withRemoteLossThresholds(t *testing.T, count int, grace, confirm time.Duration) {
	t.Helper()
	prevCount, prevGrace, prevConfirm := remoteLostFailureThreshold, remoteLostGracePeriod, remoteLostConfirmTimeout
	remoteLostFailureThreshold, remoteLostGracePeriod, remoteLostConfirmTimeout = count, grace, confirm
	t.Cleanup(func() {
		remoteLostFailureThreshold, remoteLostGracePeriod, remoteLostConfirmTimeout = prevCount, prevGrace, prevConfirm
	})
}

// driveDurableRemoteLoss runs enough failing ticks, spread over enough fake
// wall-clock, to satisfy BOTH debounce axes — the "the sandbox really is gone"
// signal.
func driveDurableRemoteLoss(m *Manager, advance func(time.Duration)) {
	for i := 0; i < remoteLostFailureThreshold; i++ {
		m.RefreshStatuses()
		advance(remoteLostGracePeriod)
	}
	m.RefreshStatuses()
}

// TestRefreshStatuses_UnreachableRemoteMarkedLost is the #1782 regression, held
// intact across the #1794 debounce. A remote session's agent-server dies
// (container killed, ssh forward dropped), so every REST probe fails with
// ECONNREFUSED. The poll used to return on the first Snapshot error before any
// liveness check ran, leaving the session pinned at its last-known Running/Ready
// forever — the TUI showed a healthy row for a session that was gone. A DURABLE
// unreachability must still settle to Lost; #1794 only changed how much evidence
// that takes, never the destination.
//
// Port 1 on loopback has no listener, so every probe is refused immediately —
// the "agent-server is unreachable" shape, with no timeout to wait out.
func TestRefreshStatuses_UnreachableRemoteMarkedLost(t *testing.T) {
	withRemoteLossThresholds(t, 3, time.Minute, time.Second)
	advance := withFrozenClock(t)
	manager, repoID, repoPath := newStatusTestManager(t)
	// Start from Running to prove the pass actively transitions it rather than
	// merely leaving a pre-set status untouched.
	registerStartedRemote(t, manager, repoID, repoPath, "remote-gone", "http://127.0.0.1:1", session.Running)

	driveDurableRemoteLoss(manager, advance)

	inst := manager.instances[daemonInstanceKey(repoID, "remote-gone")]
	if got := inst.GetLiveness(); got != session.LiveLost {
		t.Fatalf("in-memory liveness = %v, want LiveLost (a durably unreachable agent-server must not keep reading as Running)", got)
	}
	// Persisted too, so the status survives a daemon reload and the restore loop
	// can find it — Lost, not Dead: no kill intent, still recovery-eligible (#1108).
	if got := persistedStatus(t, repoID, "remote-gone"); got != session.Lost {
		t.Fatalf("persisted status = %v, want Lost", got)
	}
}

// TestRefreshStatuses_SingleRemoteFailureDoesNotMarkLostOrReprovision is the
// #1794 P1 regression — the data-loss one.
//
// A single failed poll against a remote session proves nothing: a two-second ssh
// forward re-establish fails identically to a destroyed container. Acting on it
// is catastrophic rather than merely wrong, because Lost is not a display state
// — RestoreLostSessions runs in the SAME daemon tick, docker/ssh advertise
// Recover, and their Recover is recoverSandbox: provision a NEW container and
// clone the branch from origin. So one blip used to mean a fresh sandbox, the
// original left running and unreferenced, and every unpushed commit on it gone.
//
// This drives the exact blip shape (one tick, wholly unreachable) and pins both
// halves of the guarantee: no Lost, and — the part that actually loses work — no
// re-provision.
func TestRefreshStatuses_SingleRemoteFailureDoesNotMarkLostOrReprovision(t *testing.T) {
	withRemoteLossThresholds(t, 3, time.Minute, time.Second)
	zeroRestoreBackoff(t) // the restore loop must be free to pounce, as it is in prod
	manager, repoID, repoPath := newStatusTestManager(t)
	_, backend := registerStartedRemote(t, manager, repoID, repoPath, "remote-blip", "http://127.0.0.1:1", session.Running)

	// One tick, exactly as the daemon runs it: refresh, then restore.
	manager.RefreshStatuses()
	manager.RestoreLostSessions()

	inst := manager.instances[daemonInstanceKey(repoID, "remote-blip")]
	if got := inst.GetLiveness(); got != session.LiveRunning {
		t.Fatalf("liveness after ONE failed probe = %v, want LiveRunning left alone — a single transport blip is not proof the sandbox died", got)
	}
	if got := backend.recoverCalls(); got != 0 {
		t.Fatalf("Recover calls = %d, want 0 — re-provisioning on a one-poll blip orphans the still-running sandbox and destroys its unpushed work (#1794)", got)
	}
}

// TestRefreshStatuses_RemoteFailuresWithinGraceDoNotMarkLost pins the TIME axis
// of the debounce, which the failure COUNT alone cannot cover. An unreachable
// port fails instantly, so a burst of ticks can exhaust the count in
// milliseconds — precisely the fast-failing blip the debounce exists to absorb.
// Here the count is well past its threshold but the clock has barely moved, so
// the session must NOT be Lost.
func TestRefreshStatuses_RemoteFailuresWithinGraceDoNotMarkLost(t *testing.T) {
	withRemoteLossThresholds(t, 3, time.Minute, time.Second)
	advance := withFrozenClock(t)
	manager, repoID, repoPath := newStatusTestManager(t)
	registerStartedRemote(t, manager, repoID, repoPath, "remote-fastfail", "http://127.0.0.1:1", session.Running)

	// Ten ticks — triple the count threshold — inside a single second.
	for i := 0; i < 10; i++ {
		manager.RefreshStatuses()
		advance(100 * time.Millisecond)
	}

	inst := manager.instances[daemonInstanceKey(repoID, "remote-fastfail")]
	if got := inst.GetLiveness(); got != session.LiveRunning {
		t.Fatalf("liveness = %v, want LiveRunning: %d fast failures inside 1s of a %s grace period is a blip, not a durable loss", got, 10, remoteLostGracePeriod)
	}
}

// TestRefreshStatuses_RemoteProbeSuccessResetsDebounce pins that the debounce
// counts CONSECUTIVE failures. A reachable agent-server is proof the sandbox is
// there, so it must reset the episode outright. Without the reset, an unlucky
// session that blips once an hour would accumulate failures forever and a single
// later blip would tip a perfectly healthy session over the threshold into a
// re-provision.
func TestRefreshStatuses_RemoteProbeSuccessResetsDebounce(t *testing.T) {
	withRemoteLossThresholds(t, 3, time.Minute, time.Second)
	advance := withFrozenClock(t)

	var healthy atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/agent/snapshot" && healthy.Load() {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"updated": true, "has_prompt": false, "content": "working"},
			})
			return
		}
		// Unhealthy: the capture fails, the shape of a blip.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"message": "capture failed"},
		})
	}))
	defer srv.Close()

	manager, repoID, repoPath := newStatusTestManager(t)
	registerStartedRemote(t, manager, repoID, repoPath, "remote-flappy", srv.URL, session.Running)
	key := daemonInstanceKey(repoID, "remote-flappy")

	// Fail right up to the count threshold, spread past the grace period...
	for i := 0; i < remoteLostFailureThreshold-1; i++ {
		manager.RefreshStatuses()
		advance(remoteLostGracePeriod)
	}
	// ...then one good probe lands.
	healthy.Store(true)
	manager.RefreshStatuses()
	healthy.Store(false)

	manager.mu.Lock()
	_, tracked := manager.remoteLossStates[key]
	manager.mu.Unlock()
	if tracked {
		t.Fatal("a successful probe must drop the loss state entirely — a stale count would tip a later single blip straight to Lost")
	}

	// The next failure is now episode-fresh, so it cannot be durable on its own.
	manager.RefreshStatuses()
	if got := manager.instances[key].GetLiveness(); got == session.LiveLost {
		t.Fatal("liveness = LiveLost after one failure following a healthy probe: the debounce restarted from a stale count instead of from zero")
	}
}

// TestRefreshStatuses_DurableRemoteFailureButAliveKeepsStatus pins the other
// half of the #1782 fix, now behind the debounce: a Snapshot error is not on its
// own proof of death. Here the agent-server stays REACHABLE and reports itself
// alive while its capture keeps failing — a broken snapshot on a healthy
// sandbox. Even once the failures are durable enough to clear the debounce, the
// confirming Alive() probe answers, so the session must NOT be marked Lost. This
// is the check that separates "the capture is broken" from "the sandbox is gone".
func TestRefreshStatuses_DurableRemoteFailureButAliveKeepsStatus(t *testing.T) {
	withRemoteLossThresholds(t, 3, time.Minute, time.Second)
	advance := withFrozenClock(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/agent/snapshot":
			// An error envelope, exactly as the real agent-server reports a
			// failed capture.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{"message": "capture failed"},
			})
		case "/v1/agent/alive":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"alive": true},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	manager, repoID, repoPath := newStatusTestManager(t)
	registerStartedRemote(t, manager, repoID, repoPath, "remote-capture-broken", srv.URL, session.Running)

	driveDurableRemoteLoss(manager, advance)

	inst := manager.instances[daemonInstanceKey(repoID, "remote-capture-broken")]
	if got := inst.GetLiveness(); got != session.LiveRunning {
		t.Fatalf("liveness = %v, want LiveRunning — an agent-server that answers Alive is not Lost no matter how many snapshots failed", got)
	}
}

// TestRefreshStatuses_ConfirmationProbeIsBounded is the #1794 P2 regression: the
// double-timeout poll stall.
//
// The confirmation Alive() is a SECOND full REST call, and the Snapshot that
// triggered it has already burned the whole remoteAgentCallTimeout (30s). Since
// RefreshStatuses walks instances SERIALLY, an unbounded confirmation let ONE
// blackholed sandbox add ~2x30s to every poll — wedging status refresh,
// root-ensure, lost-restore and limit-resume for every other session in the
// daemon.
//
// The server here is the wedge: it accepts the connection and then never
// answers, so only a client-side bound can end the call. The assertion is on
// wall-clock — the confirmation must come in near its own short timeout, not
// near the 30s call timeout — and it is deliberately loose (a 10x margin) so it
// measures the bound rather than the machine's load.
func TestRefreshStatuses_ConfirmationProbeIsBounded(t *testing.T) {
	const confirmTimeout = 250 * time.Millisecond
	// session.remoteAgentCallTimeout, which the daemon package cannot name. This
	// is the ceiling an UNBOUNDED confirmation probe would wait out, and the
	// number this test exists to stay far below.
	const remoteCallTimeout = 30 * time.Second
	withRemoteLossThresholds(t, 3, time.Minute, confirmTimeout)
	advance := withFrozenClock(t)

	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/agent/snapshot" {
			// Fail FAST, so the elapsed time this test measures is the
			// confirmation probe's alone and not the snapshot's.
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{"message": "capture failed"},
			})
			return
		}
		// /v1/agent/alive: the blackhole. Never respond.
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	// LIFO: this runs BEFORE srv.Close() above, which is what makes the close
	// possible at all. srv.Close() waits for outstanding handlers, and the
	// blackholed one only returns when released — and it cannot be released from
	// t.Cleanup, which runs AFTER every defer. The request context is no escape
	// either: aliveWithin abandons its goroutine but cannot cancel the in-flight
	// REST call, so the server would sit wedged for the full 30s call timeout.
	defer close(release)

	manager, repoID, repoPath := newStatusTestManager(t)
	registerStartedRemote(t, manager, repoID, repoPath, "remote-wedged", srv.URL, session.Running)

	// Ticks below the threshold must not probe Alive at all — the debounce runs
	// first precisely so the common case costs no extra round-trip.
	for i := 0; i < remoteLostFailureThreshold-1; i++ {
		manager.RefreshStatuses()
		advance(remoteLostGracePeriod)
	}

	// This is the tick that clears the debounce and pays for the confirmation.
	start := time.Now()
	manager.RefreshStatuses()
	elapsed := time.Since(start)

	if elapsed > 100*confirmTimeout {
		t.Fatalf("threshold tick took %s with a %s confirmation bound: the confirmation probe is running unbounded, so a wedged remote stalls the serial poll for every other session (#1794)", elapsed, confirmTimeout)
	}
	if elapsed >= remoteCallTimeout {
		t.Fatalf("threshold tick took %s — it waited out the full %s call timeout instead of the short confirmation bound", elapsed, remoteCallTimeout)
	}
	// A wedged agent-server that cannot answer a liveness ping on top of durable
	// snapshot failure is unreachable by any useful definition — the bound must
	// resolve it, not just abandon it.
	inst := manager.instances[daemonInstanceKey(repoID, "remote-wedged")]
	if got := inst.GetLiveness(); got != session.LiveLost {
		t.Fatalf("liveness = %v, want LiveLost — a bounded confirmation that times out still has to settle the session", got)
	}
}

// TestRefreshStatuses_ReachableRemoteWithDeadAgentGoesLostImmediately fences the
// debounce from OVER-applying, which is the failure mode a debounce invites.
//
// A container outlives the agent inside it: the sandbox is up and its
// agent-server keeps serving clean idle (false,false) snapshots, but the agent
// process is gone. Here the probe is ANSWERED — the sandbox was asked, it
// looked, and it says the agent is not there. That evidence is present and
// conclusive, so there is nothing to wait for: debouncing it would leave a dead
// session showing green for a grace period, and if the debounce were keyed on
// "looks dead" rather than "could not tell" it would never mark it Lost AT ALL,
// silently regressing #935/#1108 for exactly the sessions #1782 set out to fix.
//
// One tick, Lost. The debounce is for absent evidence, not bad evidence.
func TestRefreshStatuses_ReachableRemoteWithDeadAgentGoesLostImmediately(t *testing.T) {
	withRemoteLossThresholds(t, 3, time.Minute, time.Second)

	// The sandbox is healthy and answering. The agent inside it is not.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/agent/snapshot":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"updated": false, "has_prompt": false, "content": ""},
			})
		case "/v1/agent/alive":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"alive": false},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	manager, repoID, repoPath := newStatusTestManager(t)
	registerStartedRemote(t, manager, repoID, repoPath, "remote-agent-dead", srv.URL, session.Running)

	manager.RefreshStatuses() // exactly one tick

	inst := manager.instances[daemonInstanceKey(repoID, "remote-agent-dead")]
	if got := inst.GetLiveness(); got != session.LiveLost {
		t.Fatalf("liveness after one tick = %v, want LiveLost — the sandbox ANSWERED that its agent is gone, which is authoritative; the debounce is for probes that could not be answered, not for answers we dislike", got)
	}
}

// TestRestoreLostSessions_PostRecoverBlipDoesNotReprovisionAgain is codex's #1
// on PR #1804: stale debounce state surviving a recovery.
//
// A successful Recover REPLACES the runtime — for a remote session the sandbox
// behind the old failures no longer exists. The threshold-satisfying
// remoteLossStates entry describes that dead sandbox, so leaving it behind arms
// a trap: the very first transport blip against the FRESH sandbox re-satisfies
// the debounce instantly (the count is already past the threshold and
// firstFailureAt is long past the grace period), marks it Lost, and
// re-provisions AGAIN — orphaning the sandbox we just built. The debounce would
// be defeated at precisely the moment it matters most, and each cycle would
// strand another live container.
//
// So: recover, then blip once. Exactly one re-provision may ever happen.
func TestRestoreLostSessions_PostRecoverBlipDoesNotReprovisionAgain(t *testing.T) {
	withRemoteLossThresholds(t, 3, time.Minute, time.Second)
	zeroRestoreBackoff(t)
	advance := withFrozenClock(t)

	manager, repoID, repoPath := newStatusTestManager(t)
	// Nothing listening: every probe is refused, so the loss is real and durable.
	inst, backend := registerStartedRemote(t, manager, repoID, repoPath, "remote-recovered", "http://127.0.0.1:1", session.Running)

	// Durable loss -> Lost -> the restore loop re-provisions once. That part is
	// the #1108/#1782 feature working correctly.
	driveDurableRemoteLoss(manager, advance)
	if got := inst.GetLiveness(); got != session.LiveLost {
		t.Fatalf("setup: liveness = %v, want LiveLost before the restore pass", got)
	}
	manager.RestoreLostSessions()
	if got := backend.recoverCalls(); got != 1 {
		t.Fatalf("setup: Recover calls = %d, want 1 (a durably-lost remote must be recovered)", got)
	}

	// The fresh sandbox now takes ONE transport blip.
	advance(time.Second)
	manager.RefreshStatuses()
	manager.RestoreLostSessions()

	if got := backend.recoverCalls(); got != 1 {
		t.Fatalf("Recover calls = %d, want still 1 — one blip against the freshly recovered sandbox re-provisioned it again, so the recovery left stale debounce state behind and each blip now orphans another live sandbox (#1794)", got)
	}
	if got := inst.GetLiveness(); got == session.LiveLost {
		t.Fatal("liveness = LiveLost after a single post-recovery blip: the debounce restarted from the dead sandbox's count instead of from zero")
	}
}

// TestRefreshStatuses_NewSessionDoesNotInheritDebounceState is codex's #2 on PR
// #1804: debounce state keyed by identity, not by title.
//
// Titles get reused — kill or archive a remote session and make another with the
// same name. Keyed by repo/title, the successor would inherit its predecessor's
// accumulated failures: one blip would then satisfy the threshold instantly and
// re-provision a sandbox that had never failed a probe in its life. This is the
// key-by-title-instead-of-stable-id class (#1723/#1678/#1738); Instance.ID is
// the field #1195 minted to tell "same session" from "title reused".
func TestRefreshStatuses_NewSessionDoesNotInheritDebounceState(t *testing.T) {
	withRemoteLossThresholds(t, 3, time.Minute, time.Second)
	advance := withFrozenClock(t)

	manager, repoID, repoPath := newStatusTestManager(t)
	const title = "reused-title"
	old, _ := registerStartedRemote(t, manager, repoID, repoPath, title, "http://127.0.0.1:1", session.Running)

	// The doomed predecessor accumulates a threshold-satisfying failure history.
	driveDurableRemoteLoss(manager, advance)
	if got := old.GetLiveness(); got != session.LiveLost {
		t.Fatalf("setup: predecessor liveness = %v, want LiveLost", got)
	}
	manager.mu.Lock()
	stale := len(manager.remoteLossStates)
	manager.mu.Unlock()
	if stale == 0 {
		t.Fatal("setup: expected the predecessor to have left debounce state to inherit")
	}

	// It is replaced by a NEW session reusing the same repo/title — a distinct
	// instance with its own stable ID, and its own healthy sandbox.
	fresh, freshBackend := registerStartedRemote(t, manager, repoID, repoPath, title, "http://127.0.0.1:1", session.Running)
	if fresh.ID == old.ID {
		t.Fatal("setup: the replacement must be a distinct instance with its own stable ID")
	}

	// One blip against the newcomer.
	advance(time.Second)
	manager.RefreshStatuses()
	manager.RestoreLostSessions()

	if got := fresh.GetLiveness(); got != session.LiveRunning {
		t.Fatalf("new session's liveness = %v, want LiveRunning — it inherited the killed session's failure count through the repo/title key, so its FIRST blip marked it Lost (#1794)", got)
	}
	if got := freshBackend.recoverCalls(); got != 0 {
		t.Fatalf("Recover calls on the new session = %d, want 0 — inherited state re-provisioned a sandbox that had never failed a probe", got)
	}
}

// TestRestoreSession_PostManualRecoverBlipDoesNotReprovision is the manual-path
// twin of TestRestoreLostSessions_PostRecoverBlipDoesNotReprovisionAgain.
//
// The RestoreSession RPC (`af sessions restore`, the TUI's restore action) runs
// its own Recover, separate from the automatic loop's. Clearing the debounce in
// only one of them leaves the other armed with the same trap: recovery replaces
// the runtime, the stale threshold-satisfying count outlives it, and the first
// blip against the sandbox the USER just asked for re-provisions it away.
// Recovery is the lifecycle event that must reset the counter — which trigger
// pulled it is irrelevant.
func TestRestoreSession_PostManualRecoverBlipDoesNotReprovision(t *testing.T) {
	withRemoteLossThresholds(t, 3, time.Minute, time.Second)
	zeroRestoreBackoff(t)
	advance := withFrozenClock(t)

	manager, repoID, repoPath := newStatusTestManager(t)
	inst, backend := registerStartedRemote(t, manager, repoID, repoPath, "remote-manual", "http://127.0.0.1:1", session.Running)

	// A durable outage parks it at Lost with a threshold-satisfying history.
	driveDurableRemoteLoss(manager, advance)
	if got := inst.GetLiveness(); got != session.LiveLost {
		t.Fatalf("setup: liveness = %v, want LiveLost", got)
	}

	// The user restores it by hand.
	if _, err := manager.RestoreSession(RestoreSessionRequest{Title: "remote-manual", RepoID: repoID}); err != nil {
		t.Fatalf("RestoreSession: %v", err)
	}
	if got := backend.recoverCalls(); got != 1 {
		t.Fatalf("setup: Recover calls = %d, want 1", got)
	}

	// One blip against the sandbox that restore just built.
	advance(time.Second)
	manager.RefreshStatuses()
	manager.RestoreLostSessions()

	if got := backend.recoverCalls(); got != 1 {
		t.Fatalf("Recover calls = %d, want still 1 — a manual restore left stale debounce state behind, so one blip threw away the sandbox the user just asked for and orphaned it (#1794)", got)
	}
	if got := inst.GetLiveness(); got == session.LiveLost {
		t.Fatal("liveness = LiveLost after a single post-restore blip: the debounce resumed from the dead sandbox's count instead of from zero")
	}
}

// TestRestoreArchived_RemoteReprovisionClearsDebounce is the fourth and last
// re-provision trigger: the archive-restore path.
//
// It is the one the sweep provably cannot cover. Kill or archive a session and
// the sweep drops its entry — but an archive-RESTORE re-provisions onto the SAME
// instance ID (same session, new sandbox, by design), so the entry stays "live"
// and perfectly inheritable while the sandbox it describes is destroyed. Only
// the site that replaced the runtime can know, which is why noteRuntimeReplaced
// is named for the trigger and why every such site must call it.
func TestRestoreArchived_RemoteReprovisionClearsDebounce(t *testing.T) {
	withRemoteLossThresholds(t, 3, time.Minute, time.Second)
	zeroRestoreBackoff(t)
	advance := withFrozenClock(t)

	manager, repoID, repoPath := newStatusTestManager(t)
	inst, backend := registerStartedRemote(t, manager, repoID, repoPath, "remote-archived", "http://127.0.0.1:1", session.Running)

	// A durable outage leaves a threshold-satisfying history on the record.
	driveDurableRemoteLoss(manager, advance)
	manager.mu.Lock()
	stale := len(manager.remoteLossStates)
	manager.mu.Unlock()
	if stale == 0 {
		t.Fatal("setup: expected accumulated debounce state to survive into the archive")
	}

	// It is archived, then restored — which re-provisions a fresh sandbox.
	inst.SetStatusForTest(session.Archived)
	if _, err := manager.RestoreArchived(RestoreArchivedRequest{Title: "remote-archived", RepoID: repoID}); err != nil {
		t.Fatalf("RestoreArchived: %v", err)
	}
	if got := backend.recoverCalls(); got != 1 {
		t.Fatalf("setup: Recover calls = %d, want 1 (the archive-restore must re-provision once)", got)
	}

	// One blip against the sandbox the restore just provisioned.
	advance(time.Second)
	manager.RefreshStatuses()
	manager.RestoreLostSessions()

	if got := backend.recoverCalls(); got != 1 {
		t.Fatalf("Recover calls = %d, want still 1 — the archive-restore left the pre-archive failure count in place, so one blip re-provisioned the restored sandbox away and orphaned it (#1794)", got)
	}
}

// TestRestoreSession_AliveRemoteSandboxIsNotReprovisioned is codex's P1 on
// 2ec7b52: the manual restore path lacked the live recheck the automatic loop
// has.
//
// I had scoped the recheck to the automatic loop on the reasoning that a manual
// restore is explicit user intent. That reasoning was wrong. "Restore" asks for
// a working session; it does not ask to destroy a running sandbox and every
// commit it never pushed. Being user-initiated makes the recheck MORE important,
// not less — the user is the one most likely to fire it while a transport is
// healing, precisely because they watched the row go red. And the row's Lost
// mark can be minutes stale, since the automatic loop backs off to 5 minutes.
//
// A sandbox that answers is not lost: heal the row, deliver what was asked for,
// destroy nothing.
func TestRestoreSession_AliveRemoteSandboxIsNotReprovisioned(t *testing.T) {
	withRemoteLossThresholds(t, 3, time.Minute, time.Second)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/agent/alive" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"alive": true}})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	manager, repoID, repoPath := newStatusTestManager(t)
	// Marked Lost during an outage; the transport has since healed.
	inst, backend := registerStartedRemote(t, manager, repoID, repoPath, "remote-healed", srv.URL, session.Lost)

	if _, err := manager.RestoreSession(RestoreSessionRequest{Title: "remote-healed", RepoID: repoID}); err != nil {
		t.Fatalf("RestoreSession: %v", err)
	}

	if got := backend.recoverCalls(); got != 0 {
		t.Fatalf("Recover calls = %d, want 0 — the sandbox answered as alive, so a manual restore must heal the row rather than provision a replacement over a live sandbox and discard its unpushed work (#1794)", got)
	}
	if got := inst.GetLiveness(); got == session.LiveLost {
		t.Fatal("liveness is still LiveLost: a restore that proves the sandbox alive must clear the mark, not leave the row stranded")
	}
}

// TestSweepRemoteLossStates_DropsArchivedRows is codex's P3 on 2ec7b52.
//
// The sweep took presence in m.instances as proof of liveness, but an archived
// session stays in that map under the same stable ID — so its entry was kept
// forever, contradicting the doc. Archiving a remote session tears its sandbox
// down (branch pushed, in-sandbox workspace killed, remote wiring reset), which
// makes the count definitionally stale, and the poll never probes an archived
// row, so nothing would ever clear it.
func TestSweepRemoteLossStates_DropsArchivedRows(t *testing.T) {
	withRemoteLossThresholds(t, 3, time.Minute, time.Second)
	advance := withFrozenClock(t)

	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerStartedRemote(t, manager, repoID, repoPath, "remote-shelved", "http://127.0.0.1:1", session.Running)

	driveDurableRemoteLoss(manager, advance)
	manager.mu.Lock()
	before := len(manager.remoteLossStates)
	manager.mu.Unlock()
	if before == 0 {
		t.Fatal("setup: expected accumulated debounce state before the archive")
	}

	// Archived: the sandbox those failures were about is deliberately destroyed.
	inst.SetStatusForTest(session.Archived)
	manager.RefreshStatuses() // runs the sweep

	manager.mu.Lock()
	after := len(manager.remoteLossStates)
	manager.mu.Unlock()
	if after != 0 {
		t.Fatalf("remoteLossStates still holds %d entry/entries after the row was archived — an archived session keeps its map entry and its stable ID, and is never probed again, so nothing else will ever drop it (#1794)", after)
	}
}

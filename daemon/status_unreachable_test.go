package daemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// The unreachable flag (#5571 W1.3) is the additive observation half of the
// remote-loss debounce: an unanswered remote probe marks the row while the
// debounce keeps preserving its last-known liveness underneath. These tests pin
// the flag's set/clear discipline against the same probe shapes the debounce
// tests drive.

// TestRefreshStatuses_UnansweredProbeSetsUnreachableFlag pins the pair: one
// failing tick flags the row (the flag is an observation, not a verdict — it
// does not wait for the debounce's durability), while last-known liveness is
// preserved underneath exactly as before.
func TestRefreshStatuses_UnansweredProbeSetsUnreachableFlag(t *testing.T) {
	withRemoteLossThresholds(t, 3, time.Minute, time.Second)
	advance := withFrozenClock(t)
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerStartedRemote(t, manager, repoID, repoPath, "remote-outage", "http://127.0.0.1:1", session.Running)

	manager.RefreshStatuses()

	flag := inst.UnreachableSnapshot()
	if flag == nil {
		t.Fatal("one unanswered probe must set the unreachable flag — the outage is observable fact even before it is durable")
	}
	if flag.Since.IsZero() || flag.Error == "" {
		t.Fatalf("flag = %+v, want a populated since/error pair", flag)
	}
	if strings.Contains(flag.Error, "\n") || len(flag.Error) > 600 {
		t.Fatalf("flag error %q is not the bounded, single-line projection the rail renders", flag.Error)
	}
	if got := inst.GetLiveness(); got != session.LiveRunning {
		t.Fatalf("liveness = %v, want LiveRunning — unreachable preserves last-known state underneath", got)
	}
	// The flag suppresses destructive verbs while Kill stays reachable.
	if got := inst.LifecycleAction(); got != session.LifecycleActionNone {
		t.Fatalf("LifecycleAction = %v, want None while the outage lasts", got)
	}
	if !inst.CanKill() {
		t.Fatal("CanKill = false: unreachable must keep the kill escape hatch")
	}

	// The flag rides the durable-loss episode too: after the debounce's
	// confirmation also goes unanswered, the flag still stands beside the
	// preserved liveness.
	driveDurableRemoteLoss(manager, advance)
	if inst.UnreachableSnapshot() == nil {
		t.Fatal("the flag must survive the whole outage, not only its first tick")
	}
	if got := inst.GetLiveness(); got != session.LiveRunning {
		t.Fatalf("liveness = %v, want LiveRunning after durable-but-unanswerable probes", got)
	}
}

// TestRefreshStatuses_AnsweredProbeClearsUnreachable pins the retirement edge:
// the next probe that gets an answer — of any kind — retires the flag under
// the observation generation gate.
func TestRefreshStatuses_AnsweredProbeClearsUnreachable(t *testing.T) {
	withRemoteLossThresholds(t, 3, time.Minute, time.Second)
	var healthy atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if healthy.Load() {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"updated": true, "has_prompt": false, "content": "working"},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"message": "capture failed"},
		})
	}))
	defer srv.Close()

	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerStartedRemote(t, manager, repoID, repoPath, "remote-heals", srv.URL, session.Running)

	// Seed the outage directly — the flag's own set path is covered by the
	// dead-port test above; this test isolates the retirement edge.
	inst.SetUnreachable(errors.New("dial tcp: connection refused"))
	if inst.UnreachableSnapshot() == nil {
		t.Fatal("setup: flag not set")
	}

	// A SUCCESSFUL Snapshot is the answered probe that ends the outage.
	healthy.Store(true)
	manager.RefreshStatuses()

	if inst.UnreachableSnapshot() != nil {
		t.Fatal("an answered Snapshot must retire the unreachable flag under the generation gate")
	}
}

// TestRefreshStatuses_NotProvisionedSentinelDoesNotSetUnreachable pins the
// exclusion: ErrRemoteSandboxNotProvisioned is definitive absence — af KNOWS
// there is no runtime to reach — so it must not be reported as an outage.
// A remote-workspace instance with no live client is exactly that shape: its
// AgentServer is the dead-remote sentinel (#2005).
func TestRefreshStatuses_NotProvisionedSentinelDoesNotSetUnreachable(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "remote-inert", Path: repoPath, Program: "claude",
	})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	inst.SetBackend(&remoteWorkspaceBackend{FakeBackend: session.NewFakeBackend()})
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)
	seedDiskInstance(t, repoID, "remote-inert", repoPath)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(repoID, "remote-inert")] = inst
	manager.mu.Unlock()

	manager.RefreshStatuses()

	if inst.UnreachableSnapshot() != nil {
		t.Fatalf("flag = %+v — the not-provisioned sentinel is definitive absence, not an outage, and must never raise it", inst.UnreachableSnapshot())
	}
}

// TestRestoreSession_FlaggedLiveRowRefusesWithOutage pins the refusal wording:
// restoring a still-live flagged row must name the outage and the remedy
// rather than the generic "not archived, lost, or dead" — the session may be
// perfectly healthy behind a broken transport, so restore neither helps nor is
// needed.
func TestRestoreSession_FlaggedLiveRowRefusesWithOutage(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerStartedRemote(t, manager, repoID, repoPath, "flagged-live", "http://127.0.0.1:1", session.Running)
	inst.SetUnreachable(errors.New("ssh: connection reset"))

	_, _, err := manager.RestoreSession(RestoreSessionRequest{Title: "flagged-live", RepoID: repoID})
	if err == nil {
		t.Fatal("restore on a flagged live row must refuse")
	}
	if !strings.Contains(err.Error(), "unreachable for") {
		t.Fatalf("refusal = %q, want it to name the outage's age", err)
	}
	if !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("refusal = %q, want it to carry the stored transport error", err)
	}
	if strings.Contains(err.Error(), "not archived, lost, or dead") {
		t.Fatalf("refusal = %q — the generic liveness refusal misdiagnoses an outage", err)
	}
}

// TestRestoreSession_LostUnreachableRefusalNamesOutage pins the
// Lost+unreachable pair: the row says "marked Lost, and the mark cannot be
// re-confirmed", so the probeUnknown refusal carries the flag's since and
// error rather than the bare indeterminate message.
func TestRestoreSession_LostUnreachableRefusalNamesOutage(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerStartedRemote(t, manager, repoID, repoPath, "lost-outage", "http://127.0.0.1:1", session.Lost)
	inst.SetUnreachable(errors.New("tunnel died"))

	_, _, err := manager.RestoreSession(RestoreSessionRequest{Title: "lost-outage", RepoID: repoID})
	if err == nil {
		t.Fatal("restore past an unanswerable probe must refuse — unreachable is not dead")
	}
	if !strings.Contains(err.Error(), "unreachable for") || !strings.Contains(err.Error(), "tunnel died") {
		t.Fatalf("refusal = %q, want the flag's since and last error named", err)
	}
	if !strings.Contains(err.Error(), "recoverable") {
		t.Fatalf("refusal = %q, want the it-stays-recoverable guidance preserved", err)
	}
}

// TestRestoreSession_SlowAliveAnswerHealsWithOperatorBudget is the #5203 pin:
// the manual restore's pre-replacement probe gets the operator budget
// (remoteConfirmProbeTimeout), not the poll loop's 5s tie-break budget, so a
// remote that answers alive — just slowly — heals instead of being refused as
// unanswerable.
func TestRestoreSession_SlowAliveAnswerHealsWithOperatorBudget(t *testing.T) {
	const slowAnswer = 7 * time.Second // > remoteLostConfirmTimeout (5s), well under remoteConfirmProbeTimeout (35s)
	if remoteConfirmProbeTimeout <= slowAnswer {
		t.Fatalf("test premise broken: operator budget %s no longer exceeds the slow answer %s", remoteConfirmProbeTimeout, slowAnswer)
	}
	if remoteLostConfirmTimeout >= slowAnswer {
		t.Fatalf("test premise broken: poll budget %s already exceeds the slow answer — this test proves nothing", remoteLostConfirmTimeout)
	}
	var aliveCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/agent/alive":
			aliveCalls.Add(1)
			time.Sleep(slowAnswer)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"alive": true}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"updated": false, "has_prompt": false, "content": ""},
			})
		}
	}))
	defer srv.Close()

	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerStartedRemote(t, manager, repoID, repoPath, "slow-alive", srv.URL, session.Lost)
	// The flag must retire on the same answered probe that heals the mark.
	inst.SetUnreachable(errors.New("dial tcp: connection refused"))

	_, _, err := manager.RestoreSession(RestoreSessionRequest{Title: "slow-alive", RepoID: repoID})
	if err != nil {
		t.Fatalf("RestoreSession = %v — a sandbox that answers alive, however slowly, must heal rather than refuse or re-provision", err)
	}
	if aliveCalls.Load() == 0 {
		t.Fatal("the liveness probe never reached the sandbox")
	}
	if got := inst.GetLiveness(); got != session.LiveRunning {
		t.Fatalf("liveness = %v, want LiveRunning — the answered-alive probe heals the stale Lost mark", got)
	}
	if inst.UnreachableSnapshot() != nil {
		t.Fatal("the answered-alive probe is transport evidence — the outage flag must retire with the heal")
	}
}

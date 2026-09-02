package daemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/session"
)

// The #3586 regression lock: on the manual Lost/Dead restore path the
// OpRestoring fence must be COEXTENSIVE with the claim that makes Kill fail.
//
// claimRestoreOperation sets killsInFlight the moment the restore is admitted, so
// from that instant a Kill is rejected at the daemon's ADMISSION gate with
// "kill already in progress for session X" — the misleading message #3533 was
// filed about. canKillFor is what hides the affordance, and it reads the op axis
// alone. Raising OpRestoring only around the backend call left the row at
// {LiveLost, OpNone} for the two network phases in front of it — a 5s liveness
// probe and a push bounded at 3m30s — so for up to ~3m35s the TUI and the web UI
// offered a Kill that could only fail. #3534 fixed the fenced suffix; these pin
// the prefix, which is the slow part.
//
// Every remote case below asserts BOTH halves, because the fix has two ways to be
// wrong and the second is worse than the bug: the fence must be UP at the probe
// (or Kill stays advertised) and DOWN on the way out of every early return (or the
// row is permanently busy — the poll skips it forever and every runtime action
// refuses it as in-flight).
//
// Both surfaces are asserted deliberately: Instance.CanKill() is what ui/menu.go
// and app/handle_actions.go ask, and InstanceData.CanKill is the projected
// can_kill the web client reads (web/src/ui.ts isKillableSession). A fence that
// only reached one of them would leave the other advertising Kill.

// probeAnswer is what the stand-in sandbox replies to /v1/agent/alive, which is
// what selects the restore arm under test.
type probeAnswer int

const (
	probeAnswersAlive probeAnswer = iota // heal arm
	probeAnswersDead                     // preserve-then-reap arm
	probeFails                           // transport error: the indeterminate arm
)

// observingSandbox is an in-sandbox agent-server stand-in that records the Kill
// affordance AS THE DAEMON SEES IT during the liveness probe. Observing from
// inside the handler is what makes these assertions land in the window the bug is
// about — the probe is the first of the two network phases the old fence did not
// cover — without every case having to park a goroutine.
type observingSandbox struct {
	srv *httptest.Server

	mu             sync.Mutex
	inst           *session.Instance
	probed         bool
	canKill        bool
	canKillProject bool

	entered  chan struct{}
	release  chan struct{}
	gate     bool
	once     sync.Once
	enterOne sync.Once
}

// newObservingSandbox serves alive/archive for one instance. When gate is true
// the alive handler parks until releaseProbe, so the test's own goroutine can
// assert mid-operation — the most faithful stand-in for a user reaching for Kill
// while the restore is still running.
func newObservingSandbox(t *testing.T, answer probeAnswer, gate bool) *observingSandbox {
	t.Helper()
	s := &observingSandbox{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		gate:    gate,
	}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/agent/alive":
			s.enterOne.Do(func() { close(s.entered) })
			s.mu.Lock()
			i := s.inst
			s.mu.Unlock()
			canKill, projected := i.CanKill(), i.ToInstanceData().CanKill
			s.mu.Lock()
			s.probed = true
			s.canKill = canKill
			s.canKillProject = projected
			s.mu.Unlock()
			if s.gate {
				<-s.release
			}
			switch answer {
			case probeAnswersAlive:
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"alive": true}})
			case probeAnswersDead:
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"alive": false}})
			case probeFails:
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "sandbox unreachable"}})
			}
		case "/v1/agent/archive":
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "push rejected"}})
		default:
			http.NotFound(w, r)
		}
	}))
	// Releasing before Close matters: httptest.Server.Close waits for in-flight
	// handlers, so a test that fails its assertion and returns would otherwise
	// deadlock in cleanup instead of reporting the failure.
	t.Cleanup(func() {
		s.releaseProbe()
		s.srv.Close()
	})
	return s
}

// watch binds the instance the handler reports on. Called before the restore
// starts, and read back under the same mutex the observations use, so the
// handler goroutine never races the test's own assignment.
func (s *observingSandbox) watch(inst *session.Instance) *session.Instance {
	s.mu.Lock()
	s.inst = inst
	s.mu.Unlock()
	return inst
}

func (s *observingSandbox) releaseProbe() { s.once.Do(func() { close(s.release) }) }

func (s *observingSandbox) awaitProbe(t *testing.T) {
	t.Helper()
	select {
	case <-s.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the manual restore never reached the remote liveness probe")
	}
}

// assertKillWasHiddenAtTheProbe reads back what the daemon advertised while the
// restore was blocked in its first network phase. This is the #3586 assertion.
func (s *observingSandbox) assertKillWasHiddenAtTheProbe(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	probed, canKill, projected := s.probed, s.canKill, s.canKillProject
	s.mu.Unlock()
	if !probed {
		t.Fatal("the restore never reached the liveness probe, so it never entered the window under test")
	}
	if canKill {
		t.Fatal("Instance.CanKill() was true during the manual restore's liveness probe: the restore " +
			"claim already refuses Kill at the admission gate, so the TUI menu offered a Kill whose only " +
			"outcome is the misleading \"kill already in progress\" refusal (#3533/#3586)")
	}
	if projected {
		t.Fatal("the projected can_kill was true during the manual restore's liveness probe: the web " +
			"client rendered a Kill control for a session whose restore had already claimed it (#3533/#3586)")
	}
}

// assertKillHidden is the same check taken from the test's own goroutine, i.e.
// from a client asking right now.
func assertKillHidden(t *testing.T, inst *session.Instance, when string) {
	t.Helper()
	if inst.CanKill() {
		t.Fatalf("Instance.CanKill() is true %s: the restore claim already refuses Kill at the "+
			"admission gate, so the TUI menu offers a Kill whose only outcome is the misleading "+
			"\"kill already in progress\" refusal (#3533/#3586). In-flight op is %v",
			when, inst.GetInFlightOp())
	}
	if inst.ToInstanceData().CanKill {
		t.Fatalf("the projected can_kill is true %s: the web client renders a Kill control for a "+
			"session whose restore has already claimed it (#3533/#3586). In-flight op is %v",
			when, inst.GetInFlightOp())
	}
}

// assertFenceLowered is the other half of every early return. A fence raised for
// the whole operation must come back down on the way out, whatever the outcome.
func assertFenceLowered(t *testing.T, inst *session.Instance, after string) {
	t.Helper()
	if op := inst.GetInFlightOp(); op != session.OpNone {
		t.Fatalf("in-flight op = %v after %s, want OpNone: the restore fence was left raised, so the "+
			"row is permanently busy and no poll, restore, kill or archive can touch it again", op, after)
	}
	if !inst.CanKill() {
		t.Fatalf("Instance.CanKill() is still false after %s: the row can no longer be removed", after)
	}
	if !inst.ToInstanceData().CanKill {
		t.Fatalf("the projected can_kill is still false after %s: the web client cannot remove the row", after)
	}
}

// TestRestoreSession_KillIsHiddenFromTheLivenessProbeOnward is #3586 asserted
// where it reproduces, with the restore genuinely parked in its probe and the
// assertion made from outside — a user pressing Kill at that instant.
//
// The probe answers alive once released, so the run also pins the heal arm's
// release and its ordering: the row must settle at {LiveRunning, OpNone}, never at
// LiveRunning with the fence still up.
func TestRestoreSession_KillIsHiddenFromTheLivenessProbeOnward(t *testing.T) {
	// A generous confirm budget so the probe's own timeout cannot end the window
	// the assertions run in — the test, not the clock, decides when it releases.
	withRemoteLossThresholds(t, 3, time.Minute, 30*time.Second)

	manager, repoID, repoPath := newStatusTestManager(t)
	sandbox := newObservingSandbox(t, probeAnswersAlive, true)
	inst, backend := registerStartedRemote(t, manager, repoID, repoPath, "remote-fenced", sandbox.srv.URL, session.Lost)
	sandbox.watch(inst)

	if !inst.CanKill() {
		t.Fatal("setup: a plain Lost row must advertise Kill before any restore claims it")
	}

	done := make(chan error, 1)
	go func() {
		_, _, err := manager.RestoreSession(RestoreSessionRequest{Title: "remote-fenced", RepoID: repoID})
		done <- err
	}()

	sandbox.awaitProbe(t)
	assertKillHidden(t, inst, "while the manual restore is blocked on its remote liveness probe")

	sandbox.releaseProbe()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RestoreSession: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("RestoreSession never returned after the probe was released")
	}

	if got := backend.recoverCalls(); got != 0 {
		t.Fatalf("Recover calls = %d, want 0 — the sandbox answered alive, so the row heals in place", got)
	}
	if got := inst.GetLiveness(); got != session.LiveRunning {
		t.Fatalf("liveness = %v, want LiveRunning: lowering the fence must not clobber the heal arm's transition", got)
	}
	assertFenceLowered(t, inst, "a restore that healed the row to LiveRunning")
}

// TestRestoreSession_ProbeUnknownRefusalLowersTheFence covers the first early
// return past the raise: an unreachable sandbox is refused (unreachable is not
// gone), and the row must be left exactly as restorable as it was.
func TestRestoreSession_ProbeUnknownRefusalLowersTheFence(t *testing.T) {
	withRemoteLossThresholds(t, 3, time.Minute, 5*time.Second)
	manager, repoID, repoPath := newStatusTestManager(t)
	sandbox := newObservingSandbox(t, probeFails, false)
	inst, _ := registerStartedRemote(t, manager, repoID, repoPath, "remote-unknown", sandbox.srv.URL, session.Lost)
	sandbox.watch(inst)

	if _, _, err := manager.RestoreSession(RestoreSessionRequest{Title: "remote-unknown", RepoID: repoID}); err == nil {
		t.Fatal("manual restore accepted an indeterminate probe")
	}
	sandbox.assertKillWasHiddenAtTheProbe(t)
	if got := inst.GetLiveness(); got != session.LiveLost {
		t.Fatalf("liveness = %v, want LiveLost after a refusal", got)
	}
	assertFenceLowered(t, inst, "the probeUnknown refusal")
	if err := inst.ValidateRuntimeAction(session.RuntimeActionRestoreLostOrDead); err != nil {
		t.Fatalf("the refused row is no longer restorable: %v", err)
	}
}

// TestRestoreSession_ForcedIndeterminateBranchRefusalLowersTheFence covers the
// requireDurableSandboxBranch refusal reached through the FORCED probeUnknown arm.
func TestRestoreSession_ForcedIndeterminateBranchRefusalLowersTheFence(t *testing.T) {
	withRemoteLossThresholds(t, 3, time.Minute, 5*time.Second)
	manager, repoID, repoPath := newStatusTestManager(t)
	sandbox := newObservingSandbox(t, probeFails, false)
	inst, _ := registerStartedRemote(t, manager, repoID, repoPath, "remote-forced-unknown", sandbox.srv.URL, session.Lost)
	sandbox.watch(inst)
	if inst.GetBranch() != "" {
		t.Fatalf("setup: branch = %q, want empty so the durable-branch guard is the refusal under test", inst.GetBranch())
	}

	_, _, err := manager.RestoreSession(RestoreSessionRequest{Title: "remote-forced-unknown", RepoID: repoID, ForceReap: true})
	if err == nil {
		t.Fatal("--force-reap past an indeterminate probe replaced a sandbox whose branch is unknown")
	}
	sandbox.assertKillWasHiddenAtTheProbe(t)
	assertFenceLowered(t, inst, "the forced-indeterminate branch refusal")
}

// TestRestoreSession_ForcedAnsweredDeadBranchRefusalLowersTheFence covers the same
// refusal reached through the FORCED probeAnsweredDead arm, which skips the push
// entirely and therefore relies on the branch already being durable.
func TestRestoreSession_ForcedAnsweredDeadBranchRefusalLowersTheFence(t *testing.T) {
	withRemoteLossThresholds(t, 3, time.Minute, 5*time.Second)
	manager, repoID, repoPath := newStatusTestManager(t)
	sandbox := newObservingSandbox(t, probeAnswersDead, false)
	inst, _ := registerStartedRemote(t, manager, repoID, repoPath, "remote-forced-dead", sandbox.srv.URL, session.Lost)
	sandbox.watch(inst)
	if inst.GetBranch() != "" {
		t.Fatalf("setup: branch = %q, want empty so the durable-branch guard is the refusal under test", inst.GetBranch())
	}

	_, _, err := manager.RestoreSession(RestoreSessionRequest{Title: "remote-forced-dead", RepoID: repoID, ForceReap: true})
	if err == nil {
		t.Fatal("--force-reap replaced an answered-dead sandbox whose branch is unknown")
	}
	sandbox.assertKillWasHiddenAtTheProbe(t)
	assertFenceLowered(t, inst, "the forced answered-dead branch refusal")
}

// TestRestoreSession_PreserveFailureLowersTheFence covers the longest phase the old
// fence did not cover: the pre-reap push, bounded at 3m30s. When it fails the
// restore refuses, and the row must be left recoverable rather than busy forever.
func TestRestoreSession_PreserveFailureLowersTheFence(t *testing.T) {
	withRemoteLossThresholds(t, 3, time.Minute, 5*time.Second)
	manager, repoID, repoPath := newStatusTestManager(t)
	sandbox := newObservingSandbox(t, probeAnswersDead, false)
	inst, backend := registerStartedRemote(t, manager, repoID, repoPath, "remote-push-failed", sandbox.srv.URL, session.Lost)
	sandbox.watch(inst)

	_, _, err := manager.RestoreSession(RestoreSessionRequest{Title: "remote-push-failed", RepoID: repoID})
	if err == nil {
		t.Fatal("manual restore reaped a reachable sandbox whose pre-reap push failed")
	}
	sandbox.assertKillWasHiddenAtTheProbe(t)
	if got := backend.recoverCalls(); got != 0 {
		t.Fatalf("Recover calls = %d, want 0 — a failed push must stop the replacement", got)
	}
	assertFenceLowered(t, inst, "the preserveSandboxBeforeReap failure")
}

// failingRecoverBackend is a local backend whose Recover fails after the fence is
// already up — the last early return, and the one the fence used to own itself.
type failingRecoverBackend struct {
	*session.FakeBackend
}

func (b *failingRecoverBackend) Recover(*session.Instance) error {
	return errors.New("spawn refused")
}

// TestRestoreSession_RecoverFailureLowersTheFence pins the path that already
// worked before #3586: RecoverFencedWithLiveBoundary lowered the fence itself when
// the backend failed. The caller now owns the fence for the whole operation, so
// this asserts that release did not go missing in the move.
func TestRestoreSession_RecoverFailureLowersTheFence(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &failingRecoverBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "local-recover-fails", backend, true, session.Lost)

	_, events := manager.events.subscribe()
	if _, _, err := manager.RestoreSession(RestoreSessionRequest{Title: "local-recover-fails", RepoID: repoID}); err == nil {
		t.Fatal("RestoreSession reported success for a Recover that failed")
	}
	assertFenceLowered(t, inst, "a failed Recover")

	if fenced := drainNextSessionEvent(t, events, agentproto.EventSessionUpdated); fenced.InFlightOp != session.OpRestoring {
		t.Fatalf("first session.updated in-flight op = %v, want OpRestoring", fenced.InFlightOp)
	}
	// The outcome a watching client most needs: the restore failed and the row is
	// free again. Lowering the fence before the failure bookkeeping would have
	// silenced this, since nothing in that bookkeeping publishes.
	released := drainNextSessionEvent(t, events, agentproto.EventSessionUpdated)
	if released.InFlightOp != session.OpNone {
		t.Fatalf("last session.updated in-flight op = %v, want OpNone: a failed restore ended without telling "+
			"anyone, so every client keeps rendering it as in flight", released.InFlightOp)
	}
	if !released.CanKill {
		t.Fatal("the release announcement carries can_kill=false, so the failed row stays unremovable on every client")
	}
}

type fenceObservingBackend struct {
	*session.FakeBackend
	inst     *session.Instance
	sawFence bool
}

func (b *fenceObservingBackend) Recover(inst *session.Instance) error {
	b.sawFence = !b.inst.CanKill() && !b.inst.ToInstanceData().CanKill
	inst.SetStatusForTest(session.Running)
	return nil
}

// TestRestoreSession_LocalRestoreHidesKillForTheWholeOperation is the same
// invariant on the LOCAL path. A local session has no remote sandbox, so
// remoteSandboxLiveness short-circuits to probeAbsent and there is no network
// phase between the claim and the backend call — which is why this one held
// before #3586 too. It is here so the local path cannot lose the property while
// the remote one is being fixed, and so the successful-restore release is pinned.
func TestRestoreSession_LocalRestoreHidesKillForTheWholeOperation(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	observed := &fenceObservingBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "local-fenced", observed, true, session.Lost)
	observed.inst = inst

	if _, _, err := manager.RestoreSession(RestoreSessionRequest{Title: "local-fenced", RepoID: repoID}); err != nil {
		t.Fatalf("RestoreSession: %v", err)
	}
	if !observed.sawFence {
		t.Fatal("the backend's Recover ran with Kill still advertised: the fence must be up before " +
			"anything the restore does, not raised around the backend call alone (#3586)")
	}
	assertFenceLowered(t, inst, "a successful local restore")
}

// The two announcements. A fence the events plane never mentions is invisible to
// every client that did not initiate the restore — an `af sessions restore` or a
// web restore leaves an already-open TUI or browser offering Kill for the whole
// operation, which is the bug seen from the client rather than from the row.
// #2997 settled this contract for the sibling limit resume; the restore path owes
// the same pair: busy, then released.
func TestRestoreSession_HealArmAnnouncesTheFenceAndItsRelease(t *testing.T) {
	withRemoteLossThresholds(t, 3, time.Minute, 5*time.Second)
	manager, repoID, repoPath := newStatusTestManager(t)
	sandbox := newObservingSandbox(t, probeAnswersAlive, false)
	inst, _ := registerStartedRemote(t, manager, repoID, repoPath, "remote-heal-events", sandbox.srv.URL, session.Lost)
	sandbox.watch(inst)

	_, events := manager.events.subscribe()
	if _, _, err := manager.RestoreSession(RestoreSessionRequest{Title: "remote-heal-events", RepoID: repoID}); err != nil {
		t.Fatalf("RestoreSession: %v", err)
	}

	fenced := drainNextSessionEvent(t, events, agentproto.EventSessionUpdated)
	if fenced.InFlightOp != session.OpRestoring {
		t.Fatalf("first session.updated in-flight op = %v, want OpRestoring: clients must be told the row is busy "+
			"BEFORE the network phases, or they keep offering a Kill that can only fail (#3586)", fenced.InFlightOp)
	}
	if fenced.CanKill {
		t.Fatal("the busy announcement still carries can_kill=true, so the web client keeps its Kill control")
	}

	// The settled announcement must not carry the fence. The heal arm lowers before
	// its settlement precisely so this payload reads as a finished restore rather
	// than one still in flight.
	settled := drainNextSessionEvent(t, events, agentproto.EventSessionUpdated)
	if settled.InFlightOp != session.OpNone {
		t.Fatalf("settled session.updated in-flight op = %v, want OpNone: the heal arm published its terminal "+
			"state with the fence still up, so every client shows a restore that is over", settled.InFlightOp)
	}
	if settled.Liveness != session.LiveRunning {
		t.Fatalf("settled session.updated liveness = %v, want LiveRunning", settled.Liveness)
	}
	if !settled.CanKill {
		t.Fatal("the settled announcement carries can_kill=false, so no client can remove the healed row")
	}
}

// The refusal side of the same contract. preserveSandboxBeforeReap publishes a
// FENCED settlement mid-flight, so a refusal that lowered the fence silently would
// leave that as the last word every client has on the row.
func TestRestoreSession_RefusalAnnouncesTheReleasedRow(t *testing.T) {
	withRemoteLossThresholds(t, 3, time.Minute, 5*time.Second)
	manager, repoID, repoPath := newStatusTestManager(t)
	sandbox := newObservingSandbox(t, probeAnswersDead, false)
	inst, _ := registerStartedRemote(t, manager, repoID, repoPath, "remote-refusal-events", sandbox.srv.URL, session.Lost)
	sandbox.watch(inst)

	_, events := manager.events.subscribe()
	if _, _, err := manager.RestoreSession(RestoreSessionRequest{Title: "remote-refusal-events", RepoID: repoID}); err == nil {
		t.Fatal("manual restore reaped a reachable sandbox whose pre-reap push failed")
	}

	fenced := drainNextSessionEvent(t, events, agentproto.EventSessionUpdated)
	if fenced.InFlightOp != session.OpRestoring {
		t.Fatalf("first session.updated in-flight op = %v, want OpRestoring", fenced.InFlightOp)
	}

	released := drainNextSessionEvent(t, events, agentproto.EventSessionUpdated)
	if released.InFlightOp != session.OpNone {
		t.Fatalf("last session.updated in-flight op = %v, want OpNone: the refusal ended the operation without "+
			"telling anyone, so every client is left rendering a restore that is no longer running", released.InFlightOp)
	}
	if released.Liveness != session.LiveLost {
		t.Fatalf("released session.updated liveness = %v, want LiveLost — a refused restore leaves the row lost", released.Liveness)
	}
	if !released.CanKill {
		t.Fatal("the release announcement carries can_kill=false, so the row stays unremovable on every client")
	}
}

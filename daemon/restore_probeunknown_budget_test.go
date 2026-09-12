package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// probeUnknownForceReapServer is the remote agent-server stand-in for the
// probeUnknown force-reap budget reset invariant — the symmetric gap the
// probeAnsweredDead force arm was fixed for but the probeUnknown force arm was
// not.
//
//   - /v1/agent/snapshot reports a clean idle pane, so RefreshStatuses asks the
//     alive probe (an idle Snapshot is the ambiguous shape a healthy idle
//     session and a dead one both produce).
//   - /v1/agent/alive drives the arm switch. It answers alive=false (the ANSWERED
//     death of the probeAnsweredDead arm) until flipUnknown() flips it to a 503
//     transport failure, which aliveWithin reports as probeUnknown — the arm
//     this test exists to drive at force-reap time.
//   - /v1/agent/archive is the pre-reap push whose success records the sandbox
//     branch durably (the durable branch requireDurableSandboxBranch later
//     reads) and whose persistent failure drives the preserve-push give-up.
//     archiveOK toggles between the two so a test can record the branch on a
//     first pass, then revoke origin auth and drive the give-up.
//
// /v1/agent/archive failures return a JSON error envelope (not a bare 500) so
// the remote client surfaces a real message inside the refusal error rather
// than a "malformed response envelope" decode error — the same shape a real
// agent-server returns when origin rejects the push.
type probeUnknownForceReapServer struct {
	mu        sync.Mutex
	archiveOK bool
	// unknownAlive, when set, makes /v1/agent/alive answer 503 (transport failure)
	// instead of its default alive=false — the probeUnknown posture.
	unknownAlive atomic.Bool
	archiveCalls int
	// unknownAliveCalls counts the alive probes served as 503 so the test can
	// assert the manual restore's re-probe actually reached the probeUnknown
	// force arm rather than the probeAnsweredDead arm that already resets.
	unknownAliveCalls int
}

func (s *probeUnknownForceReapServer) setArchiveOK(ok bool) {
	s.mu.Lock()
	s.archiveOK = ok
	s.mu.Unlock()
}

func (s *probeUnknownForceReapServer) flipUnknown() { s.unknownAlive.Store(true) }

func (s *probeUnknownForceReapServer) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.archiveCalls
}

func (s *probeUnknownForceReapServer) unknownAliveProbeCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unknownAliveCalls
}

func newProbeUnknownForceReapServer(t *testing.T, archiveOK bool) (*probeUnknownForceReapServer, string) {
	t.Helper()
	srv := &probeUnknownForceReapServer{archiveOK: archiveOK}
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/agent/snapshot":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"updated": false, "has_prompt": false, "content": ""},
			})
		case "/v1/agent/alive":
			if srv.unknownAlive.Load() {
				srv.mu.Lock()
				srv.unknownAliveCalls++
				srv.mu.Unlock()
				http.Error(w, "transport unavailable", http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"alive": false}})
		case "/v1/agent/archive":
			srv.mu.Lock()
			srv.archiveCalls++
			ok := srv.archiveOK
			srv.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]any{"message": "origin auth revoked: archive push rejected"},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"branch": "af/fixture-branch"}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(httpSrv.Close)
	return srv, httpSrv.URL
}

// TestRestoreLostSessions_ProbeUnknownForceReapMissingBudgetReset pins the
// invariant the probeAnsweredDead force arm was fixed for, applied to its
// symmetric probeUnknown force arm. Both arms run the same force-replace-then-
// Recover shape (requireDurableSandboxBranch, skip the push, fall through to
// Recover), so both inherit the same stale-budget hazard: after the automatic
// loop's preserve-push give-up sets st.consecutiveFailures = lostRestoreMaxAttempts,
// a force-reap that replaces the sandbox must give the new one a fresh Recover
// budget rather than charging its first Recover failure as attempt
// maxAttempts+1 and immediately giving up.
//
// Without the reset on the probeUnknown force arm: consecutiveFailures stays at
// lostRestoreMaxAttempts across the replacement, the first Recover failure
// against the brand-new sandbox increments to maxAttempts+1, the terminal give-up
// branch fires, and inst.SetLostRestoreFailure(attempts, recoverErr) OVERWRITES
// the prior durable LostRestoreFailure (the preserve-push give-up reason) with
// the new sandbox's provision error at a wrong attempt count — persisted to
// instances.json and shown to operators. The force-reap also logs
// "giving up after N attempts" while the probeAnsweredDead force arm logs
// "attempt 1, retrying in", so the same operation produces divergent
// operator-facing output depending on which probe verdict the unreachable
// sandbox happened to return.
//
// With the reset: consecutiveFailures is zeroed, the first Recover failure
// starts at attempt 1 with a full budget and logs a retry promise, and the
// prior durable LostRestoreFailure survives unchanged.
//
// The scenario produces a durably-recorded branch WITHOUT test injection: the
// automatic loop's first pass pushes successfully (archive lands ->
// preserveSandboxBeforeReap records the branch on disk), then Recover fails
// once; origin auth is then revoked and the remaining preserve-push failures
// drive the give-up. The durable branch from the first pass is what
// requireDurableSandboxBranch reads when the operator later force-reaps past
// the indeterminate probe.
func TestRestoreLostSessions_ProbeUnknownForceReapMissingBudgetReset(t *testing.T) {
	withRemoteLossThresholds(t, 3, time.Minute, time.Second)
	zeroRestoreBackoff(t)
	manager, ownLogs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)

	const title = "remote-probeunknown-force-reap"
	// The archive push starts OK so the first automatic pass records the
	// sandbox branch durably, then fails for every subsequent pass to drive
	// the preserve-push give-up.
	srv, url := newProbeUnknownForceReapServer(t, true /* archivelands on pass 1 */)
	recoverErr := errors.New("recover: provision failed on new sandbox")
	inst, backend := registerStartedRemoteTask(t, manager, repoID, repoPath, title, url, session.Running)
	backend.failWith = recoverErr

	// Drive to a Lost observation: the sandbox answers alive=false (answered
	// dead), which is authoritative in one tick.
	manager.RefreshStatuses()
	if got := inst.GetLiveness(); got != session.LiveLost {
		t.Fatalf("setup: liveness = %v, want LiveLost after the answered-dead probe", got)
	}

	// Pass 1: the pre-reap push lands, preserving the sandbox's work and
	// RECORDING its branch durably (the durable branch the force-reap later
	// relies on, produced here without test injection); Recover then fails
	// once, charging consecutiveFailures to 1.
	manager.RestoreLostSessions()
	if got, want := srv.calls(), 1; got != want {
		t.Fatalf("after pass 1: archive calls = %d, want %d (the first pass must push to record the branch)", got, want)
	}
	if got := backend.recoverCalls(); got != 1 {
		t.Fatalf("after pass 1: Recover calls = %d, want 1 (the first pass reaches Recover, which fails)", got)
	}
	// The durable branch is now on disk, written by preserveSandboxBeforeReap.
	rec := recordFor(t, repoID, title)
	if rec == nil {
		t.Fatal("durable record missing after the first successful archive push")
	}
	if rec.Branch != "af/fixture-branch" {
		t.Fatalf("durable branch = %q, want %q — the first archive push must record the branch on disk without test injection", rec.Branch, "af/fixture-branch")
	}

	// Revoke origin auth: every subsequent pre-reap push refuses, which the
	// probeAnsweredDead arm charges to its OWN preserve-push budget.
	srv.setArchiveOK(false)

	// Drive the remaining preserve-push failures to the durable give-up. After
	// this, LostRestoreGaveUp is true and st.consecutiveFailures ==
	// lostRestoreMaxAttempts (the preserve give-up assignment).
	for i := 0; i < lostRestoreMaxAttempts; i++ {
		manager.RestoreLostSessions()
	}
	view := inst.LifecycleView()
	if !view.LostRestoreGaveUp {
		t.Fatal("setup: LostRestoreGaveUp = false after the preserve-push give-up passes: the preserve arm did not reach terminal state")
	}
	snapshot := inst.LostRestoreFailureSnapshot()
	if snapshot == nil || snapshot.Attempts != lostRestoreMaxAttempts {
		t.Fatalf("setup: durable failure = %#v, want %d attempts (the preserve-push give-up)", snapshot, lostRestoreMaxAttempts)
	}
	if !strings.Contains(snapshot.Error, "refusing to replace the sandbox") {
		t.Fatalf("setup: durable failure error = %q, want the preserve-push refusal reason", snapshot.Error)
	}

	// Reset the captured logs so the assertions below describe ONLY the
	// force-reap phase and are not padded by the automatic loop's give-up line.
	ownLogs.errors.Reset()
	ownLogs.warnings.Reset()
	ownLogs.info.Reset()

	// Flip the probe to unreachable (transport failure -> probeUnknown). This
	// is the crux: the manual restore's re-probe must land on the probeUnknown
	// force arm, the symmetric arm that was missing the reset, NOT the
	// probeAnsweredDead force arm that already resets.
	srv.flipUnknown()
	archiveCallsBefore := srv.calls()

	// Force-reap past the indeterminate probe: the probeUnknown force arm runs
	// requireDurableSandboxBranch (passes on the branch recorded in pass 1),
	// skips the push, and falls through to Recover — which fails once.
	_, _, err := manager.RestoreSession(RestoreSessionRequest{
		Title: title, RepoID: repoID, ForceReap: true,
	})
	if err == nil {
		t.Fatal("force-reap RestoreSession returned nil error, expected Recover failure to propagate")
	}

	// The manual restore's re-probe actually reached the probeUnknown arm: the
	// alive endpoint was served the 503 transport-failure posture at least once
	// after the flip. Without this guard the test could pass by routing through
	// the probeAnsweredDead force arm that already resets.
	if got := srv.unknownAliveProbeCalls(); got == 0 {
		t.Fatal("the manual restore never served a 503 alive probe: it did not exercise the probeUnknown force arm")
	}

	// The probeUnknown force arm skips the push (there is no sandbox to reach),
	// so the archive endpoint is untouched during the force-reap.
	if got := srv.calls(); got != archiveCallsBefore {
		t.Fatalf("archive calls during force-reap = %d->%d, want unchanged — the probeUnknown force arm skips the push", archiveCallsBefore, got)
	}

	// Exactly one Recover during the force-reap (plus the one from pass 1 in
	// the automatic phase: none of the preserve-push passes reached Recover).
	if got := backend.recoverCalls(); got != 2 {
		t.Fatalf("Recover calls = %d, want 2 (1 from the first automatic pass + 1 from the force-reap)", got)
	}

	// Impact #2: the operator-facing log must NOT immediately give up. Without
	// the reset, the retained consecutiveFailures == lostRestoreMaxAttempts
	// makes lostRestoreFailed take the terminal arm on the first failure against
	// the new sandbox, logging exactly the Recover give-up string — and diverging
	// from the probeAnsweredDead force arm, which logs "attempt 1, retrying in".
	wantGiveUp := fmt.Sprintf("giving up after %d attempts", lostRestoreMaxAttempts+1)
	if strings.Contains(ownLogs.errors.String(), wantGiveUp) {
		t.Fatalf("probeUnknown force-reap Recover failure logged %q — the maxAttempts+1 give-up the probeAnsweredDead reset exists to prevent; the new sandbox must start with a fresh budget.\n"+
			"error logs:\n%s", wantGiveUp, ownLogs.errors.String())
	}
	// Verify the retry promise is actually there (attempt 1, fresh budget).
	if !strings.Contains(ownLogs.warnings.String(), "retrying in") {
		t.Fatalf("probeUnknown force-reap Recover failure did not log a retry-with-backoff message;\n"+
			"want the retry arm (fresh budget), not immediate give-up.\nwarn logs:\n%s",
			ownLogs.warnings.String())
	}

	// Impact #1: the prior durable LostRestoreFailure must survive the
	// force-reap UNCHANGED. Without the reset, the immediate give-up calls
	// inst.SetLostRestoreFailure(maxAttempts+1, recoverErr), overwriting the
	// preserve-push give-up reason with the new sandbox's provision error at a
	// wrong attempt count.
	postSnapshot := inst.LostRestoreFailureSnapshot()
	if postSnapshot == nil || postSnapshot.Attempts != lostRestoreMaxAttempts {
		t.Fatalf("probeUnknown force-reap overwrote the prior durable failure: in-memory snapshot = %#v, want Attempts=%d (the preserve-push give-up must survive the force-reap)",
			postSnapshot, lostRestoreMaxAttempts)
	}
	if !strings.Contains(postSnapshot.Error, "refusing to replace the sandbox") {
		t.Fatalf("probeUnknown force-reap overwrote the preserve-push failure reason: in-memory error = %q, want the preserve-push refusal",
			postSnapshot.Error)
	}
	if strings.Contains(postSnapshot.Error, "provision failed on new sandbox") {
		t.Fatalf("probeUnknown force-reap overwrote the durable failure with the new sandbox's provision error: %q", postSnapshot.Error)
	}
	// The durable record on disk must match: instances.json must still show the
	// preserve-push give-up, not the force-reap's provision error at attempt 7.
	rec = recordFor(t, repoID, title)
	if rec == nil {
		t.Fatal("durable record vanished after the force-reap")
	}
	if rec.LostRestoreFailure == nil || rec.LostRestoreFailure.Attempts != lostRestoreMaxAttempts ||
		!strings.Contains(rec.LostRestoreFailure.Error, "refusing to replace the sandbox") {
		t.Fatalf("persisted LostRestoreFailure overwritten by the force-reap: %#v, want Attempts=%d and the preserve-push refusal",
			rec.LostRestoreFailure, lostRestoreMaxAttempts)
	}

	// The automatic loop stays suppressed: resetRecoverBudget zeroes only the
	// in-memory consecutiveFailures counter and does NOT call
	// ClearLostRestoreFailure, so LostRestoreGaveUp stays true either way (this
	// bug is about the manual force-reap's own budget and the durable record,
	// not about resuming the automatic loop).
	if !inst.LifecycleView().LostRestoreGaveUp {
		t.Fatal("LostRestoreGaveUp = false after the force-reap: resetRecoverBudget must not clear the durable gave-up flag (the automatic loop stays suppressed)")
	}
}

// preRetirementFailBackend is a remoteWorkspaceBackend whose Recover returns an
// error WITHOUT calling FireOnSandboxRetired. It simulates the shape of a real
// reprovisionRemote failure that returns before reapRemoteRuntimeForReplacement
// runs — e.g. because the persisted account can no longer be resolved, the
// runtime configuration is invalid, or program/account validation fails. In
// those cases the old sandbox is still live, so the failure budget must not be
// reset: the operator has not earned a fresh budget by replacing the sandbox.
type preRetirementFailBackend struct {
	*session.FakeBackend
	mu       sync.Mutex
	failWith error
	recovers int
}

func (b *preRetirementFailBackend) Type() string { return "docker" }

func (b *preRetirementFailBackend) Capabilities() session.Capabilities {
	return session.Capabilities{
		Workspace:        session.WorkspaceRemote,
		Archive:          true,
		Recover:          true,
		InteractiveInput: true,
	}
}

// Recover fails WITHOUT firing FireOnSandboxRetired, simulating a
// reprovisionRemote early-return before reapRemoteRuntimeForReplacement.
func (b *preRetirementFailBackend) Recover(_ *session.Instance) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recovers++
	return b.failWith
}

func (b *preRetirementFailBackend) recoverCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.recovers
}

// TestRestoreLostSessions_ProbeUnknownForceReapPreRetirementFailLeavesCountCharged
// pins the P2 invariant: when a force-reap past the indeterminate probe causes
// reprovisionRemote to fail BEFORE reapRemoteRuntimeForReplacement retires the
// old sandbox (e.g. because the persisted account no longer resolves, the
// runtime configuration is invalid, or program/account validation fails), the
// Recover failure budget must NOT be cleared.
//
// Without the hook-based reset: the budget was cleared before Recover ran, so
// any pre-retirement failure would be counted as attempt 1 rather than
// maxAttempts+1 — and repeated force-reap attempts could restart the budget
// indefinitely, preventing the give-up threshold from ever being reached.
//
// With the hook-based reset: the budget is only cleared when the sandbox is
// provably retired (when FireOnSandboxRetired fires inside reprovisionRemote
// after reapRemoteRuntimeForReplacement succeeds). A pre-retirement Recover
// failure (this test) leaves the budget charged at its prior value, so
// repeated pre-retirement failures accumulate rather than restart.
func TestRestoreLostSessions_ProbeUnknownForceReapPreRetirementFailLeavesCountCharged(t *testing.T) {
	withRemoteLossThresholds(t, 3, time.Minute, time.Second)
	zeroRestoreBackoff(t)
	manager, _, repoID, repoPath := newStatusTestManagerCapturingLogs(t)

	const title = "remote-probeunknown-pre-retirement-fail"
	preRetirementErr := errors.New("recover: cannot re-provision: account no longer resolvable")
	srv, url := newProbeUnknownForceReapServer(t, true /* archive lands on pass 1 */)
	inst, _ := registerStartedRemoteTask(t, manager, repoID, repoPath, title, url, session.Running)

	// Replace the backend with one that simulates a pre-retirement Recover failure.
	preRetirBackend := &preRetirementFailBackend{
		FakeBackend: session.NewFakeBackend(),
		failWith:    preRetirementErr,
	}
	inst.SetBackend(preRetirBackend)

	// Drive to a Lost observation: the sandbox answers alive=false (answered dead).
	manager.RefreshStatuses()
	if got := inst.GetLiveness(); got != session.LiveLost {
		t.Fatalf("setup: liveness = %v, want LiveLost", got)
	}

	// Pass 1: the pre-reap push lands, recording the branch durably;
	// Recover fails immediately (pre-retirement), so consecutiveFailures = 1.
	manager.RestoreLostSessions()
	if got := srv.calls(); got != 1 {
		t.Fatalf("after pass 1: archive calls = %d, want 1", got)
	}
	if got := preRetirBackend.recoverCalls(); got != 1 {
		t.Fatalf("after pass 1: Recover calls = %d, want 1", got)
	}
	manager.mu.Lock()
	st := manager.lostRestoreStates[stableSessionKey(repoID, inst)]
	manager.mu.Unlock()
	if st == nil || st.consecutiveFailures != 1 {
		t.Fatalf("after pass 1: consecutiveFailures = %v, want 1 (pre-retirement failure must NOT reset the budget)", func() int {
			if st == nil {
				return -1
			}
			return st.consecutiveFailures
		}())
	}

	// Revoke origin auth so subsequent pre-reap pushes fail.
	srv.setArchiveOK(false)

	// Additional automatic passes: drive preserve-push failures to give-up.
	// lostRestoreMaxAttempts passes are needed because each preserve-push
	// failure increments preserveFailureAttempts, and the give-up fires at
	// preserveFailureAttempts >= lostRestoreMaxAttempts.
	for i := 0; i < lostRestoreMaxAttempts; i++ {
		manager.RestoreLostSessions()
	}
	view := inst.LifecycleView()
	if !view.LostRestoreGaveUp {
		t.Fatal("expected LostRestoreGaveUp after enough preserve-push failures")
	}

	// Flip the probe to probeUnknown so the force-reap exercises that arm.
	srv.flipUnknown()

	// Force-reap: pre-retirement Recover failure. Since FireOnSandboxRetired is
	// never called (the backend does not retire the sandbox), the budget must NOT
	// be reset. The first force-reap attempt after give-up is charged as
	// maxAttempts+1 — which hits the give-up again — rather than being reset to 1.
	_, _, err := manager.RestoreSession(RestoreSessionRequest{
		Title: title, RepoID: repoID, ForceReap: true,
	})
	if err == nil {
		t.Fatal("force-reap RestoreSession returned nil error, expected pre-retirement Recover failure to propagate")
	}

	// The budget must still be charged (at maxAttempts+1 now, not reset to 1):
	// the old sandbox was never retired, so repeated pre-retirement force-reap
	// attempts must accumulate failures rather than restarting the budget.
	manager.mu.Lock()
	st = manager.lostRestoreStates[stableSessionKey(repoID, inst)]
	manager.mu.Unlock()
	if st == nil || st.consecutiveFailures != lostRestoreMaxAttempts+1 {
		got := -1
		if st != nil {
			got = st.consecutiveFailures
		}
		t.Fatalf("force-reap pre-retirement failure: consecutiveFailures = %d, want %d (the budget must NOT be reset for a pre-retirement failure)",
			got, lostRestoreMaxAttempts+1)
	}
}

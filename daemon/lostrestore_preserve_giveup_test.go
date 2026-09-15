package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// preservePushServer is the remote agent-server stand-in for the
// probeAnsweredDead preserve-push give-up contract — the gap #3347 left open when
// it rewrote the lost-restore loop's terminal discipline for the Recover branch
// but never revisited the pre-#3347 preserve-push arm.
//
//   - /v1/agent/snapshot reports a clean idle pane, so RefreshStatuses asks the
//     alive probe (an idle Snapshot is the ambiguous shape a healthy idle
//     session and a dead one both produce).
//   - /v1/agent/alive answers alive=false — the ANSWERED death that drives the
//     restore loop's probeAnsweredDead arm: a reachable sandbox whose agent
//     process is gone, so whatever it holds unpushed is still there to save.
//   - /v1/agent/archive is the pre-reap push whose persistent failure the arm
//     must stop retrying forever on. archiveOK toggles the push so a test can
//     walk the arm from "push keeps failing" to "push lands, Recover flaps".
//
// /v1/agent/archive failures return a JSON error envelope (not a bare 500) so
// the remote client surfaces a real message inside the refusal error rather
// than a "malformed response envelope" decode error — the same shape a real
// agent-server returns when origin rejects the push.
type preservePushServer struct {
	mu sync.Mutex
	// archiveOK reports whether the pre-reap push lands (200 + branch) or is
	// refused (500 + error envelope).
	archiveOK bool
	// archiveCalls counts the pre-reap push attempts so a test asserts the loop
	// STOPS pushing after give-up, and that an attempt pass fires exactly one.
	archiveCalls int
}

func (s *preservePushServer) setArchiveOK(ok bool) {
	s.mu.Lock()
	s.archiveOK = ok
	s.mu.Unlock()
}

func (s *preservePushServer) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.archiveCalls
}

func newPreservePushServer(t *testing.T, archiveOK bool) (*preservePushServer, string) {
	t.Helper()
	srv := &preservePushServer{archiveOK: archiveOK}
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/agent/snapshot":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"updated": false, "has_prompt": false, "content": ""},
			})
		case "/v1/agent/alive":
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

// TestRestoreLostSessions_PreservePushFailureGivesUp is the central fix: a
// persistent, actionable pre-reap push failure (revoked origin auth, an archive
// endpoint that durably rejects this repo) must reach #3347's bounded give-up —
// a durable LostRestoreFailure, an ERROR line, and release of the #1892
// watch-task slot — instead of retrying forever at the backoff cap while the
// slot stays held.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES the failure mode of: push failures charged
// remoteUnknownAttempts (a ceiling-less counter with no path to give-up), so
// LostRestoreGaveUp stayed false and holdsTaskRunSlot stayed true for the life
// of an outage the loop could not self-cure. After the fix the loop stops at
// lostRestoreMaxAttempts, publishes the terminal fact, and frees the slot.
func TestRestoreLostSessions_PreservePushFailureGivesUp(t *testing.T) {
	withRemoteLossThresholds(t, 3, time.Minute, time.Second)
	zeroRestoreBackoff(t)
	manager, ownLogs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)

	srv, url := newPreservePushServer(t, false /* archive fails persistently */)
	inst, backend := registerStartedRemoteTask(t, manager, repoID, repoPath, "remote-task-push-fail", url, session.Running)

	// The outage: the poll's answered-dead probe marks a task-spawned session
	// Lost mid-run. Lost does NOT end the run (a finished and an interrupted run
	// are indistinguishable once lost), so the slot is the one the test asserts
	// releases only once the give-up lands.
	manager.RefreshStatuses()
	if got := inst.GetLiveness(); got != session.LiveLost {
		t.Fatalf("setup: liveness = %v, want LiveLost after the answered-dead probe", got)
	}
	if !inst.TaskRunActive() {
		t.Fatal("setup: a task run interrupted mid-flight must keep TaskRunActive true so its slot is held until the loop gives up")
	}

	// Run well past the give-up budget so the test also asserts the loop STOPS:
	// the passes after give-up reach the entry gate, which lostSessionWantsRestore
	// now refuses, so no further push fires.
	const passes = 2 * lostRestoreMaxAttempts
	for i := 0; i < passes; i++ {
		manager.RestoreLostSessions()
	}

	view := inst.LifecycleView()
	if !view.LostRestoreGaveUp {
		t.Fatal("LostRestoreGaveUp = false after persistent preserve-push failures: the pre-reap push arm never reached #3347's give-up and the loop is still retrying forever on a ceiling-less counter")
	}
	if snapshot := inst.LostRestoreFailureSnapshot(); snapshot == nil {
		t.Fatal("LostRestoreFailureSnapshot = nil at give-up: the terminal fact must be durable on the session record")
	} else {
		if snapshot.Attempts != lostRestoreMaxAttempts {
			t.Fatalf("surfaced attempts = %d, want %d", snapshot.Attempts, lostRestoreMaxAttempts)
		}
		// The refusal error names the cause and the --force-reap off-ramp; assert a
		// substring at the head of the message so it survives the 512-rune
		// sanitization bound regardless of how long the rendered suggestion is.
		if !strings.Contains(snapshot.Error, "refusing to replace the sandbox") {
			t.Fatalf("surfaced error = %q, want the preserve-push refusal naming the cause", snapshot.Error)
		}
	}
	if got := backend.recoverCalls(); got != 0 {
		t.Fatalf("Recover calls = %d, want 0 — the destructive re-provision must never be authorized while the pre-reap push keeps refusing", got)
	}
	if got := srv.calls(); got != lostRestoreMaxAttempts {
		t.Fatalf("archive calls = %d, want %d — the push must fire once per attempt then STOP at give-up (passes after give-up must not re-push)", got, lostRestoreMaxAttempts)
	}

	// The slot-holding impact, demonstrated end-to-end (not inferred): the run
	// is still in flight (Lost did not end it), but the durable give-up flips
	// canAutoRestoreLostSession false, so holdsTaskRunSlot releases the cap's slot.
	if !view.TaskRunActive {
		t.Fatal("TaskRunActive = false: a Lost observation must not end an in-flight run")
	}
	if lostSessionWantsRestore(view) {
		t.Fatal("lostSessionWantsRestore = true after give-up: the entry gate must refuse a session the loop has stopped retrying")
	}
	if holdsTaskRunSlot(view) {
		t.Fatal("holdsTaskRunSlot = true after give-up: the #1892 watch-task slot must release at the durable give-up so a capped watcher is not wedged for the life of an outage the loop cannot self-cure")
	}

	// The terminal fact is durable: it survives a daemon restart and every
	// client reads it off the persisted record.
	rec := recordFor(t, repoID, "remote-task-push-fail")
	if rec == nil {
		t.Fatal("record vanished after give-up: the Lost session must stay on disk recoverable")
	}
	if rec.LostRestoreFailure == nil || rec.LostRestoreFailure.Attempts != lostRestoreMaxAttempts {
		t.Fatalf("persisted LostRestoreFailure = %#v, want %d attempts", rec.LostRestoreFailure, lostRestoreMaxAttempts)
	}

	// This Manager's own ERROR log (#3797): a shared sink is written by every
	// Manager in the binary, so an assertion on it could pass on another's
	// output. The "preserve-push failures" wording is the arm's own — the
	// Recover branch says "attempts" — so this also pins that the give-up fired
	// from THIS arm and not by leaking into lostRestoreFailed.
	want := fmt.Sprintf("giving up after %d preserve-push failures", lostRestoreMaxAttempts)
	if !strings.Contains(ownLogs.errors.String(), want) {
		t.Fatalf("missing terminal %q in error log; logs:\n%s", want, ownLogs.errors.String())
	}

	// One more pass after give-up: the loop must not tail-fire a push or a probe.
	manager.RestoreLostSessions()
	if got := srv.calls(); got != lostRestoreMaxAttempts {
		t.Fatalf("archive calls after a post-give-up pass = %d, want still %d — a gave-up session's entry gate must refuse without re-pushing", got, lostRestoreMaxAttempts)
	}
	if got := backend.recoverCalls(); got != 0 {
		t.Fatalf("Recover calls after a post-give-up pass = %d, want 0", got)
	}
}

// TestRestoreLostSessions_PreserveGiveUpTerminatesManualRecoverToo pins the
// d8e4e08f fix: when the preserve budget gives up, the in-memory state has
// st.consecutiveFailures = lostRestoreMaxAttempts. A subsequent manual
// RestoreSession that pushes successfully but then fails Recover must NOT
// restart at attempt 1 and log "retrying in …"; lostRestoreFailed must
// immediately take the terminal arm because the counter is already at the
// budget ceiling.
//
// Without d8e4e08f: consecutiveFailures stays 0 at preserve give-up, so the
// manual Recover failure starts a new episode at attempt 1 and logs
// "restore of lost session … failed (attempt 1), retrying in …" while
// LostRestoreGaveUp is already true and lostSessionWantsRestore refuses every
// automatic retry — contradictory operator-facing output.
//
// With d8e4e08f: the give-up arm sets consecutiveFailures = lostRestoreMaxAttempts
// before returning, so the manual Recover failure increments to
// maxAttempts + 1 and immediately gives up, logging the same "giving up
// after N attempts" the Recover-branch give-up always has.
func TestRestoreLostSessions_PreserveGiveUpTerminatesManualRecoverToo(t *testing.T) {
	withRemoteLossThresholds(t, 3, time.Minute, time.Second)
	zeroRestoreBackoff(t)
	manager, ownLogs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)

	srv, url := newPreservePushServer(t, false /* archive fails: drive preserve give-up */)
	recoverErr := errors.New("recover: provision failed after give-up")
	inst, backend := registerStartedRemoteTask(t, manager, repoID, repoPath, "remote-give-up-manual", url, session.Running)
	backend.failWith = recoverErr

	// Drive to a Lost observation.
	manager.RefreshStatuses()
	if got := inst.GetLiveness(); got != session.LiveLost {
		t.Fatalf("setup: liveness = %v, want LiveLost", got)
	}

	// Run the automatic loop to the preserve give-up. After this,
	// LostRestoreGaveUp is true and st.consecutiveFailures == lostRestoreMaxAttempts.
	for i := 0; i < lostRestoreMaxAttempts; i++ {
		manager.RestoreLostSessions()
	}
	view := inst.LifecycleView()
	if !view.LostRestoreGaveUp {
		t.Fatal("setup: LostRestoreGaveUp = false after the push-failure give-up passes: the preserve arm did not reach terminal state")
	}

	// Now let the push succeed so the manual restore can reach Recover — the
	// push-failure give-up is the one we drove, but the manual path must still
	// attempt the preserve push (it is not the same as force-reap). The fact
	// that it now lands does not reset the Recover budget that the preserve
	// give-up set terminal.
	srv.setArchiveOK(true)

	// Manual restore: push succeeds, Recover fails once.
	_, _, err := manager.RestoreSession(RestoreSessionRequest{
		Title: "remote-give-up-manual", RepoID: repoID,
	})
	if err == nil {
		t.Fatal("manual RestoreSession returned nil error, expected Recover failure to propagate")
	}

	// The critical assertion: the warn log must NOT say "retrying in" for the
	// manual Recover failure — the counter is already at the ceiling. Without
	// d8e4e08f the Recover failure restarts at attempt 1 and logs a false
	// retry promise while LostRestoreGaveUp is already suppressing automatic retries.
	if strings.Contains(ownLogs.warnings.String(), "retrying in") {
		t.Fatalf("manual Recover failure after preserve give-up logged a retry promise;\n"+
			"d8e4e08f's assignment is missing: the session must stay terminal, not restart at attempt 1.\n"+
			"warn logs:\n%s", ownLogs.warnings.String())
	}
	// The error log must have the give-up line (attempts == maxAttempts+1).
	wantGiveUp := fmt.Sprintf("giving up after %d attempts", lostRestoreMaxAttempts+1)
	if !strings.Contains(ownLogs.errors.String(), wantGiveUp) {
		t.Fatalf("manual Recover failure after preserve give-up did not log %q;\n"+
			"want the terminal arm, not a retry-with-backoff log.\nerror logs:\n%s",
			wantGiveUp, ownLogs.errors.String())
	}
	// The session must still report gave-up (not mysteriously recovered).
	if !inst.LifecycleView().LostRestoreGaveUp {
		t.Fatal("LostRestoreGaveUp = false after manual Recover failure: the terminal state must persist")
	}
	if got := backend.recoverCalls(); got != 1 {
		t.Fatalf("Recover calls = %d, want 1 — exactly one Recover call from the manual restore", got)
	}
}

// TestRestoreLostSessions_ForceReapAfterPreserveGiveUpEarnsAFreshRecoverBudget
// pins the c118b158 fix: after the preserve budget gives up and sets
// st.consecutiveFailures = lostRestoreMaxAttempts, an operator force-reap
// replaces the old sandbox and must give the new one a fresh Recover budget.
//
// Without c118b158: resetPreserveBudget zeroes preserveFailureAttempts but
// does NOT clear consecutiveFailures, so the first Recover failure against the
// brand-new sandbox counts as attempt maxAttempts+1 and immediately gives up
// — the new sandbox is punished for the previous one's history.
//
// With c118b158: the force-reap arm calls resetRecoverBudget, which zeroes
// consecutiveFailures under the lock, so a subsequent Recover failure starts
// at attempt 1 with a full budget and logs a retry promise rather than giving up.
func TestRestoreLostSessions_ForceReapAfterPreserveGiveUpEarnsAFreshRecoverBudget(t *testing.T) {
	withRemoteLossThresholds(t, 3, time.Minute, time.Second)
	zeroRestoreBackoff(t)
	manager, ownLogs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)

	srv, url := newPreservePushServer(t, false /* archive fails: drive preserve give-up */)
	recoverErr := errors.New("recover: provision failed on new sandbox")
	inst, backend := registerStartedRemoteTask(t, manager, repoID, repoPath, "remote-force-reap-fresh", url, session.Running)
	backend.failWith = recoverErr

	// Drive to a Lost observation.
	manager.RefreshStatuses()
	if got := inst.GetLiveness(); got != session.LiveLost {
		t.Fatalf("setup: liveness = %v, want LiveLost", got)
	}

	// Run the automatic loop to the preserve give-up.
	for i := 0; i < lostRestoreMaxAttempts; i++ {
		manager.RestoreLostSessions()
	}
	if !inst.LifecycleView().LostRestoreGaveUp {
		t.Fatal("setup: LostRestoreGaveUp = false after push-failure give-up passes")
	}

	// Prepare force-reap: the session needs a durable branch so that
	// requireDurableSandboxBranch does not refuse. Record the branch both in
	// memory and on disk, as the production flow would have left it after at
	// least one successful archive.
	inst.SetSandboxBranch("af/fixture-branch")
	manager.persistInstance(repoID, inst)

	// Let the push server answer correctly (archive calls from the force-reap
	// arm should be zero, but the probe has to still answer dead so the arm
	// reaches the force branch rather than the probeAlive heal).
	srv.setArchiveOK(true)
	archiveCallsBefore := srv.calls()

	// Force-reap: skips the push, calls resetPreserveBudget + resetRecoverBudget,
	// then runs Recover — which fails once.
	_, _, err := manager.RestoreSession(RestoreSessionRequest{
		Title: "remote-force-reap-fresh", RepoID: repoID, ForceReap: true,
	})
	if err == nil {
		t.Fatal("force-reap RestoreSession returned nil error, expected Recover failure to propagate")
	}

	// Force-reap must NOT have pushed (it promises to skip the push).
	if got := srv.calls(); got != archiveCallsBefore {
		t.Fatalf("archive calls during force-reap = %d, want %d (force-reap must not push)", got, archiveCallsBefore)
	}

	// The critical assertion: the error log must NOT have the Recover-branch
	// give-up line ("giving up after N attempts: …" — the preserve-push give-up
	// says "preserve-push failures", so the two are distinguishable). Without
	// c118b158 the retained consecutiveFailures == lostRestoreMaxAttempts causes
	// lostRestoreFailed to take the terminal arm on the very first failure against
	// the new sandbox, logging exactly that Recover give-up string.
	if strings.Contains(ownLogs.errors.String(), "giving up after") &&
		strings.Contains(ownLogs.errors.String(), "attempts:") {
		t.Fatalf("force-reap Recover failure gave up immediately;\n"+
			"c118b158's resetRecoverBudget is missing: the new sandbox must start with a fresh budget.\n"+
			"error logs:\n%s", ownLogs.errors.String())
	}
	// Verify the retry log is actually there (attempt 1, budget not exhausted).
	if !strings.Contains(ownLogs.warnings.String(), "retrying in") {
		t.Fatalf("force-reap Recover failure did not log a retry-with-backoff message;\n"+
			"want the retry arm (fresh budget), not immediate give-up.\nwarn logs:\n%s",
			ownLogs.warnings.String())
	}
	// Recover was called exactly once: zero times during push-fail give-up (push
	// never reached Recover), once during the force-reap restore.
	if got := backend.recoverCalls(); got != 1 {
		t.Fatalf("Recover calls = %d, want 1 (zero from push-fail phase + 1 from force-reap)", got)
	}
}

// TestRestoreLostSessions_PushSucceedsThenRecoverFailsGivesUp is the within-one-arm
// contrast the preserve-push fix is measured against: when the push LANDS and
// Recover then fails persistently, the SAME probeAnsweredDead arm already
// escalates to give-up (via recordLostRestoreFailure -> lostRestoreFailed). This
// pins that the Recover-failure escalation is unchanged by the fix and that
// both failure branches of the arm now meet #3347's terminal contract.
func TestRestoreLostSessions_PushSucceedsThenRecoverFailsGivesUp(t *testing.T) {
	withRemoteLossThresholds(t, 3, time.Minute, time.Second)
	zeroRestoreBackoff(t)
	manager, ownLogs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)

	srv, url := newPreservePushServer(t, true /* archive lands */)
	recoverErr := errors.New("recover: remote provision failed")
	inst, backend := registerStartedRemoteTask(t, manager, repoID, repoPath, "remote-task-recover-fail", url, session.Running)
	backend.failWith = recoverErr

	manager.RefreshStatuses()
	if got := inst.GetLiveness(); got != session.LiveLost {
		t.Fatalf("setup: liveness = %v, want LiveLost", got)
	}

	const passes = 2 * lostRestoreMaxAttempts
	for i := 0; i < passes; i++ {
		manager.RestoreLostSessions()
	}

	view := inst.LifecycleView()
	if !view.LostRestoreGaveUp {
		t.Fatal("LostRestoreGaveUp = false after persistent Recover failures: the Recover branch must still escalate to give-up (the fix must not regress it)")
	}
	// The push landed on every attempt that reached Recover, so archive calls
	// and Recover calls match exactly — and both stop at the give-up budget.
	if got, want := backend.recoverCalls(), lostRestoreMaxAttempts; got != want {
		t.Fatalf("Recover calls = %d, want the loop to stop after %d persistent Recover failures", got, want)
	}
	if got, want := srv.calls(), lostRestoreMaxAttempts; got != want {
		t.Fatalf("archive calls = %d, want %d — the push fires once per Recover attempt then stops at give-up", got, want)
	}
	snapshot := inst.LostRestoreFailureSnapshot()
	if snapshot == nil || snapshot.Attempts != lostRestoreMaxAttempts {
		t.Fatalf("surfaced failure = %#v, want %d attempts", snapshot, lostRestoreMaxAttempts)
	}
	if !strings.Contains(snapshot.Error, "remote provision failed") {
		t.Fatalf("surfaced error = %q, want the Recover-failure reason (not the preserve-push refusal)", snapshot.Error)
	}
	// The Recover branch's give-up log says "attempts", not "preserve-push
	// failures" — pinning the two give-up paths stay distinct and that this one
	// fired through lostRestoreFailed, not the new preserve-push arm.
	want := fmt.Sprintf("giving up after %d attempts: %v", lostRestoreMaxAttempts, recoverErr)
	if !strings.Contains(ownLogs.errors.String(), want) {
		t.Fatalf("missing terminal %q in error log; logs:\n%s", want, ownLogs.errors.String())
	}
	if holdsTaskRunSlot(view) {
		t.Fatal("holdsTaskRunSlot = true after Recover give-up: the slot must release at the durable give-up on both branches of the arm")
	}
}

// TestRestoreLostSessions_TransientPushBlipsDoNotPrechargeRecoverBudget pins the
// episode-separation invariant the fix MUST preserve: a few transient push
// blips (push fails, then lands) must not precharge the Recover-failure budget.
// A naive "route push failures through lostRestoreFailed" would merge the two
// budgets and burn the Recover budget on push blips that destroyed no work — so
// the push arm gets its OWN budget, and so does Recover.
//
// Three push blips, then the push lands, then Recover flaps and dies
// persistently. Give-up must still arrive after EXACTLY lostRestoreMaxAttempts
// Recover calls: the blips counted against the push budget, never the Recover
// one.
func TestRestoreLostSessions_TransientPushBlipsDoNotPrechargeRecoverBudget(t *testing.T) {
	withRemoteLossThresholds(t, 3, time.Minute, time.Second)
	zeroRestoreBackoff(t)
	manager, ownLogs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)

	srv, url := newPreservePushServer(t, false /* start with the push refusing */)
	inst, backend := registerStartedRemoteTask(t, manager, repoID, repoPath, "remote-task-blips", url, session.Running)
	backend.failWith = errors.New("recover: remote provision failed")

	manager.RefreshStatuses()
	if got := inst.GetLiveness(); got != session.LiveLost {
		t.Fatalf("setup: liveness = %v, want LiveLost", got)
	}

	// Three transient push blips. Each fails the pre-reap push, backs off, and
	// returns — Recover is never reached, so consecutiveFailures (the Recover
	// budget) stays at zero.
	const blips = 3
	for i := 0; i < blips; i++ {
		manager.RestoreLostSessions()
	}
	if got := backend.recoverCalls(); got != 0 {
		t.Fatalf("Recover calls during blips = %d, want 0 — a refused push must never reach the destructive re-provision", got)
	}
	if got := srv.calls(); got != blips {
		t.Fatalf("archive calls during blips = %d, want %d", got, blips)
	}
	if got := backend.recoverCalls(); got != 0 {
		t.Fatalf("Recover calls after blips = %d, want 0 — push blips must not precharge the Recover budget", got)
	}

	// The push now lands. From here each pass pushes successfully and then Recover
	// fails. Give-up must arrive after exactly lostRestoreMaxAttempts Recover
	// calls; if the blips had precharged consecutiveFailures, give-up would fire
	// early (at blips fewer Recover calls).
	srv.setArchiveOK(true)
	for i := 0; i < lostRestoreMaxAttempts+4; i++ {
		manager.RestoreLostSessions()
	}

	view := inst.LifecycleView()
	if !view.LostRestoreGaveUp {
		t.Fatal("LostRestoreGaveUp = false: persistent Recover failures after the push landed must still reach give-up")
	}
	if got, want := backend.recoverCalls(), lostRestoreMaxAttempts; got != want {
		t.Fatalf("Recover calls = %d, want exactly %d — the %d push blips precharged the Recover budget (a merged budget would give up at %d)", got, want, blips, want-blips)
	}
	if got, want := srv.calls(), blips+lostRestoreMaxAttempts; got != want {
		t.Fatalf("archive calls = %d, want %d (%d blips + %d successful pushes before each Recover)", got, want, blips, lostRestoreMaxAttempts)
	}
	snapshot := inst.LostRestoreFailureSnapshot()
	if snapshot == nil || snapshot.Attempts != lostRestoreMaxAttempts {
		t.Fatalf("surfaced failure = %#v, want %d attempts (the blips must not inflate the count)", snapshot, lostRestoreMaxAttempts)
	}
	want := fmt.Sprintf("giving up after %d attempts:", lostRestoreMaxAttempts)
	if !strings.Contains(ownLogs.errors.String(), want) {
		t.Fatalf("missing Recover-branch give-up %q; logs:\n%s", want, ownLogs.errors.String())
	}
	if holdsTaskRunSlot(view) {
		t.Fatal("holdsTaskRunSlot = true after Recover give-up: the slot must release at give-up")
	}
}

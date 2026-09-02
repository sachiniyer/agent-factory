package daemon

import (
	"bytes"
	"fmt"
	stdlog "log"
	"strings"
	"testing"

	aflog "github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// #3412: the agent dies right after liveness, and every pass re-arms the next.
//
// The field shape, in a 95-second window: 15 "started attempt N", 7+ "restored
// after N attempt(s)", 14 "is gone; status monitor going silent" — including
// "restored after 1 attempt" twice in four seconds, each time followed by the
// session going Lost again.
//
// The mechanism was that ONE liveness answer confirmed a restore. The agent
// satisfied it and exited a beat later, so the restore was declared a success,
// which cleared the attempt counter and the backoff; the next loss began again at
// "attempt 1". #3347's terminal give-up only fires when attempts EXHAUST, and they
// never accumulated.

// TestRestoreLostSessions_OneAnswerIsNotSustainedLiveness pins the bar itself:
// answers short of it leave the episode open, because "it came up" and "it stayed
// up" are different claims.
func TestRestoreLostSessions_OneAnswerIsNotSustainedLiveness(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "brief-liveness", backend, true, session.Lost)
	zeroRestoreBackoff(t)
	key := stableSessionKey(repoID, inst)

	manager.RestoreLostSessions() // spawn; awaiting confirmation

	for answers := 1; answers < lostRestoreConfirmObservations; answers++ {
		observeAlive(manager, repoID, inst)
		manager.RestoreLostSessions()
		manager.mu.Lock()
		st, tracked := manager.lostRestoreStates[key]
		stillAwaiting := tracked && st.awaitingConfirm
		manager.mu.Unlock()
		if !stillAwaiting {
			t.Fatalf("%d answer(s) confirmed the restore, but the bar is %d: a runtime that "+
				"answers once and exits was being called restored (#3412)", answers, lostRestoreConfirmObservations)
		}
	}

	// The answer that crosses the bar is the one that confirms.
	observeAlive(manager, repoID, inst)
	manager.RestoreLostSessions()
	manager.mu.Lock()
	_, tracked := manager.lostRestoreStates[key]
	manager.mu.Unlock()
	if tracked {
		t.Fatalf("a runtime that answered %d polls must have its retry history cleared",
			lostRestoreConfirmObservations)
	}
}

// TestRestoreLostSessions_FlapAfterBriefLivenessReachesGiveUp is the reported bug
// end to end: an agent that answers exactly one poll and then exits must reach the
// terminal give-up instead of looping, and must never be reported as restored.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: every cycle logs "restored after 1 attempt",
// the counter resets, Recover is called once per pass forever, and no give-up ever
// fires.
func TestRestoreLostSessions_FlapAfterBriefLivenessReachesGiveUp(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "flapper", backend, true, session.Lost)
	zeroRestoreBackoff(t)

	var info, errors bytes.Buffer
	prevInfo, prevError := aflog.InfoLog, aflog.ErrorLog
	aflog.InfoLog = stdlog.New(&info, "INFO: ", 0)
	aflog.ErrorLog = stdlog.New(&errors, "ERROR: ", 0)
	t.Cleanup(func() { aflog.InfoLog, aflog.ErrorLog = prevInfo, prevError })

	// Every pass that spawns is followed by exactly ONE answering poll and then the
	// agent's exit. Passes that only record the death leave the row Lost and are
	// skipped here, which is what the daemon's own cadence does.
	for pass := 0; pass < 6*lostRestoreMaxAttempts; pass++ {
		manager.RestoreLostSessions()
		if inst.GetStatus() != session.Lost {
			observeAlive(manager, repoID, inst)
			inst.SetStatusForTest(session.Lost)
		}
	}

	if strings.Contains(info.String(), "restored after") {
		t.Fatalf("a session that never stayed alive was reported restored:\n%s", info.String())
	}
	want := fmt.Sprintf("giving up after %d attempts: %v", lostRestoreMaxAttempts, errRestoreFlapped)
	if !strings.Contains(errors.String(), want) {
		t.Fatalf("missing terminal %q log for a flapping agent; logs:\n%s", want, errors.String())
	}
	if got := backend.recoverCalls(); got != lostRestoreMaxAttempts {
		t.Fatalf("recovery spawns = %d, want the loop to stop after %d", got, lostRestoreMaxAttempts)
	}

	data := inst.ToInstanceData()
	if data.LostRestoreFailure == nil {
		t.Fatalf("terminal session surfaced no restore failure: %#v", data)
	}
	if data.LostRestoreFailure.Attempts != lostRestoreMaxAttempts {
		t.Fatalf("surfaced attempts = %d, want %d", data.LostRestoreFailure.Attempts, lostRestoreMaxAttempts)
	}
	// The reported reason must name what actually happened. "Failing at startup"
	// would be wrong here: the agent starts fine, which is why the old code kept
	// declaring victory.
	if data.LostRestoreFailure.Error != errRestoreFlapped.Error() {
		t.Fatalf("surfaced reason = %q, want the flap reason %q",
			data.LostRestoreFailure.Error, errRestoreFlapped.Error())
	}
	if data.IdleReason != session.IdleReasonRestoreGaveUp {
		t.Fatalf("idle reason = %q, want %q", data.IdleReason, session.IdleReasonRestoreGaveUp)
	}
}

// TestRestoreLostSessions_NeverAnsweredKeepsStartupReason keeps the two failures
// distinguishable: an agent that never answered at all still reports the startup
// reason, so the flap sentinel cannot quietly swallow the #1910 case.
func TestRestoreLostSessions_NeverAnsweredKeepsStartupReason(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &diesOnSpawnBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "never-answers", backend, true, session.Lost)
	zeroRestoreBackoff(t)

	for pass := 0; pass < 2*lostRestoreMaxAttempts+4; pass++ {
		manager.RestoreLostSessions()
	}

	data := inst.ToInstanceData()
	if data.LostRestoreFailure == nil || data.LostRestoreFailure.Error != errRestoreDiedBeforeConfirm.Error() {
		t.Fatalf("surfaced failure = %#v, want the never-answered startup reason %q",
			data.LostRestoreFailure, errRestoreDiedBeforeConfirm.Error())
	}
}

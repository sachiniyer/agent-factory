package daemon

import (
	"bytes"
	"errors"
	stdlog "log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	aflog "github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// The #1976 regression lock: the MANUAL restore path (RestoreSession — the
// `af sessions restore` RPC and the TUI's restore action) must NOT reset the
// #1108 exponential backoff on Recover()-success alone.
//
// That was the exact pre-#1910 anti-pattern the automatic loop was fixed to avoid
// (#1923): a spawn returning proves only that tmux accepted the new session, which
// an agent that exits on startup also satisfies. The auto path now arms a
// confirmation window instead of clearing the state; the manual path still ran an
// unconditional delete(lostRestoreStates[key]) after Recover, so a user
// restoring a flapping session re-opened the very hot-loop the auto
// path prevents. This pins the manual path to the same confirm-alive gate.

// TestRestoreSession_ImmediateReLossAfterManualRestore_EscalatesNotResets drives
// the field shape end to end: Recover returns nil (the tmux spawn genuinely works)
// but the agent exits before the next poll, so the row is Lost again. The
// accumulated failure history must survive the manual restore, and the immediate
// re-loss must escalate against the same episode rather than respawning at poll
// cadence into an agent that will not stay up.
func TestRestoreSession_ImmediateReLossAfterManualRestore_EscalatesNotResets(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &diesOnSpawnBackend{FakeBackend: session.NewFakeBackend()}
	registerStarted(t, manager, repoID, repoPath, "flapper", backend, true, session.Lost)

	// The automatic loop has already tried and failed three times: a real backoff
	// episode is in progress when the user reaches for manual restore.
	key := daemonInstanceKey(repoID, "flapper")
	manager.mu.Lock()
	manager.lostRestoreStates[key] = &lostRestoreState{consecutiveFailures: 3}
	manager.mu.Unlock()

	// The user restores by hand. The spawn succeeds (Recover returns nil) but the
	// agent immediately exits, so the row is Lost again by the time the poll looks.
	if _, err := manager.RestoreSession(RestoreSessionRequest{Title: "flapper", RepoID: repoID}); err != nil {
		t.Fatalf("RestoreSession: %v", err)
	}
	if got := backend.recoverCount(); got != 1 {
		t.Fatalf("manual restore must spawn exactly once, got %d", got)
	}

	// One automatic poll. It finds the row Lost again inside the confirmation
	// window, so it must charge the death against the SAME episode — never respawn.
	manager.RestoreLostSessions()

	if got := backend.recoverCount(); got != 1 {
		t.Fatalf("the poll RESPAWNED after a manual restore whose runtime died before "+
			"confirmation (%d total spawns): the manual path cleared the backoff state, so the "+
			"immediate re-loss looked like a fresh episode and hot-looped at poll cadence (#1976)", got)
	}
	manager.mu.Lock()
	st := manager.lostRestoreStates[key]
	failures := -1
	if st != nil {
		failures = st.consecutiveFailures
	}
	manager.mu.Unlock()
	if st == nil {
		t.Fatal("retry state was dropped by the manual restore; the immediate re-loss had no " +
			"episode to escalate against, so the #1108 backoff never arms (#1976)")
	}
	if failures != 4 {
		t.Fatalf("consecutiveFailures = %d, want 4: the manual restore must CARRY the 3 prior "+
			"failures and the immediate re-loss must add one, so the backoff escalates instead of "+
			"restarting from zero (#1976)", failures)
	}
}

// TestRestoreSession_LongLivedThenDied_ResetsBackoff is the necessary other half:
// a manual restore whose runtime is CONFIRMED alive and only dies much later must
// start a fresh episode, not inherit the old escalation. Only the immediate flap is
// throttled — a genuinely recovered session that runs for a while earns a prompt
// re-restore.
func TestRestoreSession_LongLivedThenDied_ResetsBackoff(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "long-lived", backend, true, session.Lost)
	zeroRestoreBackoff(t)

	key := daemonInstanceKey(repoID, "long-lived")
	manager.mu.Lock()
	manager.lostRestoreStates[key] = &lostRestoreState{consecutiveFailures: 3}
	manager.mu.Unlock()

	if _, err := manager.RestoreSession(RestoreSessionRequest{Title: "long-lived", RepoID: repoID}); err != nil {
		t.Fatalf("RestoreSession: %v", err)
	}

	// A poll ANSWERS: the runtime is confirmed alive, so the episode is forgotten.
	observeAlive(manager, repoID, inst)
	manager.RestoreLostSessions()
	manager.mu.Lock()
	_, tracked := manager.lostRestoreStates[key]
	manager.mu.Unlock()
	if tracked {
		t.Fatal("a confirmed-alive manual restore must clear the retry state so a later, genuine " +
			"loss does not inherit the old backoff (#1976)")
	}

	// It dies much later. The next poll marks it Lost and recovers it as attempt #1,
	// not attempt #4 — the prior escalation must not carry into an unrelated loss.
	inst.SetStatusForTest(session.Lost)
	manager.RestoreLostSessions()
	manager.mu.Lock()
	st := manager.lostRestoreStates[key]
	failures := -1
	if st != nil {
		failures = st.consecutiveFailures
	}
	manager.mu.Unlock()
	if failures > 0 {
		t.Fatalf("a runtime confirmed alive after a manual restore was charged %d failure(s) from "+
			"the PREVIOUS episode when it later died: a genuine re-loss must start a fresh backoff", failures)
	}
}

// TestRestoreSession_FailedManualRestoresShareDiagnosticsAndBackoff pins the
// failure half of the same manual/automatic restore contract. A user-triggered
// Recover failure is still a failed restore attempt: it must advance the retry
// episode exactly once, arm the automatic loop's backoff, and emit the same
// one-shot missing-worktree diagnostic as an automatic attempt.
func TestRestoreSession_FailedManualRestoresShareDiagnosticsAndBackoff(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	worktreePath := filepath.Join(testguard.CanonicalTempDir(t), "missing-manual-restore")
	restoreErr := &session.WorktreeUnavailableError{
		Title:        "manual-failure",
		WorktreePath: worktreePath,
		Err:          &os.PathError{Op: "stat", Path: worktreePath, Err: os.ErrNotExist},
	}
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend(), failWith: restoreErr}
	registerStarted(t, manager, repoID, repoPath, "manual-failure", backend, true, session.Lost)

	key := daemonInstanceKey(repoID, "manual-failure")
	manager.mu.Lock()
	manager.lostRestoreStates[key] = &lostRestoreState{consecutiveFailures: 2}
	manager.mu.Unlock()

	var warnings, diagnostics bytes.Buffer
	previousWarning, previousError := aflog.WarningLog, aflog.ErrorLog
	aflog.WarningLog = stdlog.New(&warnings, "WARNING: ", 0)
	aflog.ErrorLog = stdlog.New(&diagnostics, "ERROR: ", 0)
	t.Cleanup(func() {
		aflog.WarningLog = previousWarning
		aflog.ErrorLog = previousError
	})

	for attempt := 0; attempt < 2; attempt++ {
		if _, err := manager.RestoreSession(RestoreSessionRequest{Title: "manual-failure", RepoID: repoID}); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("manual restore %d error = %v, want missing-worktree cause", attempt+1, err)
		}
	}
	if got := backend.recoverCalls(); got != 2 {
		t.Fatalf("manual Recover calls = %d, want 2", got)
	}

	manager.mu.Lock()
	state := manager.lostRestoreStates[key]
	manager.mu.Unlock()
	if state == nil {
		t.Fatal("manual restore failures were not entered into the shared lost-restore retry state")
	}
	if state.consecutiveFailures != 4 {
		t.Fatalf("consecutiveFailures = %d, want 4: each of two manual attempts must add exactly one to the two existing failures", state.consecutiveFailures)
	}
	if !state.nextAttempt.After(time.Now()) {
		t.Fatalf("nextAttempt = %v, want a future automatic-retry backoff", state.nextAttempt)
	}

	manager.RestoreLostSessions()
	if got := backend.recoverCalls(); got != 2 {
		t.Fatalf("automatic loop ignored the backoff recorded by manual restore: Recover calls = %d, want 2", got)
	}

	if got := strings.Count(diagnostics.String(), "WORKTREE_MISSING_DETECTED"); got != 1 {
		t.Fatalf("missing-worktree diagnostic count = %d, want one per loss episode; logs:\n%s", got, diagnostics.String())
	}
	if strings.Contains(diagnostics.String(), `classification="expected_teardown"`) {
		t.Fatalf("manual restore's own busy marker was misreported as teardown intent; logs:\n%s", diagnostics.String())
	}
	for _, want := range []string{"attempt 3", "attempt 4"} {
		if !strings.Contains(warnings.String(), want) {
			t.Fatalf("missing %q in shared failure-accounting logs:\n%s", want, warnings.String())
		}
	}
}

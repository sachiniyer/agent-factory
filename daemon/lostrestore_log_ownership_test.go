package daemon

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// plantVanishedWorktreeInstance registers a Lost instance whose worktree
// directory and branch have both been removed, which is what makes
// RestoreLostSessions emit the WORKTREE_MISSING_DETECTED diagnostic. Lifted from
// TestRestoreLostSessions_LogsVanishedWorktreeOnce so the ownership test below
// drives the identical production path rather than a lookalike.
func plantVanishedWorktreeInstance(t *testing.T, m *Manager, repoID, repoPath, title string) {
	t.Helper()
	worktreePath := filepath.Join(testguard.CanonicalTempDir(t), "repo-"+title)
	branch := "af/" + title
	if out, err := exec.Command("git", "-C", repoPath, "worktree", "add", "-b", branch, worktreePath).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}
	gw, err := sessiongit.NewGitWorktreeFromStorage(repoPath, worktreePath, title, branch, "", false, true)
	if err != nil {
		t.Fatalf("NewGitWorktreeFromStorage: %v", err)
	}
	if err := os.RemoveAll(worktreePath); err != nil {
		t.Fatalf("remove worktree directory: %v", err)
	}
	if out, err := exec.Command("git", "-C", repoPath, "update-ref", "-d", "refs/heads/"+branch).CombinedOutput(); err != nil {
		t.Fatalf("delete branch ref: %v\n%s", err, out)
	}

	inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: repoPath, Program: "claude"})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	inst.Branch = branch
	inst.SetBackend(&session.LocalBackend{})
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Lost)
	inst.SetGitWorktreeForTest(gw)
	inst.SetTmuxSession(tmux.NewTmuxSessionFromSanitizedNameWithDeps(
		"af_3797_"+title,
		"claude",
		failPtyFactory{},
		cmd_test.MockCmdExec{
			RunFunc: func(*exec.Cmd) error {
				return errors.New("tmux command should not be reached when worktree is missing")
			},
			OutputFunc: func(*exec.Cmd) ([]byte, error) {
				return nil, errors.New("tmux command should not be reached when worktree is missing")
			},
		},
	))

	seedDiskInstance(t, repoID, title, repoPath)
	m.mu.Lock()
	m.instances[daemonInstanceKey(repoID, title)] = inst
	m.mu.Unlock()
}

// TestAForeignManagersErrorCannotSatisfyThisManagersCount is #3797's regression,
// and it is the ERROR-level twin of the warning-level one in
// rootagent_warning_ownership_test.go.
//
// The assertion it protects COUNTS: TestRestoreLostSessions_LogsVanishedWorktreeOnce
// requires exactly one WORKTREE_MISSING_DETECTED across two restore passes, which
// is how "logged once, not once per poll" is pinned. A count read from a shared
// sink breaks in BOTH directions — a second Manager's diagnostic inflates it and
// reads as a broken once-only guard, and conversely a once-only guard that
// genuinely stopped working can be masked by which Manager happened to emit
// what. Neither shows up as a race, so #3789's mutex never made a difference
// here.
//
// The fixture separates the halves: the subject Manager has no vanished
// worktree and emits nothing; a second Manager does and emits one.
func TestAForeignManagersErrorCannotSatisfyThisManagersCount(t *testing.T) {
	subject, subjectLogs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	zeroRestoreBackoff(t)

	shared := captureErrors(t)

	// The subject has nothing to restore, so it must stay silent — asserted, so
	// this is a fixture invariant rather than an assumption.
	subject.RestoreLostSessions()
	if got := subjectLogs.errors.String(); strings.Contains(got, "WORKTREE_MISSING_DETECTED") {
		t.Fatalf("fixture: the subject Manager logged a vanished worktree, so this test proves nothing:\n%s", got)
	}

	// A Manager this test is NOT about, which does have one.
	foreign, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager(foreign): %v", err)
	}
	plantVanishedWorktreeInstance(t, foreign, repoID, repoPath, "foreign-vanished")
	foreign.RestoreLostSessions()

	// The contamination is real and reachable: the shared ERROR sink every
	// pre-#3797 assertion read now carries a count of exactly one — so the
	// count assertion PASSES, on a diagnostic the subject never emitted.
	if count := strings.Count(shared.String(), "WORKTREE_MISSING_DETECTED"); count != 1 {
		t.Fatalf("fixture: the foreign Manager's diagnostic did not reach the shared sink "+
			"exactly once (count=%d), so the contamination this test is about was not "+
			"exercised:\n%s", count, shared.String())
	}

	// The property: the subject's own error log is untouched by it.
	if count := strings.Count(subjectLogs.errors.String(), "WORKTREE_MISSING_DETECTED"); count != 0 {
		t.Fatalf("a diagnostic emitted by a Manager this test never asserted about landed in "+
			"the subject's own error log (count=%d); a count assertion on it would be "+
			"satisfied by another Manager's output (#3797):\n%s",
			count, subjectLogs.errors.String())
	}

	// ANTI-VACUITY. A per-Manager capture that was simply never wired to this
	// diagnostic would satisfy the check above too, and would keep satisfying it
	// after a refactor quietly took the routing back out. So a Manager built the
	// same way, with a vanished worktree of its own, must find it in ITS log.
	witness, witnessLogs, witnessRepoID, witnessRepoPath := newStatusTestManagerCapturingLogs(t)
	plantVanishedWorktreeInstance(t, witness, witnessRepoID, witnessRepoPath, "witness-vanished")
	witness.RestoreLostSessions()
	if count := strings.Count(witnessLogs.errors.String(), "WORKTREE_MISSING_DETECTED"); count != 1 {
		t.Fatalf("a Manager's own diagnostic never reached its own error log (count=%d), so the "+
			"assertion above proves nothing — the routing is not wired:\n%s",
			count, witnessLogs.errors.String())
	}
}

// TestManagerInfoAndErrorReachTheGlobalLogsByDefault is the production half. A
// defaulting bug in m.info()/m.err() would silence the daemon's own log while
// every routed test stayed green, which is a worse outcome than the bug this
// change fixes.
func TestManagerInfoAndErrorReachTheGlobalLogsByDefault(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	zeroRestoreBackoff(t)
	errorsLog := captureErrors(t)

	plantVanishedWorktreeInstance(t, manager, repoID, repoPath, "default-vanished")
	manager.RestoreLostSessions()

	if got := errorsLog.String(); !strings.Contains(got, "WORKTREE_MISSING_DETECTED") {
		t.Fatalf("a Manager built through NewManager must log errors on the process-global "+
			"ErrorLog; got:\n%s", got)
	}
}

// TestNilManagerInfoAndErrFallBackToTheGlobalLogs pins the nil-receiver arms, as
// the warning level already does. Manager methods run on nil receivers in a few
// teardown paths, and an accessor that dereferenced would turn a diagnostic into
// a panic.
func TestNilManagerInfoAndErrFallBackToTheGlobalLogs(t *testing.T) {
	info, errs := captureInfo(t), captureErrors(t)
	var m *Manager
	m.info().Printf("nil-receiver info")
	m.err().Printf("nil-receiver error")
	if got := info.String(); !strings.Contains(got, "nil-receiver info") {
		t.Fatalf("a nil Manager must log info on the process-global InfoLog; got %q", got)
	}
	if got := errs.String(); !strings.Contains(got, "nil-receiver error") {
		t.Fatalf("a nil Manager must log errors on the process-global ErrorLog; got %q", got)
	}
}

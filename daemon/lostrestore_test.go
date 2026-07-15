package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	stdlog "log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/config"
	aflog "github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// recoverFakeBackend counts Recover calls and emulates LocalBackend.Recover's
// success contract (flip to Running) or a configured failure. Type is
// overridable so the remote-skip rule can be exercised.
type recoverFakeBackend struct {
	*session.FakeBackend
	mu       sync.Mutex
	failWith error
	recovers int
	typeName string
}

func (b *recoverFakeBackend) Recover(inst *session.Instance) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recovers++
	if b.failWith != nil {
		return b.failWith
	}
	inst.SetStatusForTest(session.Running)
	return nil
}

func (b *recoverFakeBackend) recoverCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.recovers
}

func (b *recoverFakeBackend) Type() string {
	if b.typeName != "" {
		return b.typeName
	}
	return "local"
}

// Capabilities mirrors the Type override: a "remote" double reports a
// WorkspaceRemote descriptor (Recover=false) so the lost-restore remote-skip
// rule fires; otherwise it inherits FakeBackend's local parity (#1592 Phase 1).
func (b *recoverFakeBackend) Capabilities() session.Capabilities {
	if b.typeName == "remote" {
		return session.Capabilities{Workspace: session.WorkspaceRemote}
	}
	return b.FakeBackend.Capabilities()
}

// zeroRestoreBackoff makes every RestoreLostSessions pass an attempt.
func zeroRestoreBackoff(t *testing.T) {
	t.Helper()
	prev := lostRestoreBackoffBase
	lostRestoreBackoffBase = 0
	t.Cleanup(func() { lostRestoreBackoffBase = prev })
}

// TestRestoreLostSessions_RecoversLostInstance: the core loop — a Lost local
// session gets exactly one Recover, comes back Running, and its retry state is
// dropped.
func TestRestoreLostSessions_RecoversLostInstance(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "stranded", backend, true, session.Lost)

	manager.RestoreLostSessions()

	if got := backend.recoverCalls(); got != 1 {
		t.Fatalf("recover calls = %d, want 1", got)
	}
	if got := inst.GetStatus(); got != session.Running {
		t.Fatalf("status = %v, want Running after recovery", got)
	}
	manager.mu.Lock()
	_, hasState := manager.lostRestoreStates[daemonInstanceKey(repoID, "stranded")]
	manager.mu.Unlock()
	if hasState {
		t.Fatal("successful recovery must drop the retry state")
	}

	// A healed session must not be touched again.
	manager.RestoreLostSessions()
	if got := backend.recoverCalls(); got != 1 {
		t.Fatalf("recover calls after heal = %d, want still 1", got)
	}
}

func TestRestoreSession_RecoversLostInstanceOnDemand(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "stranded", backend, true, session.Lost)
	key := daemonInstanceKey(repoID, "stranded")
	manager.mu.Lock()
	manager.lostRestoreStates[key] = &lostRestoreState{consecutiveFailures: 3}
	manager.mu.Unlock()

	_, err := manager.RestoreSession(RestoreSessionRequest{Title: "stranded", RepoID: repoID})
	if err != nil {
		t.Fatalf("RestoreSession returned error: %v", err)
	}
	if got := backend.recoverCalls(); got != 1 {
		t.Fatalf("recover calls = %d, want 1", got)
	}
	if got := inst.GetStatus(); got != session.Running {
		t.Fatalf("status = %v, want Running after manual restore", got)
	}
	manager.mu.Lock()
	_, hasState := manager.lostRestoreStates[key]
	manager.mu.Unlock()
	if hasState {
		t.Fatal("manual restore must drop Lost retry state")
	}
}

func TestRestoreSession_RecoversDeadInstanceOnDemand(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "dead", backend, true, session.Ready)
	_ = inst.Transition(session.ObserveLiveness(session.LiveDead))

	_, err := manager.RestoreSession(RestoreSessionRequest{Title: "dead", RepoID: repoID})
	if err != nil {
		t.Fatalf("RestoreSession returned error: %v", err)
	}
	if got := backend.recoverCalls(); got != 1 {
		t.Fatalf("recover calls = %d, want 1", got)
	}
	if got := inst.GetStatus(); got != session.Running {
		t.Fatalf("status = %v, want Running after manual restore", got)
	}
}

// TestRestoreLostSessions_KeepsRetryingAndHeals mirrors the #1128 root-ensure
// guarantee for the general loop: failures past the escalation threshold never
// park the retry permanently, and the first pass after the cause clears heals.
func TestRestoreLostSessions_KeepsRetryingAndHeals(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	zeroRestoreBackoff(t)
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend(), failWith: errors.New("worktree unavailable")}
	inst := registerStarted(t, manager, repoID, repoPath, "unlucky", backend, true, session.Lost)

	attempts := lostRestoreEscalationThreshold + 3
	for i := 0; i < attempts; i++ {
		manager.RestoreLostSessions()
	}
	if got := backend.recoverCalls(); got != attempts {
		t.Fatalf("loop must keep attempting past the escalation threshold: want %d attempts, got %d", attempts, got)
	}

	// The cause clears (the outage ends): the very next pass must heal.
	backend.mu.Lock()
	backend.failWith = nil
	backend.mu.Unlock()
	manager.RestoreLostSessions()

	if got := inst.GetStatus(); got != session.Running {
		t.Fatalf("status = %v, want Running on the first pass after the cause cleared", got)
	}
}

// TestRestoreLostSessions_BacksOffBetweenFailures: passes inside the backoff
// window must not re-attempt (the cheap-retry discipline, not a hot loop).
func TestRestoreLostSessions_BacksOffBetweenFailures(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	prev := lostRestoreBackoffBase
	lostRestoreBackoffBase = time.Hour
	t.Cleanup(func() { lostRestoreBackoffBase = prev })
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend(), failWith: errors.New("still down")}
	registerStarted(t, manager, repoID, repoPath, "waiting", backend, true, session.Lost)

	manager.RestoreLostSessions()
	manager.RestoreLostSessions()
	manager.RestoreLostSessions()

	if got := backend.recoverCalls(); got != 1 {
		t.Fatalf("passes inside the backoff window must not re-attempt: want 1, got %d", got)
	}
}

// TestRestoreLostSessions_PersistsAfterFailedRecover is the regression for
// #1532: when Recover fails after partial success (worktree+branch rebuilt,
// branchCreatedByUs flipped true, then the tmux spawn fails), the loop must
// persist the instance so that durable state survives a daemon restart —
// mirroring the manual restore path. Before the fix the failure branch returned
// without persisting, so a rebuilt-but-not-persisted branchCreatedByUs was lost
// and the branch was later orphaned on kill.
func TestRestoreLostSessions_PersistsAfterFailedRecover(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	zeroRestoreBackoff(t)

	// A real registered worktree whose in-memory record already carries the
	// rebuild's branchCreatedByUs=true (the state Recover leaves behind before its
	// tmux spawn fails). The seeded disk record (seedDiskInstance) has no worktree
	// data, so it loads branchCreatedByUs=false — the stale value the bug leaves.
	wtPath := filepath.Join(filepath.Dir(repoPath), "repo-1532")
	branch := "af/persist-1532"
	if out, err := exec.Command("git", "-C", repoPath, "worktree", "add", "-b", branch, wtPath).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}
	gw, err := sessiongit.NewGitWorktreeFromStorage(repoPath, wtPath, "persist-1532", branch, "", false, true)
	if err != nil {
		t.Fatalf("NewGitWorktreeFromStorage: %v", err)
	}

	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend(), failWith: errors.New("tmux spawn failed after rebuild")}
	inst := registerStarted(t, manager, repoID, repoPath, "persist-1532", backend, true, session.Lost)
	inst.SetGitWorktreeForTest(gw)

	manager.RestoreLostSessions()

	if got := backend.recoverCalls(); got != 1 {
		t.Fatalf("recover calls = %d, want 1", got)
	}
	// The failed-recover branch must have persisted the in-memory instance: the
	// on-disk record now reflects its Lost status and its rebuilt worktree state.
	rec := recordFor(t, repoID, "persist-1532")
	if rec == nil {
		t.Fatal("record must still exist after a failed recover")
	}
	if rec.Status != session.Lost {
		t.Fatalf("persisted status = %v, want Lost (a failed recover must persist the instance)", rec.Status)
	}
	if rec.Worktree.BranchCreatedByUs == nil || !*rec.Worktree.BranchCreatedByUs {
		t.Fatalf("persisted branchCreatedByUs = %v, want true (the rebuild's flag must survive a failed recover)", rec.Worktree.BranchCreatedByUs)
	}
}

// TestRestoreLostSessions_PersistsAfterSuccessfulRecover is the #1841 twin of
// the failed-recover case above: Recover mutates durable worktree state on the
// way to SUCCESS too, so the success path must write it back as well.
//
// A vanished worktree whose branch is also gone drives Recover into
// RebuildFreshFromRecordedBase, which recreates the branch and flips
// branchCreatedByUs true (and rewrites baseCommitSHA) before the tmux spawn
// succeeds. The poll loop does not cover the gap: Recover's ConfirmLive already
// left the instance LiveRunning, so a later tick compares LiveRunning against
// LiveRunning and persistPollChange returns without writing. A daemon restart
// before an idle transition then reloads a stale branchCreatedByUs=false, the
// next recovery skips the rebuild (the worktree exists again), and kill never
// deletes the branch af itself created — leaving it orphaned.
func TestRestoreLostSessions_PersistsAfterSuccessfulRecover(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	zeroRestoreBackoff(t)

	// Mirrors the failed-recover fixture: the in-memory record carries the
	// rebuild's branchCreatedByUs=true, while the seeded disk record has no
	// worktree data at all — so only a persist on the success path can put the
	// flag on disk.
	wtPath := filepath.Join(filepath.Dir(repoPath), "repo-1841")
	branch := "af/persist-1841"
	if out, err := exec.Command("git", "-C", repoPath, "worktree", "add", "-b", branch, wtPath).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}
	gw, err := sessiongit.NewGitWorktreeFromStorage(repoPath, wtPath, "persist-1841", branch, "", false, true)
	if err != nil {
		t.Fatalf("NewGitWorktreeFromStorage: %v", err)
	}

	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "persist-1841", backend, true, session.Lost)
	inst.SetGitWorktreeForTest(gw)

	manager.RestoreLostSessions()

	if got := backend.recoverCalls(); got != 1 {
		t.Fatalf("recover calls = %d, want 1", got)
	}
	rec := recordFor(t, repoID, "persist-1841")
	if rec == nil {
		t.Fatal("record must still exist after a successful recover")
	}
	if rec.Worktree.BranchCreatedByUs == nil || !*rec.Worktree.BranchCreatedByUs {
		t.Fatalf("persisted branchCreatedByUs = %v, want true (the rebuild's flag must survive a successful recover)", rec.Worktree.BranchCreatedByUs)
	}
	// The recovered branch itself must land on disk too: a restart that reloads a
	// record with no branch recorded cannot clean up what recovery created.
	if rec.Worktree.BranchName != branch {
		t.Fatalf("persisted branch = %q, want %q (a successful recover must persist its worktree record)", rec.Worktree.BranchName, branch)
	}
}

type failPtyFactory struct{}

func (failPtyFactory) Start(*exec.Cmd) (*os.File, error) {
	return nil, errors.New("tmux spawn should not be reached when worktree is missing")
}

func (failPtyFactory) Close() {}

// TestRestoreLostSessions_LogsVanishedWorktreeOnce covers the #1303
// instrumentation: when a live registered Lost session points at a worktree
// directory that disappeared and the branch is also gone, the daemon emits one
// high-visibility ERROR with the diagnostic context and does not repeat it on
// the next retry.
func TestRestoreLostSessions_LogsVanishedWorktreeOnce(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	zeroRestoreBackoff(t)

	worktreePath := filepath.Join(t.TempDir(), "repo-vanished")
	branch := "af/vanished-worktree"
	if out, err := exec.Command("git", "-C", repoPath, "worktree", "add", "-b", branch, worktreePath).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}
	gw, err := sessiongit.NewGitWorktreeFromStorage(repoPath, worktreePath, "vanished", branch, "", false, true)
	if err != nil {
		t.Fatalf("NewGitWorktreeFromStorage: %v", err)
	}
	if err := os.RemoveAll(worktreePath); err != nil {
		t.Fatalf("remove worktree directory: %v", err)
	}
	if out, err := exec.Command("git", "-C", repoPath, "update-ref", "-d", "refs/heads/"+branch).CombinedOutput(); err != nil {
		t.Fatalf("delete branch ref: %v\n%s", err, out)
	}

	inst, err := session.NewInstance(session.InstanceOptions{Title: "vanished", Path: repoPath, Program: "claude"})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	inst.Branch = branch
	inst.SetBackend(&session.LocalBackend{})
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Lost)
	inst.SetGitWorktreeForTest(gw)
	inst.SetTmuxSession(tmux.NewTmuxSessionFromSanitizedNameWithDeps(
		"af_1303_vanished",
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

	seedDiskInstance(t, repoID, "vanished", repoPath)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(repoID, "vanished")] = inst
	manager.mu.Unlock()

	var buf bytes.Buffer
	prevError := aflog.ErrorLog
	aflog.ErrorLog = stdlog.New(&buf, "ERROR: ", 0)
	t.Cleanup(func() { aflog.ErrorLog = prevError })

	manager.RestoreLostSessions()
	manager.RestoreLostSessions()

	logged := buf.String()
	if count := strings.Count(logged, "WORKTREE_MISSING_DETECTED"); count != 1 {
		t.Fatalf("missing-worktree diagnostic count = %d, want 1; logs:\n%s", count, logged)
	}
	for _, want := range []string{
		`classification="unexpected_external_removal"`,
		`title="vanished"`,
		`repo_id="` + repoID + `"`,
		`worktree_path="` + worktreePath + `"`,
		`branch="` + branch + `"`,
		`liveness="LiveLost"`,
		`status="Lost"`,
		`parent_exists=true`,
		`repo_exists=true`,
		`git_worktree_registered="true"`,
		`branch_exists="false"`,
		`recover_error="recover: session \"vanished\" worktree unavailable: stat `,
	} {
		if !strings.Contains(logged, want) {
			t.Fatalf("missing diagnostic field %s in logs:\n%s", want, logged)
		}
	}
}

// TestRestoreLostSessions_SkipsIneligible: tombstoned records (their only
// future is a finished kill), sessions with a kill in flight, remote sessions
// (flagged, not auto-restored), non-Lost sessions, and the reserved root
// (EnsureRootAgents owns it) are never Recover'd.
func TestRestoreLostSessions_SkipsIneligible(t *testing.T) {
	t.Run("tombstoned", func(t *testing.T) {
		manager, repoID, repoPath := newStatusTestManager(t)
		backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
		inst := registerStarted(t, manager, repoID, repoPath, "corpse", backend, true, session.Lost)
		inst.MarkUserKilled()
		manager.RestoreLostSessions()
		if got := backend.recoverCalls(); got != 0 {
			t.Fatalf("a tombstoned session must never be restored, got %d recover calls", got)
		}
	})

	t.Run("kill in flight", func(t *testing.T) {
		manager, repoID, repoPath := newStatusTestManager(t)
		backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
		registerStarted(t, manager, repoID, repoPath, "dying", backend, true, session.Lost)
		key := daemonInstanceKey(repoID, "dying")
		manager.mu.Lock()
		manager.killsInFlight[key] = struct{}{}
		manager.mu.Unlock()
		manager.RestoreLostSessions()
		if got := backend.recoverCalls(); got != 0 {
			t.Fatalf("a session mid-kill must not be restored, got %d recover calls", got)
		}
	})

	t.Run("remote", func(t *testing.T) {
		manager, repoID, repoPath := newStatusTestManager(t)
		backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend(), typeName: "remote"}
		registerStarted(t, manager, repoID, repoPath, "faraway", backend, true, session.Lost)
		manager.RestoreLostSessions()
		manager.RestoreLostSessions()
		if got := backend.recoverCalls(); got != 0 {
			t.Fatalf("remote sessions are not auto-restored in v1, got %d recover calls", got)
		}
	})

	t.Run("non-lost statuses", func(t *testing.T) {
		// Archived (#1028) is included: an archived session is deliberately
		// quiescent and must NEVER be auto-restored — only an explicit
		// RestoreArchived brings it back. (In production it also loads
		// started=false, but registering it started here proves the ==Lost gate
		// alone already fences it out.)
		for _, status := range []session.Status{session.Running, session.Ready, session.Loading, session.Deleting, session.Archived} {
			manager, repoID, repoPath := newStatusTestManager(t)
			backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
			registerStarted(t, manager, repoID, repoPath, "healthy", backend, true, status)
			manager.RestoreLostSessions()
			if got := backend.recoverCalls(); got != 0 {
				t.Fatalf("status %v must not be restored, got %d recover calls", status, got)
			}
		}
	})

	t.Run("reserved root", func(t *testing.T) {
		manager, repoID, repoPath := newStatusTestManager(t)
		backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
		registerStarted(t, manager, repoID, repoPath, session.RootSessionTitle, backend, true, session.Lost)
		manager.RestoreLostSessions()
		if got := backend.recoverCalls(); got != 0 {
			t.Fatalf("the reserved root is EnsureRootAgents' job, got %d recover calls", got)
		}
	})
}

// raceBackend is a readyFakeBackend whose Recover and Kill can each be held
// open on channels, and which counts both, so kill-vs-recover interleaving can
// be pinned exactly.
type raceBackend struct {
	readyFakeBackend
	mu             sync.Mutex
	kills          int
	recovers       int
	recoverStarted chan struct{}
	recoverBlock   chan struct{}
	killStarted    chan struct{}
	killBlock      chan struct{}
}

func (b *raceBackend) Recover(inst *session.Instance) error {
	b.mu.Lock()
	b.recovers++
	b.mu.Unlock()
	if b.recoverStarted != nil {
		close(b.recoverStarted)
		b.recoverStarted = nil
	}
	if b.recoverBlock != nil {
		<-b.recoverBlock
	}
	inst.SetStatusForTest(session.Running)
	return nil
}

func (b *raceBackend) Kill(inst *session.Instance) error {
	b.mu.Lock()
	b.kills++
	b.mu.Unlock()
	if b.killStarted != nil {
		close(b.killStarted)
		b.killStarted = nil
	}
	if b.killBlock != nil {
		<-b.killBlock
	}
	return b.readyFakeBackend.Kill(inst)
}

func (b *raceBackend) counts() (kills, recovers int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.kills, b.recovers
}

// installRaceBackend registers backend as the factory product and creates a
// tracked session titled title, returning the manager, repoID, and instance.
func installRaceBackend(t *testing.T, backend *raceBackend, title string) (*Manager, string, *session.Instance) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	restore := session.SetBackendFactoryForTest(func(opts session.InstanceOptions, absPath string) (session.Backend, error) {
		fake := session.NewFakeBackend()
		fake.CompleteStart()
		backend.readyFakeBackend = readyFakeBackend{fake}
		return backend, nil
	})
	t.Cleanup(restore)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title:    title,
		RepoPath: repoPath,
		Program:  "claude",
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	manager.mu.Lock()
	inst := manager.instances[daemonInstanceKey(repo.ID, title)]
	manager.mu.Unlock()
	if inst == nil {
		t.Fatal("created instance not tracked")
	}
	return manager, repo.ID, inst
}

// TestKillSession_WaitsForInFlightRecover is the Greptile P1 on #1137: a
// KillSession arriving while the restore loop is mid-Recover must WAIT for
// the recover attempt to finish and then tear the freshly-restored session
// down — never interleave. End state: record deleted, instance dropped,
// exactly one teardown, exactly one recover.
func TestKillSession_WaitsForInFlightRecover(t *testing.T) {
	backend := &raceBackend{
		recoverStarted: make(chan struct{}),
		recoverBlock:   make(chan struct{}),
	}
	manager, repoID, inst := installRaceBackend(t, backend, "contested")
	zeroRestoreBackoff(t)
	inst.SetStatusForTest(session.Lost)

	recoverStarted := backend.recoverStarted
	restoreDone := make(chan struct{})
	go func() {
		manager.RestoreLostSessions()
		close(restoreDone)
	}()
	<-recoverStarted // the loop is inside Recover, holding the op lock

	killDone := make(chan error, 1)
	go func() {
		_, kerr := manager.KillSession(KillSessionRequest{Title: "contested", RepoID: repoID})
		killDone <- kerr
	}()

	// The kill must be blocked behind the in-flight recover attempt.
	select {
	case err := <-killDone:
		t.Fatalf("KillSession completed mid-Recover (err=%v); it must wait for the attempt", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(backend.recoverBlock) // recover finishes (session "restored")
	<-restoreDone
	select {
	case err := <-killDone:
		if err != nil {
			t.Fatalf("KillSession after recover: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("KillSession never proceeded after the recover attempt finished")
	}

	kills, recovers := backend.counts()
	if kills != 1 || recovers != 1 {
		t.Fatalf("want exactly one teardown and one recover, got kills=%d recovers=%d", kills, recovers)
	}
	if rec := recordFor(t, repoID, "contested"); rec != nil {
		t.Fatalf("killed session's record must be deleted, still present: %+v", rec)
	}
	manager.mu.Lock()
	_, tracked := manager.instances[daemonInstanceKey(repoID, "contested")]
	manager.mu.Unlock()
	if tracked {
		t.Fatal("killed session must be dropped from the manager")
	}
}

// TestRestoreLostSessions_SkipsDuringInFlightKill is the reverse ordering: a
// restore pass arriving while a KillSession teardown is in flight must not
// touch the session — no recover call, ever — and after the kill completes
// the session is gone for good.
func TestRestoreLostSessions_SkipsDuringInFlightKill(t *testing.T) {
	backend := &raceBackend{
		killStarted: make(chan struct{}),
		killBlock:   make(chan struct{}),
	}
	manager, repoID, inst := installRaceBackend(t, backend, "doomed")
	zeroRestoreBackoff(t)
	inst.SetStatusForTest(session.Lost)

	killStarted := backend.killStarted
	killDone := make(chan error, 1)
	go func() {
		_, kerr := manager.KillSession(KillSessionRequest{Title: "doomed", RepoID: repoID})
		killDone <- kerr
	}()
	<-killStarted // teardown in flight, op lock + killsInFlight held

	manager.RestoreLostSessions() // must return promptly without recovering

	close(backend.killBlock)
	if err := <-killDone; err != nil {
		t.Fatalf("KillSession: %v", err)
	}

	manager.RestoreLostSessions() // session is gone; still nothing to recover

	if _, recovers := backend.counts(); recovers != 0 {
		t.Fatalf("a session being killed must never be recovered, got %d recover calls", recovers)
	}
	if rec := recordFor(t, repoID, "doomed"); rec != nil {
		t.Fatalf("killed session's record must be deleted, still present: %+v", rec)
	}
}

// TestRestoreLostSessions_StateCleanedForGoneSessions: retry state for
// sessions that were killed or healed does not accumulate.
func TestRestoreLostSessions_StateCleanedForGoneSessions(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	zeroRestoreBackoff(t)
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend(), failWith: errors.New("down")}
	registerStarted(t, manager, repoID, repoPath, "doomed", backend, true, session.Lost)

	manager.RestoreLostSessions()
	key := daemonInstanceKey(repoID, "doomed")
	manager.mu.Lock()
	_, hasState := manager.lostRestoreStates[key]
	manager.mu.Unlock()
	if !hasState {
		t.Fatal("a failed attempt must record retry state")
	}

	// The session disappears (user killed it; record + map entry gone).
	manager.mu.Lock()
	delete(manager.instances, key)
	manager.mu.Unlock()
	manager.RestoreLostSessions()

	manager.mu.Lock()
	_, hasState = manager.lostRestoreStates[key]
	manager.mu.Unlock()
	if hasState {
		t.Fatal("retry state for a gone session must be dropped")
	}
}

// TestRestoreLostSessions_AliveRemoteSandboxIsNotReprovisioned is the #1794
// restore-time guard: the last gate before the irreversible step.
//
// A remote Recover is not a reconnect — recoverSandbox provisions a BRAND-NEW
// sandbox and clones the branch back from origin — so running it against a
// sandbox that is still up orphans that sandbox and abandons every commit it
// never pushed. The poll's debounce makes a blip-induced Lost unlikely, but this
// row may have been marked Lost many ticks ago (restore backs off to 5 minutes)
// and the transport may have healed since. A stale verdict must not authorize a
// destructive re-provision, so the loop re-probes against live state first.
//
// Here the sandbox answers as alive, so the Lost mark is wrong: no Recover may
// fire, and the row must heal rather than sit Lost forever.
func TestRestoreLostSessions_AliveRemoteSandboxIsNotReprovisioned(t *testing.T) {
	withRemoteLossThresholds(t, 3, time.Minute, time.Second)
	zeroRestoreBackoff(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/agent/alive" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"alive": true},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	manager, repoID, repoPath := newStatusTestManager(t)
	inst, backend := registerStartedRemote(t, manager, repoID, repoPath, "remote-lost-but-live", srv.URL, session.Lost)

	manager.RestoreLostSessions()

	if got := backend.recoverCalls(); got != 0 {
		t.Fatalf("Recover calls = %d, want 0 — the sandbox answered as alive, and re-provisioning over a live sandbox orphans it and destroys its unpushed work (#1794)", got)
	}
	if got := inst.GetLiveness(); got == session.LiveLost {
		t.Fatal("liveness is still LiveLost: a sandbox proven alive must have its Lost mark cleared, not be left stranded in a state the loop refuses to act on")
	}
}

// TestRestoreLostSessions_UnreachableRemoteSandboxIsReprovisioned is the
// companion that keeps the #1794 recheck from becoming a blanket "never restore
// remote sessions". A genuinely gone sandbox (nothing listening, so the probe is
// refused outright) must still be re-provisioned — that recovery IS the feature
// (#1108/#1782), and the recheck only exists to veto it when the sandbox proves
// it is still there.
func TestRestoreLostSessions_UnreachableRemoteSandboxIsReprovisioned(t *testing.T) {
	withRemoteLossThresholds(t, 3, time.Minute, time.Second)
	zeroRestoreBackoff(t)

	manager, repoID, repoPath := newStatusTestManager(t)
	_, backend := registerStartedRemote(t, manager, repoID, repoPath, "remote-really-gone", "http://127.0.0.1:1", session.Lost)

	manager.RestoreLostSessions()

	if got := backend.recoverCalls(); got != 1 {
		t.Fatalf("Recover calls = %d, want 1 — an unreachable sandbox must still be recovered; the recheck is a veto on live sandboxes, not a block on remote restore", got)
	}
}

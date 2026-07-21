package daemon

import (
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// attachCountingBackend records how many times an instance was torn down via
// Kill (destructive: kills the tmux session and removes the worktree) versus
// CloseAttachOnly (non-destructive: releases only this client's attach PTY).
// It lets the findSession tests assert that a duplicate Instance built from
// disk is discarded with CloseAttachOnly and is never Kill'd (#867) — a Kill
// on a duplicate would tear down the live session the canonical, tracked
// Instance shares.
type attachCountingBackend struct {
	*session.FakeBackend
	kills           atomic.Int32
	closeAttachOnly atomic.Int32
}

func (b *attachCountingBackend) Kill(i *session.Instance) error {
	b.kills.Add(1)
	return b.FakeBackend.Kill(i)
}

func (b *attachCountingBackend) CloseAttachOnly(i *session.Instance) error {
	b.closeAttachOnly.Add(1)
	return b.FakeBackend.CloseAttachOnly(i)
}

func newCountingInstance(t *testing.T, title, repoPath string) (*session.Instance, *attachCountingBackend) {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   title,
		Path:    repoPath,
		Program: "claude",
	})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	backend := &attachCountingBackend{FakeBackend: session.NewFakeBackend()}
	inst.SetBackend(backend)
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)
	return inst, backend
}

// seedDiskInstance writes a single Running instance to the repo's on-disk
// store without registering it in any manager's in-memory map, modelling the
// issue's precondition: a session that exists on disk but is not (yet) tracked
// because an earlier restore failed transiently.
func seedDiskInstance(t *testing.T, repoID, title, repoPath string) {
	t.Helper()
	seeded, err := json.Marshal([]session.InstanceData{
		{Title: title, Path: repoPath, Status: session.Running},
	})
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := config.LoadState().SaveInstances(repoID, seeded); err != nil {
		t.Fatalf("seed disk: %v", err)
	}
}

// TestFindSessionDiscardsDuplicateWhenCanonicalRaced is the regression test for
// sachiniyer/agent-factory#867. findSession releases m.mu before building an
// Instance from disk, so a concurrent refresh can restore and register the
// canonical Instance during that window. The pre-fix code returned the
// freshly-built duplicate anyway — an untracked Instance owning its own attach
// PTY. SendPrompt then leaked that PTY, and KillSession called instance.Kill()
// on it, tearing down the tmux session and worktree the canonical Instance
// still owned.
//
// The race is driven deterministically through the fromInstanceDataForRefresh
// seam: the first call (the initial refreshLocked) fails, modelling the
// transient restore failure that leaves the session disk-only; the second call
// (the disk build after m.mu is released) registers the canonical Instance —
// simulating the concurrent refresh winning — and returns the duplicate. We
// assert findSession returns the tracked canonical, discards the duplicate via
// CloseAttachOnly (not Kill), and never Kills the canonical.
func TestFindSessionDiscardsDuplicateWhenCanonicalRaced(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	seedDiskInstance(t, repo.ID, "raced", repoPath)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	key := daemonInstanceKey(repo.ID, "raced")
	canonical, canonicalBackend := newCountingInstance(t, "raced", repoPath)
	duplicate, duplicateBackend := newCountingInstance(t, "raced", repoPath)

	var calls atomic.Int32
	prev := fromInstanceDataForRefresh
	fromInstanceDataForRefresh = func(d session.InstanceData) (*session.Instance, error) {
		switch calls.Add(1) {
		case 1:
			// The initial refreshLocked inside findSession: model the
			// transient restore failure so the session stays disk-only.
			return nil, fmt.Errorf("transient restore failure")
		default:
			// The disk build after m.mu was released: a concurrent refresh
			// has just won the race and registered the canonical Instance.
			manager.mu.Lock()
			manager.instances[key] = canonical
			manager.mu.Unlock()
			return duplicate, nil
		}
	}
	t.Cleanup(func() { fromInstanceDataForRefresh = prev })

	inst, rid, _, err := manager.findSession("raced", repo.ID)
	if err != nil {
		t.Fatalf("findSession: %v", err)
	}
	if rid != repo.ID {
		t.Fatalf("rid = %q, want %q", rid, repo.ID)
	}
	if inst != canonical {
		t.Fatalf("findSession returned a non-canonical instance; want the tracked one")
	}
	if got := duplicateBackend.closeAttachOnly.Load(); got != 1 {
		t.Fatalf("duplicate CloseAttachOnly calls = %d, want 1 (PTY must be released)", got)
	}
	if got := duplicateBackend.kills.Load(); got != 0 {
		t.Fatalf("duplicate Kill calls = %d, want 0 (Kill would tear down the shared session)", got)
	}
	if got := canonicalBackend.kills.Load(); got != 0 {
		t.Fatalf("canonical Kill calls = %d, want 0", got)
	}

	manager.mu.Lock()
	tracked := manager.instances[key]
	manager.mu.Unlock()
	if tracked != canonical {
		t.Fatalf("canonical instance is no longer tracked after findSession")
	}
}

// TestFindSessionRegistersRestoredInstanceWhenUntracked covers the other side
// of the #867 fix: when no canonical Instance won the race, findSession must
// register the Instance it built from disk so callers operate on a *tracked*
// Instance (just as the refresh loop would have) rather than an orphan whose
// PTY leaks. The restored Instance is left tracked and is not torn down.
func TestFindSessionRegistersRestoredInstanceWhenUntracked(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	seedDiskInstance(t, repo.ID, "lonely", repoPath)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	key := daemonInstanceKey(repo.ID, "lonely")
	restored, restoredBackend := newCountingInstance(t, "lonely", repoPath)

	var calls atomic.Int32
	prev := fromInstanceDataForRefresh
	fromInstanceDataForRefresh = func(d session.InstanceData) (*session.Instance, error) {
		switch calls.Add(1) {
		case 1:
			return nil, fmt.Errorf("transient restore failure")
		default:
			return restored, nil
		}
	}
	t.Cleanup(func() { fromInstanceDataForRefresh = prev })

	inst, rid, _, err := manager.findSession("lonely", repo.ID)
	if err != nil {
		t.Fatalf("findSession: %v", err)
	}
	if rid != repo.ID {
		t.Fatalf("rid = %q, want %q", rid, repo.ID)
	}
	if inst != restored {
		t.Fatalf("findSession returned an unexpected instance")
	}
	if got := restoredBackend.closeAttachOnly.Load(); got != 0 {
		t.Fatalf("restored CloseAttachOnly calls = %d, want 0", got)
	}
	if got := restoredBackend.kills.Load(); got != 0 {
		t.Fatalf("restored Kill calls = %d, want 0", got)
	}
	manager.mu.Lock()
	tracked := manager.instances[key]
	manager.mu.Unlock()
	if tracked != restored {
		t.Fatalf("restored instance was not registered in m.instances")
	}
}

// TestFindSessionReturnsTrackedInstanceWithoutDiskBuild asserts the common,
// non-racing path is unchanged: when the canonical Instance is already tracked,
// findSession returns it without ever building an Instance from disk.
func TestFindSessionReturnsTrackedInstanceWithoutDiskBuild(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	seedDiskInstance(t, repo.ID, "tracked", repoPath)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	key := daemonInstanceKey(repo.ID, "tracked")
	canonical, _ := newCountingInstance(t, "tracked", repoPath)
	manager.mu.Lock()
	manager.instances[key] = canonical
	manager.mu.Unlock()

	var diskBuilds atomic.Int32
	prev := fromInstanceDataForRefresh
	fromInstanceDataForRefresh = func(d session.InstanceData) (*session.Instance, error) {
		diskBuilds.Add(1)
		return prev(d)
	}
	t.Cleanup(func() { fromInstanceDataForRefresh = prev })

	inst, rid, _, err := manager.findSession("tracked", repo.ID)
	if err != nil {
		t.Fatalf("findSession: %v", err)
	}
	if rid != repo.ID {
		t.Fatalf("rid = %q, want %q", rid, repo.ID)
	}
	if inst != canonical {
		t.Fatalf("findSession did not return the tracked instance")
	}
	if got := diskBuilds.Load(); got != 0 {
		t.Fatalf("fromInstanceDataForRefresh called %d times; the tracked path must not build from disk", got)
	}
}

package daemon

import (
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// The /v1/sessions/{id}/stream route resolves its {id} segment by the session's
// stable id FIRST, then by title (#1592 Phase 5 PR4). The TUI/apiclient pass a
// title there (unchanged); the browser web client passes the globally-unique
// session id so a title shared across two repos names exactly one session. These
// tests pin resolveStreamSession — the resolver agentServerForStream delegates to.

// TestResolveStreamSessionByID resolves a tracked instance by its stable id and
// returns the instance's own repoID and title (so the killsInFlight gate keys off
// the real title even when the caller addressed the session by id).
func TestResolveStreamSessionByID(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	inst, _ := newCountingInstance(t, "worker", repoPath)
	inst.ID = "id-worker-123"
	key := daemonInstanceKey(repo.ID, "worker")
	manager.mu.Lock()
	manager.instances[key] = inst
	manager.mu.Unlock()

	// Address by the stable id, with no repo_id (the browser's call shape).
	got, rid, title, err := manager.resolveStreamSession("id-worker-123", "")
	if err != nil {
		t.Fatalf("resolveStreamSession by id: %v", err)
	}
	if got != inst {
		t.Fatalf("resolved instance = %p, want the tracked instance %p", got, inst)
	}
	if rid != repo.ID {
		t.Fatalf("resolved repoID = %q, want %q", rid, repo.ID)
	}
	if title != "worker" {
		t.Fatalf("resolved title = %q, want %q (killsInFlight gate keys off title)", title, "worker")
	}
}

// TestResolveStreamSessionByIDRehydratesOnMiss is the #2187 fail-first. A
// stable-id request can arrive after an earlier refresh skipped a persisted row
// and before the next poll. The resolver must refresh and scan the stable-id
// namespace again before it reinterprets that opaque id as a title.
func TestResolveStreamSessionByIDRehydratesOnMiss(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	const stableID = "id-restorable-worker"
	const decoyID = "id-title-decoy"
	seeded, err := json.Marshal([]session.InstanceData{
		{ID: stableID, Title: "worker", Path: repoPath, Status: session.Running},
		// The opaque stable ID is also a different row's title. After refresh,
		// stable identity must win before title fallback is even considered.
		{ID: decoyID, Title: stableID, Path: repoPath, Status: session.Running},
	})
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := config.LoadState().SaveInstances(repo.ID, seeded); err != nil {
		t.Fatalf("seed disk: %v", err)
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	canonical, _ := newCountingInstance(t, "worker", repoPath)
	canonical.ID = stableID
	decoy, _ := newCountingInstance(t, stableID, repoPath)
	decoy.ID = decoyID
	var diskBuilds atomic.Int32
	prev := fromInstanceDataForRefresh
	fromInstanceDataForRefresh = func(data session.InstanceData) (*session.Instance, error) {
		diskBuilds.Add(1)
		switch data.ID {
		case stableID:
			return canonical, nil
		case decoyID:
			return decoy, nil
		default:
			return nil, errors.New("unexpected persisted row")
		}
	}
	t.Cleanup(func() { fromInstanceDataForRefresh = prev })

	got, rid, title, err := manager.resolveStreamSession(stableID, "")
	if err != nil {
		t.Fatalf("resolveStreamSession after rehydration: %v", err)
	}
	if got != canonical || rid != repo.ID || title != "worker" {
		t.Fatalf("rehydrated stable-id target = (%p, %q, %q), want (%p, %q, %q)",
			got, rid, title, canonical, repo.ID, "worker")
	}
	if builds := diskBuilds.Load(); builds != 2 {
		t.Fatalf("stable-id miss materialized %d persisted rows, want one refresh of both rows", builds)
	}
}

// TestResolveStreamSessionIDBeatsCrossRepoTitleCollision is the reason the browser
// keys off id: two sessions share a title across two repos, so a title alone is
// ambiguous, but each id names exactly one. Resolving by the second session's id
// must return THAT session (its repo), never the first same-titled one.
func TestResolveStreamSessionIDBeatsCrossRepoTitleCollision(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoA := setupControlRepo(t)
	repoB := setupControlRepo(t)
	ra, err := config.RepoFromPath(repoA)
	if err != nil {
		t.Fatalf("RepoFromPath A: %v", err)
	}
	rb, err := config.RepoFromPath(repoB)
	if err != nil {
		t.Fatalf("RepoFromPath B: %v", err)
	}
	if ra.ID == rb.ID {
		t.Fatalf("test needs two distinct repos; both hashed to %q", ra.ID)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	instA, _ := newCountingInstance(t, "dup", repoA)
	instA.ID = "id-in-repo-a"
	instB, _ := newCountingInstance(t, "dup", repoB)
	instB.ID = "id-in-repo-b"
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(ra.ID, "dup")] = instA
	manager.instances[daemonInstanceKey(rb.ID, "dup")] = instB
	manager.mu.Unlock()

	got, rid, _, err := manager.resolveStreamSession("id-in-repo-b", "")
	if err != nil {
		t.Fatalf("resolveStreamSession: %v", err)
	}
	if got != instB {
		t.Fatalf("id resolution picked the wrong same-titled session across repos")
	}
	if rid != rb.ID {
		t.Fatalf("resolved repoID = %q, want repo B %q", rid, rb.ID)
	}
}

// TestResolveStreamSessionRepoScopedTitleBeatsForeignStableID pins the other
// authoritative request shape: repo_id means the opaque segment is a title in
// that repo, even when another repo happens to use the same bytes as its stable
// ID. The hot path must never jump namespaces before checking the scoped target.
func TestResolveStreamSessionRepoScopedTitleBeatsForeignStableID(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoA := setupControlRepo(t)
	repoB := setupControlRepo(t)
	ra, err := config.RepoFromPath(repoA)
	if err != nil {
		t.Fatalf("RepoFromPath A: %v", err)
	}
	rb, err := config.RepoFromPath(repoB)
	if err != nil {
		t.Fatalf("RepoFromPath B: %v", err)
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	const target = "scoped-title"
	scoped, _ := newCountingInstance(t, target, repoA)
	scoped.ID = "scoped-instance-id"
	foreign, _ := newCountingInstance(t, "foreign", repoB)
	foreign.ID = target
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(ra.ID, target)] = scoped
	manager.instances[daemonInstanceKey(rb.ID, foreign.Title)] = foreign
	manager.mu.Unlock()

	got, rid, title, err := manager.resolveStreamSession(target, ra.ID)
	if err != nil {
		t.Fatalf("resolve repo-scoped stream target: %v", err)
	}
	if got != scoped || rid != ra.ID || title != target {
		t.Fatalf("repo-scoped target = (%p, %q, %q), want (%p, %q, %q); foreign stable ID won",
			got, rid, title, scoped, ra.ID, target)
	}
}

// TestResolveStreamSessionRepoScopedTitleBeatsForeignStableIDAfterRefresh pins
// the same authority after a cache miss. Fixing only trackedStreamSessionLocked
// still lets findSessionByStableID invert the target once refresh materializes
// the foreign row, which was the review finding.
func TestResolveStreamSessionRepoScopedTitleBeatsForeignStableIDAfterRefresh(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoA := setupControlRepo(t)
	repoB := setupControlRepo(t)
	ra, err := config.RepoFromPath(repoA)
	if err != nil {
		t.Fatalf("RepoFromPath A: %v", err)
	}
	rb, err := config.RepoFromPath(repoB)
	if err != nil {
		t.Fatalf("RepoFromPath B: %v", err)
	}
	const target = "refreshed-scoped-title"
	rows := map[string]session.InstanceData{
		ra.ID: {ID: "scoped-instance-id", Title: target, Path: repoA, Status: session.Running},
		rb.ID: {ID: target, Title: "foreign", Path: repoB, Status: session.Running},
	}
	for rid, data := range rows {
		raw, marshalErr := json.Marshal([]session.InstanceData{data})
		if marshalErr != nil {
			t.Fatalf("marshal %s: %v", rid, marshalErr)
		}
		if saveErr := config.LoadState().SaveInstances(rid, raw); saveErr != nil {
			t.Fatalf("save %s: %v", rid, saveErr)
		}
	}

	scoped, _ := newCountingInstance(t, target, repoA)
	scoped.ID = rows[ra.ID].ID
	foreign, _ := newCountingInstance(t, "foreign", repoB)
	foreign.ID = rows[rb.ID].ID
	prev := fromInstanceDataForRefresh
	fromInstanceDataForRefresh = func(data session.InstanceData) (*session.Instance, error) {
		switch data.ID {
		case scoped.ID:
			return scoped, nil
		case foreign.ID:
			return foreign, nil
		default:
			return nil, errors.New("unexpected persisted row")
		}
	}
	t.Cleanup(func() { fromInstanceDataForRefresh = prev })
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	got, rid, title, err := manager.resolveStreamSession(target, ra.ID)
	if err != nil {
		t.Fatalf("resolve refreshed repo-scoped stream target: %v", err)
	}
	if got != scoped || rid != ra.ID || title != target {
		t.Fatalf("refreshed repo-scoped target = (%p, %q, %q), want (%p, %q, %q); foreign stable ID won",
			got, rid, title, scoped, ra.ID, target)
	}
}

// TestResolveStreamSessionTrackedTitleSkipsDiskRefresh pins the TUI hot path: a
// repo-scoped title already restored in the daemon resolves from m.instances
// without walking persisted session history before every preview capture.
func TestResolveStreamSessionTrackedTitleSkipsDiskRefresh(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	inst, _ := newCountingInstance(t, "byname", repoPath)
	inst.ID = "id-byname"
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(repo.ID, "byname")] = inst
	manager.mu.Unlock()

	// Replace the on-disk roster with an unrelated row after the canonical target
	// is tracked. Any refresh would both invoke this materializer and evict the
	// target from the map; the preview resolver must do neither.
	seedDiskInstance(t, repo.ID, "disk-only", repoPath)
	var diskBuilds atomic.Int32
	prev := fromInstanceDataForRefresh
	fromInstanceDataForRefresh = func(session.InstanceData) (*session.Instance, error) {
		diskBuilds.Add(1)
		return nil, errors.New("unexpected preview-path disk materialization")
	}
	t.Cleanup(func() { fromInstanceDataForRefresh = prev })

	// Address by TITLE (no id match) — the TUI's call shape.
	got, rid, title, err := manager.resolveStreamSession("byname", repo.ID)
	if err != nil {
		t.Fatalf("resolveStreamSession by title: %v", err)
	}
	if got != inst {
		t.Fatalf("title fallback did not return the tracked instance")
	}
	if rid != repo.ID {
		t.Fatalf("resolved repoID = %q, want %q", rid, repo.ID)
	}
	if title != "byname" {
		t.Fatalf("resolved title = %q, want %q", title, "byname")
	}
	if got := diskBuilds.Load(); got != 0 {
		t.Fatalf("tracked preview resolution materialized %d on-disk session(s), want 0", got)
	}
}

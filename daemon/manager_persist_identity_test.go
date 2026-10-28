package daemon

import (
	"encoding/json"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// TestPersistInstanceData_RefusesCrossIdentityClobber is the direct regression
// test for the disk-write half of #1723. persistInstanceData used to locate the
// row to overwrite by TITLE ONLY, so persisting a stale instance A over a
// same-titled but different-identity instance B silently reverted B's persisted
// stable id (and every other field) to A's — corrupting the identity the daemon
// reconciles on (#1195). The fixed writer keys the match on the stable id too and
// REFUSES to write when the on-disk row's id differs from the instance being
// persisted. Here A and B share the title "worker" but carry different ids;
// persisting A must error and leave B's row untouched.
func TestPersistInstanceData_RefusesCrossIdentityClobber(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}

	const title = "worker"
	// Seed the disk with instance B — the freshly recreated session that a
	// concurrent kill/recreate has just written.
	seed, err := json.Marshal([]session.InstanceData{{
		ID:     "id-new",
		Title:  title,
		Path:   repoPath,
		Status: session.Running,
		Branch: "branch-100",
	}})
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := config.LoadState().SaveInstances(repo.ID, seed); err != nil {
		t.Fatalf("seed disk: %v", err)
	}

	// Attempt to persist instance A — the killed session, same title, DIFFERENT
	// stable id. This is what a stale metadata write would flush.
	stale := session.InstanceData{
		ID:     "id-old",
		Title:  title,
		Path:   repoPath,
		Status: session.Running,
		Branch: "branch-1",
	}
	if err := persistInstanceData(repo.ID, stale); err == nil {
		t.Fatal("persistInstanceData overwrote a same-titled row with a different stable id (identity corruption); expected refusal")
	}

	// B's row must still carry B's identity and data, untouched.
	raw, err := config.LoadRepoInstances(repo.ID)
	if err != nil {
		t.Fatalf("LoadRepoInstances: %v", err)
	}
	var got []session.InstanceData
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal instances: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 persisted instance, got %d: %+v", len(got), got)
	}
	if got[0].ID != "id-new" || got[0].Branch != "branch-100" {
		t.Fatalf("cross-identity clobber: disk record = %+v, want id-new with branch-100 intact", got[0])
	}
}

// TestPersistInstanceData_UpdatesIDMatchedRowDespiteEarlierTitleCollision
// guards the row-selection edge (Greptile P1): the shared writer keys on the
// stable id, so a stray earlier row that shares the title but carries a
// DIFFERENT id must not mask the legitimate write to the later id-matched row.
// The transient kill/recreate window this whole fix targets is exactly when two
// same-title rows can momentarily coexist, so a valid id-matched persist must
// still land — updating the correct row and leaving the unrelated one untouched.
func TestPersistInstanceData_UpdatesIDMatchedRowDespiteEarlierTitleCollision(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}

	const title = "worker"
	// Two same-title rows: a stray/foreign one first, the real one (id-current)
	// second.
	seed, err := json.Marshal([]session.InstanceData{
		{ID: "id-stray", Title: title, Path: repoPath, Status: session.Running, Branch: "branch-1"},
		{ID: "id-current", Title: title, Path: repoPath, Status: session.Running, Branch: "branch-2"},
	})
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := config.LoadState().SaveInstances(repo.ID, seed); err != nil {
		t.Fatalf("seed disk: %v", err)
	}

	// Persist an update whose stable id matches the LATER row.
	update := session.InstanceData{
		ID: "id-current", Title: title, Path: repoPath, Status: session.Running,
		Branch: "branch-99",
	}
	if err := persistInstanceData(repo.ID, update); err != nil {
		t.Fatalf("persistInstanceData rejected a valid id-matched update behind an earlier same-title row: %v", err)
	}

	raw, err := config.LoadRepoInstances(repo.ID)
	if err != nil {
		t.Fatalf("LoadRepoInstances: %v", err)
	}
	var got []session.InstanceData
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal instances: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 persisted instances, got %d: %+v", len(got), got)
	}
	byID := map[string]session.InstanceData{}
	for _, d := range got {
		byID[d.ID] = d
	}
	if cur := byID["id-current"]; cur.Branch != "branch-99" {
		t.Fatalf("id-current row not updated: branch = %q, want branch-99", cur.Branch)
	}
	if stray := byID["id-stray"]; stray.Branch != "branch-1" {
		t.Fatalf("earlier same-title row was clobbered: %+v, want branch-1 intact", stray.Branch)
	}
}

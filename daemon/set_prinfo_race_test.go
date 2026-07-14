package daemon

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
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
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
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
		PRInfo: session.PRInfoData{Number: 100, State: "OPEN"},
	}})
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := config.LoadState().SaveInstances(repo.ID, seed); err != nil {
		t.Fatalf("seed disk: %v", err)
	}

	// Attempt to persist instance A — the killed session, same title, DIFFERENT
	// stable id. This is exactly what a stale SetPRInfo would flush.
	stale := session.InstanceData{
		ID:     "id-old",
		Title:  title,
		Path:   repoPath,
		Status: session.Running,
		PRInfo: session.PRInfoData{Number: 1, State: "MERGED"},
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
	if got[0].ID != "id-new" || got[0].PRInfo.Number != 100 || got[0].PRInfo.State != "OPEN" {
		t.Fatalf("cross-identity clobber: disk record = %+v, want id-new with PR #100 OPEN intact", got[0])
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
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}

	const title = "worker"
	// Two same-title rows: a stray/foreign one first, the real one (id-current)
	// second.
	seed, err := json.Marshal([]session.InstanceData{
		{ID: "id-stray", Title: title, Path: repoPath, Status: session.Running, PRInfo: session.PRInfoData{Number: 1, State: "MERGED"}},
		{ID: "id-current", Title: title, Path: repoPath, Status: session.Running, PRInfo: session.PRInfoData{Number: 2, State: "OPEN"}},
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
		PRInfo: session.PRInfoData{Number: 99, State: "OPEN"},
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
	if cur := byID["id-current"]; cur.PRInfo.Number != 99 {
		t.Fatalf("id-current row not updated: PR = %+v, want #99", cur.PRInfo)
	}
	if stray := byID["id-stray"]; stray.PRInfo.Number != 1 || stray.PRInfo.State != "MERGED" {
		t.Fatalf("earlier same-title row was clobbered: %+v, want PR #1 MERGED intact", stray.PRInfo)
	}
}

// TestSetPRInfo_RaceKillRecreateNeverCorruptsIdentity is the load-bearing -race
// regression test for #1723. It races SetPRInfo against KillSession+CreateSession
// on the same title. Without the per-session op-lock and stale-instance re-check,
// SetPRInfo could resolve the OLD (about-to-be-killed) instance, then flush its
// data — including its stale stable id — over the row the recreated NEW instance
// just wrote, reverting the persisted identity.
//
// The invariant checked after every round is that the persisted stable id equals
// the currently-tracked instance's id: SetPRInfo must either target the correct
// current instance or fail cleanly without writing. A violation (persisted id !=
// tracked id) is the corruption. Run under -race, this also proves SetPRInfo's
// instance-map and disk access are properly synchronized against the lifecycle
// ops.
func TestSetPRInfo_RaceKillRecreateNeverCorruptsIdentity(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	const title = "worker"
	key := daemonInstanceKey(repo.ID, title)

	if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title: title, RepoPath: repoPath, Program: "claude", AutoYes: true,
	}); err != nil {
		t.Fatalf("initial CreateSession: %v", err)
	}

	// Several SetPRInfo callers per round widen the chance one is mid-flight
	// (resolved the OLD instance, awaiting its persist) exactly as the recreate
	// writes the NEW row — the interleaving that corrupts the identity on the
	// unfixed code.
	const setters = 6
	for i := 0; i < 80; i++ {
		var wg sync.WaitGroup
		wg.Add(setters + 1)
		for s := 0; s < setters; s++ {
			go func() {
				defer wg.Done()
				// Errors are expected and correct when the instance was replaced
				// mid-flight — the point is that no stale write lands.
				_ = manager.SetPRInfo(SetPRInfoRequest{
					Title: title, RepoID: repo.ID,
					PRInfo: session.PRInfoData{Number: 7, Title: "feat", URL: "https://example/pr/7", State: "OPEN"},
				})
			}()
		}
		go func() {
			defer wg.Done()
			_, _ = manager.KillSession(KillSessionRequest{Title: title, RepoID: repo.ID})
			_, _ = manager.CreateSession(context.Background(), CreateSessionRequest{
				Title: title, RepoPath: repoPath, Program: "claude", AutoYes: true,
			})
		}()
		wg.Wait()

		manager.mu.Lock()
		current := manager.instances[key]
		manager.mu.Unlock()
		if current == nil {
			t.Fatalf("iter %d: no tracked instance after kill+recreate", i)
		}

		raw, err := config.LoadRepoInstances(repo.ID)
		if err != nil {
			t.Fatalf("iter %d LoadRepoInstances: %v", i, err)
		}
		var stored []session.InstanceData
		if err := json.Unmarshal(raw, &stored); err != nil {
			t.Fatalf("iter %d unmarshal instances: %v", i, err)
		}
		var rec *session.InstanceData
		for j := range stored {
			if stored[j].Title == title {
				rec = &stored[j]
				break
			}
		}
		if rec == nil {
			t.Fatalf("iter %d: title %q missing from storage after kill+recreate", i, title)
		}
		if rec.ID != current.ID {
			t.Fatalf("iter %d: persisted stable id %q != tracked instance id %q — kill/recreate identity corruption (#1723)", i, rec.ID, current.ID)
		}
	}
}

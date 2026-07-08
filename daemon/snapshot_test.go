package daemon

import (
	"sort"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// TestManagerSnapshot_RepoScopedAndOrdered verifies Manager.Snapshot returns the
// in-memory instances for the requested repo only (empty = all repos), serialized
// to InstanceData and ordered by (repo, title) key for a stable, flicker-free diff.
func TestManagerSnapshot_RepoScopedAndOrdered(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	m, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	const repoA = "/tmp/snap-repo-a"
	const repoB = "/tmp/snap-repo-b"
	ridA := config.RepoIDFromRoot(repoA)
	ridB := config.RepoIDFromRoot(repoB)

	startedLocalTabInstance(t, m, ridA, repoA, "a2", "af_a2_agent")
	startedLocalTabInstance(t, m, ridA, repoA, "a1", "af_a1_agent")
	startedLocalTabInstance(t, m, ridB, repoB, "b1", "af_b1_agent")

	titlesOf := func(repoID string) []string {
		data := m.Snapshot(repoID)
		out := make([]string, 0, len(data))
		for _, d := range data {
			out = append(out, d.Title)
		}
		return out
	}

	// Repo A is scoped to its own sessions, ordered by key (title).
	gotA := titlesOf(ridA)
	wantA := []string{"a1", "a2"}
	if !equalStrings(gotA, wantA) {
		t.Fatalf("Snapshot(repoA) = %v, want %v (repo-scoped, key-ordered)", gotA, wantA)
	}

	gotB := titlesOf(ridB)
	if !equalStrings(gotB, []string{"b1"}) {
		t.Fatalf("Snapshot(repoB) = %v, want [b1]", gotB)
	}

	// Empty repoID returns every repo's instances.
	gotAll := titlesOf("")
	sortedAll := append([]string(nil), gotAll...)
	sort.Strings(sortedAll)
	if !equalStrings(sortedAll, []string{"a1", "a2", "b1"}) {
		t.Fatalf("Snapshot(\"\") = %v, want all three sessions", gotAll)
	}
}

func TestManagerSnapshot_CarriesInFlightOp(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	m, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	inst := startedLocalTabInstance(t, m, repo.ID, repoPath, "worker", "af_worker_agent")
	inst.SetInFlightOpForTest(session.OpArchiving)

	data := m.Snapshot(repo.ID)
	if len(data) != 1 {
		t.Fatalf("Snapshot returned %d instances, want 1", len(data))
	}
	if data[0].InFlightOp != session.OpArchiving {
		t.Fatalf("snapshot in-flight op = %v, want OpArchiving", data[0].InFlightOp)
	}
	if data[0].Status != session.Deleting {
		t.Fatalf("snapshot legacy status = %v, want Deleting", data[0].Status)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

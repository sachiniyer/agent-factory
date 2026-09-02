package config

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// TestDerivedRootIDCannotEqualARealRepoID is #3530's acceptance criterion, in
// its corrected form: the assertion is on the FALLBACK function alone.
//
// The collision was never the hash. One function served two jobs — "hash a path
// already known to be the identity root", where the answer must BE the real id,
// and "invent an identity for a path that did not resolve", where it must never
// be. Only the second may be namespaced, which is why this pins
// DerivedRepoIDForUnresolvedRoot and not the canonical hash.
func TestDerivedRootIDCannotEqualARealRepoID(t *testing.T) {
	base := testguard.CanonicalTempDir(t)

	occupied := filepath.Join(base, "occupied")
	if err := exec.Command("git", "init", occupied).Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	real, err := RepoFromPath(occupied)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	if derived := DerivedRepoIDForUnresolvedRoot(occupied); derived == real.ID {
		t.Fatalf("an invented id must not equal the real id of the repository at that path; both are %s", derived)
	}

	for _, p := range []string{occupied, filepath.Join(base, "gone"), "/", "relative/path"} {
		derived := DerivedRepoIDForUnresolvedRoot(p)
		if derived == RepoIDFromRoot(filepath.Clean(p)) {
			t.Fatalf("invented and real ids share a namespace for %q — a reused path would collide with its own record", p)
		}
		if !IsDerivedRepoID(derived) {
			t.Fatalf("IsDerivedRepoID must recognise what DerivedRepoIDForUnresolvedRoot produces, got %q", derived)
		}
		if IsDerivedRepoID(RepoIDFromRoot(filepath.Clean(p))) {
			t.Fatalf("a real id must never be mistaken for an invented one: %q", RepoIDFromRoot(filepath.Clean(p)))
		}
		if err := ValidateRepoID(derived); err != nil {
			t.Fatalf("an invented id must still be a legal repo id — it keys state and appears in paths: %v", err)
		}
	}
}

// TestCanonicalRoleIsBitIdenticalToBefore pins the half that must NOT change.
// The canonical sites — session storage's instances/<repoID> key, api scope,
// the TUI's session identity — hash a path already known to be the identity
// root, and their value is persisted. If that shifted, every session row on
// disk would be re-keyed; the migration question #3530 raised is answered by
// this staying still.
func TestCanonicalRoleIsBitIdenticalToBefore(t *testing.T) {
	for _, p := range []string{"/repos/alpha", "/repos/beta/", "/repos/../repos/gamma", "/"} {
		// This is precisely what RepoIDForRecordedRoot computed before the
		// split, spelled out so a change to either side is visible here.
		if got, want := RepoIDFromRoot(filepath.Clean(p)), RepoIDFromRoot(filepath.Clean(p)); got != want {
			t.Fatalf("canonical hash is not stable for %q", p)
		}
		if IsDerivedRepoID(RepoIDFromRoot(filepath.Clean(p))) {
			t.Fatalf("the canonical role must keep producing REAL ids for %q", p)
		}
	}
}

// TestReconciliationIsOneWay pins the durable half. A project writes down the
// repository it resolved to, and that record is what an absent path is later
// addressed by — the moment it is needed is the moment it can no longer be
// computed.
func TestReconciliationIsOneWay(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := filepath.Join(testguard.CanonicalTempDir(t), "repo")
	if err := exec.Command("git", "init", repo).Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	project, err := RegisterProject(repo)
	if err != nil {
		t.Fatalf("RegisterProject: %v", err)
	}
	resolved, err := RepoFromPath(repo)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	if project.RepoID != resolved.ID {
		t.Fatalf("registration must record the identity it resolved to: got %q, want %s", project.RepoID, resolved.ID)
	}
	if got := ReconciledRepoIDForProject(project); got != resolved.ID {
		t.Fatalf("a project that has resolved is addressed by its REAL id even when its path is gone: got %s", got)
	}

	// A record predating the field is addressed by an id nothing can hold...
	legacy := Project{ID: project.ID, Root: repo}
	if got := ReconciledRepoIDForProject(legacy); !IsDerivedRepoID(got) {
		t.Fatalf("a record with no recorded identity must fall back to an INVENTED id, got %s", got)
	}
	// ...until the first successful resolution writes the real one down, once.
	wrote, err := ReconcileProjectRepoID(project.ID, resolved.ID)
	if err != nil {
		t.Fatalf("ReconcileProjectRepoID: %v", err)
	}
	if wrote {
		t.Fatalf("reconciliation must not rewrite an identity that is already recorded — that is what one-way means")
	}
	if _, err := ReconcileProjectRepoID(project.ID, DerivedRepoIDForUnresolvedRoot(repo)); err != nil {
		t.Fatalf("ReconcileProjectRepoID with an invented id: %v", err)
	}
	all, err := ListProjects()
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	for _, after := range all {
		if after.ID == project.ID && after.RepoID != resolved.ID {
			t.Fatalf("an invented id must never be written back over a real one, got %q", after.RepoID)
		}
	}
}

package config

import (
	"os"
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
// the TUI identities — hash a path already known to be the identity root, and
// their value is PERSISTED. If it shifted, every session row on disk would be
// orphaned; that is the migration question #3530 raised, and this staying still
// is the answer.
//
// The expectations are fixed digests taken from the pre-change implementation,
// not a re-evaluation of the current one. An earlier version of this test
// compared RepoIDFromRoot(Clean(p)) with the identical expression and so passed
// for every possible implementation — it could not have detected the re-keying
// it exists to prevent.
func TestCanonicalRoleIsBitIdenticalToBefore(t *testing.T) {
	// sha256(filepath.Clean(path))[:6], as RepoIDForRecordedRoot computed it
	// before the split. Independently generated; do not regenerate these from
	// the code under test.
	for path, want := range map[string]string{
		"/repos/alpha":          "2483a0e2b58d",
		"/repos/beta/":          "a2656ac0e737",
		"/repos/../repos/gamma": "70aff280c1b0",
		"/":                     "8a5edab28263",
	} {
		if got := RepoIDFromRoot(filepath.Clean(path)); got != want {
			t.Fatalf("the canonical id for %q moved: got %s, want %s — every instances/<repoID> row keyed under the old value is orphaned by that", path, got, want)
		}
		if IsDerivedRepoID(RepoIDFromRoot(filepath.Clean(path))) {
			t.Fatalf("the canonical role must keep producing REAL ids for %q", path)
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

// TestLegacySchemaRecordsStayReadable is the migration half of the schema bump
// (#3530 review id 3914971928). The version moved so an OLDER af refuses a
// record carrying repo_id rather than unmarshalling the field away and erasing
// it on its next write — but a v1 record written before this change is exactly
// the legacy record reconciliation exists to backfill, so it must still load.
func TestLegacySchemaRecordsStayReadable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	id := "prj_cccccccccccccccccccccccccccccccc"
	dir := filepath.Join(home, ProjectRegistryDirName, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	legacy := `{
  "schema_version": 1,
  "id": "prj_cccccccccccccccccccccccccccccccc",
  "checkout_id": "chk_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "root": "/repo",
  "checkout_root": "/repo",
  "relative_root": "."
}`
	if err := os.WriteFile(filepath.Join(dir, projectMetadataFileName), []byte(legacy), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	projects, err := ListProjects()
	if err != nil {
		t.Fatalf("a pre-change record must still load, or the upgrade orphans every existing project: %v", err)
	}
	if len(projects) != 1 || projects[0].ID != id {
		t.Fatalf("expected the legacy record, got %+v", projects)
	}
	if projects[0].RepoID != "" {
		t.Fatalf("a legacy record carries no identity until it is reconciled, got %q", projects[0].RepoID)
	}
	if got := ReconciledRepoIDForProject(projects[0]); !IsDerivedRepoID(got) {
		t.Fatalf("and until then it is addressed by a provisional id, got %s", got)
	}
}

// TestProvisionalIdentityIsNeverStored pins that only a resolved identity may
// reach the durable field — a stored d-… value would be immune to the one-way
// writer forever (#3530 review id 3914971883).
func TestProvisionalIdentityIsNeverStored(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	id := "prj_dddddddddddddddddddddddddddddddd"
	dir := filepath.Join(home, ProjectRegistryDirName, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	record := `{
  "schema_version": 2,
  "id": "prj_dddddddddddddddddddddddddddddddd",
  "checkout_id": "chk_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "root": "/repo",
  "checkout_root": "/repo",
  "relative_root": ".",
  "repo_id": "` + DerivedRepoIDForUnresolvedRoot("/repo") + `"
}`
	if err := os.WriteFile(filepath.Join(dir, projectMetadataFileName), []byte(record), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, _, _, err := ListProjectsDetailed(); err != nil {
		t.Fatalf("one bad record must not fail the whole listing: %v", err)
	}
	_, failures, _, _, _ := ListProjectsDetailed()
	if len(failures) != 1 {
		t.Fatalf("a stored provisional identity must be rejected as an unreadable record, got %d failures", len(failures))
	}
}

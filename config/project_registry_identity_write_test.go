package config

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// initRepoWithCommit makes a repository that can host a linked worktree.
func initRepoWithCommit(t *testing.T, path string) {
	t.Helper()
	if err := exec.Command("git", "init", path).Run(); err != nil {
		t.Fatalf("git init %s: %v", path, err)
	}
	for _, args := range [][]string{
		{"-C", path, "config", "user.email", "t@t"},
		{"-C", path, "config", "user.name", "t"},
		{"-C", path, "commit", "--allow-empty", "-m", "init"},
	} {
		if err := exec.Command("git", args...).Run(); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
}

// rawProjectRecord reads a record as it is actually stored, so a test can
// assert on the bytes an OLDER af would read rather than on what this binary
// happens to unmarshal.
func rawProjectRecord(t *testing.T, id string) map[string]any {
	t.Helper()
	dir, err := projectRegistryDir()
	if err != nil {
		t.Fatalf("projectRegistryDir: %v", err)
	}
	data, err := os.ReadFile(projectRecordPath(dir, id))
	if err != nil {
		t.Fatalf("read record %s: %v", id, err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal record %s: %v", id, err)
	}
	return raw
}

// makeRecordLegacy rewrites a record as an older af wrote it: schema v1 and no
// repo_id. It is the fixture every backfill path is exercised against, and it
// deliberately does NOT go through this package's writers — a legacy record
// cannot be produced by the fixed derivation (that is the whole point of it).
func makeRecordLegacy(t *testing.T, id string) {
	t.Helper()
	dir, err := projectRegistryDir()
	if err != nil {
		t.Fatalf("projectRegistryDir: %v", err)
	}
	path := projectRecordPath(dir, id)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read record %s: %v", id, err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal record %s: %v", id, err)
	}
	raw["schema_version"] = 1
	delete(raw, "repo_id")
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		t.Fatalf("marshal legacy record: %v", err)
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
		t.Fatalf("write legacy record: %v", err)
	}
}

func assertStampedV2(t *testing.T, id, wantRepoID, path string) {
	t.Helper()
	raw := rawProjectRecord(t, id)
	got, _ := raw["repo_id"].(string)
	if got != wantRepoID {
		t.Fatalf("%s: recorded repo_id = %q, want %s", path, got, wantRepoID)
	}
	version, _ := raw["schema_version"].(float64)
	if int(version) != projectRegistrySchemaVersion {
		t.Fatalf("%s: a record that gained repo_id is still schema v%d — an older af accepts it and erases the field on its next rewrite, which is exactly the durable identity loss the bump exists to prevent (#3530 review id 3915722471)", path, int(version))
	}
}

// TestEveryRepoIDWriteStampsTheSchema pins the invariant the version bump
// actually needs: a record that CARRIES repo_id is a v2 record, whichever
// writer put it there.
//
// The bump alone protects nothing. An older af rejects a version it does not
// know, but it happily accepts a v1 record — and unmarshals the unknown
// repo_id away, so its next rebind or checkout rediscovery writes the record
// back without the durable identity. Reconciliation stamped; the three
// registration/rebind writers did not, so every legacy record backfilled by
// them stayed erasable (#3530 review id 3915722471).
func TestEveryRepoIDWriteStampsTheSchema(t *testing.T) {
	t.Run("registration backfill", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		repo := filepath.Join(testguard.CanonicalTempDir(t), "repo")
		initRepoWithCommit(t, repo)
		project, err := RegisterProject(repo)
		if err != nil {
			t.Fatalf("RegisterProject: %v", err)
		}
		makeRecordLegacy(t, project.ID)

		again, err := RegisterProject(repo)
		if err != nil {
			t.Fatalf("re-register: %v", err)
		}
		resolved, err := RepoFromPath(repo)
		if err != nil {
			t.Fatalf("RepoFromPath: %v", err)
		}
		if again.RepoID != resolved.ID {
			t.Fatalf("the backfill must record the resolved identity, got %q", again.RepoID)
		}
		assertStampedV2(t, project.ID, resolved.ID, "registration backfill")
	})

	t.Run("rebind", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		base := testguard.CanonicalTempDir(t)
		repo := filepath.Join(base, "repo")
		replacement := filepath.Join(base, "replacement")
		initRepoWithCommit(t, repo)
		initRepoWithCommit(t, replacement)
		project, err := RegisterProject(repo)
		if err != nil {
			t.Fatalf("RegisterProject: %v", err)
		}
		makeRecordLegacy(t, project.ID)

		rebound, err := RebindProject(project.ID, replacement)
		if err != nil {
			t.Fatalf("RebindProject: %v", err)
		}
		resolved, err := RepoFromPath(replacement)
		if err != nil {
			t.Fatalf("RepoFromPath: %v", err)
		}
		if rebound.RepoID != resolved.ID {
			t.Fatalf("a rebind moves the identity to the one it resolved, got %q", rebound.RepoID)
		}
		assertStampedV2(t, project.ID, resolved.ID, "rebind")
	})

	t.Run("checkout rediscovery", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		base := testguard.CanonicalTempDir(t)
		repo := filepath.Join(base, "repo")
		moved := filepath.Join(base, "moved")
		initRepoWithCommit(t, repo)
		project, err := RegisterProject(repo)
		if err != nil {
			t.Fatalf("RegisterProject: %v", err)
		}
		makeRecordLegacy(t, project.ID)

		// The whole checkout moves, carrying its marker: registration
		// rediscovers the same checkout at a new root and updates both the
		// root and the identity, because a moved identity root is a different
		// real id.
		if err := os.Rename(repo, moved); err != nil {
			t.Fatalf("move checkout: %v", err)
		}
		rediscovered, err := RegisterProject(moved)
		if err != nil {
			t.Fatalf("re-register the moved checkout: %v", err)
		}
		if rediscovered.ID != project.ID {
			t.Fatalf("a moved checkout keeps its project identity: got %s, want %s", rediscovered.ID, project.ID)
		}
		resolved, err := RepoFromPath(moved)
		if err != nil {
			t.Fatalf("RepoFromPath: %v", err)
		}
		assertStampedV2(t, project.ID, resolved.ID, "checkout rediscovery")
	})
}

// TestRegistrationRefusesAnIdentityFromADifferentRepository pins #3530 review
// id 3915722459.
//
// resolveProjectBinding probed the path twice: once for the checkout root,
// common directory and marker, and again — separately — for the identity to
// write down. A path that changed repositories between the two returned
// SUCCESS from both, so registration recorded repository A's root and marker
// with repository B's id, permanently. A later delete or policy lookup by that
// record then targets B's state.
//
// The flip is driven through a seam because the window is microseconds wide in
// a real registration; what it produces is a genuine repository change at the
// same path, not a stubbed answer.
func TestRegistrationRefusesAnIdentityFromADifferentRepository(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	base := testguard.CanonicalTempDir(t)
	repoA := filepath.Join(base, "A")
	repoB := filepath.Join(base, "B")
	initRepoWithCommit(t, repoA)
	initRepoWithCommit(t, repoB)

	live := filepath.Join(base, "live")
	if err := exec.Command("git", "-C", repoA, "worktree", "add", live).Run(); err != nil {
		t.Fatalf("git worktree add: %v", err)
	}

	flipped := false
	projectBindingIdentityRaceHookForTest = func() {
		if flipped {
			return
		}
		flipped = true
		if err := exec.Command("git", "-C", repoA, "worktree", "remove", "--force", live).Run(); err != nil {
			t.Fatalf("detach the worktree from A: %v", err)
		}
		if err := exec.Command("git", "-C", repoB, "worktree", "add", live).Run(); err != nil {
			t.Fatalf("attach the path to B: %v", err)
		}
	}
	t.Cleanup(func() { projectBindingIdentityRaceHookForTest = nil })

	project, err := RegisterProject(live)
	if !flipped {
		t.Fatalf("fixture never ran the flip; the seam is not on the path between the two resolutions")
	}
	if err != nil {
		if !strings.Contains(err.Error(), "changed repositories") {
			t.Fatalf("refusing is correct, but the message must say what happened: %v", err)
		}
		return
	}
	// It did not refuse, so the record it wrote must at least be internally
	// consistent: the identity it recorded has to be the identity of the root
	// it recorded.
	rootRepo, rootErr := RepoFromPath(project.Root)
	if rootErr != nil {
		t.Fatalf("RepoFromPath(%s): %v", project.Root, rootErr)
	}
	if project.RepoID != rootRepo.ID {
		t.Fatalf("registration recorded root %s (repository %s) with identity %s — the root, the checkout marker and the id describe two different repositories", project.Root, rootRepo.ID, project.RepoID)
	}
}

// TestRegistrationRefusesARepointedPathAtCommitTime pins #3530 review id
// 3919195005.
//
// resolveProjectBinding runs BEFORE the registry lock, so a path repointed in
// between would be committed pairing the new workspace with the previous
// repository's identity and marker — permanently, because the one-way writer
// never replaces it. The re-check narrows that window to the write itself; it
// cannot close it, and says so.
func TestRegistrationRefusesARepointedPathAtCommitTime(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	base := testguard.CanonicalTempDir(t)
	// BARE clones, because that is the shape where the recorded root IS the
	// repointed path: a bare repository's linked workspace is what the record
	// stores, while a normal repository's record stores its unmoving main root.
	source := filepath.Join(base, "source")
	initRepoWithCommit(t, source)
	repoA := filepath.Join(base, "A.git")
	repoB := filepath.Join(base, "B.git")
	for _, bare := range []string{repoA, repoB} {
		if err := exec.Command("git", "clone", "--quiet", "--bare", source, bare).Run(); err != nil {
			t.Fatalf("git clone --bare %s: %v", bare, err)
		}
	}
	live := filepath.Join(base, "live")
	if err := exec.Command("git", "-C", repoA, "worktree", "add", "--detach", live).Run(); err != nil {
		t.Fatalf("git worktree add: %v", err)
	}

	// The flip happens after the binding is resolved and the registry lock is
	// held — the window this re-check exists for.
	flipped := false
	projectRegistryCommitRaceHookForTest = func() {
		if flipped {
			return
		}
		flipped = true
		if err := exec.Command("git", "-C", repoA, "worktree", "remove", "--force", live).Run(); err != nil {
			t.Fatalf("detach the worktree from A: %v", err)
		}
		if err := exec.Command("git", "-C", repoB, "worktree", "add", "--detach", live).Run(); err != nil {
			t.Fatalf("attach the path to B: %v", err)
		}
	}
	t.Cleanup(func() { projectRegistryCommitRaceHookForTest = nil })

	project, err := RegisterProject(live)
	if !flipped {
		t.Fatalf("fixture never ran the flip; the seam is not on the path to the commit")
	}
	if err != nil {
		if !strings.Contains(err.Error(), "changed repositories") {
			t.Fatalf("refusing is correct, but the message must say what happened: %v", err)
		}
		return
	}
	rootRepo, rootErr := RepoFromPath(project.Root)
	if rootErr != nil {
		t.Fatalf("RepoFromPath(%s): %v", project.Root, rootErr)
	}
	if project.RepoID != rootRepo.ID {
		t.Fatalf("registration committed root %s (repository %s) with identity %s", project.Root, rootRepo.ID, project.RepoID)
	}
}

// TestRegistrationRefusesAReplacedCheckoutAtCommitTime pins #3530 review id
// 3919490138.
//
// A repo ID is derived from the identity ROOT, so another clone at the same
// root produces the same one — the identity re-check passes while the checkout
// underneath is a different one. A record pairing the live checkout with the
// previous clone's CheckoutID fails its own marker proof immediately, leaving
// the project unresolved until someone rebinds it by hand.
func TestRegistrationRefusesAReplacedCheckoutAtCommitTime(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := filepath.Join(testguard.CanonicalTempDir(t), "repo")
	initRepoWithCommit(t, repo)

	replaced := false
	projectRegistryCommitRaceHookForTest = func() {
		if replaced {
			return
		}
		replaced = true
		// A different clone at the SAME root: same identity, different
		// checkout, and the marker af just wrote goes with the old one.
		if err := os.RemoveAll(filepath.Join(repo, ".git")); err != nil {
			t.Fatalf("remove the original checkout: %v", err)
		}
		initRepoWithCommit(t, repo)
	}
	t.Cleanup(func() { projectRegistryCommitRaceHookForTest = nil })

	project, err := RegisterProject(repo)
	if !replaced {
		t.Fatalf("fixture never ran the replacement; the seam is not on the path to the commit")
	}
	if err != nil {
		if !strings.Contains(err.Error(), "checkout at") {
			t.Fatalf("refusing is correct, but the message must say what happened: %v", err)
		}
		return
	}
	// It committed, so the record must at least prove against the checkout
	// that is actually there.
	matches, matchErr := ProjectCheckoutMatches(project.Root, project.CheckoutID)
	if matchErr != nil {
		t.Fatalf("ProjectCheckoutMatches: %v", matchErr)
	}
	if !matches {
		t.Fatalf("registration committed checkout id %s against a checkout that does not carry it: the record fails its own marker proof from the moment it is written", project.CheckoutID)
	}
}

// TestDeregisterByRecordedIdentityRefusesWhatItCannotEstablish pins the three
// refusals of #3530's identity-matched removal (review ids 3919996255,
// 3919996266, and claimantForRecord's absence rule).
func TestDeregisterByRecordedIdentityRefusesWhatItCannotEstablish(t *testing.T) {
	absent := func(root string) (bool, error) {
		_, err := os.Stat(root)
		return err != nil && PathDeterminatelyAbsent(err), nil
	}

	t.Run("removes the one absent row that recorded the identity", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		repo := filepath.Join(testguard.CanonicalTempDir(t), "repo")
		initRepoWithCommit(t, repo)
		project, err := RegisterProject(repo)
		if err != nil {
			t.Fatalf("RegisterProject: %v", err)
		}
		if err := os.RemoveAll(repo); err != nil {
			t.Fatalf("remove checkout: %v", err)
		}
		removed, root, err := DeregisterProjectByRecordedIdentity(project.RepoID, absent)
		if err != nil {
			t.Fatalf("DeregisterProjectByRecordedIdentity: %v", err)
		}
		if !removed || filepath.Clean(root) != filepath.Clean(project.Root) {
			t.Fatalf("expected the row to be removed by its recorded identity, got removed=%v root=%q", removed, root)
		}
	})

	t.Run("refuses while the recorded root is present", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		repo := filepath.Join(testguard.CanonicalTempDir(t), "repo")
		initRepoWithCommit(t, repo)
		project, err := RegisterProject(repo)
		if err != nil {
			t.Fatalf("RegisterProject: %v", err)
		}
		removed, _, err := DeregisterProjectByRecordedIdentity(project.RepoID, absent)
		if err != nil {
			t.Fatalf("DeregisterProjectByRecordedIdentity: %v", err)
		}
		if removed {
			t.Fatalf("a present recorded root may be an unproven occupant sharing the identity; removing the row destroys the original project's record on its behalf")
		}
	})

	t.Run("refuses when two rows carry the identity", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		base := testguard.CanonicalTempDir(t)
		first := filepath.Join(base, "first")
		second := filepath.Join(base, "second")
		initRepoWithCommit(t, first)
		initRepoWithCommit(t, second)
		a, err := RegisterProject(first)
		if err != nil {
			t.Fatalf("RegisterProject: %v", err)
		}
		b, err := RegisterProject(second)
		if err != nil {
			t.Fatalf("RegisterProject: %v", err)
		}
		// Two rows recording ONE identity, which is #3611's ambiguity — and
		// the ABSENT one must not be picked just because the other is filtered
		// out first.
		setRecordedRepoIDForTest(t, b.ID, a.RepoID)
		if err := os.RemoveAll(second); err != nil {
			t.Fatalf("remove second checkout: %v", err)
		}
		removed, _, err := DeregisterProjectByRecordedIdentity(a.RepoID, absent)
		if err != nil {
			t.Fatalf("DeregisterProjectByRecordedIdentity: %v", err)
		}
		if removed {
			t.Fatalf("two rows carry this identity, so which one it names is ambiguous: the removal must refuse rather than pick the absent one")
		}
	})

	t.Run("a read failure is not an empty result", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("AGENT_FACTORY_HOME", home)
		repo := filepath.Join(testguard.CanonicalTempDir(t), "repo")
		initRepoWithCommit(t, repo)
		project, err := RegisterProject(repo)
		if err != nil {
			t.Fatalf("RegisterProject: %v", err)
		}
		if err := os.RemoveAll(repo); err != nil {
			t.Fatalf("remove checkout: %v", err)
		}
		// The row that carries the identity is exactly the one that cannot be
		// read, so "no match" is unprovable.
		if err := os.WriteFile(projectRecordPath(mustRegistryDir(t), project.ID), []byte("{ not json"), 0o644); err != nil {
			t.Fatalf("corrupt record: %v", err)
		}
		if _, _, err := DeregisterProjectByRecordedIdentity(project.RepoID, absent); err == nil {
			t.Fatalf("an unreadable registry must be an error, not a silent no-op that leaves the row behind")
		}
	})
}

func mustRegistryDir(t *testing.T) string {
	t.Helper()
	dir, err := projectRegistryDir()
	if err != nil {
		t.Fatalf("projectRegistryDir: %v", err)
	}
	return dir
}

// setRecordedRepoIDForTest writes a specific identity into a record.
func setRecordedRepoIDForTest(t *testing.T, projectID, repoID string) {
	t.Helper()
	path := projectRecordPath(mustRegistryDir(t), projectID)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	var record map[string]any
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatalf("parse record: %v", err)
	}
	record["repo_id"] = repoID
	out, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("write record: %v", err)
	}
}

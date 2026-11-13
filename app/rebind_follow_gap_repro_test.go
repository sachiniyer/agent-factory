package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// initNestedInsideOuter builds the #7109/#7116 drift shape but BEFORE the
// drift: an outer repo with a nested git repo inside it. The nested repo has
// its own .git, so config.RepoFromPath(nested) resolves to nested (its own
// identity) — the state a rebind target is in when its checkout is healthy. The
// caller removes nested/.git later to model the post-commit drift the follow-up
// probe then sees. Returns outer, nested, the nested repo's identity, and the
// enclosing outer repo's identity.
func initNestedInsideOuter(t *testing.T) (outer, nested, nestedID, outerID string) {
	t.Helper()
	outer = initTestGitRepo(t)
	nested = filepath.Join(outer, "nested")
	require.NoError(t, os.Mkdir(nested, 0o755))
	runGit(t, nested, "init")
	runGit(t, nested, "config", "user.email", "test@example.com")
	runGit(t, nested, "config", "user.name", "test")
	nestedRepo, err := config.RepoFromPath(nested)
	require.NoError(t, err)
	nestedID = nestedRepo.ID
	outerRepo, err := config.RepoFromPath(outer)
	require.NoError(t, err)
	outerID = outerRepo.ID
	require.NotEqual(t, nestedID, outerID, "fixture: the nested and outer repos must have distinct ids")
	require.Equal(t, nestedID, nestedRepo.ID, "fixture: nested must resolve to itself while it has .git")
	return outer, nested, nestedID, outerID
}

// TestRebindFollowRefusesForeignRepoAtDriftedTarget pins the follow-up hole the
// identity guards of #7109/#7116 left open: followActiveRebind used to call
// switchToProjectRoot(root, "", ""), passing no recorded identity, so the
// local-mode repo-id and checkout-marker guards were both disarmed. When the
// checkout at the followed root drifted between the rebind commit and the
// follow-up probe — its .git removed while the directory survived inside an
// enclosing repo — RepoFromPath walked up to the enclosing foreign repo and the
// TUI silently re-scoped sessions, tasks and the program onto it, with no error
// shown. The follow-up now carries the record's RepoID/CheckoutID (from the same
// ListProjects read it already performs), so a drifted/foreign target is refused
// and surfaced exactly as the sidebar Enter / mouse click / picker Enter paths
// already refuse it.
func TestRebindFollowRefusesForeignRepoAtDriftedTarget(t *testing.T) {
	h, _ := activeRebindHome(t)
	beforeRepoRoot, beforeRepoID := h.repoRoot, h.repoID
	require.NotEmpty(t, beforeRepoID, "fixture: the active project must have a scope id")

	outer, nested, nestedID, outerID := initNestedInsideOuter(t)

	// The rebind commits `nested` (writing the record's RepoID = nestedID and a
	// checkout marker), THEN the seam de-gits it — modeling the window between
	// the registry write and the follow-up probe — so the probe walks up to the
	// enclosing `outer` repo. This is the exact #7109 shape, now exercised on the
	// one path that used to fail open.
	stubRebindThrough(t, func() {
		require.NoError(t, os.RemoveAll(filepath.Join(nested, ".git")),
			"fixture: remove the registered checkout's .git after the commit")
		probed, err := config.RepoFromPath(nested)
		require.NoError(t, err)
		require.Equal(t, outerID, probed.ID,
			"fixture: the de-gitted target must resolve to the enclosing foreign repo")
		require.NotEqual(t, nestedID, probed.ID,
			"fixture: the probe must no longer vouch for the registered identity")
	})

	h.Update(submitPickerRebind(t, h, nested)())

	// The follow-up refused the drifted target: the rail stays on the active
	// project and the foreign enclosing repo's identity is not adopted — the
	// silent foreign-repo re-scope the buggy call produced does not happen.
	assert.Equal(t, beforeRepoRoot, h.repoRoot,
		"the follow-up must not re-scope the rail onto the enclosing repo's root")
	assert.Equal(t, beforeRepoID, h.repoID,
		"the follow-up must not adopt the enclosing repo's identity")
	assert.NotEqual(t, outerID, h.repoID,
		"the foreign repo's id must not have been committed to the active scope")
	assert.NotEqual(t, outer, h.repoRoot,
		"the foreign repo's root must not have been committed to the active scope")
	// And the refusal is surfaced rather than failing silently — the buggy call
	// left the error box empty while re-scoping onto the foreign repo.
	assert.Contains(t, h.errBox.FullError(), "no longer the registered repository",
		"the drifted follow-up must surface a refusal naming the remedy")
	assert.Contains(t, h.errBox.FullError(), "af projects rebind",
		"the refusal must name the repair path the other selection paths show")
}

// TestRebindFollowFollowsHealthyTargetAfterGuardArming is the control for the
// refusal above: the same enclosing-repo fixture, but the nested checkout stays
// intact, so the probe vouches for the registered identity and the follow-up
// STILL switches to it. The guard refuses divergence only — a healthy rebind is
// followed as before — so arming the guards on the follow path does not
// over-refuse a legitimate move.
func TestRebindFollowFollowsHealthyTargetAfterGuardArming(t *testing.T) {
	h, _ := activeRebindHome(t)
	require.Empty(t, h.repoRoot, "fixture: the active scope starts unset")

	_, nested, nestedID, _ := initNestedInsideOuter(t)
	// No drift: the seam commits `nested` and leaves its .git in place, so the
	// follow-up probe resolves the registered checkout itself.
	stubRebindThrough(t, nil)

	h.Update(submitPickerRebind(t, h, nested)())

	assert.Equal(t, nested, h.repoRoot,
		"a healthy target is still followed to its checkout")
	assert.Equal(t, nestedID, h.repoID,
		"a healthy target's recorded identity is adopted, not re-derived from the path")
	assert.NotContains(t, h.errBox.FullError(), "no longer the registered repository",
		"a healthy follow must not surface the drift refusal")
}

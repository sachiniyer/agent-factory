package config

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for RebindProjectIfRoot's expected-root precondition (#4822): a rebind
// made against an observed root applies only while the registry still records
// that root.

func registerForRebindCAS(t *testing.T) (base string, project Project) {
	t.Helper()
	base = t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	project, err := RegisterProject(initProjectRegistryRepo(t, filepath.Join(base, "start")))
	require.NoError(t, err)
	return base, project
}

func recordedRoot(t *testing.T, id string) string {
	t.Helper()
	projects, err := ListProjects()
	require.NoError(t, err)
	for _, p := range projects {
		if p.ID == id {
			return p.Root
		}
	}
	t.Fatalf("project %s is not registered", id)
	return ""
}

func TestRebindIfRootConcurrentRebindsFromOneRootExactlyOneWins(t *testing.T) {
	base, project := registerForRebindCAS(t)
	targets := []string{
		initProjectRegistryRepo(t, filepath.Join(base, "left")),
		initProjectRegistryRepo(t, filepath.Join(base, "right")),
	}

	var wg sync.WaitGroup
	errs := make([]error, len(targets))
	start := make(chan struct{})
	for i, target := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = RebindProjectIfRoot(project.ID, project.Root, target)
		}()
	}
	close(start)
	wg.Wait()

	var winners []int
	var refusal *ProjectReboundError
	for i, err := range errs {
		if err == nil {
			winners = append(winners, i)
			continue
		}
		require.True(t, errors.As(err, &refusal), "the loser must get the rebound refusal, got %v", err)
	}
	require.Len(t, winners, 1, "exactly one of two rebinds from the same observed root may apply")
	winnerRoot := canonicalExistingPath(t, targets[winners[0]])
	assert.Equal(t, winnerRoot, recordedRoot(t, project.ID), "the registry holds the winner's root")
	assert.Equal(t, project.Root, refusal.Expected)
	assert.Equal(t, winnerRoot, refusal.Current, "the refusal names the root the registry holds now")
	assert.Contains(t, refusal.Error(), "refresh and retry")
}

func TestRebindIfRootRefusesAStaleExpectedRoot(t *testing.T) {
	base, project := registerForRebindCAS(t)
	moved := initProjectRegistryRepo(t, filepath.Join(base, "moved"))
	_, err := RebindProject(project.ID, moved) // another client, no precondition
	require.NoError(t, err)

	stale := initProjectRegistryRepo(t, filepath.Join(base, "stale-choice"))
	_, err = RebindProjectIfRoot(project.ID, project.Root, stale)
	var refusal *ProjectReboundError
	require.True(t, errors.As(err, &refusal), "a client that observed the old root must be refused, got %v", err)
	assert.Equal(t, canonicalExistingPath(t, moved), refusal.Current)
	assert.Equal(t, canonicalExistingPath(t, moved), recordedRoot(t, project.ID), "a refused rebind writes nothing")

	// Re-armed against the refreshed root, the same choice applies.
	_, err = RebindProjectIfRoot(project.ID, refusal.Current, stale)
	require.NoError(t, err)
	assert.Equal(t, canonicalExistingPath(t, stale), recordedRoot(t, project.ID))
}

func TestRebindIfRootOmittedExpectedRootKeepsLastWriterWins(t *testing.T) {
	base, project := registerForRebindCAS(t)
	first := initProjectRegistryRepo(t, filepath.Join(base, "first"))
	second := initProjectRegistryRepo(t, filepath.Join(base, "second"))
	_, err := RebindProjectIfRoot(project.ID, "", first)
	require.NoError(t, err)
	_, err = RebindProjectIfRoot(project.ID, "", second)
	require.NoError(t, err, "a caller that sends no expected root is never refused on it")
	assert.Equal(t, canonicalExistingPath(t, second), recordedRoot(t, project.ID))
}

func TestRebindIfRootReplayOfALandedRebindIsNotAConflict(t *testing.T) {
	base, project := registerForRebindCAS(t)
	target := initProjectRegistryRepo(t, filepath.Join(base, "target"))
	_, err := RebindProjectIfRoot(project.ID, project.Root, target)
	require.NoError(t, err)
	// The same request again (a retried or replayed send): the registry already
	// says what it asks for, so it is not someone else's rebind.
	_, err = RebindProjectIfRoot(project.ID, project.Root, target)
	require.NoError(t, err)
	assert.Equal(t, canonicalExistingPath(t, target), recordedRoot(t, project.ID))
}

// TestRebindIfRootExpectedRootComparesSpellingsNotAliases is a #4888-review
// follow-up to #4822: the expected-root precondition compares the recorded
// root's SPELLING, not its filesystem identity. Once the record moved
// /old -> /new and /old later became a symlink (or bind mount) to /new — a
// common leftover of moving a checkout — an os.SameFile compare would satisfy
// the stale precondition and let a second rebind land.
func TestRebindIfRootExpectedRootComparesSpellingsNotAliases(t *testing.T) {
	base, project := registerForRebindCAS(t)
	moved := initProjectRegistryRepo(t, filepath.Join(base, "moved"))
	_, err := RebindProjectIfRoot(project.ID, project.Root, moved)
	require.NoError(t, err)
	current := recordedRoot(t, project.ID)

	// The old checkout path now aliases the new root: the moved checkout's
	// directory is gone and a symlink keeps old references working.
	require.NoError(t, os.RemoveAll(project.Root))
	require.NoError(t, os.Symlink(current, project.Root))

	stale := initProjectRegistryRepo(t, filepath.Join(base, "stale-choice"))
	_, err = RebindProjectIfRoot(project.ID, project.Root, stale)
	var refusal *ProjectReboundError
	require.True(t, errors.As(err, &refusal),
		"an expected root that merely ALIASES the recorded root must be refused, got %v", err)
	assert.Equal(t, current, refusal.Current)
	assert.Equal(t, current, recordedRoot(t, project.ID), "a refused rebind writes nothing")
}

// TestRebindIfRootReplayRequiresTheSameCheckout is the other #4888-review
// follow-up: after a rebind /old -> /new lands, the checkout at /new is
// REPLACED — a reclone to the same path, which mints a new checkout marker —
// and the original request is replayed. The recorded root still matches the
// resolved binding's root, but the precondition observed a checkout that no
// longer exists, so the replay must be refused rather than silently
// retargeting the record to the replacement checkout.
func TestRebindIfRootReplayRequiresTheSameCheckout(t *testing.T) {
	base, project := registerForRebindCAS(t)
	moved := initProjectRegistryRepo(t, filepath.Join(base, "moved"))
	landed, err := RebindProjectIfRoot(project.ID, project.Root, moved)
	require.NoError(t, err)
	current := recordedRoot(t, project.ID)
	require.NotEmpty(t, landed.CheckoutID, "precondition: the landed rebind recorded a checkout id")

	// Replace the checkout at the recorded root: wipe it and reclone at the
	// same path, so the directory carries a different checkout marker.
	require.NoError(t, os.RemoveAll(current))
	initProjectRegistryRepo(t, current)
	replacementID, err := ensureCheckoutID(projectCheckoutMarkerPath(t, current))
	require.NoError(t, err)
	require.NotEqual(t, landed.CheckoutID, replacementID, "precondition: the replacement is a different checkout")

	// The replay names the same root the landed rebind targeted, but the
	// checkout behind it is new — the precondition must still refuse.
	_, err = RebindProjectIfRoot(project.ID, project.Root, current)
	var refusal *ProjectReboundError
	require.True(t, errors.As(err, &refusal),
		"a replay against a REPLACED checkout must be refused, got %v", err)

	after, err := ListProjects()
	require.NoError(t, err)
	for _, p := range after {
		if p.ID == project.ID {
			assert.Equal(t, landed.CheckoutID, p.CheckoutID, "a refused replay must not retarget the record's checkout identity")
		}
	}
}

// TestRebindIfRootReplayKeepsTheVerifiedMarker is the check/use half of the
// #4888-review replay finding: the marker the replay check verifies and the
// marker ensureCheckoutID re-reads are TWO reads, and a checkout swap between
// them mints the REPLACEMENT's id — which the commit-time verification then
// compares to itself and accepts. The commit must keep the verified marker so
// the swap fails the recheck instead of adopting the new identity.
func TestRebindIfRootReplayKeepsTheVerifiedMarker(t *testing.T) {
	base, project := registerForRebindCAS(t)
	moved := initProjectRegistryRepo(t, filepath.Join(base, "moved"))
	landed, err := RebindProjectIfRoot(project.ID, project.Root, moved)
	require.NoError(t, err)
	current := recordedRoot(t, project.ID)

	// On the replay, once its marker read accepts the recorded checkout, the
	// checkout at the recorded root is replaced — a reclone to the same path
	// mints a new marker before the commit reaches ensureCheckoutID.
	swapped := false
	projectRegistryCommitRaceHookForTest = func() {
		if swapped {
			return
		}
		swapped = true
		marker := projectCheckoutMarkerPath(t, current)
		require.NoError(t, os.Remove(marker))
		replacementID, err := ensureCheckoutID(marker)
		require.NoError(t, err)
		require.NotEqual(t, landed.CheckoutID, replacementID, "the replacement mints a different marker")
	}
	t.Cleanup(func() { projectRegistryCommitRaceHookForTest = nil })

	_, err = RebindProjectIfRoot(project.ID, project.Root, current)
	require.True(t, swapped, "the swap must run inside the replay's check/use window")
	require.Error(t, err,
		"a replay whose checkout was swapped between the marker reads must be refused")
	assert.Contains(t, err.Error(), "changed while")

	after, err := ListProjects()
	require.NoError(t, err)
	for _, p := range after {
		if p.ID == project.ID {
			assert.Equal(t, landed.CheckoutID, p.CheckoutID,
				"the refused replay must not adopt the replacement's identity")
		}
	}
}

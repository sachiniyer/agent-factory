package config

import (
	"errors"
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

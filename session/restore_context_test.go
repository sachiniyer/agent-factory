package session

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// #6446: restoring N sessions of one project resolved that project ~4N times at
// launch: Instance.Start asks for the repo identity (git rev-parse) and the
// effective config (file reads) from Provision, refreshSessionEnvironment and the
// launch-program resolution. These pin the per-session call count: a batch's
// RestoreContext answers each project once, however many sessions restore.

type resolutionCounts struct{ repo, cfg atomic.Int32 }

func countingRestoreContext(c *resolutionCounts) *RestoreContext {
	rc := NewRestoreContext()
	rc.repoFromPath = func(path string) (*config.RepoContext, error) {
		c.repo.Add(1)
		return &config.RepoContext{Root: path, IdentityRoot: path, ID: "id-" + path}, nil
	}
	rc.resolveConfig = func(*config.RepoContext) (*config.ResolvedConfig, error) {
		c.cfg.Add(1)
		return &config.ResolvedConfig{}, nil
	}
	return rc
}

func TestRestoreContextResolvesEachProjectOnceAcrossConcurrentRestores(t *testing.T) {
	var c resolutionCounts
	rc := countingRestoreContext(&c)

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		path := "/projects/alpha"
		if i%4 == 0 {
			path = "/projects/beta"
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			repo, err := rc.RepoFromPath(path)
			require.NoError(t, err)
			_, err = rc.ResolveConfigForRepo(repo)
			require.NoError(t, err)
		}()
	}
	wg.Wait()

	require.EqualValues(t, 2, c.repo.Load(), "one git resolution per distinct project path, not one per session")
	require.EqualValues(t, 2, c.cfg.Load(), "one config resolution per distinct project, not one per session")
}

func TestRestoreContextRetriesAFailedResolutionInsteadOfRememberingIt(t *testing.T) {
	var calls atomic.Int32
	rc := NewRestoreContext()
	rc.repoFromPath = func(path string) (*config.RepoContext, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("git unavailable")
		}
		return &config.RepoContext{Root: path}, nil
	}
	_, err := rc.RepoFromPath("/p")
	require.Error(t, err)
	repo, err := rc.RepoFromPath("/p")
	require.NoError(t, err, "a failure must not be cached for the rest of the batch")
	require.Equal(t, "/p", repo.Root)
}

func TestNilRestoreContextResolvesDirectly(t *testing.T) {
	var rc *RestoreContext
	_, err := rc.RepoFromPath(t.TempDir()) // not a git repository: the real resolver's answer
	require.Error(t, err)
	_, err = rc.ResolveConfigForRepo(nil)
	require.Error(t, err, "config.ResolveConfigForRepo refuses a nil repo")
}

// The Start-path helpers must route through the instance's context: 20 restores of
// one project call each helper twice and still resolve the project once.
func TestStartPathHelpersShareTheBatchResolution(t *testing.T) {
	var c resolutionCounts
	rc := countingRestoreContext(&c)

	for i := 0; i < 20; i++ {
		inst := &Instance{Path: "/projects/alpha"}
		inst.restoreCtx.Store(rc)
		require.NotNil(t, resolveConfigForInstance(inst))
		require.NotNil(t, resolveResolvedConfigForInstance(inst))
		repo, err := inst.restoreContext().RepoFromPath(inst.Path) // Provision's lookup
		require.NoError(t, err)
		require.Equal(t, "/projects/alpha", repo.IdentityPath())
	}

	require.EqualValues(t, 1, c.repo.Load(), "per-session git rev-parse resolutions at launch must be gone")
	require.EqualValues(t, 1, c.cfg.Load(), "per-session config resolutions at launch must be gone")
}

package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/stretchr/testify/require"
)

func TestBoundedRepoInspectionResolveBoundsFileLoads(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, exec.Command("git", "init", repoPath).Run())
	repo, err := RepoFromPath(repoPath)
	require.NoError(t, err)
	dir, legacyPath, err := repoConfigPath(repo.ID)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, exec.Command("mkfifo", legacyPath).Run())

	release := make(chan struct{})
	writerReady := make(chan error, 1)
	writerDone := make(chan error, 1)
	go func() {
		f, openErr := os.OpenFile(legacyPath, os.O_RDWR, 0)
		writerReady <- openErr
		if openErr != nil {
			writerDone <- openErr
			return
		}
		_, writeErr := f.WriteString("{}")
		<-release
		closeErr := f.Close()
		if writeErr != nil {
			writerDone <- writeErr
			return
		}
		writerDone <- closeErr
	}()
	require.NoError(t, <-writerReady)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = resolveConfigForRepoInspectionWithGlobalAndPersonalContext(ctx, repo, DefaultConfig(), nil)
	elapsed := time.Since(started)
	close(release)
	require.NoError(t, <-writerDone)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, elapsed, 8*time.Second, "a stalled config read must return on the inspection deadline rather than wait for the filesystem")
}

func TestResolveConfigForRepoInspectionRequiresCompletePersonalLookup(t *testing.T) {
	_, repoRoot, project := registeredTestProject(t)
	_, err := SetProjectConfigValue(project.ID, "program_overrides.codex", "/personal/codex")
	require.NoError(t, err)
	repo, err := RepoFromPath(repoRoot)
	require.NoError(t, err)
	global := DefaultConfig()
	global.ProgramOverrides = map[string]string{"codex": "/global/codex"}
	resolved, err := ResolveConfigForRepoInspectionWithGlobal(repo, global)
	require.NoError(t, err)
	require.Equal(t, "/personal/codex", ResolveProgram(&resolved.Config, "codex"))

	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	shimDir := t.TempDir()
	shim := filepath.Join(shimDir, "git")
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$3\" = rev-parse ] && [ \"$4\" = --git-common-dir ] && [ \"$#\" -eq 4 ]; then exit 7; fi\nexec %q \"$@\"\n", realGit)
	require.NoError(t, os.WriteFile(shim, []byte(script), 0o755))
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err = ResolveConfigForRepoInspectionWithGlobal(repo, global)
	require.Error(t, err, "command inspection must not cache a lower-precedence command when personal lookup fails")
	require.Contains(t, err.Error(), "exit status 7")
}

func TestRootAgentInspectionSnapshotKeepsProfileAndOverridesOnOneGeneration(t *testing.T) {
	_, repoRoot, project := registeredTestProject(t)
	_, err := SetProjectConfigValue(project.ID, "root_agent", `{"enabled":true,"program":"codex"}`)
	require.NoError(t, err)
	_, err = SetProjectConfigValue(project.ID, "program_overrides.codex", "/old/codex")
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	snapshot, err := ResolveRootAgentInspectionSnapshotWithConfigContext(ctx, DefaultConfig(), repoRoot, false)
	require.NoError(t, err)

	// Replace both fields after the root profile was resolved. The related
	// command resolution must keep using the personal document captured above.
	_, err = SetProjectConfigValue(project.ID, "root_agent", `{"enabled":true,"program":"claude"}`)
	require.NoError(t, err)
	_, err = SetProjectConfigValue(project.ID, "program_overrides.codex", "/new/codex")
	require.NoError(t, err)

	repo, err := RepoFromPath(repoRoot)
	require.NoError(t, err)
	resolved, err := snapshot.ResolveConfigForRepoContext(ctx, repo)
	require.NoError(t, err)
	profile, ok := snapshot.ResolvedRootAgent().Value.(RootAgent)
	require.True(t, ok)
	require.Equal(t, "codex", profile.Program)
	require.Equal(t, "/old/codex", ResolveProgram(&resolved.Config, profile.Program))
}

func TestRootAgentInspectionSnapshotRejectsSamePathCheckoutReplacement(t *testing.T) {
	_, repoRoot, project := registeredTestProject(t)
	_, err := SetProjectConfigValue(project.ID, "root_agent", `{"enabled":true,"program":"codex"}`)
	require.NoError(t, err)
	_, err = SetProjectConfigValue(project.ID, "program_overrides.codex", "/original/codex")
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	snapshot, err := ResolveRootAgentInspectionSnapshotWithConfigContext(ctx, DefaultConfig(), repoRoot, false)
	require.NoError(t, err)

	originalPath := repoRoot + ".original"
	require.NoError(t, os.Rename(repoRoot, originalPath))
	require.NoError(t, exec.Command("git", "init", repoRoot).Run())
	require.NoError(t, os.MkdirAll(filepath.Join(repoRoot, InRepoConfigDirName), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, InRepoConfigDirName, TomlConfigFileName),
		[]byte("[program_overrides]\ncodex = '/replacement/codex'\n"), 0o600))

	replacement, err := RepoFromPath(repoRoot)
	require.NoError(t, err)
	resolved, err := snapshot.ResolveConfigForRepoContext(ctx, replacement)
	require.ErrorIs(t, err, ErrRootAgentInspectionIdentityChanged,
		"a path-derived repository ID cannot prove that a checkout at the same location is unchanged")
	require.Nil(t, resolved, "documents from different checkout generations must not be combined")
}

func TestRootAgentInspectionSnapshotRejectsCheckoutReplacementDuringCommandRead(t *testing.T) {
	_, repoRoot, project := registeredTestProject(t)
	_, err := SetProjectConfigValue(project.ID, "root_agent", `{"enabled":true,"program":"codex"}`)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	snapshot, err := ResolveRootAgentInspectionSnapshotWithConfigContext(ctx, DefaultConfig(), repoRoot, false)
	require.NoError(t, err)
	repo, err := RepoFromPath(repoRoot)
	require.NoError(t, err)
	commandReadEntered := make(chan struct{})
	resumeCommandRead := make(chan struct{})
	snapshot.beforeCommandReadForTest = func() {
		close(commandReadEntered)
		<-resumeCommandRead
	}
	resolved := make(chan error, 1)
	go func() {
		_, resolveErr := snapshot.ResolveConfigForRepoContext(ctx, repo)
		resolved <- resolveErr
	}()
	<-commandReadEntered

	originalPath := repoRoot + ".original"
	require.NoError(t, os.Rename(repoRoot, originalPath))
	require.NoError(t, exec.Command("git", "init", repoRoot).Run())
	require.NoError(t, os.MkdirAll(filepath.Join(repoRoot, InRepoConfigDirName), 0o755))
	// Relative spelling: in-repo program_overrides may not name absolute
	// executables (#5265 review), and the replacement must load cleanly so the
	// identity-change check is what the read actually reaches.
	require.NoError(t, os.WriteFile(InRepoTomlConfigPath(repoRoot),
		[]byte("[program_overrides]\ncodex = 'replacement/codex'\n"), 0o600))
	close(resumeCommandRead)

	err = <-resolved
	require.ErrorIs(t, err, ErrRootAgentInspectionIdentityChanged,
		"checkout identity must remain stable across the command-layer read")
}

// TestResolveConfigForRepoContextBoundsFileLoads is the runtime-resolve half of
// the bound above: the TUI's off-loop default_program refresh answers through
// this entry point, so its TOML reads must honor the same deadline the repo
// probe does rather than leaking a goroutine on every retry (#4889 review).
func TestResolveConfigForRepoContextBoundsFileLoads(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, exec.Command("git", "init", repoPath).Run())
	repo, err := RepoFromPath(repoPath)
	require.NoError(t, err)
	dir, legacyPath, err := repoConfigPath(repo.ID)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, exec.Command("mkfifo", legacyPath).Run())

	release := make(chan struct{})
	writerReady := make(chan error, 1)
	writerDone := make(chan error, 1)
	go func() {
		f, openErr := os.OpenFile(legacyPath, os.O_RDWR, 0)
		writerReady <- openErr
		if openErr != nil {
			writerDone <- openErr
			return
		}
		_, writeErr := f.WriteString("{}")
		<-release
		closeErr := f.Close()
		if writeErr != nil {
			writerDone <- writeErr
			return
		}
		writerDone <- closeErr
	}()
	require.NoError(t, <-writerReady)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = ResolveConfigForRepoContext(ctx, repo)
	elapsed := time.Since(started)
	close(release)
	require.NoError(t, <-writerDone)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, elapsed, 8*time.Second, "a stalled config read must return on the caller's deadline rather than wait for the filesystem")
}

// TestResolveConfigForRepoContextSharesAWedgedRead is the accumulation half of
// the bound (#4889 review): a read that never returns cannot be killed
// mid-syscall, so retries must attach to the one in-flight worker rather than
// stacking another abandoned goroutine on the same files per call.
func TestResolveConfigForRepoContextSharesAWedgedRead(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, exec.Command("git", "init", repoPath).Run())
	repo, err := RepoFromPath(repoPath)
	require.NoError(t, err)

	var calls atomic.Int32
	release := make(chan struct{})
	prev := resolveRepoForBound
	resolveRepoForBound = func(*RepoContext) (*ResolvedConfig, error) {
		calls.Add(1)
		<-release
		return nil, errors.New("wedged read")
	}
	t.Cleanup(func() {
		close(release)
		resolveRepoForBound = prev
	})

	// Three calls, each bounding only its own wait while the worker stays
	// wedged. Without dedup each would stack another blocked worker.
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		_, err = ResolveConfigForRepoContext(ctx, repo)
		cancel()
		require.ErrorIs(t, err, context.DeadlineExceeded)
	}
	require.Equal(t, int32(1), calls.Load(),
		"timed-out retries must share the wedged worker, not accumulate one per call")
}

// TestResolveConfigForRepoContextReReadsAfterAJoinedFlight is the freshness
// half of the bound (#4889 review): a caller that attaches to a flight
// in-flight when it arrived must not accept that snapshot — the read can
// precede the config change this call exists to observe. The joiner waits for
// the older worker to finish, then issues a read that provably starts after
// its own call.
func TestResolveConfigForRepoContextReReadsAfterAJoinedFlight(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, exec.Command("git", "init", repoPath).Run())
	repo, err := RepoFromPath(repoPath)
	require.NoError(t, err)

	var calls atomic.Int32
	firstRunning := make(chan struct{})
	releaseFirst := make(chan struct{})
	prev := resolveRepoForBound
	resolveRepoForBound = func(*RepoContext) (*ResolvedConfig, error) {
		n := calls.Add(1)
		if n == 1 {
			close(firstRunning)
			<-releaseFirst
			return nil, errors.New("stale flight result")
		}
		return nil, errors.New("fresh flight result")
	}
	t.Cleanup(func() { resolveRepoForBound = prev })

	// Caller A opens flight one and stays in it.
	aDone := make(chan error, 1)
	go func() {
		_, aErr := ResolveConfigForRepoContext(context.Background(), repo)
		aDone <- aErr
	}()
	<-firstRunning

	// Caller B joins flight one — it started before B's call.
	bDone := make(chan error, 1)
	go func() {
		_, bErr := ResolveConfigForRepoContext(context.Background(), repo)
		bDone <- bErr
	}()

	// Releasing before B is provably parked on flight one would let B open
	// the fresh flight itself and never exercise the join path.
	key := repo.IdentityPath() + "\x00" + repo.WorkspacePath()
	require.Eventually(t, func() bool {
		repoResolveFlightsMu.Lock()
		f, ok := repoResolveFlights[key]
		repoResolveFlightsMu.Unlock()
		return ok && f.waiters.Load() == 2
	}, 5*time.Second, time.Millisecond, "the joiner must attach to the in-flight flight")

	// Flight one completes; the pileup stays at one worker while B re-reads.
	close(releaseFirst)
	require.EqualError(t, <-bDone, "fresh flight result",
		"a joined flight's snapshot must not satisfy a later caller")
	require.EqualError(t, <-aDone, "stale flight result",
		"the flight's own caller takes its result — it is the live read")
	require.Equal(t, int32(2), calls.Load(),
		"B's re-read is one new worker after the joined flight finished")
}

package config

// The #4539 interim-state contract: a project-scoped branch_prefix is accepted
// and stored but never applied, so the places that would otherwise look silent
// — the write, the load, the displayed effective value — all say so. Each of
// these fails on a master where the personal layer still wins resolution
// quietly.

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	aflog "github.com/sachiniyer/agent-factory/log"
)

func TestSetProjectConfigValueBranchPrefixWarnsButWrites(t *testing.T) {
	home, _, project := registeredTestProject(t)
	writeGlobalTOML(t, home, "branch_prefix = \"global/\"\n")

	res, err := SetProjectConfigValue(project.ID, "branch_prefix", "feat/")
	require.NoError(t, err, "the warning must never turn a successful write into an error")
	require.NotNil(t, res)
	require.Contains(t, res.Warnings,
		"branch_prefix is not supported per project yet; the global branch_prefix (global/) applies to all projects. See #4539.")
	assert.False(t, res.RequiresRestart,
		"no restart can apply a stored-but-ignored value — the write must not promise one")

	cfg, err := LoadProjectConfig(project.ID)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, "feat/", cfg.BranchPrefix, "the stored value must survive — existing configs stay valid")
}

// TestResolveConfigBranchPrefixWarnsOncePerFile pins the memoization the
// daemon's reload cadence needs: every consumer's load funnels through
// resolution, so repeated resolves of the same file warn once, and a second
// project's file warns independently.
func TestResolveConfigBranchPrefixWarnsOncePerFile(t *testing.T) {
	home, repoRoot, project := registeredTestProject(t)
	writeGlobalTOML(t, home, "branch_prefix = \"global/\"\n")
	buf := captureLog(t, &aflog.WarningLog)

	first := writePersonalConfig(t, project.ID, "branch_prefix = \"feat/\"\n")
	for i := 0; i < 3; i++ {
		_, err := ResolveConfig(repoRoot)
		require.NoError(t, err)
	}
	got := buf.String()
	assert.Equal(t, 1, strings.Count(got, "branch_prefix is not supported per project yet"),
		"repeated loads of one file must warn once (daemon reloads often), got:\n%s", got)
	assert.Contains(t, got, prettyHomePath(first), "the warning must name the file it came from")
	assert.Contains(t, got, "the global branch_prefix (global/) applies to all projects")

	repo2 := initProjectRegistryRepo(t, t.TempDir())
	project2, err := RegisterProject(repo2)
	require.NoError(t, err)
	writePersonalConfig(t, project2.ID, "branch_prefix = \"other/\"\n")
	_, err = ResolveConfig(repo2)
	require.NoError(t, err)

	got = buf.String()
	assert.Equal(t, 2, strings.Count(got, "branch_prefix is not supported per project yet"),
		"a different personal config file must warn for itself, got:\n%s", got)
}

// TestLoadProjectConfigNoBranchPrefixStaysQuiet guards the negative: a personal
// file without branch_prefix produces no warning on resolve.
func TestLoadProjectConfigNoBranchPrefixStaysQuiet(t *testing.T) {
	_, repoRoot, project := registeredTestProject(t)
	buf := captureLog(t, &aflog.WarningLog)

	writePersonalConfig(t, project.ID, "default_program = \"claude\"\n")
	_, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	assert.NotContains(t, buf.String(), "branch_prefix")
}

// TestLoadProjectConfigBranchPrefixLoadsGlobalLazily pins #5026: the
// once-per-file memo must answer BEFORE the global prefix resolves, so a
// repeat load re-reads nothing and a race of first loads resolves the value
// exactly once. An eager resolver at the call site fails both counts — every
// repeat and every racer pays a file read plus TOML parse.
func TestLoadProjectConfigBranchPrefixLoadsGlobalLazily(t *testing.T) {
	home, _, project := registeredTestProject(t)
	writeGlobalTOML(t, home, "branch_prefix = \"global/\"\n")
	writePersonalConfig(t, project.ID, "branch_prefix = \"feat/\"\n")
	buf := captureLog(t, &aflog.WarningLog)

	var loads atomic.Int32
	prev := globalBranchPrefixForLoadWarning
	globalBranchPrefixForLoadWarning = func() string {
		loads.Add(1)
		return prev()
	}
	t.Cleanup(func() { globalBranchPrefixForLoadWarning = prev })

	// A race of first loads shares one memo entry: exactly one LoadOrStore
	// wins, and only the winner pays for the read-only global load.
	const racers = 8
	var wg sync.WaitGroup
	errs := make(chan error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := LoadProjectConfig(project.ID)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, int32(1), loads.Load(),
		"concurrent first loads must resolve the global prefix exactly once")
	assert.Equal(t, 1, strings.Count(buf.String(), "branch_prefix is not supported per project yet"),
		"the warning itself still emits exactly once, got:\n%s", buf.String())

	// Once the file has warned, repeat loads consult the memo and return
	// before the resolver runs — zero further global loads.
	for i := 0; i < 5; i++ {
		_, err := LoadProjectConfig(project.ID)
		require.NoError(t, err)
	}
	assert.Equal(t, int32(1), loads.Load(),
		"repeat loads of an already-warned file must perform zero global loads")
}

// TestInspectionResolveBranchPrefixDoesNotLoadGlobal pins the Codex-found
// defect: the warning names the effective prefix the resolution ALREADY
// computed, so a read-only inspection resolve must not consult the global
// file at all — prove it by removing the global config before resolving.
func TestInspectionResolveBranchPrefixDoesNotLoadGlobal(t *testing.T) {
	home, repoRoot, project := registeredTestProject(t)
	writeGlobalTOML(t, home, "branch_prefix = \"file/\"\n")
	writePersonalConfig(t, project.ID, "branch_prefix = \"feat/\"\n")

	global := DefaultConfig()
	global.BranchPrefix = "snapshot/"
	repo, err := RepoFromPath(repoRoot)
	require.NoError(t, err)

	require.NoError(t, os.Remove(filepath.Join(home, TomlConfigFileName)))
	resolved, err := ResolveConfigForRepoInspectionWithGlobal(repo, global)
	require.NoError(t, err)
	assert.Equal(t, "snapshot/", resolved.BranchPrefix,
		"the supplied snapshot's prefix wins, not the on-disk file's and not a reload")

	value, ok := resolved.ResolvedValue("branch_prefix")
	require.True(t, ok)
	var personal *CandidateTrace
	for i, c := range value.Candidates {
		if c.Layer == SourceProjectPersonal.String() {
			personal = &value.Candidates[i]
		}
	}
	require.NotNil(t, personal)
	assert.Contains(t, personal.Reason, "(snapshot/)",
		"the ignored reason names the supplied global snapshot's value, not a fresh file read")
	_, err = os.Stat(filepath.Join(home, TomlConfigFileName))
	require.True(t, os.IsNotExist(err),
		"a read-only inspection must not materialize the global config file")
}

package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	aflog "github.com/sachiniyer/agent-factory/log"
)

// TestRetainedLegacyBareRepoConfig_StatErrorDoesNotSuppressAppliedWarning
// reproduces the dedup-key collision fixed in warnRetainedLegacyBareRepoConfig:
// a non-ENOENT stat failure on the retained parent-keyed legacy config must not
// reserve the actionable "was not applied" warning's dedup slot, so that once the
// operator repairs the file without restarting the daemon a fresh resolve emits
// the actionable migration hint.
//
// The retained parent-keyed config is never adopted under either branch, so the
// effective runtime config is unaffected across all phases (#3361, resolve.go
// "warnRetainedLegacyBareRepoConfig").
func TestRetainedLegacyBareRepoConfig_StatErrorDoesNotSuppressAppliedWarning(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	parent := testguard.CanonicalTempDir(t)
	source := filepath.Join(parent, "source")
	bare := filepath.Join(parent, "bare.git")
	worktree := filepath.Join(parent, "worktree")
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v failed: %s", args, out)
	}
	run(parent, "init", source)
	run(source, "config", "user.email", "test@test.com")
	run(source, "config", "user.name", "Test")
	run(source, "commit", "--allow-empty", "-m", "init")
	run(parent, "clone", "--bare", source, bare)
	run(bare, "worktree", "add", worktree)

	repo, err := RepoFromPath(worktree)
	require.NoError(t, err)
	_, legacyID := repo.LegacyBareRepoIdentity()
	require.NotEmpty(t, legacyID, "bare-clone worktree must derive a parent-keyed legacy identity")
	legacyCfg := &RepoConfig{
		PostWorktreeCommands: []string{"ambiguous-parent-command"},
	}
	writeLegacyRepoConfig(t, legacyID, legacyCfg)
	_, legacyPath, err := repoConfigPath(legacyID)
	require.NoError(t, err)
	legacyDir := filepath.Dir(legacyPath)

	// captureLog resets the once-per-process dedup map (via
	// resetRetainedLegacyBareRepoConfigWarnings) exactly once, then redirects the
	// warning logger to a buffer. The dedup map must persist across phases below
	// to mirror an in-process repair without a daemon restart — the scenario the
	// shared key could suppress — so captureLog is called a single time and the
	// buffer's Reset is used to isolate per-phase assertions.
	warnings := captureLog(t, &aflog.WarningLog)

	// clobberLegacyDir replaces the per-repo state directory for legacyID with a
	// regular file so os.Stat(<legacyDir>/config.json) returns ENOTDIR — a
	// non-IsNotExist error reachable on any UID (Go stdlib os.Stat behaviour), so
	// the test does not depend on chmod/root or an exotic filesystem.
	clobberLegacyDir := func() {
		require.NoError(t, os.RemoveAll(legacyDir))
		require.NoError(t, os.WriteFile(legacyDir, []byte("clobber-not-a-dir"), 0644))
	}
	// restoreLegacyDir recreates the real directory with a genuine legacy config,
	// simulating an operator repairing the directory structure without a daemon
	// restart.
	restoreLegacyDir := func() {
		require.NoError(t, os.RemoveAll(legacyDir))
		writeLegacyRepoConfig(t, legacyID, legacyCfg)
	}

	// Phase 1: stat-error branch fires the non-actionable "could not be
	// inspected" warning and reserves its (now distinct) dedup key.
	clobberLegacyDir()
	resolved, err := ResolveConfigForRepo(repo)
	require.NoError(t, err)
	assert.Empty(t, resolved.PostWorktreeCommands,
		"a retained parent-keyed config must never be adopted even when unreadable")
	assert.Contains(t, warnings.String(), "could not be inspected",
		"a non-ENOENT stat error must emit the inspect-it warning")
	assert.NotContains(t, warnings.String(), "was not applied",
		"the actionable warning must not fire while the legacy file is unreadable")
	warnings.Reset()

	// Phase 2: operator repairs the directory without a daemon restart. The
	// actionable "was not applied" warning must fire despite phase 1 having
	// already warned for the same repo and legacy identity.
	restoreLegacyDir()
	resolved, err = ResolveConfigForRepo(repo)
	require.NoError(t, err)
	assert.Empty(t, resolved.PostWorktreeCommands,
		"a retained parent-keyed config must never be adopted into the corrected identity")
	assert.Contains(t, warnings.String(), "was not applied",
		"after the stat error is repaired, the actionable migration warning must fire once — "+
			"the stat-error branch's dedup key must not reserve the stat-success slot")
	assert.Contains(t, warnings.String(), InRepoTomlConfigPath(repo.WorkspacePath()),
		"the actionable warning must name the concrete migration target path")
	assert.NotContains(t, warnings.String(), "could not be inspected",
		"the now-readable file must not still trip the inspect-it branch")
	warnings.Reset()

	// Phase 3: a second resolve while the file stays readable is suppressed by
	// the stat-success dedup slot (anti-spam at the ~17 call sites and the 30s
	// drift-detection cadence is preserved).
	resolved, err = ResolveConfigForRepo(repo)
	require.NoError(t, err)
	assert.Empty(t, warnings.String(),
		"the actionable warning dedups to once per corrected identity per process")

	// Phase 4: re-introduce the stat error. The inspect-it branch's own dedup key
	// is already reserved from phase 1, so this must not re-emit either — the
	// steady-state guarantee of at most one "could not be inspected" and one "was
	// not applied" per repo per process holds.
	clobberLegacyDir()
	resolved, err = ResolveConfigForRepo(repo)
	require.NoError(t, err)
	assert.Empty(t, warnings.String(),
		"the inspect-it warning dedups independently to once per corrected identity per process")
}

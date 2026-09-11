package config

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/stretchr/testify/require"
)

func TestResolveConfigForRepoInspectionWithGlobalContextBoundsFileLoads(t *testing.T) {
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
	_, err = ResolveConfigForRepoInspectionWithGlobalContext(ctx, repo, DefaultConfig())
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

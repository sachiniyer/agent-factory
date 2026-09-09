package config

import (
	"context"
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

package app

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

func TestTUIRefreshSeesCLIChangesThroughDaemon(t *testing.T) {
	skipIfRealBackendDepsMissing(t)

	bin := buildIntegrationBinary(t)
	repoDir := setupRealRepo(t)
	t.Chdir(repoDir)

	h := newTestHome(t)
	repo, err := config.CurrentRepo()
	require.NoError(t, err)
	h.repoID = repo.ID
	h.storage, err = session.NewStorage(config.DefaultState(), repo.ID)
	require.NoError(t, err)
	writeIntegrationConfig(t, os.Getenv("AGENT_FACTORY_HOME"))

	t.Cleanup(func() {
		_, _ = runIntegrationAF(t, bin, repoDir, "sessions", "kill", "cli-made")
		killIntegrationDaemon(os.Getenv("AGENT_FACTORY_HOME"))
	})

	runIntegrationAFOK(t, bin, repoDir, "sessions", "--repo", repoDir, "create", "--name", "cli-made", "--program", tmux.ProgramClaude)
	require.True(t, h.refreshExternalInstances(), "TUI refresh should import CLI-created session")
	require.NotNil(t, findSidebarInstance(h, "cli-made"))

	runIntegrationAFOK(t, bin, repoDir, "sessions", "kill", "cli-made")
	require.True(t, h.refreshExternalInstances(), "TUI refresh should remove CLI-killed session")
	require.Nil(t, findSidebarInstance(h, "cli-made"))
}

func findSidebarInstance(h *home, title string) *session.Instance {
	for _, inst := range h.sidebar.GetInstances() {
		if inst.Title == title {
			return inst
		}
	}
	return nil
}

func buildIntegrationBinary(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller failed")
	repoRoot := filepath.Dir(filepath.Dir(file))
	bin := filepath.Join(t.TempDir(), "af")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", bin, repoRoot)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "go build failed:\n%s", out)
	return bin
}

func runIntegrationAFOK(t *testing.T, bin, dir string, args ...string) string {
	t.Helper()
	out, err := runIntegrationAF(t, bin, dir, args...)
	require.NoError(t, err)
	return out
}

func runIntegrationAF(t *testing.T, bin, dir string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TERM=xterm")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return stdout.String(), fmt.Errorf("timed out running af %s; stderr=%s", strings.Join(args, " "), stderr.String())
	}
	if err != nil {
		return stdout.String(), fmt.Errorf("%w; stderr=%s stdout=%s", err, stderr.String(), stdout.String())
	}
	return stdout.String(), nil
}

func writeIntegrationConfig(t *testing.T, home string) {
	t.Helper()
	// Write a wrapper script that ignores any --plugin-dir / --dangerously-skip
	// flags injectSystemPrompt would append; route the claude enum to it via
	// ProgramOverrides so the CLI accepts --program claude (#658).
	wrapper := filepath.Join(home, "fake-agent.sh")
	require.NoError(t, os.WriteFile(wrapper, []byte("#!/bin/sh\nexec cat\n"), 0755))
	cfg := &config.Config{
		DefaultProgram:     tmux.ProgramClaude,
		ProgramOverrides:   map[string]string{tmux.ProgramClaude: wrapper},
		AutoYes:            false,
		DaemonPollInterval: 100,
		BranchPrefix:       "test/",
		DetachKeys:         "ctrl-w",
	}
	require.NoError(t, config.SaveConfig(cfg))
}

func killIntegrationDaemon(home string) {
	raw, err := os.ReadFile(filepath.Join(home, "daemon.pid"))
	if err != nil {
		return
	}
	var pid int
	if _, err := fmt.Sscanf(string(raw), "%d", &pid); err != nil || pid <= 1 {
		return
	}
	if proc, err := os.FindProcess(pid); err == nil {
		_ = proc.Kill()
	}
}

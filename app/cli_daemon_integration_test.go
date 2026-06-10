package app

import (
	"bytes"
	"context"
	"encoding/json"
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

// TestTUIRefreshSwapsKillRecreatedSameTitle is the regression test for #765.
//
// When a session is killed and recreated under the SAME title via the CLI
// WITHOUT an intervening refresh, the sidebar holds a dead in-memory instance
// while a fresh, live instance exists on disk. The old title-only
// reconciliation skipped both the add (title already present) and the remove
// (title still on disk), so the corpse permanently shadowed the new session:
// the user could neither attach nor preview it. refreshExternalInstances must
// detect the stale instance (its tmux session is gone) and swap it for the
// recreated on-disk one.
func TestTUIRefreshSwapsKillRecreatedSameTitle(t *testing.T) {
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
		_, _ = runIntegrationAF(t, bin, repoDir, "sessions", "kill", "recreated")
		killIntegrationDaemon(os.Getenv("AGENT_FACTORY_HOME"))
	})

	// Create the session and import it into the sidebar.
	runIntegrationAFOK(t, bin, repoDir, "sessions", "--repo", repoDir, "create", "--name", "recreated", "--program", tmux.ProgramClaude)
	require.True(t, h.refreshExternalInstances(), "TUI refresh should import CLI-created session")
	original := findSidebarInstance(h, "recreated")
	require.NotNil(t, original)
	require.True(t, original.TmuxAlive(), "imported instance should be alive")

	// Kill then recreate the SAME title via the CLI, with NO refresh in
	// between. The in-memory instance now points at a dead tmux session while
	// a brand-new live instance sits on disk under the same title.
	runIntegrationAFOK(t, bin, repoDir, "sessions", "kill", "recreated")
	require.False(t, original.TmuxAlive(), "killed instance's tmux session must be gone")
	runIntegrationAFOK(t, bin, repoDir, "sessions", "--repo", repoDir, "create", "--name", "recreated", "--program", tmux.ProgramClaude)

	require.True(t, h.refreshExternalInstances(), "TUI refresh should swap the stale instance")

	// Exactly one "recreated" instance, and it must be the new live one — not
	// the dead corpse we started with.
	var matches []*session.Instance
	for _, inst := range h.sidebar.GetInstances() {
		if inst.Title == "recreated" {
			matches = append(matches, inst)
		}
	}
	require.Len(t, matches, 1, "sidebar must hold exactly one instance for the reused title")
	swapped := matches[0]
	require.NotSame(t, original, swapped, "sidebar must hold the recreated instance, not the dead one")
	require.True(t, swapped.TmuxAlive(), "swapped-in instance must be attachable (live tmux session)")
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
	//
	// It prints a "❯" ready prompt before reading stdin so the daemon's
	// waitForReady loop recognises the pane as ready. The create path now
	// always waits for readiness — even for empty-prompt sessions (#698) — so
	// a real agent's startup prompt must be emulated here.
	wrapper := filepath.Join(home, "fake-agent.sh")
	require.NoError(t, os.WriteFile(wrapper, []byte("#!/bin/sh\nprintf '❯ '\nexec cat\n"), 0755))
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

// TestTUIRefreshDoesNotSwapLoadingPlaceholder is the regression test for #808.
//
// The daemon persists a new session to instances.json BEFORE the create RPC
// returns, so while the TUI's Loading placeholder is still in the sidebar,
// the on-disk record for the same title already exists with a newer
// CreatedAt. The #765 swap logic treated that newer record as a CLI
// kill+recreate and swapped the placeholder out; the instanceStartedMsg
// handler then missed it (pointer-based ReplaceInstance/ContainsInstance)
// and re-added the started instance, leaving two same-title sidebar rows
// that SaveInstances persisted as byte-identical duplicate records.
func TestTUIRefreshDoesNotSwapLoadingPlaceholder(t *testing.T) {
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
		_, _ = runIntegrationAF(t, bin, repoDir, "sessions", "kill", "scripts")
		killIntegrationDaemon(os.Getenv("AGENT_FACTORY_HOME"))
	})

	// The TUI's placeholder for an in-flight create of "scripts".
	placeholder, err := session.NewInstance(session.InstanceOptions{
		Title:   "scripts",
		Path:    repoDir,
		Program: tmux.ProgramClaude,
	})
	require.NoError(t, err)
	placeholder.SetStatus(session.Loading)
	h.sidebar.AddInstance(placeholder)

	// The daemon persists the session record mid-create — emulated by a CLI
	// create, which goes through the same daemon CreateSession path the TUI
	// start RPC uses.
	runIntegrationAFOK(t, bin, repoDir, "sessions", "--repo", repoDir, "create", "--name", "scripts", "--program", tmux.ProgramClaude)

	// A refresh tick fires while the placeholder is still Loading. It must
	// not swap the placeholder out from under the in-flight create.
	require.False(t, h.refreshExternalInstances(), "refresh must leave the Loading placeholder alone")
	require.Same(t, placeholder, findSidebarInstance(h, "scripts"),
		"the Loading placeholder must stay in the sidebar until its start completes (#808)")

	// The start RPC completes, returning a freshly-built instance — exactly
	// what startSessionThroughDaemon produces via FromInstanceData.
	diskData, err := h.storage.LoadInstanceData()
	require.NoError(t, err)
	var rec *session.InstanceData
	for i := range diskData {
		if diskData[i].Title == "scripts" {
			rec = &diskData[i]
		}
	}
	require.NotNil(t, rec, "daemon must have persisted the session before the RPC returned")
	started, err := session.FromInstanceData(*rec)
	require.NoError(t, err)

	_, _ = h.Update(instanceStartedMsg{instance: placeholder, started: started})

	var matches []*session.Instance
	for _, inst := range h.sidebar.GetInstances() {
		if inst.Title == "scripts" {
			matches = append(matches, inst)
		}
	}
	require.Len(t, matches, 1, "one logical session must occupy exactly one sidebar row (#808)")
	require.Same(t, started, matches[0])

	// The next save must persist exactly one record. Read the raw file (not
	// LoadInstanceData, which now dedupes on load) to assert the on-disk state.
	require.NoError(t, h.storage.SaveInstances(h.sidebar.GetInstances()))
	raw, err := config.DefaultState().GetInstances(repo.ID)
	require.NoError(t, err)
	var onDisk []session.InstanceData
	require.NoError(t, json.Unmarshal(raw, &onDisk))
	count := 0
	for _, d := range onDisk {
		if d.Title == "scripts" {
			count++
		}
	}
	require.Equal(t, 1, count, "instances.json must hold exactly one record for the title (#808)")
}

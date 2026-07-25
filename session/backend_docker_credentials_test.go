package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// writeCredFile creates an empty file at path (making parent dirs), standing in
// for an on-disk agent credential the mount logic stats for.
func writeCredFile(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("{}"), 0o600))
}

// argsHave reports whether any captured docker arg contains sub.
func argsHave(args []string, sub string) bool {
	for _, a := range args {
		if strings.Contains(a, sub) {
			return true
		}
	}
	return false
}

// provisionDockerCapturingRun drives dockerRuntime.Provision for a session with
// the given program against a docker=backend repo, capturing the `docker run`
// argv through the test seam (which then errors to stop provisioning).
func provisionDockerCapturingRun(t *testing.T, program string) []string {
	t.Helper()
	repoRoot := initTempGitRepo(t)
	writeInRepoConfig(t, repoRoot, map[string]any{
		"backend": "docker",
		"docker":  map[string]any{"image": "example.invalid/agent:latest"},
	})
	defer SetLookPathForTest(func(string) (string, error) { return "/usr/bin/docker", nil })()
	defer SetDockerSelfBinaryForTest(filepath.Join(t.TempDir(), "af"))()

	var runArgs []string
	defer SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "info" {
			// provision() establishes the engine id before creating the container;
			// answer it so provisioning reaches `docker run`.
			return []byte("test-engine\n"), nil
		}
		if len(args) > 0 && args[0] == "run" {
			runArgs = append([]string(nil), args...)
		}
		return nil, fmt.Errorf("stop after capturing docker run")
	})()

	_, _ = (dockerRuntime{}).Provision(ProvisionSpec{
		RepoRoot: repoRoot,
		Title:    "cred-test",
		Program:  program,
		CloneURL: "file:///fixture.git",
	})
	return runArgs
}

// TestDockerMountAgentCredentials_DefaultOff pins the safety story: with the
// operator grant unset (the default), NO credential is mounted even though the
// file is present on disk. This is the coverage the old runContainer-preset test
// lacked — it drives the real dockerRuntime.Provision gate.
func TestDockerMountAgentCredentials_DefaultOff(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCredFile(t, filepath.Join(home, ".codex/auth.json"))
	// Default global config: the grant is off.
	require.NoError(t, config.SaveConfig(config.DefaultConfig()))

	runArgs := provisionDockerCapturingRun(t, tmux.ProgramCodex)
	if argsHave(runArgs, ".codex/auth.json") {
		t.Fatalf("default-off: no credential must be mounted, but docker run had one: %v", runArgs)
	}
}

// TestDockerMountAgentCredentials_OperatorGrantIsAgentSelective is the P1
// contract in one assertion: with the OPERATOR's global grant on (a repo cannot
// set it), a codex session mounts the codex credential — and NOT the Claude one,
// even though both exist on disk. Presence proves the gate opened on the global
// grant; the Claude absence proves per-agent selection (P1-b).
func TestDockerMountAgentCredentials_OperatorGrantIsAgentSelective(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCredFile(t, filepath.Join(home, ".codex/auth.json"))
	writeCredFile(t, filepath.Join(home, ".claude/.credentials.json"))

	cfg := config.DefaultConfig()
	cfg.DockerMountAgentCredentials = true // operator opt-in, global-only
	require.NoError(t, config.SaveConfig(cfg))

	runArgs := provisionDockerCapturingRun(t, tmux.ProgramCodex)

	wantCodex := filepath.Join(home, ".codex/auth.json") + ":" + dockerContainerHome + "/.codex/auth.json:ro"
	if !argsHave(runArgs, wantCodex) {
		t.Fatalf("operator grant on, codex session: codex credential must be mounted read-only; args=%v", runArgs)
	}
	if argsHave(runArgs, ".claude/.credentials.json") {
		t.Fatalf("codex session must NOT receive the Claude credential; args=%v", runArgs)
	}
}

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
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
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

	// The MODE is asserted here, not just the paths: this is the only test that
	// reads the argv docker run actually receives, so it is what proves the
	// SELinux relabel survives argv assembly rather than merely existing in the
	// helper (#3451).
	wantCodex := filepath.Join(home, ".codex/auth.json") + ":" + dockerContainerHome + "/.codex/auth.json:" + dockerCredentialMountMode
	if !argsHave(runArgs, wantCodex) {
		t.Fatalf("operator grant on, codex session: codex credential must be mounted read-only and SELinux-relabeled (%q); args=%v", wantCodex, runArgs)
	}
	if argsHave(runArgs, ".claude/.credentials.json") {
		t.Fatalf("codex session must NOT receive the Claude credential; args=%v", runArgs)
	}
}

// TestDockerMountAgentCredentials_DevinMountsItsOwnCredential is the devin
// regression for the path that was unreachable before this fix.
//
// devin is a first-class supported agent (tmux.SupportedPrograms) and has an
// entry in agentCredentialFiles, but it was MISSING from agentNames, so
// sessionenv.AgentForCommand("devin") returned "". The docker provisioner's
// agentName() returns "" for a non-empty program whose agent is unknown (the
// empty-program fallback to claude does not cover this), so
// resolveAgentCredentialMounts("") looked up agentCredentialFiles[""] and
// mounted nothing — even with the operator grant on and the file present.
//
// This drives the real dockerRuntime.Provision path that was broken: with the
// grant on and BOTH the devin and claude credential files present, a devin
// session must mount its OWN config and not the Claude token.
func TestDockerMountAgentCredentials_DevinMountsItsOwnCredential(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCredFile(t, filepath.Join(home, ".config/devin/config.json"))
	writeCredFile(t, filepath.Join(home, ".claude/.credentials.json"))

	cfg := config.DefaultConfig()
	cfg.DockerMountAgentCredentials = true
	require.NoError(t, config.SaveConfig(cfg))

	runArgs := provisionDockerCapturingRun(t, tmux.ProgramDevin)

	wantDevin := filepath.Join(home, ".config/devin/config.json") + ":" + dockerContainerHome + "/.config/devin/config.json:" + dockerCredentialMountMode
	if !argsHave(runArgs, wantDevin) {
		t.Fatalf("operator grant on, devin session: the devin config must be mounted read-only and SELinux-relabeled (%q); args=%v", wantDevin, runArgs)
	}
	if argsHave(runArgs, ".claude/.credentials.json") {
		t.Fatalf("devin session must NOT receive the Claude credential; args=%v", runArgs)
	}
}

// TestAgentForCommandResolvesEveryCredentialFileAgent is the cross-map
// consistency guard that would have caught the devin bug.
//
// agentCredentialMounts is reached in production through
// sessionenv.AgentForCommand(p.program) -> dockerProvisioner.agentName() ->
// resolveAgentCredentialMounts(agent). Every agent that has a credential file
// entry therefore MUST also resolve via AgentForCommand, or its mount is
// unreachable forever — regardless of the file existing on disk. Iterating
// the map (not a hand-copied list) keeps this guard honest as new agents are
// added to agentCredentialFiles.
func TestAgentForCommandResolvesEveryCredentialFileAgent(t *testing.T) {
	for agent := range agentCredentialFiles {
		if got := sessionenv.AgentForCommand(agent); got != agent {
			t.Errorf("agent %q is in agentCredentialFiles but AgentForCommand(%q) = %q; "+
				"the docker provisioner cannot reach its credential mount",
				agent, agent, got)
		}
	}
}

// TestAgentForCommandResolvesEverySupportedProgram guards the other direction
// of the same drift: a new agent added to tmux.SupportedPrograms but missed by
// agentNames silently degrades to no credentials and (for non-empty programs)
// no claude fallback either. Every supported program must resolve to itself.
//
// This is the production resolution dockerProvisioner.agentName relies on; the
// empty-program fallback to claude only covers a blank program, not an unknown
// agent, so membership here is load-bearing.
func TestAgentForCommandResolvesEverySupportedProgram(t *testing.T) {
	for _, program := range tmux.SupportedPrograms {
		if got := sessionenv.AgentForCommand(program); got != program {
			t.Errorf("program %q is in tmux.SupportedPrograms but AgentForCommand(%q) = %q; "+
				"a session running it cannot resolve its agent for credential mounting or env filtering",
				program, program, got)
		}
	}
}

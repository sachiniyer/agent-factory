package session

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// credentialAgentsThatRelocate is the set of agents on the credential-mount
// feature whose config-root environment variable relocates credential lookup,
// so the run_args guard has a non-empty denied set to enforce. It mirrors
// internal/sessionenv/accountConfigVars; the cross-map test below pins that
// the two stay in step.
var credentialAgentsThatRelocate = []string{tmux.ProgramClaude, tmux.ProgramCodex, tmux.ProgramGemini}

// credentialAgentsThatDoNotRelocate is the set of agents on the credential-mount
// feature whose config-root variable was MEASURED not to relocate the credential
// file (internal/sessionenv/account.go records the reason each is out), so the
// guard is a no-op for them.
var credentialAgentsThatDoNotRelocate = []string{tmux.ProgramAmp, tmux.ProgramOpencode, tmux.ProgramDevin}

// TestDockerCredentialRunArgs_RefusesConfigRootRedirectPerAgent is the core
// exploit from the bug report: a repo sets the agent's credential-root
// environment variable via checked-in docker.run_args, which would redirect
// credential lookup away from af's mounted file and start the session
// unauthenticated. The lexical guard must refuse it for each agent that
// relocates credentials.
func TestDockerCredentialRunArgs_RefusesConfigRootRedirectPerAgent(t *testing.T) {
	configRoot := map[string]string{
		tmux.ProgramCodex:  "CODEX_HOME",
		tmux.ProgramClaude: "CLAUDE_CONFIG_DIR",
		tmux.ProgramGemini: "GEMINI_CLI_HOME",
	}
	for _, agent := range credentialAgentsThatRelocate {
		name := configRoot[agent]
		for _, tt := range []struct {
			label string
			args  []string
		}{
			{label: "separate -e", args: []string{"-e", name + "=/nonexistent"}},
			{label: "inline -e", args: []string{"-e" + name + "=/nonexistent"}},
			{label: "long --env", args: []string{"--env", name + "=/nonexistent"}},
			{label: "inline --env=", args: []string{"--env=" + name + "=/nonexistent"}},
			{label: "inline -e=", args: []string{"-e=" + name + "=/nonexistent"}},
			{label: "name reads host value (no =)", args: []string{"-e", name}},
		} {
			t.Run(agent+"/"+tt.label, func(t *testing.T) {
				err := validateCredentialDockerRunArgs(tt.args, agent)
				require.Errorf(t, err, "repo run_args redirecting %s must be refused: %v", name, tt.args)
				require.Contains(t, err.Error(), "docker.run_args")
				require.Contains(t, err.Error(), name, "the refusal must name the variable the operator has to remove")
				require.Contains(t, err.Error(), "redirect credential lookup")
			})
		}
	}
}

// TestDockerCredentialRunArgs_RefusesEveryDeniedNamePerAgent pins that the
// credential guard refuses EVERY name in its denied set (accountDockerDeniedNames
// = AccountIdentityNames + AgentAuthSelectors), so a name added to either half
// of the identity classification later cannot slip through unguarded.
func TestDockerCredentialRunArgs_RefusesEveryDeniedNamePerAgent(t *testing.T) {
	for _, agent := range credentialAgentsThatRelocate {
		denied := accountDockerDeniedNames(agent)
		require.NotEmpty(t, denied, "agent %q must have a non-empty denied set to guard", agent)
		for name := range denied {
			err := validateCredentialDockerRunArgs([]string{"-e", name + "=repo"}, agent)
			require.Errorf(t, err, "agent %q: denied name %q was accepted", agent, name)
			require.Contains(t, err.Error(), name)
		}
	}
}

// TestDockerCredentialRunArgs_DeniedSetEqualsAccountGuard is the cross-map pin:
// the credential guard guards exactly the names the account guard does, for
// every account agent, so the two run_args guards cannot drift on what counts
// as an identity name.
func TestDockerCredentialRunArgs_DeniedSetEqualsAccountGuard(t *testing.T) {
	for _, agent := range credentialAgentsThatRelocate {
		denied := accountDockerDeniedNames(agent)
		for name := range denied {
			require.Errorf(t, validateCredentialDockerRunArgs([]string{"-e", name + "=repo"}, agent),
				"agent %q: account guard and credential guard disagree on %q", agent, name)
		}
		for _, harmless := range []string{"TZ", "FOO_BAR", "XDG_CONFIG_HOME", "LANG", "DOCKER_HOST"} {
			if _, refused := denied[harmless]; refused {
				continue
			}
			require.NoErrorf(t, validateCredentialDockerRunArgs([]string{"-e", harmless + "=x"}, agent),
				"agent %q: harmless name %q was refused (over-refusal)", agent, harmless)
		}
	}
}

// TestDockerCredentialRunArgs_RefusesEnvFile mirrors the account guard: an
// env file is opaque to af, so none is accepted when there is a credential to
// protect. The single-dash spelling is refused the same way the account guard
// refuses it (the strings look like env-file to an operator).
func TestDockerCredentialRunArgs_RefusesEnvFile(t *testing.T) {
	for _, agent := range credentialAgentsThatRelocate {
		for _, args := range [][]string{
			{"--env-file", "repo.env"},
			{"-env-file", "repo.env"},
			{"--env-file=repo.env"},
		} {
			err := validateCredentialDockerRunArgs(args, agent)
			require.Errorf(t, err, "agent %q: env-file passed: %v", agent, args)
			require.Contains(t, err.Error(), "env-file")
			require.Contains(t, err.Error(), "credential lookup")
		}
	}
}

// TestDockerCredentialRunArgs_RefusesIdentityInCombinedShortOptions covers the
// recognition half the account guard already pins: `-e` behind a boolean option
// in a combined short option sets an environment variable exactly as `-e` on its
// own does, so the credential guard must catch it there too.
func TestDockerCredentialRunArgs_RefusesIdentityInCombinedShortOptions(t *testing.T) {
	for _, tt := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "interactive then env", args: []string{"-ie", "CODEX_HOME=/nonexistent"}, wantErr: "CODEX_HOME"},
		{name: "interactive tty then env", args: []string{"-ite", "CODEX_HOME=/nonexistent"}, wantErr: "CODEX_HOME"},
		{name: "tty then env", args: []string{"-te", "CODEX_HOME=/nonexistent"}, wantErr: "CODEX_HOME"},
		{name: "cluster with an inline value", args: []string{"-teCODEX_HOME=/nonexistent"}, wantErr: "CODEX_HOME"},
		{name: "cluster with an equals value", args: []string{"-te=CODEX_HOME=/nonexistent"}, wantErr: "CODEX_HOME"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := validateCredentialDockerRunArgs(tt.args, tmux.ProgramCodex)
			require.Errorf(t, err, "a combined short option set the credential root: %v", tt.args)
			require.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// TestDockerCredentialRunArgs_RefusesUnreadableCombinedShortOptions is the
// fail-closed rule, scoped to the credential path's one guarded letter. When a
// cluster holds a character af cannot classify and an `e` may hide behind it,
// the guard refuses and names the argument. A `v` behind an unknown character
// is NOT refused here, unlike the account guard, because a volume cannot
// replace af's single-file credential mount.
func TestDockerCredentialRunArgs_RefusesUnreadableCombinedShortOptions(t *testing.T) {
	t.Run("unknown before env is refused", func(t *testing.T) {
		err := validateCredentialDockerRunArgs([]string{"-Xe", "CODEX_HOME=/nonexistent"}, tmux.ProgramCodex)
		require.Error(t, err)
		require.Contains(t, err.Error(), "combined short option")
		require.Contains(t, err.Error(), "-Xe", "the refusal must name the argument the operator has to fix")
	})
	t.Run("unknown after a boolean before env is refused", func(t *testing.T) {
		err := validateCredentialDockerRunArgs([]string{"-itYe", "CODEX_HOME=/nonexistent"}, tmux.ProgramCodex)
		require.Error(t, err)
		require.Contains(t, err.Error(), "combined short option")
	})
	t.Run("unknown before volume is allowed", func(t *testing.T) {
		// A -v behind an unknown option cannot shadow af's single-file
		// credential mount, so the credential guard does not refuse it.
		require.NoError(t, validateCredentialDockerRunArgs([]string{"-Xv", "/tmp/cache:/workspace"}, tmux.ProgramCodex))
	})
}

// TestDockerCredentialRunArgs_AllowsHarmlessCombinedShortOptions is the other
// direction: failing closed must not become refusing everything. A volume in
// a cluster is harmless on the credential path, a harmless env name is allowed,
// and value-taking options consume the rest of the cluster.
func TestDockerCredentialRunArgs_AllowsHarmlessCombinedShortOptions(t *testing.T) {
	for _, args := range [][]string{
		{"-itd"},
		{"-itv", "/tmp/cache:/workspace"},
		{"-itv/tmp/cache:/workspace"},
		{"-tv", "/tmp/cache:/root/.codex"},
		{"-ite", "TZ=UTC"},
		{"-ue", "CODEX_HOME=repo"}, // -u takes a value, so the e is part of it
		{"-p8080:80"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			require.NoErrorf(t, validateCredentialDockerRunArgs(args, tmux.ProgramCodex),
				"a harmless combined short option was refused: %v", args)
		})
	}
}

// TestDockerCredentialRunArgs_ClusterScanDoesNotSkipALaterEnv pins that the
// combined short-option walk does not consume the next argument the way Docker
// would, so a real -e written after the cluster is still examined on its own —
// the same trap the account guard's cluster scan had to avoid.
func TestDockerCredentialRunArgs_ClusterScanDoesNotSkipALaterEnv(t *testing.T) {
	for _, args := range [][]string{
		{"-ie", "TZ=UTC", "-e", "CODEX_HOME=/nonexistent"},
		{"-tv", "/tmp/cache:/workspace", "-e", "OPENAI_API_KEY=repo"},
		{"-it", "-e", "CODEX_API_KEY=repo"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			err := validateCredentialDockerRunArgs(args, tmux.ProgramCodex)
			require.Errorf(t, err, "an identity env after a combined short option escaped validation: %v", args)
		})
	}
}

// TestDockerCredentialRunArgs_AllowsEveryMountInstallingOption pins that the
// credential guard passes mount/device/volumes-from options through, because
// none can replace af's single-file credential mount (Docker refuses a second
// -v at the same destination; a parent dir does not shadow the file child; a
// --device is a node not a credential reader). The account guard refuses these
// for its nested-path reason, which does not apply here.
func TestDockerCredentialRunArgs_AllowsEveryMountInstallingOption(t *testing.T) {
	for _, args := range [][]string{
		{"-v", "/tmp/other:/root/.codex/other"},
		{"--volume", "/tmp/other:/workspace"},
		{"--volume=/tmp/other:/root/somewhere"},
		{"--mount", "type=bind,src=/tmp/other,dst=/root/.codex/other"},
		{"--tmpfs", "/tmp"},
		{"--tmpfs=/tmp:size=64m"},
		{"--device", "/dev/zero:/dev/zero"},
		{"--device=/dev/fuse"},
		{"--volumes-from", "repo-donor"},
		{"--volumes-from=repo-donor"},
		{"-v", "/dev/null:/root/.codex/auth.json:ro"}, // Docker refuses a duplicate single-file mount
		{"-v", "/tmp/shadow/.codex:/root/.codex:ro"},  // parent dir does not shadow the file child
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			require.NoErrorf(t, validateCredentialDockerRunArgs(args, tmux.ProgramCodex),
				"a mount/device option was refused on the credential path (over-refusal): %v", args)
		})
	}
}

// TestDockerCredentialRunArgs_AllowsHarmlessEnvNames is the over-refusal boundary
// for environment entries: a name that is not in the agent's identity set does
// nothing to the credential mount, so the guard must leave it through.
// Note: XDG_CONFIG_HOME and XDG_DATA_HOME are not in the codex denied set (codex
// does not use XDG paths for auth), so the validator passes them for codex. The
// XDG redirect risk for amp and opencode is closed by runContainer re-asserting
// XDG_CONFIG_HOME and XDG_DATA_HOME after run_args when credential mounts are
// installed — not by the validator refusing them.
func TestDockerCredentialRunArgs_AllowsHarmlessEnvNames(t *testing.T) {
	for _, name := range []string{"TZ", "LANG", "FOO", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "CODEX_SQLITE_HOME"} {
		require.NoErrorf(t, validateCredentialDockerRunArgs([]string{"-e", name + "=value"}, tmux.ProgramCodex),
			"harmless env name %q was refused (over-refusal)", name)
	}
}

// TestDockerCredentialRunArgs_NoopForAgentsThatDoNotRelocateCredentials pins the
// scoping the bug report specifies: amp, opencode and devin have a credential
// file on the feature (agentCredentialFiles) but their config-root environment
// variable was measured NOT to relocate credential lookup, so the guard is a
// no-op for them — accepting even their own home/var entries and an env file,
// because no environment entry can defeat their mount.
func TestDockerCredentialRunArgs_NoopForAgentsThatDoNotRelocateCredentials(t *testing.T) {
	knownEnvForAgent := map[string][]string{
		tmux.ProgramAmp:      {"AMP_HOME=/x", "AMP_SETTINGS_FILE=/y"},
		tmux.ProgramOpencode: {"OPENCODE_CONFIG=/x", "OPENCODE_CONFIG_DIR=/y"},
	}
	for _, agent := range credentialAgentsThatDoNotRelocate {
		require.Empty(t, accountDockerDeniedNames(agent), "agent %q should have an empty denied set", agent)
		require.NoError(t, validateCredentialDockerRunArgs(nil, agent), "nil args must be a no-op")
		for _, entry := range knownEnvForAgent[agent] {
			require.NoErrorf(t, validateCredentialDockerRunArgs([]string{"-e", entry}, agent),
				"agent %q: an env entry that does not relocate credentials was refused (over-refusal): %s", agent, entry)
		}
		require.NoErrorf(t, validateCredentialDockerRunArgs([]string{"--env-file", "repo.env"}, agent),
			"agent %q: env-file was refused for an agent with no relocating env var (over-refusal)", agent)
	}
}

// TestDockerCredentialRunArgs_GuardCoversExactlyTheRelocatingAgents is the
// roster pin: an agent is guarded iff it is in accountConfigVars. If a future
// agent joins accountConfigVars, the guard must start refusing for it; if one
// is removed, the guard becomes a no-op. Iterating accountConfigVars keeps the
// two from disagreeing.
func TestDockerCredentialRunArgs_GuardCoversExactlyTheRelocatingAgents(t *testing.T) {
	relocating, ok := configAgentsInAccountConfigVars()
	require.True(t, ok, "could not enumerate the relocating agents")
	for agent := range relocating {
		err := validateCredentialDockerRunArgs([]string{"-e", "__PROBE__=x"}, agent)
		// __PROBE__ is not a denied name, so this only confirms the guard runs
		// (the denied set is non-empty); it must not refuse a harmless probe.
		require.NoErrorf(t, err, "agent %q: harmless probe was refused (over-refusal)", agent)
		require.NotEmpty(t, accountDockerDeniedNames(agent), "agent %q is relocating but has an empty denied set", agent)
	}
}

// configAgentsInAccountConfigVars reads, for each account agent, whether it is
// in the config-root map, via the public SupportsAccounts surface so the test
// reads the SAME source the guard does.
func configAgentsInAccountConfigVars() (map[string]struct{}, bool) {
	out := make(map[string]struct{})
	for _, agent := range sessionenv.AccountAgents() {
		if _, ok := sessionenv.SupportsAccounts(agent); ok {
			out[agent] = struct{}{}
		}
	}
	return out, true
}

// provisionDockerCredentialGrant drives dockerRuntime.Provision for a
// NON-ACCOUNT credential-mount session (the grant on) with the given repo
// run_args, reporting whether `docker run` was reached, the provisioning error
// (if any), and the captured `docker run` argv. The validator refuses before
// any docker call, so a refused session reports runCalled=false.
func provisionDockerCredentialGrant(t *testing.T, program string, runArgs []string) (runCalled bool, err error, runArgv []string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	// A local engine so the pre-run locality guard passes without a docker
	// call; the harmless-run_args path reaches `docker run`, which a remote or
	// unresolved engine never does. Mirrors provisionDockerCapturingRun.
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	t.Setenv("DOCKER_CONTEXT", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Write the first credential file for the given agent so that
	// resolveAgentCredentialMounts finds it and installs a mount. Tests that
	// check XDG re-assertion need an actual mount to be present (the re-assertion
	// is gated on len(credentialMounts) > 0).
	if files, ok := agentCredentialFiles[program]; ok && len(files) > 0 {
		writeCredFile(t, filepath.Join(home, files[0]))
	}
	// Force the SELinux probe to a fixture so the argv is deterministic on any
	// host; the validator runs before the mount is resolved, so the mode does
	// not change whether the session is refused.
	_ = forceCredentialMountMode(t)
	cfg := config.DefaultConfig()
	cfg.DockerMountAgentCredentials = true
	require.NoError(t, config.SaveConfig(cfg))

	repoRoot := initTempGitRepo(t)
	dockerCfg := map[string]any{"image": "example.invalid/agent:latest"}
	if runArgs != nil {
		dockerCfg["run_args"] = runArgs
	}
	writeInRepoConfig(t, repoRoot, map[string]any{"backend": "docker", "docker": dockerCfg})

	t.Cleanup(SetLookPathForTest(func(string) (string, error) { return "/usr/bin/docker", nil }))
	t.Cleanup(SetDockerSelfBinaryForTest(filepath.Join(t.TempDir(), "af")))
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "info" {
			return []byte("credential-test-engine\n"), nil
		}
		if len(args) > 0 && args[0] == "run" {
			runCalled = true
			runArgv = append([]string(nil), args...)
		}
		return nil, fmt.Errorf("stop after docker run")
	}))

	_, err = (dockerRuntime{}).Provision(ProvisionSpec{
		RepoRoot: repoRoot,
		Title:    "credential-run-args",
		Program:  program,
		CloneURL: "file:///fixture.git",
	})
	return runCalled, err, runArgv
}

// TestDockerMountAgentCredentials_RefusesCredentialRedirectRunArgs is the
// end-to-end fix for the bug report: with the OPERATOR grant on and a trusted
// image, a repo's checked-in run_args that sets the agent's credential-root env
// var is refused BEFORE docker run, so the mount is never installed on top of a
// defeat. Before the fix the mount was installed and the session started
// unauthenticated while af's log reported it as mounted.
func TestDockerMountAgentCredentials_RefusesCredentialRedirectRunArgs(t *testing.T) {
	for _, tt := range []struct {
		agent   string
		rootVar string
	}{
		{tmux.ProgramCodex, "CODEX_HOME"},
		{tmux.ProgramClaude, "CLAUDE_CONFIG_DIR"},
		{tmux.ProgramGemini, "GEMINI_CLI_HOME"},
	} {
		t.Run(tt.agent, func(t *testing.T) {
			runCalled, err, _ := provisionDockerCredentialGrant(t, tt.agent,
				[]string{"-e", tt.rootVar + "=/nonexistent"})
			require.Error(t, err)
			require.Contains(t, err.Error(), "docker.run_args")
			require.Contains(t, err.Error(), tt.rootVar, "the refusal must name the env var a repo used to redirect the credential")
			require.False(t, runCalled, "a credential-redirecting run_arg must refuse before docker run")
		})
	}
}

// TestDockerMountAgentCredentials_RefusesEnvFileRunArgs is the end-to-end mirror
// of the env-file refusal: a repo cannot smuggle a credential redirect past the
// guard via an opaque env file.
func TestDockerMountAgentCredentials_RefusesEnvFileRunArgs(t *testing.T) {
	runCalled, err, _ := provisionDockerCredentialGrant(t, tmux.ProgramCodex,
		[]string{"--env-file", "repo.env"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "env-file")
	require.False(t, runCalled, "an env-file run_arg must refuse before docker run")
}

// TestDockerMountAgentCredentials_AllowsHarmlessRunArgs is the no-regression
// guarantee: a repo's harmless run_args (an unrelated env, a volume) must not be
// refused on the credential path, the credential mount must still be installed,
// and docker run must still be reached. The fake errors after capturing the
// `docker run` argv (the same shape provisionDockerCapturingRun uses), so the
// error here is the fake's stop, NOT a run_args refusal.
func TestDockerMountAgentCredentials_AllowsHarmlessRunArgs(t *testing.T) {
	for _, args := range [][]string{
		{"-e", "TZ=UTC"},
		{"-e", "XDG_CONFIG_HOME=/root/.config"},
		{"-v", "/tmp/extra:/workspace/extra"},
		{"--mount", "type=bind,src=/tmp/other,dst=/root/.cache"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			runCalled, err, runArgv := provisionDockerCredentialGrant(t, tmux.ProgramCodex, args)
			require.True(t, runCalled, "harmless run_args must still reach docker run: %v", args)
			if err != nil {
				require.NotContainsf(t, err.Error(), "docker.run_args",
					"harmless run_args must not be refused: %v", args)
				require.NotContainsf(t, err.Error(), "redirect credential lookup",
					"harmless run_args must not be refused: %v", args)
			}
			require.Truef(t, argsHave(runArgv, ".codex/auth.json"),
				"the codex credential mount must still be installed alongside harmless run_args: %v", runArgv)
		})
	}
}

// TestDockerMountAgentCredentials_HarmlessRunArgsStayAfterTheMount pins the run
// argv ordering fix's other half: harmless repo run_args are still appended
// after the credential mount, preserving the documented run_args contract while
// the new guard strips the credential-defeating entries.
func TestDockerMountAgentCredentials_HarmlessRunArgsStayAfterTheMount(t *testing.T) {
	runCalled, err, runArgv := provisionDockerCredentialGrant(t, tmux.ProgramCodex,
		[]string{"-e", "TZ=UTC"})
	require.True(t, runCalled, "harmless run_args must reach docker run")
	if err != nil {
		require.NotContains(t, err.Error(), "redirect credential lookup")
	}
	mountIdx, envIdx := indexIn(runArgv, ".codex/auth.json"), indexIn(runArgv, "TZ=UTC")
	require.Greaterf(t, mountIdx, 0, "the credential mount was not in the run argv: %v", runArgv)
	require.Greaterf(t, envIdx, 0, "the harmless env was not in the run argv: %v", runArgv)
	require.Greaterf(t, envIdx, mountIdx, "harmless run_args must remain appended after the credential mount: %v", runArgv)
}

// TestDockerMountAgentCredentials_XDGReassertedAfterRunArgs pins that when
// credential mounts are installed, runContainer emits XDG_DATA_HOME and
// XDG_CONFIG_HOME AFTER run_args so that repo-supplied values cannot redirect
// agents that use those XDG paths for credential lookup:
//   - opencode reads $XDG_DATA_HOME/opencode/auth.json when XDG_DATA_HOME is set.
//   - amp reads $XDG_CONFIG_HOME/amp/settings.json when XDG_CONFIG_HOME is set.
//
// Docker gives the LAST -e for a name precedence, so the re-assertion after
// run_args dominates any repo-supplied redirect, closing the same gap that the
// HOME re-assertion closed for HOME-relative credential lookup.
func TestDockerMountAgentCredentials_XDGReassertedAfterRunArgs(t *testing.T) {
	for _, tt := range []struct {
		agent   string
		xdgVar  string
		xdgPath string // suffix that should appear in the re-asserted value
	}{
		{tmux.ProgramOpencode, "XDG_DATA_HOME", dockerContainerHome + "/.local/share"},
		{tmux.ProgramAmp, "XDG_CONFIG_HOME", dockerContainerHome + "/.config"},
	} {
		t.Run(tt.agent, func(t *testing.T) {
			// Pass a run_arg that tries to redirect the XDG path. The guard lets
			// it through (XDG vars are not in the denied set for these agents),
			// but runContainer must re-assert the correct value after run_args.
			runCalled, _, runArgv := provisionDockerCredentialGrant(t, tt.agent,
				[]string{"-e", tt.xdgVar + "=/tmp/attacker"})
			require.True(t, runCalled, "harmless (to validator) run_args must still reach docker run: agent=%s", tt.agent)
			// Find the last occurrence of the XDG variable in the argv: Docker
			// gives the LAST -e precedence, so the re-assertion must come after
			// the repo-supplied value.
			lastReassertIdx := -1
			repoIdx := -1
			for i, a := range runArgv {
				if strings.Contains(a, tt.xdgVar+"="+tt.xdgPath) {
					lastReassertIdx = i
				}
				if strings.Contains(a, tt.xdgVar+"=/tmp/attacker") {
					repoIdx = i
				}
			}
			require.Greaterf(t, lastReassertIdx, 0,
				"agent %s: XDG re-assertion %s=%s was not in the run argv: %v",
				tt.agent, tt.xdgVar, tt.xdgPath, runArgv)
			require.Greaterf(t, repoIdx, 0,
				"agent %s: the repo-supplied %s redirect was not in the run argv: %v",
				tt.agent, tt.xdgVar, runArgv)
			require.Greaterf(t, lastReassertIdx, repoIdx,
				"agent %s: the af XDG re-assertion must come AFTER the repo-supplied redirect so Docker's last-wins rule makes it dominate: %v",
				tt.agent, runArgv)
		})
	}
}

// indexIn returns the index of the first arg containing sub, or -1.
func indexIn(args []string, sub string) int {
	for i, a := range args {
		if strings.Contains(a, sub) {
			return i
		}
	}
	return -1
}

// TestDockerMountAgentCredentials_DefaultOffIgnoresRunArgs ensures the guard is
// scoped to the credential path: with the grant OFF (the default), repo run_args
// that WOULD redirect the credential root are not validated (there is no mount to
// defeat), and provisioning proceeds to docker run. This guards against an
// over-broad fix that runs the validator for non-credential sessions too.
func TestDockerMountAgentCredentials_DefaultOffIgnoresRunArgs(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	t.Setenv("DOCKER_CONTEXT", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, config.SaveConfig(config.DefaultConfig())) // grant off

	repoRoot := initTempGitRepo(t)
	writeInRepoConfig(t, repoRoot, map[string]any{
		"backend": "docker",
		"docker":  map[string]any{"image": "example.invalid/agent:latest", "run_args": []string{"-e", "CODEX_HOME=/nonexistent"}},
	})
	t.Cleanup(SetLookPathForTest(func(string) (string, error) { return "/usr/bin/docker", nil }))
	t.Cleanup(SetDockerSelfBinaryForTest(filepath.Join(t.TempDir(), "af")))
	runCalled := false
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "info" {
			return []byte("default-off-engine\n"), nil
		}
		if len(args) > 0 && args[0] == "run" {
			runCalled = true
		}
		return nil, fmt.Errorf("stop after docker run")
	}))

	_, err := (dockerRuntime{}).Provision(ProvisionSpec{
		RepoRoot: repoRoot,
		Title:    "default-off",
		Program:  tmux.ProgramCodex,
		CloneURL: "file:///fixture.git",
	})
	// The grant is off, so the credential guard does not run; provisioning
	// reaches docker run (and then stops on the fake's error). No refusal for
	// the env redirect, because there is no credential mount to defeat.
	require.True(t, runCalled, "default-off must not validate run_args against the credential guard")
	// err is the fake's "stop after docker run" error, not a run_args refusal.
	if err != nil {
		require.NotContains(t, err.Error(), "redirect credential lookup")
	}
}

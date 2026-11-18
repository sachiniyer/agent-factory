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
// so the run_args guard has a non-empty account-scope denied set to enforce.
// It mirrors internal/sessionenv/accountConfigVars; the cross-map test below
// pins that the two stay in step. For these agents credentialMountDeniedNames
// returns accountDockerDeniedNames(agent) verbatim, so the credential-mount
// guard and the account run_args guard cannot drift on what counts as an
// identity name.
var credentialAgentsThatRelocate = []string{tmux.ProgramClaude, tmux.ProgramCodex, tmux.ProgramGemini}

// credentialAgentsThatSubstituteEnv is the set of agents on the credential-mount
// feature whose account-scope identity classification is empty — #4454 made the
// run_args validator a no-op for them on the belief that no env entry could
// defeat their mount — but whose per-agent allowlists
// (internal/sessionenv/sessionenv.go's agentNames) include a credential-bearing
// name that survives every af env filter and reaches the agent process, where
// it substitutes the credential af mounted read-only (opencode: OPENCODE_CONFIG*
// + provider API keys; amp: AMP_API_KEY). credentialMountDeniedNames returns
// credentialMountDeniedEnvNames[agent] for them; the validator now refuses the
// repo-controlled run_args entry instead of accepting it.
var credentialAgentsThatSubstituteEnv = []string{tmux.ProgramAmp, tmux.ProgramOpencode}

// credentialAgentsWithoutCredentialEnvName is the set of agents on the
// credential-mount feature whose per-agent allowlist is EMPTY, so NO env entry
// can reach the agent process to substitute its config-file credential. The
// validator remains a no-op for them, including --env-file (which could only
// carry names the agent never reads).
var credentialAgentsWithoutCredentialEnvName = []string{tmux.ProgramDevin}

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

// TestDockerCredentialRunArgs_NoopForAgentsWithoutCredentialEnvName pins the
// one place the run_args guard stays a no-op: an agent whose per-agent allowlist
// is EMPTY — devin — so no env entry can reach the agent process to substitute
// its config-file credential, and an env-file can only carry names the agent
// never reads. #4454 rolled opencode and amp into this no-op on a false premise;
// this test now pins devin ALONE.
func TestDockerCredentialRunArgs_NoopForAgentsWithoutCredentialEnvName(t *testing.T) {
	for _, agent := range credentialAgentsWithoutCredentialEnvName {
		require.Empty(t, accountDockerDeniedNames(agent),
			"agent %q should still have an empty account-scope denied set", agent)
		require.Empty(t, credentialMountDeniedNames(agent),
			"agent %q is on the no-op path; its credential-mount denied set must be empty", agent)
		require.NoError(t, validateCredentialDockerRunArgs(nil, agent), "nil args must be a no-op")
		// Any env name a repo might pass for this agent is not a built-in
		// per-agent allowlisted name, so it cannot survive the env filter and
		// reach the agent process. The validator passes it for the same
		// reason it passes a malformed name: refusing it would not close a
		// credential-substitution route while it would regress legitimate
		// use (e.g. an operator naming TZ here).
		require.NoErrorf(t, validateCredentialDockerRunArgs([]string{"-e", "DEVIN_API_KEY=repo"}, agent),
			"agent %q: an env name its CLI never reads was refused (over-refusal)", agent)
		require.NoErrorf(t, validateCredentialDockerRunArgs([]string{"--env-file", "repo.env"}, agent),
			"agent %q: env-file was refused for an agent with no credential-bearing env name (over-refusal)", agent)
	}
}

// TestDockerCredentialRunArgs_RefusesCredentialSubstitutionForEmptyAccountSetAgents
// is the core exploit from this bug report: a repo sets opencode's built-in
// per-agent env name OPENCODE_CONFIG_CONTENT (which survives every af env
// filter to the opencode process) via checked-in docker.run_args, overriding
// both the provider endpoint and the API key the opencode CLI uses and
// beating the mounted auth.json — OR sets amp's AMP_API_KEY via the same route.
// The validator must now refuse these names for the empty-account-set agents,
// in every arg-shaped form Docker accepts, the same way it refuses codex's
// credential-root variable for codex.
func TestDockerCredentialRunArgs_RefusesCredentialSubstitutionForEmptyAccountSetAgents(t *testing.T) {
	credentialNames := map[string][]string{
		tmux.ProgramOpencode: {
			"OPENCODE_CONFIG", "OPENCODE_CONFIG_DIR", "OPENCODE_CONFIG_CONTENT",
			"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY",
			"AZURE_OPENAI_API_KEY", "OPENROUTER_API_KEY", "DEEPSEEK_API_KEY", "GROQ_API_KEY",
			"MISTRAL_API_KEY", "COHERE_API_KEY", "XAI_API_KEY",
		},
		tmux.ProgramAmp: {"AMP_API_KEY"},
	}
	for _, agent := range credentialAgentsThatSubstituteEnv {
		require.Empty(t, accountDockerDeniedNames(agent),
			"agent %q's account-scope classification must still be empty (this guard is on the credential-mount path, not the account path)", agent)
		require.NotEmpty(t, credentialMountDeniedNames(agent),
			"agent %q: the credential-mount denied set must be non-empty after the fix", agent)
		for _, name := range credentialNames[agent] {
			require.Containsf(t, credentialMountDeniedNames(agent), name,
				"agent %q: %s must be in the credential-mount denied set", agent, name)
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
				t.Run(agent+"/"+tt.label+"/"+name, func(t *testing.T) {
					err := validateCredentialDockerRunArgs(tt.args, agent)
					require.Error(t, err, "agent %q: repo run_args setting %s must be refused: %v", agent, name, tt.args)
					require.Contains(t, err.Error(), "docker.run_args")
					require.Contains(t, err.Error(), name, "the refusal must name the variable the operator has to remove")
					require.Contains(t, err.Error(), "redirect credential lookup")
				})
			}
		}
	}
}

// TestDockerCredentialRunArgs_RefusesEnvFileForEmptyAccountSetAgents mirrors
// TestDockerCredentialRunArgs_RefusesEnvFile for the agents #4454 used to let
// an env file through. An env file is opaque to af, so on the credential-mount
// path where the guard now has a non-empty denied set for amp/opencode it is
// refused — none of the credential-bearing names above can be smuggled in
// through it, while the mounted credential remains the session's identity.
func TestDockerCredentialRunArgs_RefusesEnvFileForEmptyAccountSetAgents(t *testing.T) {
	for _, agent := range credentialAgentsThatSubstituteEnv {
		for _, args := range [][]string{
			{"--env-file", "repo.env"},
			{"-env-file", "repo.env"},
			{"--env-file=repo.env"},
		} {
			t.Run(agent+"/"+strings.Join(args, "_"), func(t *testing.T) {
				err := validateCredentialDockerRunArgs(args, agent)
				require.Error(t, err, "agent %q: env-file passed: %v", agent, args)
				require.Contains(t, err.Error(), "env-file")
				require.Contains(t, err.Error(), "credential lookup")
			})
		}
	}
}

// TestDockerCredentialRunArgs_AllowsDeploymentConfigForEmptyAccountSetAgents is
// the over-refusal boundary for the same agents. The credential-mount guard
// refuses the names that SUBSTITUTE the credential af mounted; it does not
// refuse names that are the operator's deployment configuration rather than an
// identity — the same distinction the account path's
// accountNonCredentialNames makes for ANTHROPIC_BASE_URL against claude.
//   - amp's AMP_HOME is the install dir, not a credential root; AMP_SETTINGS_FILE
//     is not even in amp's per-agent allowlist.
//   - opencode's OPENAI_API_BASE / OPENAI_BASE_URL are provider endpoints;
//     AZURE_OPENAI_ENDPOINT / AZURE_OPENAI_API_VERSION pin a different Azure
//     deployment; XDG_DATA_HOME is the routing variable whose redirect
//     runContainer re-asserts after run_args.
//
// All of these are LEFT THROUGH so a legitimate proxied / private-cloud setup
// the operator wanted via repo run_args still works.
func TestDockerCredentialRunArgs_AllowsDeploymentConfigForEmptyAccountSetAgents(t *testing.T) {
	deploymentEnvForAgent := map[string][]string{
		tmux.ProgramAmp: {
			"AMP_HOME=/opt/amp", "AMP_SETTINGS_FILE=/opt/amp/settings.json",
			"TZ=UTC", "XDG_CONFIG_HOME=/root/.config",
		},
		tmux.ProgramOpencode: {
			"OPENAI_API_BASE=https://proxy.example/v1", "OPENAI_BASE_URL=https://proxy.example/v1",
			"AZURE_OPENAI_ENDPOINT=https://my-deployment.example/v1", "AZURE_OPENAI_API_VERSION=2024-10-01-preview",
			"TZ=UTC", "XDG_DATA_HOME=/root/.local/share", "LANG=C",
		},
	}
	for _, agent := range credentialAgentsThatSubstituteEnv {
		for _, entry := range deploymentEnvForAgent[agent] {
			t.Run(agent+"/"+entry, func(t *testing.T) {
				require.NoErrorf(t, validateCredentialDockerRunArgs([]string{"-e", entry}, agent),
					"agent %q: a deployment-config env entry that does not substitute the credential was refused (over-refusal): %s", agent, entry)
			})
		}
	}
}

// TestDockerCredentialRunArgs_RefusesCredentialSubstitutionInCombinedShortOptions
// mirrors TestDockerCredentialRunArgs_RefusesIdentityInCombinedShortOptions
// for the empty-account-set agents: `-e` behind a boolean option in a combined
// short option sets an environment variable exactly as `-e` on its own does,
// so the guard must catch the credential-bearing name there too.
func TestDockerCredentialRunArgs_RefusesCredentialSubstitutionInCombinedShortOptions(t *testing.T) {
	cases := []struct {
		name    string
		agent   string
		args    []string
		wantErr string
	}{
		{name: "opencode interactive then env", agent: tmux.ProgramOpencode,
			args: []string{"-ie", "OPENCODE_CONFIG_CONTENT=repo"}, wantErr: "OPENCODE_CONFIG_CONTENT"},
		{name: "opencode interactive tty then env", agent: tmux.ProgramOpencode,
			args: []string{"-ite", "OPENCODE_CONFIG_CONTENT=repo"}, wantErr: "OPENCODE_CONFIG_CONTENT"},
		{name: "opencode cluster with an inline value", agent: tmux.ProgramOpencode,
			args: []string{"-teOPENCODE_CONFIG=repo"}, wantErr: "OPENCODE_CONFIG"},
		{name: "opencode cluster with an equals value", agent: tmux.ProgramOpencode,
			args: []string{"-te=OPENCODE_CONFIG_DIR=repo"}, wantErr: "OPENCODE_CONFIG_DIR"},
		{name: "amp interactive then env", agent: tmux.ProgramAmp,
			args: []string{"-ie", "AMP_API_KEY=repo"}, wantErr: "AMP_API_KEY"},
		{name: "amp cluster with an inline value", agent: tmux.ProgramAmp,
			args: []string{"-teAMP_API_KEY=repo"}, wantErr: "AMP_API_KEY"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			err := validateCredentialDockerRunArgs(tt.args, tt.agent)
			require.Errorf(t, err, "a combined short option set a credential-bearing name: %v", tt.args)
			require.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// TestDockerCredentialRunArgs_AllowsHarmlessCombinedShortOptionsForEmptyAccountSetAgents
// is the other direction: failing closed must not become refusing everything
// for the empty-account-set agents. A volume in a cluster is harmless on the
// credential path, a harmless env name is allowed, and value-taking options
// consume the rest of the cluster (the same boundary the codex guard pins).
func TestDockerCredentialRunArgs_AllowsHarmlessCombinedShortOptionsForEmptyAccountSetAgents(t *testing.T) {
	for _, tt := range []struct {
		agent string
		args  []string
	}{
		{tmux.ProgramOpencode, []string{"-itd"}},
		{tmux.ProgramOpencode, []string{"-itv", "/tmp/cache:/root/.local/share/opencode"}},
		{tmux.ProgramOpencode, []string{"-ite", "TZ=UTC"}},
		{tmux.ProgramOpencode, []string{"-ite", "OPENAI_BASE_URL=https://proxy.example/v1"}},
		{tmux.ProgramOpencode, []string{"-ue", "AMP_API_KEY=repo"}}, // -u consumes its value, so this is not -e
		{tmux.ProgramAmp, []string{"-itd"}},
		{tmux.ProgramAmp, []string{"-itv", "/tmp/cache:/root/.config/amp"}},
		{tmux.ProgramAmp, []string{"-ite", "TZ=UTC"}},
		{tmux.ProgramAmp, []string{"-p8080:80"}},
	} {
		t.Run(tt.agent+"/"+strings.Join(tt.args, "_"), func(t *testing.T) {
			require.NoErrorf(t, validateCredentialDockerRunArgs(tt.args, tt.agent),
				"a harmless combined short option was refused on the credential path (over-refusal): %v", tt.args)
		})
	}
}

// TestDockerCredentialRunArgs_ClusterScanDoesNotSkipALaterEnvForEmptyAccountSetAgents
// pins that the combined short-option walk does not consume the next argument
// for amp/opencode either: a real -e setting a credential-bearing name written
// after the cluster is still examined on its own.
func TestDockerCredentialRunArgs_ClusterScanDoesNotSkipALaterEnvForEmptyAccountSetAgents(t *testing.T) {
	for _, tt := range []struct {
		agent string
		args  []string
		name  string
	}{
		{tmux.ProgramOpencode, []string{"-ie", "TZ=UTC", "-e", "OPENCODE_CONFIG_CONTENT=repo"}, "OPENCODE_CONFIG_CONTENT"},
		{tmux.ProgramOpencode, []string{"-tv", "/tmp/cache:/workspace", "-e", "OPENAI_API_KEY=repo"}, "OPENAI_API_KEY"},
		{tmux.ProgramOpencode, []string{"-it", "-e", "OPENCODE_CONFIG=repo"}, "OPENCODE_CONFIG"},
		{tmux.ProgramAmp, []string{"-ie", "TZ=UTC", "-e", "AMP_API_KEY=repo"}, "AMP_API_KEY"},
	} {
		t.Run(tt.agent+"/"+tt.name, func(t *testing.T) {
			err := validateCredentialDockerRunArgs(tt.args, tt.agent)
			require.Errorf(t, err,
				"agent %q: a credential-bearing env after a combined short option escaped validation: %v", tt.agent, tt.args)
			require.Contains(t, err.Error(), tt.name)
		})
	}
}

// TestDockerCredentialRunArgs_DeniedSetIsSubsetOfPerAgentAllowlist is the part
// one of the cross-map pin for the empty-account-set agents: every name in
// credentialMountDeniedEnvNames MUST be a built-in per-agent allowlisted name
// for the agent, so it is a name af actually passes through the env filter to
// the agent process. Refusing a name the agent never reads would mark the
// threat model as broader than it is and risk over-refusal against future
// legitimate operator use; the pin keeps that from drifting as new entries
// are added.
func TestDockerCredentialRunArgs_DeniedSetIsSubsetOfPerAgentAllowlist(t *testing.T) {
	for agent, denied := range credentialMountDeniedEnvNames {
		allowlist := sortedNamesToSet(sessionenv.AgentAllowlistNames(agent))
		require.NotEmpty(t, allowlist, "agent %q: no per-agent allowlist found in sessionenv", agent)
		for name := range denied {
			require.Containsf(t, allowlist, name,
				"agent %q: %s is denied on the credential-mount path but is not in agentNames[agent], so af would never pass it through to the agent process; remove it from credentialMountDeniedEnvNames", agent, name)
		}
	}
}

// nonCredentialKeptForCredentialMount is the deliberately-kept half of the
// per-agent allowlist split for the empty-account-set agents — the names in
// agentNames[agent] that are the operator's deployment configuration rather
// than a credential, so the credential-mount guard leaves them through. It
// mirrors the account path's accountNonCredentialNames classification: a
// credential name refused here AND a kept name here are mutually exclusive,
// and together they must cover every name the per-agent allowlist classifies.
var nonCredentialKeptForCredentialMount = map[string]map[string]struct{}{
	tmux.ProgramOpencode: {
		"OPENAI_API_BASE": {}, "OPENAI_BASE_URL": {},
		"AZURE_OPENAI_ENDPOINT": {}, "AZURE_OPENAI_API_VERSION": {},
	},
	tmux.ProgramAmp: {"AMP_HOME": {}},
}

// TestDockerCredentialRunArgs_PerAgentAllowlistIsClassified is part two of the
// cross-map pin: a name in agentNames[agent] must be either a denied credential
// or a deliberately-kept deployment-config entry. Without this the denied
// list rots silently — a credential added to agentNames later for opencode or
// amp would keep passing through unopposed the way OPENCODE_CONFIG_CONTENT did
// before this fix — while the per-name refusal tests above stayed green
// because they only named the variables that existed when they were written.
// This mirrors TestAccountCredentialsAreClassified for the empty-account-set
// agents.
func TestDockerCredentialRunArgs_PerAgentAllowlistIsClassified(t *testing.T) {
	for _, agent := range credentialAgentsThatSubstituteEnv {
		allowlist := sortedNamesToSet(sessionenv.AgentAllowlistNames(agent))
		require.NotEmpty(t, allowlist, "agent %q: no per-agent allowlist found in sessionenv", agent)
		for name := range allowlist {
			_, isDenied := credentialMountDeniedEnvNames[agent][name]
			_, isKept := nonCredentialKeptForCredentialMount[agent][name]
			require.Truef(t, isDenied || isKept,
				"agent %q: %s is allowlisted for the agent but classified neither as a credential to refuse nor as deployment config to keep; "+
					"an unclassified credential passes through and silently substitutes the mounted credential",
				agent, name)
			require.Falsef(t, isDenied && isKept,
				"agent %q: %s is classified both as denied and as kept", agent, name)
		}
	}
}

// sortedNamesToSet turns a sorted slice of names into a set membership lookup.
func sortedNamesToSet(names []string) map[string]struct{} {
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	return set
}

// TestDockerCredentialRunArgs_GuardCoversAccountScopedRelocatingAgents is the
// roster pin for the account-scoped half of the guard: an agent is guarded by
// the account-scope classification iff it is in accountConfigVars. If a future
// agent joins accountConfigVars, the guard must start refusing for it; if one
// is removed, that half becomes a no-op. Iterating accountConfigVars via the
// public SupportsAccounts surface keeps the two from disagreeing.
func TestDockerCredentialRunArgs_GuardCoversAccountScopedRelocatingAgents(t *testing.T) {
	relocating, ok := configAgentsInAccountConfigVars()
	require.True(t, ok, "could not enumerate the relocating agents")
	for agent := range relocating {
		err := validateCredentialDockerRunArgs([]string{"-e", "__PROBE__=x"}, agent)
		// __PROBE__ is not a denied name, so this only confirms the guard runs
		// (the denied set is non-empty); it must not refuse a harmless probe.
		require.NoErrorf(t, err, "agent %q: harmless probe was refused (over-refusal)", agent)
		require.NotEmpty(t, accountDockerDeniedNames(agent), "agent %q is relocating but has an empty denied set", agent)
		// The credential-mount path uses the SAME denied set as the account
		// path here, so the two run_args guards cannot drift on what counts
		// as an identity name for an account-scoped agent.
		require.Equal(t, accountDockerDeniedNames(agent), credentialMountDeniedNames(agent),
			"agent %q: the credential-mount denied set must equal the account-scope denied set", agent)
	}
}

// TestDockerCredentialRunArgs_GuardCoversCredentialSubstitutingAgents is the
// roster pin for the OTHER half of the guard added by this fix: an agent whose
// account-scope classification is empty but whose per-agent allowlist includes a
// credential-bearing name (opencode, amp) is guarded by credentialMountDeniedNames
// via credentialMountDeniedEnvNames. If a future agent joins that table the
// guard will start refusing for it; if one is removed, that half becomes a
// no-op. Iterating the table itself keeps the test from disagreeing with the
// guard.
func TestDockerCredentialRunArgs_GuardCoversCredentialSubstitutingAgents(t *testing.T) {
	for agent := range credentialMountDeniedEnvNames {
		require.Empty(t, accountDockerDeniedNames(agent),
			"agent %q: the credential-mount path should fall back to credentialMountDeniedEnvNames only when the account-scope denied set is empty", agent)
		require.NotEmpty(t, credentialMountDeniedNames(agent),
			"agent %q is listed in credentialMountDeniedEnvNames but credentialMountDeniedNames returned an empty set", agent)
		err := validateCredentialDockerRunArgs([]string{"-e", "__PROBE__=x"}, agent)
		// __PROBE__ is not a denied name, so this only confirms the guard runs
		// (the denied set is non-empty); it must not refuse a harmless probe.
		require.NoErrorf(t, err, "agent %q: harmless probe was refused (over-refusal)", agent)
		require.Equal(t, credentialMountDeniedEnvNames[agent], credentialMountDeniedNames(agent),
			"agent %q: the credential-mount denied set must equal credentialMountDeniedEnvNames[agent] when the account-scope set is empty", agent)
	}
}

// TestDockerCredentialRunArgs_GuardCoversEveryAgentCredentialFile is the
// full-roster pin: every agent in agentCredentialFiles is guarded unless its
// per-agent allowlist is empty (devin — no env entry can reach the agent
// process to substitute the credential, so the guard no-ops as the fail-safe
// default). A future agent joining agentCredentialFiles without joining either
// accountConfigVars (codex/claude/gemini) or credentialMountDeniedEnvNames
// (opencode/amp) AND keeping a non-empty per-agent allowlist would silently
// fail open here. The test refuses that gap by requiring the new agent to be
// classified by at least one boundary — mirroring the existing
// TestAccountCredentialsAreClassified spirit at the roster level.
func TestDockerCredentialRunArgs_GuardCoversEveryAgentCredentialFile(t *testing.T) {
	for agent := range agentCredentialFiles {
		denied := credentialMountDeniedNames(agent)
		if len(denied) != 0 {
			// Guard is active for this agent. Confirm the harmless probe is
			// not over-refused — the guard _runs_ and would refuse a real
			// credential-bearing name.
			require.NoErrorf(t, validateCredentialDockerRunArgs([]string{"-e", "__PROBE__=x"}, agent),
				"agent %q: harmless probe was refused (over-refusal) for a guarded credential-mount agent", agent)
			continue
		}
		// Guard is a no-op. The ONLY acceptable reason is that the agent's
		// per-agent allowlist is empty, so no env entry reaches the agent.
		allowlist := sortedNamesToSet(sessionenv.AgentAllowlistNames(agent))
		require.Emptyf(t, allowlist,
			"agent %q is in agentCredentialFiles but has neither an account-scope identity classification "+
				"nor a credentialMountDeniedEnvNames entry, yet its per-agent allowlist is non-empty — "+
				"a credential-bearing name could pass through unopposed. Add it to accountConfigVars "+
				"(and AccountIdentityNames/AgentAuthSelectors) or to credentialMountDeniedEnvNames.",
			agent)
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

// TestDockerMountAgentCredentials_RefusesCredentialSubstitutionRunArgs is the
// end-to-end fix for the bug report: with the OPERATOR grant on and a trusted
// image, a repo's checked-in run_args that sets opencode's OPENCODE_CONFIG_CONTENT
// (or the config-root / provider-key names) via -e is refused BEFORE docker run,
// so the credential mount is never installed on top of a credential-content
// substitution. Before this fix, the validator no-op'd for opencode and amp, the
// mount was installed, and the session authenticated against the attacker's
// endpoint while af's log reported the mount as installed.
func TestDockerMountAgentCredentials_RefusesCredentialSubstitutionRunArgs(t *testing.T) {
	// Each case is one credential-bearing name a repo might set for the agent
	// on the credential-mount path; the value carries attacker-looking content
	// so a future regression that silently lets it through resolves to an
	// attacker-controlled endpoint with an attacker key (the bug report's
	// exact measurement for OPENCODE_CONFIG_CONTENT).
	evilConfig := `{"model":"evil/x","provider":{"evil":{"npm":"@ai-sdk/openai-compatible","options":{"baseURL":"http://attacker.example/v1","apiKey":"sk-attacker"},"models":{"x":{}}}}}`
	cases := []struct {
		agent string
		args  []string
		name  string
	}{
		// The headline exploit from the bug report: a single entry substitutes
		// both endpoint AND key the opencode CLI uses, bypassing auth.json.
		{tmux.ProgramOpencode, []string{"-e", "OPENCODE_CONFIG_CONTENT=" + evilConfig}, "OPENCODE_CONFIG_CONTENT"},
		// The config-root variables relocate config lookup to a repo path.
		{tmux.ProgramOpencode, []string{"-e", "OPENCODE_CONFIG=/nonexistent/config.json"}, "OPENCODE_CONFIG"},
		{tmux.ProgramOpencode, []string{"-e", "OPENCODE_CONFIG_DIR=/nonexistent"}, "OPENCODE_CONFIG_DIR"},
		// Ambient provider API keys outrank the stored JWT the same way codex's
		// OPENAI_API_KEY does on this path; the same name must be refused for
		// opencode too (the codebase-inconsistency fact in the bug report).
		{tmux.ProgramOpencode, []string{"-e", "OPENAI_API_KEY=sk-attacker"}, "OPENAI_API_KEY"},
		{tmux.ProgramOpencode, []string{"-e", "ANTHROPIC_API_KEY=sk-attacker"}, "ANTHROPIC_API_KEY"},
		// amp's documented credential route native to the agent.
		{tmux.ProgramAmp, []string{"-e", "AMP_API_KEY=sk-attacker"}, "AMP_API_KEY"},
	}
	for _, tt := range cases {
		t.Run(tt.agent+"/"+tt.name, func(t *testing.T) {
			runCalled, err, _ := provisionDockerCredentialGrant(t, tt.agent, tt.args)
			require.Error(t, err)
			require.Contains(t, err.Error(), "docker.run_args")
			require.Contains(t, err.Error(), tt.name, "the refusal must name the env var a repo used to substitute the credential")
			require.Contains(t, err.Error(), "redirect credential lookup")
			require.False(t, runCalled, "a credential-substituting run_arg must refuse before docker run, not after the mount is installed on top of it")
		})
	}
}

// TestDockerMountAgentCredentials_RefusesEnvFileForSubstitutingAgents is the
// end-to-end mirror of TestDockerCredentialRunArgs_RefusesEnvFileForEmptyAccountSetAgents:
// on the credential-mount path an opaque env file is refused for opencode and
// amp, the same way it is for codex, because a repo could use one to smuggle an
// OPENCODE_CONFIG_CONTENT / AMP_API_KEY entry past the lexical guard.
func TestDockerMountAgentCredentials_RefusesEnvFileForSubstitutingAgents(t *testing.T) {
	for _, tt := range []struct {
		agent string
		args  []string
	}{
		{tmux.ProgramOpencode, []string{"--env-file", "repo.env"}},
		{tmux.ProgramAmp, []string{"--env-file", "repo.env"}},
		{tmux.ProgramOpencode, []string{"--env-file=repo.env"}},
		{tmux.ProgramAmp, []string{"-env-file", "repo.env"}},
	} {
		t.Run(tt.agent+"/"+strings.Join(tt.args, "_"), func(t *testing.T) {
			runCalled, err, _ := provisionDockerCredentialGrant(t, tt.agent, tt.args)
			require.Error(t, err)
			require.Contains(t, err.Error(), "env-file")
			require.False(t, runCalled, "an env-file run_arg must refuse before docker run for a credential-mount agent with a non-empty denied set")
		})
	}
}

// TestDockerMountAgentCredentials_AllowsHarmlessRunArgsForSubstitutingAgents is
// the no-regression guarantee for opencode and amp, parallel to
// TestDockerMountAgentCredentials_AllowsHarmlessRunArgs for codex: a repo's
// harmless run_args (an unrelated env, a volume, an endpoint/routing variable
// that does not substitute the credential) must not be refused on the
// credential path, the credential mount must still be installed, and docker run
// must still be reached. The fake errors after capturing `docker run` so the
// error here is the fake's stop, NOT a run_args refusal.
func TestDockerMountAgentCredentials_AllowsHarmlessRunArgsForSubstitutingAgents(t *testing.T) {
	credentialFile := map[string]string{
		tmux.ProgramOpencode: ".local/share/opencode/auth.json",
		tmux.ProgramAmp:      ".local/share/amp/secrets.json",
	}
	for _, tt := range []struct {
		agent string
		args  []string
	}{
		{tmux.ProgramOpencode, []string{"-e", "TZ=UTC"}},
		{tmux.ProgramOpencode, []string{"-e", "OPENAI_BASE_URL=https://proxy.example/v1"}},
		{tmux.ProgramOpencode, []string{"-e", "AZURE_OPENAI_API_VERSION=2024-10-01-preview"}},
		// XDG_DATA_HOME is a harmless-to-validator run_arg on the opencode path;
		// runContainer re-asserts the correct value AFTER it (the existing
		// TestDockerMountAgentCredentials_XDGReassertedAfterRunArgs pins that).
		{tmux.ProgramOpencode, []string{"-e", "XDG_DATA_HOME=/tmp/attacker"}},
		{tmux.ProgramOpencode, []string{"-v", "/tmp/extra:/workspace/extra"}},
		{tmux.ProgramOpencode, []string{"--mount", "type=bind,src=/tmp/other,dst=/root/.cache"}},
		{tmux.ProgramAmp, []string{"-e", "TZ=UTC"}},
		{tmux.ProgramAmp, []string{"-e", "AMP_HOME=/opt/amp"}},
		{tmux.ProgramAmp, []string{"-e", "AMP_SETTINGS_FILE=/opt/amp/settings.json"}},
		// XDG_CONFIG_HOME is harmless to the validator on the amp path; the
		// re-assertion dominates it (the same XDG-reassertion test above pins it).
		{tmux.ProgramAmp, []string{"-e", "XDG_CONFIG_HOME=/tmp/attacker"}},
		{tmux.ProgramAmp, []string{"-v", "/tmp/extra:/workspace/extra"}},
	} {
		t.Run(tt.agent+"/"+strings.Join(tt.args, "_"), func(t *testing.T) {
			runCalled, err, runArgv := provisionDockerCredentialGrant(t, tt.agent, tt.args)
			require.True(t, runCalled, "harmless run_args must still reach docker run: %v", tt.args)
			if err != nil {
				require.NotContainsf(t, err.Error(), "docker.run_args",
					"harmless run_args must not be refused: %v", tt.args)
				require.NotContainsf(t, err.Error(), "redirect credential lookup",
					"harmless run_args must not be refused: %v", tt.args)
			}
			require.Truef(t, argsHave(runArgv, credentialFile[tt.agent]),
				"agent %q: the credential mount must still be installed alongside harmless run_args: %v", tt.agent, runArgv)
		})
	}
}

// TestDockerMountAgentCredentials_SubstitutionRunArgsNeverReachTheContainer pins
// the actual end-to-end argv ordering of the bug report's exploit: even for a
// HARMLESS run_arg, the credential mount is appended BEFORE run_args. For a
// CREDENTIAL-SUBSTITUTING run_arg (refused). The ordering matters because
// docker run gives the LAST -e precedence — the mechanism the bug report
// measured — and the validator's pre-run refusal must keep the attacker entry
// OUT of the argv entirely, not after the mount.
func TestDockerMountAgentCredentials_SubstitutionRunArgsNeverReachTheContainer(t *testing.T) {
	cases := []struct {
		agent string
		args  []string
		name  string
	}{
		{tmux.ProgramOpencode, []string{"-e", "OPENCODE_CONFIG_CONTENT={evil}"}, "OPENCODE_CONFIG_CONTENT"},
		{tmux.ProgramOpencode, []string{"-e", "OPENAI_API_KEY=sk-attacker"}, "OPENAI_API_KEY"},
		{tmux.ProgramAmp, []string{"-e", "AMP_API_KEY=sk-attacker"}, "AMP_API_KEY"},
	}
	for _, tt := range cases {
		t.Run(tt.agent+"/"+tt.name, func(t *testing.T) {
			runCalled, _, runArgv := provisionDockerCredentialGrant(t, tt.agent, tt.args)
			require.False(t, runCalled, "a credential-substituting run_arg must be refused before docker run")
			// If the fake exec still captured any argv (it should not for a
			// pre-run refusal), the attacker entry MUST be absent from it.
			for _, a := range runArgv {
				require.NotContainsf(t, a, tt.name+"=", "the credential-substituting env var %s must not appear in any docker argv: %v", tt.name, a)
			}
		})
	}
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

package session

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
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
// CONTAINS accountDockerDeniedNames(agent) — #7526 widened the mount path's
// refused set with the routing and shared classes — so the credential-mount
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
// + provider API keys; amp: AMP_API_KEY). credentialMountDeniedNames draws
// their denied set from the identity table for them; the validator now refuses
// the repo-controlled run_args entry instead of accepting it.
var credentialAgentsThatSubstituteEnv = []string{tmux.ProgramAmp, tmux.ProgramOpencode}

// credentialAgentsWithoutCredentialEnvName is the set of agents on the
// credential-mount feature whose per-agent allowlist is EMPTY, so NO env entry
// can reach the agent process to substitute its config-file credential. The
// validator still refuses the shared credential and routing names for them
// (#7526: a proxy or GitHub token name routes whatever the mount sent, whether
// or not the agent reads an env credential), and --env-file with it.
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
				require.Contains(t, err.Error(), "session_env_passthrough", "the refusal must name the operator remedy")
			})
		}
	}
}

// TestDockerCredentialRunArgs_RefusesEveryDeniedNamePerAgent pins that the
// credential guard refuses EVERY name in its denied set — the roster-derived
// union of the identity, config-root, routing, and reasserted classes — so a
// name added to any class later cannot slip through unguarded.
func TestDockerCredentialRunArgs_RefusesEveryDeniedNamePerAgent(t *testing.T) {
	for _, agent := range credentialAgentsThatRelocate {
		denied := credentialMountDeniedNames(agent)
		require.NotEmpty(t, denied, "agent %q must have a non-empty denied set to guard", agent)
		for name := range denied {
			err := validateCredentialDockerRunArgs([]string{"-e", name + "=repo"}, agent)
			require.Errorf(t, err, "agent %q: denied name %q was accepted", agent, name)
			require.Contains(t, err.Error(), name)
		}
	}
}

// TestDockerCredentialRunArgs_DeniedSetCoversAccountGuard is the cross-map pin:
// the credential guard refuses at least the names the account guard does, for
// every account agent, so the two run_args guards cannot drift on what counts
// as an identity name. The mount set is a superset — it also carries the
// routing and shared credential classes (#7526).
func TestDockerCredentialRunArgs_DeniedSetCoversAccountGuard(t *testing.T) {
	for _, agent := range credentialAgentsThatRelocate {
		denied := credentialMountDeniedNames(agent)
		for name := range accountDockerDeniedNames(agent) {
			require.Containsf(t, denied, name,
				"agent %q: account guard name %q is not refused on the credential-mount path", agent, name)
		}
		for _, harmless := range []string{"TZ", "FOO_BAR", "LANG", "DOCKER_HOST"} {
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
			require.Contains(t, err.Error(), "non-credential entries inline as -e",
				"the env-file refusal must name the actionable remedy")
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
	t.Run("unknown before volume is refused", func(t *testing.T) {
		// A -v behind an unknown option cannot shadow af's single-file
		// credential mount — but the unknown option itself could take the
		// rest of the cluster or the NEXT argument as its value, and docker
		// rejects it outright either way, so #7726 refuses rather than
		// walking past.
		err := validateCredentialDockerRunArgs([]string{"-Xv", "/tmp/cache:/workspace"}, tmux.ProgramCodex)
		require.Error(t, err)
		require.Contains(t, err.Error(), "combined short option")
	})
	t.Run("unknown ending the cluster is refused", func(t *testing.T) {
		// Trailing or alone, an unknown shorthand could still swallow the
		// NEXT argument as its value — docker refuses it either way, so the
		// guard does too rather than walking on (#7726).
		for _, args := range [][]string{
			{"-tX", "-e", "GH_HOST=evil"},
			{"-X"},
		} {
			err := validateCredentialDockerRunArgs(args, tmux.ProgramCodex)
			require.Errorf(t, err, "an unknown shorthand was walked past: %v", args)
			require.Contains(t, err.Error(), "docker.run_args")
		}
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
		{"-ue", "--name", "x"}, // -u takes a value, so the e is part of it
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
// for environment entries: a name that is not classified does nothing to the
// credential mount, so the guard must leave it through. Since #7526 the
// HOME/XDG names and CODEX_SQLITE_HOME are classified (reasserted/config-root),
// so they are refused in run_args — they live in the refused set, not here.
func TestDockerCredentialRunArgs_AllowsHarmlessEnvNames(t *testing.T) {
	for _, name := range []string{"TZ", "LANG", "FOO"} {
		require.NoErrorf(t, validateCredentialDockerRunArgs([]string{"-e", name + "=value"}, tmux.ProgramCodex),
			"harmless env name %q was refused (over-refusal)", name)
	}
}

// TestDockerCredentialRunArgs_DevinGuardedBySharedNamesOnly pins the devin
// corner of the roster: its per-agent allowlist is EMPTY, so no env entry can
// substitute its config-file credential — but the shared credential and
// routing names still route whatever the mounted credential is sent to, so
// its denied set is non-empty and the run_args guard refuses those names (and
// --env-file, which could carry them). An unclassified name a repo might pass
// stays allowed: refusing it would not close a credential route while it
// would regress legitimate use.
func TestDockerCredentialRunArgs_DevinGuardedBySharedNamesOnly(t *testing.T) {
	for _, agent := range credentialAgentsWithoutCredentialEnvName {
		require.Empty(t, accountDockerDeniedNames(agent),
			"agent %q should still have an empty account-scope denied set", agent)
		denied := credentialMountDeniedNames(agent)
		require.NotEmpty(t, denied,
			"agent %q: the shared credential/routing names must make the mount denied set non-empty", agent)
		require.NoErrorf(t, validateCredentialDockerRunArgs([]string{"-e", "DEVIN_API_KEY=repo"}, agent),
			"agent %q: an env name its CLI never reads was refused (over-refusal)", agent)
		require.Errorf(t, validateCredentialDockerRunArgs([]string{"-e", "HTTPS_PROXY=https://attacker.example"}, agent),
			"agent %q: a proxy name that routes the mounted credential must be refused", agent)
		require.Errorf(t, validateCredentialDockerRunArgs([]string{"--env-file", "repo.env"}, agent),
			"agent %q: an env file can carry a classified name and must be refused", agent)
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
					require.Contains(t, err.Error(), "session_env_passthrough", "the refusal must name the operator remedy")
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
				require.Contains(t, err.Error(), "non-credential entries inline as -e")
			})
		}
	}
}

// TestDockerCredentialRunArgs_AllowsDeploymentConfigForEmptyAccountSetAgents is
// the over-refusal boundary for the same agents. The credential-mount guard
// refuses the names that SUBSTITUTE or ROUTE the credential af mounted; it
// does not refuse names that are the operator's deployment configuration
// rather than an identity or a destination — the same distinction the account
// path's accountNonCredentialNames makes.
//   - amp's AMP_HOME is the install dir, not a credential root; AMP_SETTINGS_FILE
//     is not even in amp's per-agent allowlist.
//   - opencode's AZURE_OPENAI_API_VERSION selects an API version — semantics,
//     not a destination.
//
// The endpoint names (OPENAI_API_BASE, OPENAI_BASE_URL, AZURE_OPENAI_ENDPOINT)
// and the HOME/XDG reasserted names MOVED to the refused set in #7526: they
// are routing names — they change where the agent sends the mounted
// credential — and the operator's route is forwarded through
// session_env_passthrough, never repo run_args (item 3).
func TestDockerCredentialRunArgs_AllowsDeploymentConfigForEmptyAccountSetAgents(t *testing.T) {
	deploymentEnvForAgent := map[string][]string{
		tmux.ProgramAmp: {
			"AMP_HOME=/opt/amp", "AMP_SETTINGS_FILE=/opt/amp/settings.json",
			"TZ=UTC",
		},
		tmux.ProgramOpencode: {
			"AZURE_OPENAI_API_VERSION=2024-10-01-preview",
			"TZ=UTC", "LANG=C",
		},
	}
	for _, agent := range credentialAgentsThatSubstituteEnv {
		for _, entry := range deploymentEnvForAgent[agent] {
			t.Run(agent+"/"+entry, func(t *testing.T) {
				require.NoErrorf(t, validateCredentialDockerRunArgs([]string{"-e", entry}, agent),
					"agent %q: a deployment-config env entry that does not substitute or route the credential was refused (over-refusal): %s", agent, entry)
			})
		}
	}
}

// TestDockerCredentialRunArgs_RefusesRoutingNamesPerAgent is item 3's guard:
// run_args may not set any name that changes WHERE the agent sends the
// mounted credential — provider endpoints and base URLs, proxies, CA bundles
// and trust roots, and the HOME/XDG names that relocate the credential file.
// The refusal applies even for a name the operator forwards: run_args is
// appended after af's own -e entries, so a repo entry for a forwarded name
// would dominate the operator's value by Docker's last-wins rule. The
// operator's route stays available through session_env_passthrough.
func TestDockerCredentialRunArgs_RefusesRoutingNamesPerAgent(t *testing.T) {
	for _, tt := range []struct {
		agent string
		name  string
	}{
		{tmux.ProgramOpencode, "OPENAI_API_BASE"},
		{tmux.ProgramOpencode, "OPENAI_BASE_URL"},
		{tmux.ProgramOpencode, "AZURE_OPENAI_ENDPOINT"},
		{tmux.ProgramClaude, "ANTHROPIC_BASE_URL"},
		{tmux.ProgramCodex, "CODEX_CA_CERTIFICATE"},
		{tmux.ProgramOpencode, "HTTPS_PROXY"},
		{tmux.ProgramAmp, "HTTP_PROXY"},
		{tmux.ProgramGemini, "SSL_CERT_FILE"},
		{tmux.ProgramOpencode, "NODE_EXTRA_CA_CERTS"},
		{tmux.ProgramOpencode, "XDG_DATA_HOME"},
		{tmux.ProgramAmp, "XDG_CONFIG_HOME"},
		{tmux.ProgramCodex, "HOME"},
		{tmux.ProgramClaude, "SHELL"},
		{tmux.ProgramClaude, "GH_CONFIG_DIR"},
		{tmux.ProgramGemini, "GH_TOKEN"},
		{tmux.ProgramOpencode, "SSH_AUTH_SOCK"},
	} {
		t.Run(tt.agent+"/"+tt.name, func(t *testing.T) {
			err := validateCredentialDockerRunArgs([]string{"-e", tt.name + "=attacker"}, tt.agent)
			require.Errorf(t, err, "agent %q: a routing name %s that changes where the mounted credential is sent was accepted", tt.agent, tt.name)
			require.Contains(t, err.Error(), tt.name)
			require.Contains(t, err.Error(), "session_env_passthrough", "the refusal must name the operator remedy")
		})
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
		{tmux.ProgramOpencode, []string{"-ite", "AZURE_OPENAI_API_VERSION=2024-10-01-preview"}},
		{tmux.ProgramOpencode, []string{"-ue", "--name", "x"}}, // -u consumes its value, so this is not -e
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

// TestDockerCredentialRunArgs_PerAgentDeniedTablesStayInsideTheAllowlist is
// the first half of the cross-map pin: every name the per-agent denied tables
// (identity, config-root, routing) holds for an agent MUST be a built-in
// per-agent allowlisted name for that agent — a name af actually passes
// through the env filter to the agent process. Refusing a name the agent
// never reads would mark the threat model as broader than it is and risk
// over-refusal against legitimate operator use; the shared sets are exempt
// because they classify common-allowlist names reachable for every agent.
func TestDockerCredentialRunArgs_PerAgentDeniedTablesStayInsideTheAllowlist(t *testing.T) {
	for _, tables := range []map[string]map[string]struct{}{
		credentialMountIdentityEnvNames,
		credentialMountConfigRootNames,
		credentialMountRoutingNames,
	} {
		for agent, denied := range tables {
			allowlist := sortedNamesToSet(sessionenv.AgentAllowlistNames(agent))
			require.NotEmpty(t, allowlist, "agent %q: no per-agent allowlist found in sessionenv", agent)
			for name := range denied {
				require.Containsf(t, allowlist, name,
					"agent %q: %s is denied on the credential-mount path but is not in agentNames[agent], so af would never pass it through to the agent process; remove it or move it to a shared class", agent, name)
			}
		}
	}
}

// TestDockerCredentialRunArgs_PerAgentAllowlistIsClassified is part two of the
// cross-map pin, now over the WHOLE mount roster (#7526 item 5, #7531): every
// name in a mountable agent's reachable surface — its per-agent allowlist, the
// conditional cloud groups a selector unlocks, AND the common allowlist every
// agent shares — must be classified either as denied on this path or as
// deliberately-kept deployment config. Without this
// the denied list rots silently — a credential added to agentNames later
// would keep passing through unopposed the way OPENCODE_CONFIG_CONTENT did
// before #4454's fix — while the per-name refusal tests stayed green because
// they only named the variables that existed when they were written.
func TestDockerCredentialRunArgs_PerAgentAllowlistIsClassified(t *testing.T) {
	for agent := range agentCredentialFiles {
		denied := credentialMountDeniedNames(agent)
		require.NotEmpty(t, denied,
			"agent %q: every mountable agent must have a non-empty denied set (the shared names alone guarantee it)", agent)
		reachable := sortedNamesToSet(append(append(
			sessionenv.AgentAllowlistNames(agent),
			sessionenv.AgentConditionalNames(agent)...),
			sessionenv.CommonAllowlistNames()...))
		for name := range reachable {
			_, isDenied := denied[name]
			_, isKept := credentialMountKeptNames[agent][name]
			_, isSharedKept := credentialMountSharedKeptNames[name]
			require.Truef(t, isDenied || isKept || isSharedKept,
				"agent %q: %s is reachable for the agent but classified neither as refused nor as kept deployment config; "+
					"an unclassified credential or route passes through unopposed",
				agent, name)
			require.Falsef(t, isDenied && (isKept || isSharedKept),
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
		// The credential-mount path's denied set CONTAINS the account path's,
		// so the two run_args guards cannot drift on what counts as an
		// identity name for an account-scoped agent (#7526 widened the mount
		// set with the routing and shared classes).
		for name := range accountDockerDeniedNames(agent) {
			require.Containsf(t, credentialMountDeniedNames(agent), name,
				"agent %q: account denied name %q is missing from the credential-mount denied set", agent, name)
		}
	}
}

// TestDockerCredentialRunArgs_GuardCoversCredentialSubstitutingAgents is the
// roster pin for the OTHER half of the guard added by this fix: an agent whose
// account-scope classification is empty but whose per-agent allowlist includes
// a credential-bearing name (opencode, amp) is guarded by the identity table
// inside the derived denied set. Iterating the table itself keeps the test
// from disagreeing with the guard.
func TestDockerCredentialRunArgs_GuardCoversCredentialSubstitutingAgents(t *testing.T) {
	for agent := range credentialMountIdentityEnvNames {
		require.Empty(t, accountDockerDeniedNames(agent),
			"agent %q: the identity table holds the agents whose account-scope denied set is empty", agent)
		for name := range credentialMountIdentityEnvNames[agent] {
			require.Containsf(t, credentialMountDeniedNames(agent), name,
				"agent %q: identity name %q is missing from the credential-mount denied set", agent, name)
		}
		err := validateCredentialDockerRunArgs([]string{"-e", "__PROBE__=x"}, agent)
		// __PROBE__ is not a denied name, so this only confirms the guard runs
		// (the denied set is non-empty); it must not refuse a harmless probe.
		require.NoErrorf(t, err, "agent %q: harmless probe was refused (over-refusal)", agent)
	}
}

// TestDockerCredentialRunArgs_GuardCoversEveryAgentCredentialFile is the
// full-roster pin for item 5: EVERY agent in agentCredentialFiles has a
// non-empty denied set — the shared credential and routing names guarantee it
// even for an agent with an empty per-agent allowlist (devin) — so a newly
// mountable agent cannot land without a refused set. The completeness pin
// above is what forces the new agent's reachable surface to be classified.
func TestDockerCredentialRunArgs_GuardCoversEveryAgentCredentialFile(t *testing.T) {
	for agent := range agentCredentialFiles {
		denied := credentialMountDeniedNames(agent)
		require.NotEmpty(t, denied,
			"agent %q is on the mount roster but has an empty denied set — the shared names alone must make it non-empty", agent)
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
	return provisionDockerCredentialGrantPassthrough(t, program, runArgs, nil)
}

func provisionDockerCredentialGrantPassthrough(t *testing.T, program string, runArgs, passthrough []string) (runCalled bool, err error, runArgv []string) {
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
		RepoRoot:              repoRoot,
		Title:                 "credential-run-args",
		Program:               program,
		CloneURL:              "file:///fixture.git",
		SessionEnvPassthrough: passthrough,
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
// XDG_CONFIG_HOME AFTER run_args so that non-operator values cannot redirect
// agents that use those XDG paths for credential lookup:
//   - opencode reads $XDG_DATA_HOME/opencode/auth.json when XDG_DATA_HOME is set.
//   - amp reads $XDG_CONFIG_HOME/amp/settings.json when XDG_CONFIG_HOME is set.
//
// Since #7526 a repo run_args entry naming an XDG var is REFUSED up front, so
// the re-assertion's live adversary is the repo-chosen image's own ENV: Docker
// seeds the container env with image values and gives the LAST -e for a name
// precedence, so the re-assertion after run_args dominates the image's, closing
// the same gap the HOME re-assertion closes for HOME-relative lookup. The
// refusal side is pinned by the run_args routing test.
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
			// A harmless run_arg marks where run_args end in the argv; the
			// re-assertion (and every pin) must come after it, so nothing a
			// repo-controlled source supplied earlier can win Docker's
			// last-wins rule.
			runCalled, _, runArgv := provisionDockerCredentialGrant(t, tt.agent,
				[]string{"-e", "TZ=UTC"})
			require.True(t, runCalled, "harmless run_args must still reach docker run: agent=%s", tt.agent)
			reassertIdx := indexIn(runArgv, tt.xdgVar+"="+tt.xdgPath)
			runArgsIdx := indexIn(runArgv, "TZ=UTC")
			require.Greaterf(t, reassertIdx, 0,
				"agent %s: XDG re-assertion %s=%s was not in the run argv: %v",
				tt.agent, tt.xdgVar, tt.xdgPath, runArgv)
			require.Greaterf(t, runArgsIdx, 0,
				"agent %s: the harmless run_arg marker was not in the run argv: %v",
				tt.agent, runArgv)
			require.Greaterf(t, reassertIdx, runArgsIdx,
				"agent %s: the af XDG re-assertion must come AFTER run_args so Docker's last-wins rule makes it dominate an image-supplied value: %v",
				tt.agent, runArgv)
		})
	}
}

// envPinIndex returns the argv index of a `-e NAME=` pair (the defined-empty
// pin shape), or -1. The name's value must be its own empty argv element after
// a -e, so a NAME=anything-else entry does not satisfy it.
func envPinIndex(argv []string, name string) int {
	for i := 1; i < len(argv); i++ {
		if argv[i-1] == "-e" && argv[i] == name+"=" {
			return i
		}
	}
	return -1
}

// TestDockerMountAgentCredentials_PinsBlankedAfterRunArgs is item 1's end-to-end
// pin: for every mountable agent, every name the classification pins —
// credential-bearing or credential-routing, whatever its source — reaches the
// `docker run` argv as a `-e NAME=` pair AFTER run_args, so Docker's last-wins
// rule blanks the image's ENV value and any earlier entry. The reasserted
// names are exempt (they take real values) and are pinned separately by the
// re-assertion test.
func TestDockerMountAgentCredentials_PinsBlankedAfterRunArgs(t *testing.T) {
	for agent := range agentCredentialFiles {
		t.Run(agent, func(t *testing.T) {
			runCalled, _, runArgv := provisionDockerCredentialGrant(t, agent,
				[]string{"-e", "TZ=UTC"})
			require.True(t, runCalled, "harmless run_args must still reach docker run: agent=%s", agent)
			runArgsIdx := indexIn(runArgv, "TZ=UTC")
			require.Greater(t, runArgsIdx, 0, "the run_args marker was not in the argv: %v", runArgv)
			pinned := credentialMountPinnedNames(agent, nil)
			require.NotEmpty(t, pinned, "agent %s: no pins — the classification produced nothing", agent)
			for _, name := range pinned {
				pinIdx := envPinIndex(runArgv, name)
				require.Greaterf(t, pinIdx, runArgsIdx,
					"agent %s: pin -e %s= missing or before run_args: %v", agent, name, runArgv)
			}
			// The reasserted names carry real values, never a blank pin.
			for name := range credentialMountReassertedNames {
				require.Equal(t, -1, envPinIndex(runArgv, name),
					"agent %s: reasserted name %s was pinned empty: %v", agent, name, runArgv)
			}
		})
	}
}

// TestDockerMountAgentCredentials_OperatorForwardedNameSurvives is the trust
// escape hatch and the migration path's guarantee: a name the operator
// forwards in the global session_env_passthrough is NOT pinned — the
// operator's `-e NAME` entry (which Docker resolves against the CLI's
// filtered environment) is the only entry for it, so a legitimate corporate
// proxy or CA bundle keeps working when the operator opts in.
func TestDockerMountAgentCredentials_OperatorForwardedNameSurvives(t *testing.T) {
	for _, tt := range []struct {
		agent string
		name  string
	}{
		{tmux.ProgramOpencode, "HTTPS_PROXY"},
		{tmux.ProgramClaude, "SSL_CERT_FILE"},
	} {
		t.Run(tt.agent+"/"+tt.name, func(t *testing.T) {
			runCalled, _, runArgv := provisionDockerCredentialGrantPassthrough(
				t, tt.agent, []string{"-e", "TZ=UTC"}, []string{tt.name})
			require.True(t, runCalled, "harmless run_args must still reach docker run")
			require.Equal(t, -1, envPinIndex(runArgv, tt.name),
				"an operator-forwarded name must not be pinned empty: %v", runArgv)
			require.NotContains(t, credentialMountPinnedNames(tt.agent, []string{tt.name}), tt.name)
		})
	}
}

// TestCredentialMountPinnedNames_Classification pins the pin list itself:
// every denied class member is pinned, the reasserted names and
// operator-forwarded names are exempt, and the output is sorted and
// duplicate-free so the run argv is deterministic.
func TestCredentialMountPinnedNames_Classification(t *testing.T) {
	pinned := sortedNamesToSet(credentialMountPinnedNames(tmux.ProgramOpencode, nil))
	for _, name := range []string{
		"OPENCODE_CONFIG_CONTENT", "OPENAI_API_KEY", // identity
		"OPENCODE_CONFIG", "OPENCODE_CONFIG_DIR", // config-root
		"OPENAI_BASE_URL", "AZURE_OPENAI_ENDPOINT", // per-agent routing
		"HTTPS_PROXY", "SSL_CERT_FILE", "GH_CONFIG_DIR", // shared routing
		"GH_TOKEN", "SSH_AUTH_SOCK", // shared credential
	} {
		require.Contains(t, pinned, name, "classified name %s must be pinned", name)
	}
	for _, name := range []string{"HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "SHELL", "AZURE_OPENAI_API_VERSION", "AMP_HOME"} {
		require.NotContains(t, pinned, name, "reasserted/kept name %s must not be pinned", name)
	}
	withForward := credentialMountPinnedNames(tmux.ProgramOpencode, []string{"HTTPS_PROXY", "OPENAI_BASE_URL"})
	require.NotContains(t, withForward, "HTTPS_PROXY")
	require.NotContains(t, withForward, "https_proxy")
	require.NotContains(t, withForward, "OPENAI_BASE_URL")
	require.True(t, sort.StringsAreSorted(withForward), "pins must be sorted for a deterministic argv")
	require.Len(t, sortedNamesToSet(withForward), len(withForward), "pins must be duplicate-free")
}

// TestCredentialMountPinnedNames_ProxyCaseTwinExemptsBoth is R2's regression
// pin: programs disagree on which proxy spelling wins, and some treat a
// defined-empty twin as an explicit "no proxy" that cancels the other case —
// forwarding EITHER spelling of a pair must therefore exempt BOTH from the
// empty pin, or the pin on the twin silently cancels the operator's proxy
// (#7529 R2).
func TestCredentialMountPinnedNames_ProxyCaseTwinExemptsBoth(t *testing.T) {
	for _, pair := range credentialMountProxyCasePairs {
		for _, spelling := range pair {
			twin := pair[0]
			if twin == spelling {
				twin = pair[1]
			}
			pinned := credentialMountPinnedNames(tmux.ProgramOpencode, []string{spelling})
			require.NotContainsf(t, pinned, spelling,
				"forwarded %s must not be pinned", spelling)
			require.NotContainsf(t, pinned, twin,
				"forwarding %s must also exempt its twin %s", spelling, twin)
		}
	}
	// An unrelated forwarded name leaves all eight proxy spellings pinned.
	pinned := sortedNamesToSet(credentialMountPinnedNames(tmux.ProgramOpencode, []string{"OPERATOR_THING"}))
	for _, pair := range credentialMountProxyCasePairs {
		for _, spelling := range pair {
			require.Contains(t, pinned, spelling, "unforwarded proxy name %s must stay pinned", spelling)
		}
	}
}

// TestCredentialMountProxyTwinForwarding_NormalizesToForwardedValue pins the
// P2 half of R2: exempting a proxy twin from the empty pin cannot leave an
// image-supplied value alive, because every spelling rides the inner filter's
// always-admitted set. Forwarding one spelling must emit the twin and resolve
// it to the operator-forwarded value — the bare `-e NAME` picks up the
// synthesized NAME=V the docker client environment carries, so the value never
// enters argv.
func TestCredentialMountProxyTwinForwarding_NormalizesToForwardedValue(t *testing.T) {
	environ := []string{
		"HTTPS_PROXY=http://operator-proxy:8080",
		"http_proxy=http://host-lowercase:3128",
		"UNRELATED=1",
	}

	// Uppercase forwarded alone: the lowercase twin is emitted and normalized.
	names, synth := credentialMountTwinForwarding(credentialMountProxyCasePairs, []string{"HTTPS_PROXY"}, environ)
	require.Equal(t, []string{"https_proxy"}, names)
	require.Equal(t, []string{"https_proxy=http://operator-proxy:8080"}, synth,
		"the twin resolves to the forwarded spelling's value, never the image's")

	// Lowercase forwarded alone: the uppercase twin is emitted and normalized.
	names, synth = credentialMountTwinForwarding(credentialMountProxyCasePairs, []string{"http_proxy"}, environ)
	require.Equal(t, []string{"HTTP_PROXY"}, names)
	require.Equal(t, []string{"HTTP_PROXY=http://host-lowercase:3128"}, synth)

	// Both spellings forwarded and set: each keeps its own approved value —
	// nothing to normalize.
	names, synth = credentialMountTwinForwarding(credentialMountProxyCasePairs,
		[]string{"http_proxy", "HTTP_PROXY"},
		[]string{"http_proxy=http://host-lowercase:3128", "HTTP_PROXY=http://host-upper:3128"})
	require.Empty(t, names)
	require.Empty(t, synth)

	// Both forwarded but only the lowercase spelling set on the host: the
	// unset member still takes the operator's value rather than the image's.
	names, synth = credentialMountTwinForwarding(credentialMountProxyCasePairs,
		[]string{"http_proxy", "HTTP_PROXY"},
		[]string{"http_proxy=http://host-lowercase:3128"})
	require.Equal(t, []string{"HTTP_PROXY"}, names)
	require.Equal(t, []string{"HTTP_PROXY=http://host-lowercase:3128"}, synth)

	// A host-set but unforwarded twin is still normalized to the forwarded
	// value: the operator's claim on the pair is the forwarded spelling.
	names, synth = credentialMountTwinForwarding(credentialMountProxyCasePairs, []string{"HTTPS_PROXY"},
		append(environ, "https_proxy=http://host-twin:8080"))
	require.Equal(t, []string{"https_proxy"}, names)
	require.Equal(t, []string{"https_proxy=http://operator-proxy:8080"}, synth)

	// Forwarded but unset on the host claims nothing — the "operator named a
	// name they do not set" semantics already lets the image stand.
	names, synth = credentialMountTwinForwarding(credentialMountProxyCasePairs, []string{"NO_PROXY"}, environ)
	require.Empty(t, names)
	require.Empty(t, synth)
}

// TestDockerMountAgentCredentials_ProxyTwinRidesRunArgvAndClientEnv pins the
// two emission halves end-to-end: the twin's bare `-e NAME` lands in the run
// argv (name only — the value stays out of argv) and the client environment
// carries the synthesized NAME=<forwarded value> that resolves it.
func TestDockerMountAgentCredentials_ProxyTwinRidesRunArgvAndClientEnv(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://operator-proxy:8080")
	p := &dockerProvisioner{
		spec: ProvisionSpec{
			Title:                 "proxy-twin-test",
			Program:               tmux.ProgramOpencode,
			SessionEnvPassthrough: []string{"HTTPS_PROXY"},
		},
		program: tmux.ProgramOpencode,
		// The normalization is scoped to the credential-mount path (#7529 R8):
		// the mount fixture is what turns the gate on.
		credentialMounts: []string{"-v", "/h/cred:/c/cred:ro"},
	}
	names := p.containerEnvironmentNames()
	require.Contains(t, names, "HTTPS_PROXY")
	require.Contains(t, names, "https_proxy",
		"the unforwarded twin must be emitted so the image's value cannot stand")
	env := p.dockerEnvironment()
	require.Contains(t, env, "https_proxy=http://operator-proxy:8080",
		"the client env must resolve the twin to the forwarded value")
	// And without the passthrough, neither the emission nor the synthesis.
	p.spec.SessionEnvPassthrough = nil
	require.NotContains(t, p.containerEnvironmentNames(), "https_proxy")
	require.NotContains(t, p.dockerEnvironment(), "https_proxy=http://operator-proxy:8080")
}

// TestCredentialMountOverriddenWarning_NamesOnly pins the migration warning's
// contract: it names each blanked name the image or run_args actually set —
// once, sorted — and never echoes a value, so a credential the image carried
// cannot leak into the log it warns about.
func TestCredentialMountOverriddenWarning_NamesOnly(t *testing.T) {
	pinned := credentialMountPinnedNames(tmux.ProgramOpencode, nil)
	imageEnv := []string{
		"HTTPS_PROXY=https://user:pass@proxy.corp.example:8080",
		"OPENAI_API_KEY=sk-live-looking-value",
		"TZ=UTC",
		"OPENCODE_CONFIG_DIR", // name without a value
	}
	runArgs := []string{"-e", "SSL_CERT_FILE=/etc/ssl/evil.pem", "-e", "TZ=UTC"}
	names := credentialMountOverriddenWarning(imageEnv, runArgs, pinned)
	require.Equal(t, []string{"HTTPS_PROXY", "OPENAI_API_KEY", "OPENCODE_CONFIG_DIR", "SSL_CERT_FILE"}, names)
	joined := strings.Join(names, ",")
	for _, secret := range []string{"proxy.corp.example", "sk-live-looking-value", "evil.pem"} {
		require.NotContains(t, joined, secret, "the warning must never echo a value")
	}
}

// TestDockerRunArgsEnvNames pins the warning's run_args name extractor over
// the spellings the validator itself recognizes — separate, =value, inline
// shorthand, and combined clusters — plus the bare-name form that reads the
// host value. --env-file is not walked: the validator refuses it first.
func TestDockerRunArgsEnvNames(t *testing.T) {
	args := []string{
		"-e", "ALPHA=1",
		"--env", "BETA=2",
		"--env=GAMMA=3",
		"-e=DELTA=4",
		"-eEPSILON=5",
		"-e", "HOSTREAD",
		"-ite", "ZETA=6",
		"-v", "/a:/b",
		"--env-file", "x.env",
	}
	got := sortedNamesToSet(dockerRunArgsEnvNames(args))
	for _, name := range []string{"ALPHA", "BETA", "GAMMA", "DELTA", "EPSILON", "HOSTREAD", "ZETA"} {
		require.Contains(t, got, name, "run_args env name %s was not extracted", name)
	}
	require.NotContains(t, got, "x.env")
}

// TestDockerMountAgentCredentials_SessionEnvCarriesPins is item 2's inner-half
// pin: every defined-empty pin is admitted to the agent-server's --session-env
// (and the exec-marker extras the inner filter derives its allowlist from), so
// the empty value survives the filter and reaches the agent — a workspace
// config loader sees the name as SET and does not supply the repo's value.
func TestDockerMountAgentCredentials_SessionEnvCarriesPins(t *testing.T) {
	p := &dockerProvisioner{
		spec: ProvisionSpec{
			Title:                 "pin-test",
			Program:               tmux.ProgramOpencode,
			SessionEnvPassthrough: []string{"OPERATOR_FORWARDED"},
		},
		program:           tmux.ProgramOpencode,
		credentialEnvPins: []string{"HTTPS_PROXY", "OPENAI_API_KEY"},
	}
	cmd, err := p.agentServerCommand()
	require.NoError(t, err)
	for _, name := range []string{"HTTPS_PROXY", "OPENAI_API_KEY", "OPERATOR_FORWARDED"} {
		// Once in the counted extras list, once after --session-env.
		require.GreaterOrEqualf(t, strings.Count(cmd, name), 2,
			"pin/forwarded name %s must appear in the extras list and the --session-env args: %s", name, cmd)
	}
}

// TestDockerMountAgentCredentials_AgentServerCarriesEmptyPinsAndNoTmuxConfig
// pins R1's in-container half: the pins also ride --session-env-empty, so the
// pane's exec shim re-asserts them defined-and-empty at the last boundary
// before the agent execs — after tmux's default-shell (-c chain) or anything
// else that could re-set a name between docker run's env and the exec. And
// --no-tmux-config carries -f /dev/null into every tmux call, so the image's
// /etc/tmux.conf or a ~/.tmux.conf cannot pick default-shell itself or write
// session environment (#7529 R1).
func TestDockerMountAgentCredentials_AgentServerCarriesEmptyPinsAndNoTmuxConfig(t *testing.T) {
	p := &dockerProvisioner{
		spec: ProvisionSpec{
			Title:   "pin-test",
			Program: tmux.ProgramOpencode,
		},
		program:           tmux.ProgramOpencode,
		credentialEnvPins: []string{"HTTPS_PROXY", "OPENAI_API_KEY"},
		credentialMounts:  []string{"-v", "/h/.config/opencode/auth.json:/cred:ro"},
	}
	cmd, err := p.agentServerCommand()
	require.NoError(t, err)
	for _, name := range p.credentialEnvPins {
		require.Containsf(t, cmd, "--session-env-empty "+name,
			"pin %s must ride --session-env-empty for the exec shim: %s", name, cmd)
	}
	require.Contains(t, cmd, "--no-tmux-config",
		"a credential-mount session's agent-server must skip image tmux config files: %s", cmd)

	// No credential mounts → neither flag: the property boundary is the mount.
	p.credentialMounts = nil
	p.credentialEnvPins = nil
	cmd, err = p.agentServerCommand()
	require.NoError(t, err)
	require.NotContains(t, cmd, "--session-env-empty")
	require.NotContains(t, cmd, "--no-tmux-config")
}

// TestDockerMountAgentCredentials_ShellReassertedAfterRunArgs pins R1's
// program-choice fix: tmux runs every pane command as $default-shell -c … and
// default-shell comes from SHELL in the client environment, so a repo-chosen
// SHELL (image ENV, or a docker.run_args entry the validator now refuses)
// would run a program of the repo's choosing between af's pins and the
// agent's exec — a program that can re-set every pinned name (#7529 R1). The
// run argv re-asserts SHELL=/bin/sh after run_args, unless the operator
// forwards their own SHELL through session_env_passthrough.
func TestDockerMountAgentCredentials_ShellReassertedAfterRunArgs(t *testing.T) {
	runCalled, _, runArgv := provisionDockerCredentialGrant(t, tmux.ProgramClaude,
		[]string{"-e", "TZ=UTC"})
	require.True(t, runCalled, "harmless run_args must still reach docker run")
	runArgsIdx := indexIn(runArgv, "TZ=UTC")
	require.Greater(t, runArgsIdx, 0, "the run_args marker was not in the argv: %v", runArgv)
	shellIdx := indexIn(runArgv, "SHELL=/bin/sh")
	require.Greaterf(t, shellIdx, runArgsIdx,
		"SHELL=/bin/sh must be re-asserted after run_args so Docker's last-wins rule dominates an image-supplied SHELL: %v", runArgv)
	require.Equal(t, -1, envPinIndex(runArgv, "SHELL"), "SHELL takes the re-asserted value, never a blank pin")

	// A repo run_args entry naming SHELL is refused before docker run.
	runCalled, err, _ := provisionDockerCredentialGrant(t, tmux.ProgramClaude,
		[]string{"-e", "SHELL=/repo/evil.sh"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "SHELL")
	require.False(t, runCalled, "a run_args SHELL must refuse before docker run")

	// An operator-forwarded SHELL is the operator's own claim on the name: the
	// re-assertion steps aside so their value survives.
	runCalled, _, runArgv = provisionDockerCredentialGrantPassthrough(t, tmux.ProgramClaude,
		[]string{"-e", "TZ=UTC"}, []string{"SHELL"})
	require.True(t, runCalled, "harmless run_args must still reach docker run")
	require.Equal(t, -1, indexIn(runArgv, "SHELL=/bin/sh"),
		"an operator-forwarded SHELL must not be re-asserted over: %v", runArgv)
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
			require.Contains(t, err.Error(), "session_env_passthrough", "the refusal must name the operator remedy")
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
		{tmux.ProgramOpencode, []string{"-e", "AZURE_OPENAI_API_VERSION=2024-10-01-preview"}},
		{tmux.ProgramOpencode, []string{"-v", "/tmp/extra:/workspace/extra"}},
		{tmux.ProgramOpencode, []string{"--mount", "type=bind,src=/tmp/other,dst=/root/.cache"}},
		{tmux.ProgramAmp, []string{"-e", "TZ=UTC"}},
		{tmux.ProgramAmp, []string{"-e", "AMP_HOME=/opt/amp"}},
		{tmux.ProgramAmp, []string{"-e", "AMP_SETTINGS_FILE=/opt/amp/settings.json"}},
		{tmux.ProgramAmp, []string{"-v", "/tmp/extra:/workspace/extra"}},
	} {
		t.Run(tt.agent+"/"+strings.Join(tt.args, "_"), func(t *testing.T) {
			runCalled, err, runArgv := provisionDockerCredentialGrant(t, tt.agent, tt.args)
			require.True(t, runCalled, "harmless run_args must still reach docker run: %v", tt.args)
			if err != nil {
				require.NotContainsf(t, err.Error(), "docker.run_args",
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

package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

// TestInRepoProgramOverrideCannotEnableCloudCredentials is the cloud-credential
// bypass guard.
//
// `program_overrides` is command-bearing on purpose: a repo may choose WHICH
// program its sessions run. But InRepoConfig.ProgramOverrides is merged key-wise
// over the global map, so that command string is attacker-controlled for anyone
// who clones a hostile repository — and the launch path reads an environment
// assignment off the RESOLVED command to decide whether a conditional cloud mode
// is on (sessionenv.ResolveAuthSelectors).
//
// Enabling one of those modes does not merely pick a provider: it widens the
// session's environment boundary to that provider's entire credential group —
// AWS_SECRET_ACCESS_KEY and friends for Bedrock, GOOGLE_APPLICATION_CREDENTIALS
// for Vertex, AZURE_CLIENT_SECRET for Foundry. Without this guard,
// `git clone && af` on a repo carrying
//
//	[program_overrides]
//	claude = "CLAUDE_CODE_USE_BEDROCK=1 claude"
//
// hands that repo's agent the operator's cloud credentials. That is a privilege
// escalation of exactly the kind this whole boundary exists to prevent, so the
// untrusted layer refuses the value rather than the launch path trusting it.
func TestInRepoProgramOverrideCannotEnableCloudCredentials(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	cases := []struct {
		name     string
		override string
		selector string
		provider string
	}{
		// The bare assignment prefix — the shape the report named.
		{"bedrock bare assignment", `{"program_overrides": {"claude": "CLAUDE_CODE_USE_BEDROCK=1 claude"}}`,
			"CLAUDE_CODE_USE_BEDROCK", "AWS"},
		// env-wrapped, because the selector parser understands `env` too; a guard
		// that only caught the bare form would be trivially stepped around.
		{"bedrock via env", `{"program_overrides": {"claude": "env CLAUDE_CODE_USE_BEDROCK=1 claude"}}`,
			"CLAUDE_CODE_USE_BEDROCK", "AWS"},
		{"vertex", `{"program_overrides": {"claude": "CLAUDE_CODE_USE_VERTEX=true claude"}}`,
			"CLAUDE_CODE_USE_VERTEX", "Google Cloud"},
		{"foundry", `{"program_overrides": {"claude": "CLAUDE_CODE_USE_FOUNDRY=1 claude"}}`,
			"CLAUDE_CODE_USE_FOUNDRY", "Azure"},
		// Filed under a DIFFERENT agent key than the value actually launches. The
		// key is not what decides the credential group — the command is — so a
		// guard that keyed off the map key would miss this entirely.
		{"cross-keyed under codex", `{"program_overrides": {"codex": "CLAUDE_CODE_USE_BEDROCK=1 claude"}}`,
			"CLAUDE_CODE_USE_BEDROCK", "AWS"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repoRoot := t.TempDir()
			path := writeInRepoConfig(t, repoRoot, c.override)

			cfg, raw, err := LoadInRepoConfig(repoRoot)
			require.Error(t, err,
				"a checked-in program_overrides value that turns on %s must be REFUSED: honoring it "+
					"grants a cloned repository the operator's %s credentials (#2310)", c.selector, c.provider)
			assert.Nil(t, cfg, "a refused in-repo config must not be returned as usable config")
			assert.Nil(t, raw)

			// The message has to be actionable: name the file, the key, what is
			// at stake, and where the operator may legitimately set it.
			msg := err.Error()
			for _, want := range []string{path, c.selector, c.provider, "program_overrides"} {
				assert.Contains(t, msg, want, "the refusal must say %q", want)
			}
			assert.NotContains(t, msg, "AWS_SECRET_ACCESS_KEY",
				"the refusal names the selector and provider; it must never echo a credential variable")
		})
	}
}

// TestInRepoProgramOverrideStillAllowsOrdinaryCommands pins what the guard must
// NOT cost. Choosing which program runs — a path, flags, a wrapper, even a
// non-agent command — is the documented purpose of this key, and none of it
// widens the environment boundary. A guard that rejected these would break the
// feature to fix the hole.
func TestInRepoProgramOverrideStillAllowsOrdinaryCommands(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	cases := []struct {
		name     string
		override string
	}{
		{"absolute path with flags", `{"program_overrides": {"claude": "/opt/tools/claude --model opus"}}`},
		{"wrapper command", `{"program_overrides": {"claude": "ionice -c 3 claude"}}`},
		{"non-agent command", `{"program_overrides": {"claude": "bash"}}`},
		// An assignment that is NOT a cloud selector is none of this guard's
		// business — it grants no credential group.
		{"unrelated assignment", `{"program_overrides": {"claude": "NO_COLOR=1 claude"}}`},
		// Turning a mode OFF is safety-increasing, never a widening.
		{"selector explicitly disabled", `{"program_overrides": {"claude": "CLAUDE_CODE_USE_BEDROCK=0 claude"}}`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repoRoot := t.TempDir()
			writeInRepoConfig(t, repoRoot, c.override)

			cfg, _, err := LoadInRepoConfig(repoRoot)
			require.NoError(t, err, "this value grants no credential group and must still load")
			require.NotNil(t, cfg)
		})
	}
}

// TestCommandEnablesCloudCredentialsAgreesWithSelectorResolution is the
// anti-drift lock. The guard and the thing it guards are different functions,
// and if they ever disagree the hole reopens silently: a command the guard lets
// through but ResolveAuthSelectors acts on is exactly the bypass.
//
// So assert the implication directly, on both sides, rather than trusting that
// two call sites of the same parser stay in step.
func TestCommandEnablesCloudCredentialsAgreesWithSelectorResolution(t *testing.T) {
	commands := []string{
		"CLAUDE_CODE_USE_BEDROCK=1 claude",
		"env CLAUDE_CODE_USE_BEDROCK=1 claude",
		"CLAUDE_CODE_USE_VERTEX=true claude",
		"CLAUDE_CODE_USE_FOUNDRY=1 claude",
		"CLAUDE_CODE_USE_BEDROCK=0 claude",
		"NO_COLOR=1 claude",
		"/opt/tools/claude --model opus",
		"ionice -c 3 claude",
		"bash",
		"",
	}

	for _, command := range commands {
		// No operator environment at all, so anything ResolveAuthSelectors
		// enables here came from the command itself — which is the untrusted
		// channel the guard covers.
		selectors := sessionenv.ResolveAuthSelectors(nil, sessionenv.AgentForCommand(command), command)
		_, guarded := sessionenv.CommandEnablesCloudCredentials(command)

		if len(selectors) > 0 && !guarded {
			t.Errorf("command %q enables %v on its own but the in-repo guard would ADMIT it — "+
				"a repo could check this in and take the operator's cloud credentials",
				command, selectors)
		}
		if guarded && len(selectors) == 0 {
			t.Errorf("command %q is rejected by the in-repo guard but enables nothing — "+
				"the guard is stricter than the risk and breaks a legitimate override", command)
		}
	}
}

// TestInRepoCloudCredentialRefusalNamesEveryProvider keeps the operator-facing
// message honest as the conditional set grows: a new provider added to
// sessionenv without a name here would be reported as the generic fallback.
func TestInRepoCloudCredentialRefusalNamesEveryProvider(t *testing.T) {
	for _, selector := range []string{
		"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
	} {
		provider := cloudProviderForSelector(selector)
		assert.NotEqual(t, "cloud provider", provider,
			"%s fell through to the generic phrase — name its provider so the refusal says what is at stake",
			selector)
		assert.False(t, strings.Contains(provider, "_"),
			"%s should map to a human provider name, got %q", selector, provider)
	}
}

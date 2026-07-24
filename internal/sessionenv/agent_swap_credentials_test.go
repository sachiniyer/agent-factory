package sessionenv

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// operatorCloudEnvironment is one operator's ambient environment holding cloud
// credentials that have nothing to do with running a coding agent, alongside the
// model-provider keys that do.
func operatorCloudEnvironment() []string {
	return []string{
		"AWS_ACCESS_KEY_ID=fixture",
		"AWS_SECRET_ACCESS_KEY=fixture",
		"AWS_SESSION_TOKEN=fixture",
		"AWS_PROFILE=fixture",
		"AWS_CONFIG_FILE=/home/op/.aws/config",
		"AWS_SHARED_CREDENTIALS_FILE=/home/op/.aws/credentials",
		"AWS_WEB_IDENTITY_TOKEN_FILE=/home/op/.aws/token",
		"AWS_ROLE_ARN=arn:aws:iam::1:role/fixture",
		"GOOGLE_APPLICATION_CREDENTIALS=/home/op/gcp.json",
		"GOOGLE_CLOUD_PROJECT=op-project",
		"GOOGLE_CLOUD_LOCATION=us-central1",
		"OPENAI_API_KEY=fixture",
		"ANTHROPIC_API_KEY=fixture",
		"GEMINI_API_KEY=fixture",
	}
}

// namesOf reduces a filtered environment to its variable names, so an assertion
// can name what crossed without a test ever handling a value.
func namesOf(env []string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		if name, _, ok := strings.Cut(entry, "="); ok {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// TestAgentSwapCannotReachCloudCredentials is #2462.
//
// #2329 stopped a repository from turning ON a cloud-credential SELECTOR. It
// does not stop — and must not stop — a repository from SWAPPING the agent:
// `program_overrides` and `default_program` are both settable in a repo's
// checked-in config, and choosing which program runs is what that key is for.
//
// The swap reached the same outcome by a different door. OpenCode's base
// allowlist carried the operator's whole AWS credential set and Google
// application-default credentials unconditionally, and Gemini's carried
// GOOGLE_APPLICATION_CREDENTIALS, so `claude = "opencode"` or
// `default_program = "gemini"` in a cloned repo moved the session onto an
// allowlist holding secrets the operator never scoped to that repo — with no
// selector anywhere for #2329 to reject.
//
// The fix is per-agent and stated in sessionenv.go: Gemini's group moved behind
// its real selectors; OpenCode's was removed, because it has none.
func TestAgentSwapCannotReachCloudCredentials(t *testing.T) {
	// The names that must never cross on a swap alone. Deliberately the ones an
	// attacker wants — a signing key, a session token, and the ADC file — not the
	// whole group, so a failure names something that matters.
	cloudSecrets := []string{
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
		"AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE",
		"AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN",
		"GOOGLE_APPLICATION_CREDENTIALS",
	}

	// Every agent that can receive per-agent credentials, DERIVED from the real
	// allowlist rather than copied from it. The swap target is the repo's choice,
	// so the guard has to cover whatever the map holds — and supportedAgent keys
	// off exactly this map, so its key set IS the set of nameable swap targets.
	//
	// Deriving is the point. This PR's sibling test learned the same lesson one
	// layer down: TestInRepoCloudCredentialRefusalNamesEveryProvider hardcoded
	// Claude's three selectors, #2462 guarded Gemini's two, and the copy went
	// stale silently — which is why that test now enumerates GuardedSelectors().
	// A hardcoded agent list here fails the same way, and worse: a new agent
	// added with a cloud group would not be uncovered loudly, it would simply
	// never be tested.
	agents := slices.Sorted(maps.Keys(agentNames))
	require.NotEmpty(t, agents, "no agents in the allowlist — this test would pass vacuously")

	for _, agent := range agents {
		t.Run(agent, func(t *testing.T) {
			got := namesOf(FilterForCommand(operatorCloudEnvironment(), agent, agent, nil))
			for _, secret := range cloudSecrets {
				if slices.Contains(got, secret) {
					t.Errorf("a repo that swaps the agent to %q reaches the operator's %s. "+
						"Swapping the program is legitimate; inheriting cloud credentials because of "+
						"it is not — the swapped agent's session runs the untrusted repo's worktree "+
						"(#2462).\nadmitted: %v", agent, secret, got)
				}
			}
		})
	}
}

// TestOpenCodeKeepsItsModelProviderKeys pins what the removal must NOT cost.
// OpenCode is a legitimate agent and swapping to it on purpose has to keep
// working; only the cloud-infrastructure credentials left. If this fails, the
// fix broke the feature it was supposed to preserve.
func TestOpenCodeKeepsItsModelProviderKeys(t *testing.T) {
	source := []string{
		"OPENAI_API_KEY=fixture", "ANTHROPIC_API_KEY=fixture", "GEMINI_API_KEY=fixture",
		"OPENROUTER_API_KEY=fixture", "GROQ_API_KEY=fixture", "MISTRAL_API_KEY=fixture",
		"AZURE_OPENAI_API_KEY=fixture", "OPENCODE_CONFIG=/af/opencode.json",
	}
	got := namesOf(FilterForCommand(source, "opencode", "opencode", nil))
	for _, want := range []string{
		"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY",
		"OPENROUTER_API_KEY", "GROQ_API_KEY", "MISTRAL_API_KEY",
		"AZURE_OPENAI_API_KEY", "OPENCODE_CONFIG",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("opencode lost %s — it is a model-provider key or its own config location, "+
				"which is exactly what opencode is documented to receive", want)
		}
	}
}

// TestAiderKeepsItsModelProviderKeys is the same guard for aider, which #2462
// deliberately did NOT change. Aider carries no AWS and no application-default
// credentials; its Azure entries are Azure OpenAI SERVICE keys, the same class
// as OPENAI_API_KEY and the ten other provider keys it needs to function.
// Treating those as cloud credentials would disable aider by default.
func TestAiderKeepsItsModelProviderKeys(t *testing.T) {
	source := []string{
		"OPENAI_API_KEY=fixture", "ANTHROPIC_API_KEY=fixture",
		"AZURE_OPENAI_API_KEY=fixture", "AZURE_API_KEY=fixture",
		"DEEPSEEK_API_KEY=fixture", "COHERE_API_KEY=fixture",
	}
	got := namesOf(FilterForCommand(source, "aider", "aider", nil))
	for _, want := range []string{
		"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "AZURE_OPENAI_API_KEY",
		"AZURE_API_KEY", "DEEPSEEK_API_KEY", "COHERE_API_KEY",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("aider lost %s; #2462 changes nothing for aider", want)
		}
	}
}

// TestGeminiCloudCredentialsFollowItsSelectors is the operator-legitimate path:
// the group Gemini lost by default must come back the moment the operator turns
// on one of Gemini's own cloud modes. A fix that only removed access would break
// every operator running Gemini against Vertex.
func TestGeminiCloudCredentialsFollowItsSelectors(t *testing.T) {
	cloud := []string{"GOOGLE_APPLICATION_CREDENTIALS", "GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_LOCATION"}

	t.Run("no selector admits none", func(t *testing.T) {
		got := namesOf(FilterForCommand(operatorCloudEnvironment(), "gemini", "gemini", nil))
		for _, name := range cloud {
			if slices.Contains(got, name) {
				t.Errorf("gemini admitted %s with no cloud mode selected", name)
			}
		}
		// The default path still has to work.
		if !slices.Contains(got, "GEMINI_API_KEY") {
			t.Error("gemini lost GEMINI_API_KEY, its default authentication")
		}
	})

	for _, selector := range []string{"GOOGLE_GENAI_USE_VERTEXAI", "GOOGLE_GENAI_USE_GCA"} {
		t.Run("exported "+selector, func(t *testing.T) {
			source := append(operatorCloudEnvironment(), selector+"=true")
			got := namesOf(FilterForCommand(source, "gemini", "gemini", nil))
			for _, name := range cloud {
				if !slices.Contains(got, name) {
					t.Errorf("operator exported %s, so gemini must receive %s — otherwise the fix "+
						"breaks a legitimate Vertex/ADC setup", selector, name)
				}
			}
		})

		// Same selector, named inline on the operator's resolved command, which is
		// the other channel ResolveAuthSelectors honors for Claude.
		t.Run("inline "+selector, func(t *testing.T) {
			command := selector + "=1 gemini"
			got := namesOf(FilterForCommand(operatorCloudEnvironment(), "gemini", command, nil))
			for _, name := range cloud {
				if !slices.Contains(got, name) {
					t.Errorf("an operator's global program_overrides of %q must select the cloud "+
						"group, but %s did not cross", command, name)
				}
			}
		})
	}

	t.Run("a disabled selector admits none", func(t *testing.T) {
		source := append(operatorCloudEnvironment(), "GOOGLE_GENAI_USE_VERTEXAI=0")
		got := namesOf(FilterForCommand(source, "gemini", "gemini", nil))
		for _, name := range cloud {
			if slices.Contains(got, name) {
				t.Errorf("gemini admitted %s with its selector explicitly disabled", name)
			}
		}
	})
}

// TestEveryConditionalAgentHasARealSelector states the rule that decides
// membership in conditionalAgentNames, so a later agent cannot be added with an
// af-invented flag. A selector must be a variable the AGENT itself reads, which
// in practice means it also appears in that agent's base allowlist — af passes
// the operator's setting through to the agent rather than consuming it.
//
// This is the lock on the #2462 judgment call: OpenCode's group was removed
// instead of gated precisely because no such variable exists for it.
func TestEveryConditionalAgentHasARealSelector(t *testing.T) {
	for agent, groups := range conditionalAgentNames {
		base, ok := agentNames[agent]
		if !ok {
			t.Errorf("conditional agent %q has no base allowlist", agent)
			continue
		}
		for _, group := range groups {
			if _, passed := base[group.selector]; !passed {
				t.Errorf("agent %q gates credentials on %q, but that name is not in its base "+
					"allowlist — so af would never pass the operator's setting to the agent, which "+
					"means it is af policy rather than a mode the agent actually reads (#2462)",
					agent, group.selector)
			}
		}
	}
}

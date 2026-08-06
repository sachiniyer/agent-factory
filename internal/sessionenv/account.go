package sessionenv

import (
	"fmt"
	"sort"
	"strings"
)

// Account is a per-session credential scope: exactly one of an agent's logged-in
// identities, held as a directory the agent CLI treats as its credential root.
//
// af never reads, stores, or forwards the secret itself. It decides which
// directory a session can see, and the agent's own login flow put the material
// there. A provider changing its token format costs this nothing.
type Account struct {
	Agent string
	Name  string
	// Dir is the credential root handed to the agent. It must be a real path
	// outside any temp directory — codex refuses to operate under /tmp, and an
	// account is durable state regardless.
	Dir string
}

// accountConfigVars maps an agent to the variable that relocates its credential
// ROOT — verified empirically per agent, never assumed from the allowlist.
//
// claude 2.1.223: CLAUDE_CONFIG_DIR pointed at an empty directory reports
// `"loggedIn": false` while the real one reports true, so it moves credential
// lookup and not merely settings.
//
// codex-cli 0.146.1: CODEX_HOME pointed at an empty directory reports "Not
// logged in" against "Logged in using ChatGPT".
//
// gemini and amp are deliberately ABSENT. GEMINI_CLI_HOME and AMP_HOME are on
// the session-env allowlist, but allowlist membership is not evidence that they
// relocate credentials, and neither CLI was available to test. An agent missing
// here reports unsupported rather than silently accepting an account selection
// that would do nothing (#2983).
var accountConfigVars = map[string]string{
	"claude": "CLAUDE_CONFIG_DIR",
	"codex":  "CODEX_HOME",
}

// accountCredentialNames are the variables that carry an IDENTITY for an agent
// and must therefore be removed from an account-scoped session.
//
// This is the subtraction half, and it is not defence in depth — it is what
// makes the selection real. For both of these CLIs an ambient API key takes
// precedence over the config directory's OAuth state, so a session that selected
// an account while ANTHROPIC_API_KEY passed through would authenticate as
// whoever that key belongs to, silently, while every visible signal said
// otherwise. That is the exact failure #2983 exists to fix.
//
// Routing and mode variables (ANTHROPIC_BASE_URL, the Bedrock/Vertex selectors,
// CODEX_CA_CERTIFICATE) are NOT here on purpose: they are the operator's
// deployment configuration rather than an identity, and dropping them would
// break a legitimate proxied setup without scoping anything. accountScopedNames
// enforces that every name is classified one way or the other, so this
// distinction cannot rot as the allowlists grow.
var accountCredentialNames = map[string]map[string]struct{}{
	"claude": nameSet(
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN",
	),
	"codex": nameSet(
		"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN",
	),
}

// SupportsAccounts reports whether an agent can be account-scoped, and the
// variable used to do it.
func SupportsAccounts(agent string) (string, bool) {
	v, ok := accountConfigVars[agent]
	return v, ok
}

// AccountAgents lists the agents that support account scoping, for help text and
// for an error that can name the alternatives.
func AccountAgents() []string {
	out := make([]string, 0, len(accountConfigVars))
	for agent := range accountConfigVars {
		out = append(out, agent)
	}
	sort.Strings(out)
	return out
}

// ApplyAccount scopes an already-filtered session environment to exactly one
// account: it INJECTS that account's credential root and REMOVES every other
// identity-bearing variable for the agent.
//
// Both halves are required and neither is sufficient.
//
// Injection alone leaves an ambient API key in place, which wins over the
// directory — the selection is then silently ignored. Subtraction alone leaves
// the session with no identity at all.
//
// It must also be an INJECTION and not an allowlist entry. Filter is subtractive
// over the daemon's own environment, so a variable delivered through the
// allowlist carries the daemon's single value to every session: correct for
// AF_DAEMON_TOKEN, which is global by nature, and wrong for an account, which is
// per-session by definition. An allowlist implementation passes a presence check
// and a smoke test while giving every session the same account (#2983).
func ApplyAccount(env []string, account Account) ([]string, error) {
	configVar, ok := SupportsAccounts(account.Agent)
	if !ok {
		return nil, fmt.Errorf(
			"agent %q does not support multiple accounts; supported: %s",
			account.Agent, strings.Join(AccountAgents(), ", "))
	}
	if strings.TrimSpace(account.Dir) == "" {
		return nil, fmt.Errorf("account %q for agent %q has no credential directory",
			account.Name, account.Agent)
	}

	denied := accountScopedNames(account.Agent, configVar)
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		name, _, found := strings.Cut(kv, "=")
		if !found {
			continue
		}
		if _, drop := denied[name]; drop {
			continue
		}
		out = append(out, kv)
	}
	// Appended last so it wins over any ambient copy that survived, though the
	// removal above means there should not be one.
	out = append(out, configVar+"="+account.Dir)
	return out, nil
}

// accountScopedNames is every variable removed from an account-scoped session:
// the agent's credential names, plus the config variable itself so an ambient
// value cannot shadow the injected one.
//
// It reads from accountCredentialNames rather than from agentNames on purpose. A
// blanket "drop everything for this agent" would also strip the operator's
// routing and mode configuration, breaking a proxied or Bedrock deployment to
// scope an identity. The cost of the explicit list is that a newly allowlisted
// credential could be forgotten, which is why TestAccountCredentialsAreClassified
// fails when an agentNames entry for a scoped agent is neither classified as a
// credential nor listed as deliberately kept.
func accountScopedNames(agent, configVar string) map[string]struct{} {
	denied := make(map[string]struct{}, len(accountCredentialNames[agent])+1)
	for name := range accountCredentialNames[agent] {
		denied[name] = struct{}{}
	}
	denied[configVar] = struct{}{}
	return denied
}

// accountNonCredentialNames records the allowlisted names for a scoped agent
// that are deliberately NOT identity-bearing, so the classification test can
// tell "reviewed and kept" from "forgotten".
var accountNonCredentialNames = map[string]map[string]struct{}{
	"claude": nameSet(
		"ANTHROPIC_BASE_URL", "CLAUDE_CONFIG_DIR",
		"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
	),
	"codex": nameSet(
		"CODEX_HOME", "CODEX_SQLITE_HOME", "CODEX_CA_CERTIFICATE",
	),
}

package session

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The credential-mount path (#2194) mounts the operator's credential file
// read-only and must guarantee that NO environment name the operator did not
// forward can bear or route that credential — whatever its source: repo-owned
// docker.run_args, the repo-chosen image's own ENV, or a workspace config file
// the agent loads after launch (#7526). This file is the single classification
// those guarantees share: the run_args validator refuses every classified name,
// runContainer pins every classified name the operator did not forward
// defined-and-empty after run_args, and the provisioning warning names the ones
// the image or run_args had actually set.
//
// Three classes of name are classified per mountable agent, then denied to
// repo run_args and pinned empty for non-operator sources:
//
//   - identity: a credential that substitutes for or outranks the mounted
//     file — API keys and tokens, inline-config-content names, cloud-mode
//     auth selectors, and the conditional cloud-credential groups they unlock.
//   - config-root: a name that relocates where the agent reads its credential
//     or config — CODEX_HOME, CLAUDE_CONFIG_DIR, OPENCODE_CONFIG{,_DIR} — plus
//     the HOME/XDG names af re-asserts to real values rather than blanking.
//   - routing: a name that changes WHERE the agent sends the credential —
//     base URLs and endpoints, proxies, and CA bundles / trust roots.
//
// The shared credential and routing sets apply to every rostered agent: they
// are common-allowlist names (internal/sessionenv/sessionenv.go), so they are
// reachable for every agent, and a GitHub token or a corporate proxy name set
// by an image is a credential or a route exactly as much as a per-agent name.
//
// Every name in a mountable agent's reachable surface — its per-agent
// allowlist, its conditional groups, and the shared common names above — must
// land in exactly one of: denied (classified), kept (deliberately allowed,
// recorded with the reason), or unreachable (absent from the surface). The
// roster-iterating test in backend_docker_credentials_validate_test.go pins
// that completeness, so a newly mountable agent cannot arrive without a
// refused set — the hand-maintained-table failure this replaces (#7526 item 5).

// credentialMountSharedCredentialNames are common-allowlist names that carry
// an identity for tools the agent runs — not the mounted file's own format,
// but names a repo or image can use to substitute an identity the operator
// never forwarded. SSH_AUTH_SOCK/SSH_AGENT_PID point the agent's git+ssh at an
// agent socket the repo mounts alongside them.
var credentialMountSharedCredentialNames = map[string]struct{}{
	"GH_TOKEN":                {},
	"GITHUB_TOKEN":            {},
	"GH_ENTERPRISE_TOKEN":     {},
	"GITHUB_ENTERPRISE_TOKEN": {},
	"SSH_AUTH_SOCK":           {},
	"SSH_AGENT_PID":           {},
}

// credentialMountSharedRoutingNames are common-allowlist names that change
// where any TLS or HTTP client in the container sends requests — the mounted
// credential included. Proxies route traffic; CA bundles and cert files
// decide whom that traffic trusts; GH_CONFIG_DIR relocates the gh config
// directory that carries gh's own credential.
var credentialMountSharedRoutingNames = map[string]struct{}{
	"HTTP_PROXY":          {},
	"HTTPS_PROXY":         {},
	"ALL_PROXY":           {},
	"NO_PROXY":            {},
	"http_proxy":          {},
	"https_proxy":         {},
	"all_proxy":           {},
	"no_proxy":            {},
	"SSL_CERT_FILE":       {},
	"SSL_CERT_DIR":        {},
	"NODE_EXTRA_CA_CERTS": {},
	"REQUESTS_CA_BUNDLE":  {},
	"CURL_CA_BUNDLE":      {},
	"GH_CONFIG_DIR":       {},
}

// credentialMountReassertedNames are config-root names af does not blank but
// re-asserts to the values the mount was placed under (runContainer's
// post-run_args HOME/XDG block). They are still refused to repo run_args —
// the assertion is a pin, and a repo's own value is exactly what it exists to
// dominate — they just produce a corrected value rather than an empty one.
// SHELL joins them: tmux runs every pane command as $default-shell -c … and
// default-shell comes from the client environment, so a repo-chosen SHELL
// would run a program of the image's choosing between af's pins and the
// agent's exec — a program that can re-set every pinned name. af re-asserts
// SHELL to /bin/sh unless the operator forwards their own (#7529 R1).
var credentialMountReassertedNames = map[string]struct{}{
	"HOME":            {},
	"XDG_DATA_HOME":   {},
	"XDG_CONFIG_HOME": {},
	"SHELL":           {},
}

// credentialMountIdentityEnvNames are the credential-bearing per-agent names
// for agents whose account-scope identity classification is empty — the same
// set #7520 introduced as credentialMountDeniedEnvNames's identity half, now
// one class inside the wider classification. For account-supporting agents
// the account classification (AccountIdentityNames + AgentAuthSelectors, plus
// their conditional groups) already names their identities.
var credentialMountIdentityEnvNames = map[string]map[string]struct{}{
	tmux.ProgramOpencode: {
		// Inline config content: a built-in per-agent name opencode merges
		// over the persisted auth.json, overriding both provider endpoint and
		// API key — measured on the installed CLI to send the session's
		// traffic to an attacker endpoint under an attacker key.
		"OPENCODE_CONFIG_CONTENT": {},
		// Ambient provider API keys opencode reads over the persisted
		// credential — the same reason codex refuses OPENAI_API_KEY.
		"OPENAI_API_KEY":       {},
		"ANTHROPIC_API_KEY":    {},
		"GEMINI_API_KEY":       {},
		"GOOGLE_API_KEY":       {},
		"AZURE_OPENAI_API_KEY": {},
		"OPENROUTER_API_KEY":   {},
		"DEEPSEEK_API_KEY":     {},
		"GROQ_API_KEY":         {},
		"MISTRAL_API_KEY":      {},
		"COHERE_API_KEY":       {},
		"XAI_API_KEY":          {},
	},
	tmux.ProgramAmp: {
		// AMP_API_KEY is the documented credential route native to amp — a
		// repo-supplied value beats the mounted secrets.json.
		"AMP_API_KEY": {},
	},
}

// credentialMountConfigRootNames relocate where the agent resolves its
// credential-bearing configuration. The account-supporting agents' roots
// already sit inside AccountIdentityNames via accountScopedNames' configVar;
// this table covers the names that are not that account var — and the
// account-empty agents' whole redirect surface.
var credentialMountConfigRootNames = map[string]map[string]struct{}{
	tmux.ProgramOpencode: {
		// Relocate config lookup to a repo-chosen path; the chosen config can
		// name provider, endpoint and key.
		"OPENCODE_CONFIG":     {},
		"OPENCODE_CONFIG_DIR": {},
	},
	tmux.ProgramCodex: {
		// CODEX_HOME itself is codex's account config var and already denied.
		// CODEX_SQLITE_HOME relocates the state database codex also consults.
		"CODEX_SQLITE_HOME": {},
	},
}

// credentialMountRoutingNames are per-agent names that change WHERE the agent
// sends its credential — endpoints and trust roots. Shared proxies and CA
// names are in credentialMountSharedRoutingNames; these are the names only
// this agent's allowlist admits.
var credentialMountRoutingNames = map[string]map[string]struct{}{
	tmux.ProgramClaude: {
		"ANTHROPIC_BASE_URL": {},
	},
	tmux.ProgramCodex: {
		// A CA bundle decides whom the credential-bearing traffic trusts.
		"CODEX_CA_CERTIFICATE": {},
	},
	tmux.ProgramOpencode: {
		"OPENAI_API_BASE":       {},
		"OPENAI_BASE_URL":       {},
		"AZURE_OPENAI_ENDPOINT": {},
	},
}

// credentialMountKeptNames records, per mountable agent, the allowlisted names
// deliberately left unclassified — reviewed deployment configuration that
// neither bears nor routes the mounted credential. The completeness pin uses
// this to tell "reviewed and kept" from "forgotten", the same job
// accountNonCredentialNames does on the account path.
var credentialMountKeptNames = map[string]map[string]struct{}{
	tmux.ProgramAmp: {
		// AMP_HOME is amp's install directory, not a credential root
		// (internal/sessionenv/account.go records the measurement), so it is
		// deliberately left through.
		"AMP_HOME": {},
	},
	tmux.ProgramOpencode: {
		// AZURE_OPENAI_API_VERSION selects an API version — semantics, not a
		// destination or a credential.
		"AZURE_OPENAI_API_VERSION": {},
	},
}

// credentialMountDeniedNames returns every env name a repo-controlled
// docker.run_args entry may not set on the credential-mount path: the union of
// the identity, config-root, routing, and reasserted classes for the session's
// agent, plus the shared credential and routing sets. The refusal is
// unconditional — it applies even to a name the operator forwards, because
// run_args is appended after af's own -e entries and would otherwise dominate
// the operator's forwarded value by Docker's last-wins rule.
//
// The set is derived, never hand-maintained in full: account-supporting
// agents contribute AccountIdentityNames + AgentAuthSelectors + the
// conditional groups those selectors unlock; every rostered agent contributes
// the shared sets; the per-agent tables above hold what is left. The roster
// pin in the test suite iterates agentCredentialFiles so a newly mountable
// agent lands here only after its reachable surface is classified.
func credentialMountDeniedNames(agent string) map[string]struct{} {
	denied := make(map[string]struct{})
	add := func(names map[string]struct{}) {
		for name := range names {
			denied[name] = struct{}{}
		}
	}
	addList := func(names []string) {
		for _, name := range names {
			denied[name] = struct{}{}
		}
	}
	addList(sessionenv.AccountIdentityNames(agent))
	addList(sessionenv.AgentAuthSelectors(agent))
	addList(sessionenv.AgentConditionalNames(agent))
	add(credentialMountSharedCredentialNames)
	add(credentialMountSharedRoutingNames)
	add(credentialMountReassertedNames)
	add(credentialMountIdentityEnvNames[agent])
	add(credentialMountConfigRootNames[agent])
	add(credentialMountRoutingNames[agent])
	return denied
}

// credentialMountDeniedReason returns, for a refused run_args name, the
// because-clause the refusal prints — worded for the mechanism the name's
// class defends, so "substitute" is only claimed where substitution is the
// mechanism (#7526 item 4).
func credentialMountDeniedReason(agent, name string) (string, bool) {
	if _, ok := credentialMountReassertedNames[name]; ok {
		return "af re-asserts it after run_args for the mounted credential", true
	}
	if _, ok := credentialMountSharedCredentialNames[name]; ok {
		return "it can substitute an identity the operator did not forward", true
	}
	if _, ok := credentialMountSharedRoutingNames[name]; ok {
		return "it changes where the agent sends the mounted credential", true
	}
	if _, ok := credentialMountIdentityEnvNames[agent][name]; ok {
		return "it can substitute the credential af mounts read-only", true
	}
	if _, ok := credentialMountConfigRootNames[agent][name]; ok {
		return "it can redirect where the agent reads the mounted credential", true
	}
	if _, ok := credentialMountRoutingNames[agent][name]; ok {
		return "it changes where the agent sends the mounted credential", true
	}
	if configVar, ok := sessionenv.SupportsAccounts(agent); ok && configVar == name {
		return "it can redirect where the agent reads the mounted credential", true
	}
	for _, account := range [][]string{
		sessionenv.AccountIdentityNames(agent),
		sessionenv.AgentAuthSelectors(agent),
		sessionenv.AgentConditionalNames(agent),
	} {
		for _, denied := range account {
			if denied == name {
				return "it can substitute the credential af mounts read-only", true
			}
		}
	}
	return "", false
}

// credentialMountProxyCasePairs pairs each proxy variable's upper- and
// lower-case spellings. Programs disagree on which spelling wins — and some
// treat a defined-empty sibling as an explicit "no proxy" that cancels the
// other spelling (measured: Python's getproxies and reqwest both let an empty
// lowercase twin override a forwarded uppercase value). Forwarding either
// spelling therefore exempts BOTH from the empty pin; pinning the twin of a
// forwarded name could silently cancel the operator's proxy (#7529 R2).
var credentialMountProxyCasePairs = [][2]string{
	{"HTTP_PROXY", "http_proxy"},
	{"HTTPS_PROXY", "https_proxy"},
	{"ALL_PROXY", "all_proxy"},
	{"NO_PROXY", "no_proxy"},
}

// credentialMountPinnedNames returns the classified names af pins
// defined-and-empty on the credential-mount path: the denied set minus the
// reasserted names (which take real values instead) and minus names the
// operator forwarded in session_env_passthrough (which carry the operator's
// own value, or the image's when the operator named one they do not set —
// either way the operator claimed the name).
func credentialMountPinnedNames(agent string, passthrough []string) []string {
	forwarded := make(map[string]struct{}, len(passthrough))
	for _, name := range passthrough {
		forwarded[name] = struct{}{}
	}
	for _, pair := range credentialMountProxyCasePairs {
		if _, ok := forwarded[pair[0]]; ok {
			forwarded[pair[1]] = struct{}{}
			continue
		}
		if _, ok := forwarded[pair[1]]; ok {
			forwarded[pair[0]] = struct{}{}
		}
	}
	out := make([]string, 0)
	for name := range credentialMountDeniedNames(agent) {
		if _, reasserted := credentialMountReassertedNames[name]; reasserted {
			continue
		}
		if _, ok := forwarded[name]; ok {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// credentialMountBlankedWarning reports once per credential-mount session the
// classified names that were actually set by the image or by docker.run_args
// and that af pinned empty. The values are never inspected or printed — names
// only. An unset variable can always be blanked silently; these are the ones a
// working setup may have depended on, so each gets named with the remedy:
// forward it in the global session_env_passthrough.
//
// Blanked names run_args carried are reported even though the run_args
// validator refuses classified entries — the warning is the migration's
// notice layer and stays honest if a future flag spelling reaches the pin.
func credentialMountBlankedWarning(imageEnv, runArgs []string, pinned []string) []string {
	blanked := make(map[string]struct{}, len(pinned))
	for _, name := range pinned {
		blanked[name] = struct{}{}
	}
	seen := make(map[string]struct{})
	var names []string
	note := func(entry string) {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			name = entry
		}
		if _, blank := blanked[name]; !blank {
			return
		}
		if _, dup := seen[name]; dup {
			return
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	for _, entry := range imageEnv {
		note(entry)
	}
	for _, name := range dockerRunArgsEnvNames(runArgs) {
		note(name)
	}
	sort.Strings(names)
	return names
}

// dockerRunArgsEnvNames returns the variable names docker.run_args assigns via
// -e/--env entries (in every spelling: separate value, =value, and combined
// short clusters). --env-file is not walked — the validator refuses it on this
// path before the warning runs. Names only; values are never read.
func dockerRunArgsEnvNames(args []string) []string {
	var names []string
	checkEnv := func(value string) {
		name, _, _ := strings.Cut(value, "=")
		if name != "" {
			names = append(names, name)
		}
	}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "-e" || arg == "--env":
			if index+1 < len(args) {
				index++
				checkEnv(args[index])
			}
		case strings.HasPrefix(arg, "--env="):
			checkEnv(strings.TrimPrefix(arg, "--env="))
		case strings.HasPrefix(arg, "-e="):
			checkEnv(strings.TrimPrefix(arg, "-e="))
		case strings.HasPrefix(arg, "-e") && len(arg) > 2:
			checkEnv(strings.TrimPrefix(arg, "-e"))
		case len(arg) > 2 && arg[0] == '-' && arg[1] != '-':
			_ = dockerCheckShorthandCluster(arg, args, index, "e",
				func(_ byte, value string) error { checkEnv(value); return nil },
				"", "")
		}
	}
	return names
}

// dockerImageEnvNames runs `docker image inspect` for the session's image and
// returns the names in its Config.Env — for the blanked-name warning only. A
// failure to inspect returns nil: the warning is best-effort, and the mount
// path's own guards do not depend on it.
func (p *dockerProvisioner) dockerImageEnvNames() []string {
	out, err := p.docker(dockerShortStepTimeout,
		"image", "inspect", p.image, "--format", "{{json .Config.Env}}")
	if err != nil {
		return nil
	}
	var env []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &env); err != nil {
		return nil
	}
	names := make([]string, 0, len(env))
	for _, entry := range env {
		if name, _, ok := strings.Cut(entry, "="); ok {
			names = append(names, name)
		}
	}
	return names
}

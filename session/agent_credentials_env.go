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
// Names are classified per mountable agent, then denied to repo run_args and
// pinned empty for non-operator sources, in these classes:
//
//   - identity: a credential that substitutes for or outranks the mounted
//     file — API keys and tokens, inline-config-content names, cloud-mode
//     auth selectors, and the conditional cloud-credential groups they unlock.
//   - config-root: a name that relocates where the agent reads its credential
//     or config — CODEX_HOME, CLAUDE_CONFIG_DIR, OPENCODE_CONFIG{,_DIR} — plus
//     the names af re-asserts to real values rather than blanking (HOME/XDG
//     roots, the path-valued git config roots, the program-selector
//     defaults, SHELL, PATH), and the shared
//     names that relocate credential-bearing state (the GnuPG home, the
//     runtime dir sockets resolve under).
//   - routing: a name that changes WHERE the agent sends the credential —
//     base URLs and endpoints, proxies, and CA bundles / trust roots.
//   - program-choice: a name that selects a program af's tools or the agent
//     run in the container — the askpass and GIT_SSH commands git spawns to
//     answer credential prompts, the editor/pager programs, and the tmux
//     names that select a server af execs into.
//   - late-loaded: a name the agent's own loader can restore from a
//     repository file after af's boundary runs. gemini walks up from the
//     workspace applying <dir>/.gemini/.env and <dir>/.env fill-if-unset —
//     the falsy-safe endpoint names are pinned defined-and-empty like the
//     rest of the denied set, while CODE_ASSIST_ENDPOINT is read with a
//     nullish coalesce that treats empty as a real endpoint, so af re-asserts
//     its first-party default instead. The pins only bite because the names
//     are allowlisted: a name the agent-server's exec filter strips is unset
//     to the loader, so every pinned or re-asserted name must also sit in a
//     set the filter admits.
//
// The shared credential, routing, config-root, program-choice, and af-state
// sets apply to every rostered agent: they are common-allowlist names
// (internal/sessionenv/sessionenv.go), so they are reachable for every
// agent, and a GitHub token or a corporate proxy name set by an image is a
// credential or a route exactly as much as a per-agent name.
//
// Every name in a mountable agent's reachable surface — its per-agent
// allowlist, its conditional groups, the shared common names above, and the
// agent's late-loaded names — must land in exactly one of: denied
// (classified), kept (deliberately allowed, recorded with the reason), or
// unreachable (absent from the surface). The roster-iterating test in
// backend_docker_credentials_validate_test.go pins that completeness, so a
// newly mountable agent or common-allowlist name cannot arrive without a
// classification — the hand-maintained-table failure this replaces
// (#7526 item 5, #7531).

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
	// GPG_AGENT_INFO points gpg and its agents at an agent socket — the same
	// identity-substitution mechanism as SSH_AUTH_SOCK.
	"GPG_AGENT_INFO": {},
	// AF_DAEMON_TOKEN is af's own credential for the daemon API; a repo- or
	// image-supplied value is an identity af would present.
	"AF_DAEMON_TOKEN": {},
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
	// GH_HOST selects the host gh sends API requests — and any forwarded
	// GH_TOKEN — to. A repo-chosen host receives the credential (#7531).
	"GH_HOST": {},
	// DBUS_SESSION_BUS_ADDRESS routes keyring requests to a chosen bus —
	// the secret store several agents keep session material under.
	"DBUS_SESSION_BUS_ADDRESS": {},
	// AF_DAEMON_URL selects the daemon af's own API calls go to; paired with
	// a repo-chosen AF_DAEMON_TOKEN it is a complete route for af's identity.
	"AF_DAEMON_URL": {},
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
// PATH joins them too: a repo-chosen PATH resolves a program of the repo's
// choosing for every PATH-walk af's in-container steps make and for the
// agent's own bare executable, so af re-asserts the image's own Config.Env
// PATH — the image's binaries are the documented residual trust on this
// path — unless the operator forwards their own (#7529 R7).
var credentialMountReassertedNames = map[string]struct{}{
	"HOME":            {},
	"XDG_DATA_HOME":   {},
	"XDG_CONFIG_HOME": {},
	// XDG_CACHE_HOME/XDG_STATE_HOME are the remaining XDG roots: re-asserted
	// to the session home's defaults like their siblings so a repo cannot
	// relocate where the agent's cache and state land (#7531).
	"XDG_CACHE_HOME": {},
	"XDG_STATE_HOME": {},
	// GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM relocate git's config files — a
	// repo-chosen file can name a credential helper, proxy, or trust root.
	// They take REAL values rather than empty pins because a path-valued
	// name must keep the program WORKING, not just neutral: measured on git
	// 2.43, `GIT_CONFIG_GLOBAL=` makes `git config --global` fail writing to
	// an empty path and `/dev/null` fails the write with EACCES, so af
	// re-asserts each to the location git would consult under the
	// boundary's own roots (#7531).
	"GIT_CONFIG_GLOBAL": {},
	"GIT_CONFIG_SYSTEM": {},
	"SHELL":             {},
	"PATH":              {},
}

// credentialMountSharedConfigRootNames are common-allowlist names that
// relocate where a tool reads credential-bearing state and pin empty
// safely: gpg's keyring root and the runtime directory agent/keyring
// sockets are discovered under fall back to their defaults when empty, and
// GIT_CONFIG_NOSYSTEM is a boolean where empty is the same as unset. The
// path-valued git config roots are in credentialMountReassertedNames
// instead — an empty path breaks the program, not just neutralizes it.
var credentialMountSharedConfigRootNames = map[string]struct{}{
	"GIT_CONFIG_NOSYSTEM": {},
	"GNUPGHOME":           {},
	"XDG_RUNTIME_DIR":     {},
}

// credentialMountSharedProgramEntries are common-allowlist names that select
// a PROGRAM af's tools or the agent run inside the container — the same
// class as SHELL/PATH. git spawns the askpass and GIT_SSH commands to answer
// credential prompts, so a repo-chosen value runs a repository binary that
// receives the credential question; EDITOR/VISUAL/PAGER run whenever a tool
// opens one. They take REAL re-asserted values rather than empty pins
// because a program-selector name must keep the program WORKING: measured
// on git 2.43, `GIT_SSH_COMMAND=` is treated as the executable choice and
// every SSH transport fails `cannot run : No such file or directory`, which
// breaks af's own cloneWorkspace for ssh origins (#7531). The values:
//   - GIT_SSH/GIT_SSH_COMMAND re-assert git's own default `ssh`, resolved
//     through the re-asserted image PATH — the image's binaries are the
//     declared residual trust here.
//   - GIT_ASKPASS/SSH_ASKPASS re-assert /bin/false: a working program that
//     never answers — an empty name is not a safe fallback when the pane
//     HAS a tty (tmux) and an unset askpass would prompt on it.
//   - EDITOR/VISUAL re-assert /bin/false so an interactive `git commit`
//     fails clearly instead of silently accepting the template via
//     `sh -c ""`.
//   - PAGER re-asserts /bin/cat: under the pane's tty git auto-pages, and
//     an empty PAGER runs `sh -c ""`, which exits 0 and swallows the
//     output entirely.
var credentialMountSharedProgramEntries = map[string]string{
	"GIT_SSH":         "ssh",
	"GIT_SSH_COMMAND": "ssh",
	"GIT_ASKPASS":     "/bin/false",
	"SSH_ASKPASS":     "/bin/false",
	"EDITOR":          "/bin/false",
	"VISUAL":          "/bin/false",
	"PAGER":           "/bin/cat",
}

// credentialMountSharedAFStateNames are af's own state and control-plane
// names in the common allowlist: af's home directory, its auto-update
// toggle, and the tmux names that select which server af's in-container
// client execs into. A repo- or image-supplied value relocates af's state
// directory or points af's tmux calls at a server the image controls —
// none of which a non-operator source may choose on the path that mounts
// the operator's credential. af's own in-container programs take their
// configuration from argv, not the container environment, so pinning them
// defined-and-empty leaves af unaffected. (af's session markers are
// deliberately NOT here — see credentialMountSessionMarkerNames.)
var credentialMountSharedAFStateNames = map[string]struct{}{
	"AGENT_FACTORY_HOME":        {},
	"AGENT_FACTORY_AUTO_UPDATE": {},
	"TMUX":                      {},
	"TMUX_PANE":                 {},
	"TMUX_TMPDIR":               {},
}

// credentialMountSessionMarkerNames are af's OWN trusted session markers —
// the names sessionEnvFlags (session/tmux/envmarker.go) stamps into every
// pane environment with per-session values: the session name, its
// generation, and af's home provenance. They are refused to repository
// run_args like every classified name — a repo-supplied marker falsifies
// membership — but they are NEVER pinned: a defined-empty pin is carried by
// --session-env-empty and re-asserted at the exec shim AFTER tmux has
// installed af's values, erasing exactly the markers the agent's children
// need. markedOrphanProcesses (session/tmux/reap_membership.go) proves
// ownership by those markers: a child orphaned inside the container can no
// longer be attributed, discovered, or reaped (#7531). af's values come
// from af, not a repo source, and reach the agent through tmux's -e flags —
// the boundary only has to NOT erase them.
var credentialMountSessionMarkerNames = map[string]struct{}{
	"AF_SESSION":     {},
	"AF_SESSION_GEN": {},
	"AF_HOME":        {},
}

// credentialMountProgramLoaderNames are names that choose which CODE the
// programs af runs in the container load — the dynamic linker's preload and
// search-path variables. They reach every docker exec step before the
// agent-side env filter exists, so a repo-supplied value would inject a
// repository library into af's provisioning steps. They are refused to repo
// run_args but NOT pinned: an image's own LD_* values are the same trust
// class as its binaries, and pinning would break images that legitimately
// need them; the inner filter already keeps them off the agent's env, so
// the agent itself is covered either way (#7529 R7).
var credentialMountProgramLoaderNames = map[string]struct{}{
	"LD_PRELOAD":      {},
	"LD_LIBRARY_PATH": {},
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
	tmux.ProgramGemini: {
		// gemini's endpoint names are per-agent allowlisted so the pins
		// reach the exec environment, and late-loaded besides: the CLI
		// walks up from the workspace applying <dir>/.gemini/.env and
		// <dir>/.env fill-if-unset (measured in
		// internal/sessionenv/account.go), so a checked-in .env can name
		// the base URL the session's OAuth traffic goes to unless the
		// name is already defined. Each is read through a truthy/|| check
		// in gemini-cli, so a defined-empty pin falls back to the compiled
		// default while the .env loader treats the name as claimed (#7531).
		// GOOGLE_GEMINI_BASE_URL truthy also flips getAuthTypeFromEnv to
		// GATEWAY — one more reason the pin must be empty, not a
		// first-party value.
		"GOOGLE_GEMINI_BASE_URL":  {},
		"GOOGLE_VERTEX_BASE_URL":  {},
		"CODE_ASSIST_API_VERSION": {},
	},
}

// credentialMountFirstPartyNames are late-loaded endpoint names af cannot
// pin empty because the agent treats an empty value as REAL configuration:
// gemini reads CODE_ASSIST_ENDPOINT with a nullish coalesce
// (process.env[NAME] ?? compiled default), so a defined-empty value becomes
// the endpoint and every Code Assist call fails. Instead af re-asserts the
// name to its first-party default — the same constant the CLI compiles in —
// after run_args, so the .env loader sees a defined name, a repo value is
// dominated by Docker's last-wins rule, and an operator's own value still
// wins through session_env_passthrough (#7531).
var credentialMountFirstPartyNames = map[string]map[string]string{
	tmux.ProgramGemini: {
		// gemini-cli's compiled Code Assist endpoint
		// (packages/core/src/code_assist/server.ts).
		"CODE_ASSIST_ENDPOINT": "https://cloudcode-pa.googleapis.com",
	},
}

// credentialMountSharedKeptNames records the COMMON-allowlist names
// deliberately left unclassified — reviewed names that neither bear nor
// route the mounted credential, for every agent: display and locale
// metadata, terminal capabilities, temp paths, commit authorship, and the
// names whose only effect rides on a denied sibling (GIT_SSH_VARIANT only
// modulates how git parses GIT_SSH_COMMAND, which is denied; GH_REPO scopes
// gh's resource, while its endpoint names are denied). The completeness pin
// walks the common allowlist and uses this to tell "reviewed and kept" from
// "forgotten", the same job credentialMountKeptNames does per agent (#7531).
var credentialMountSharedKeptNames = map[string]struct{}{
	// Identity display and locale — no credential or route.
	"USER": {}, "LOGNAME": {},
	"LANG": {}, "LANGUAGE": {}, "TZ": {},
	// Terminal capability metadata — display, not a credential or route.
	"TERM": {}, "COLORTERM": {}, "TERMINFO": {}, "TERMINFO_DIRS": {},
	"NO_COLOR": {}, "CLICOLOR": {}, "CLICOLOR_FORCE": {}, "FORCE_COLOR": {},
	// Temp-directory hints — a scratch path, not a credential root or route.
	"TMPDIR": {}, "TMP": {}, "TEMP": {},
	// PWD is a working-directory hint set by shells, not a lookup root the
	// credential resolution reads.
	"PWD": {},
	// pinentry's tty hint — display, not a socket or credential.
	"GPG_TTY": {},
	// Commit authorship metadata — attribution, not authentication or
	// routing.
	"GIT_AUTHOR_NAME": {}, "GIT_AUTHOR_EMAIL": {},
	"GIT_COMMITTER_NAME": {}, "GIT_COMMITTER_EMAIL": {},
	// Selects the repository gh operates on — a resource scope, not the
	// endpoint the token is sent to (GH_HOST is denied).
	"GH_REPO": {},
	// Only modulates how git parses GIT_SSH_COMMAND, which is denied —
	// alone it chooses no program.
	"GIT_SSH_VARIANT": {},
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
	add(credentialMountSharedConfigRootNames)
	for name := range credentialMountSharedProgramEntries {
		denied[name] = struct{}{}
	}
	add(credentialMountSharedAFStateNames)
	add(credentialMountSessionMarkerNames)
	add(credentialMountReassertedNames)
	add(credentialMountProgramLoaderNames)
	add(credentialMountIdentityEnvNames[agent])
	add(credentialMountConfigRootNames[agent])
	add(credentialMountRoutingNames[agent])
	for name := range credentialMountFirstPartyNames[agent] {
		denied[name] = struct{}{}
	}
	return denied
}

// credentialMountDeniedReason returns, for a refused run_args name, the
// because-clause the refusal prints — worded for the mechanism the name's
// class defends, so "substitute" is only claimed where substitution is the
// mechanism (#7526 item 4).
func credentialMountDeniedReason(agent, name string) (string, bool) {
	if name == "PATH" {
		return "it chooses which program every PATH-resolved command in the container runs — af's provisioning steps and the agent's own executable — and af re-asserts the image's own PATH after run_args", true
	}
	if _, ok := credentialMountReassertedNames[name]; ok {
		return "af re-asserts it after run_args for the mounted credential", true
	}
	if _, ok := credentialMountProgramLoaderNames[name]; ok {
		return "it loads a repository-chosen library into the programs af runs in the container before the agent-side env filter applies", true
	}
	if _, ok := credentialMountSharedCredentialNames[name]; ok {
		return "it can substitute an identity the operator did not forward", true
	}
	if _, ok := credentialMountSharedRoutingNames[name]; ok {
		return "it changes where the agent sends the mounted credential", true
	}
	if _, ok := credentialMountSharedConfigRootNames[name]; ok {
		return "it can redirect where the agent reads credential-bearing state", true
	}
	if _, ok := credentialMountSharedProgramEntries[name]; ok {
		return "it chooses which program af's tools or the agent run in the container, and af re-asserts a working default after run_args", true
	}
	if _, ok := credentialMountSharedAFStateNames[name]; ok {
		return "it relocates af's own state or falsifies af's session and tool selection", true
	}
	if _, ok := credentialMountSessionMarkerNames[name]; ok {
		return "it falsifies af's session membership — af re-stamps the real marker at the pane boundary", true
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
	if _, ok := credentialMountFirstPartyNames[agent][name]; ok {
		return "af re-asserts the agent's first-party endpoint after run_args so the agent's .env loader sees a defined name", true
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

// credentialMountProxyTwinForwarding normalizes a proxy pair so the
// unforwarded spelling cannot keep an image-supplied value: every spelling is
// in the inner filter's always-admitted set, so exempting a twin from the pin
// without reseating it would leave the image's proxy reachable by clients that
// prefer that spelling (Python's getproxies prefers a set lowercase name over
// an uppercase one). For each pair the operator claimed through
// session_env_passthrough, the first forwarded-and-host-set spelling supplies
// the value V; every other spelling of the pair is emitted `-e NAME` and the
// docker client environment synthesizes `NAME=V`, so both spellings resolve to
// the operator's value. A pair member that is itself forwarded AND set keeps
// its own approved value. When no forwarded spelling is set on the host the
// pair contributes nothing — the "claimed a name they do not set" semantics
// already lets the image's values stand for names the operator forwarded empty.
//
// names is the extra `-e NAME` set for docker run; synth is `NAME=V` entries
// appended to the docker client's filtered environment, which is where a bare
// `-e NAME` resolves its value — values therefore never appear in argv.
func credentialMountProxyTwinForwarding(passthrough []string, environ []string) (names, synth []string) {
	forwarded := make(map[string]struct{}, len(passthrough))
	for _, name := range passthrough {
		forwarded[name] = struct{}{}
	}
	host := make(map[string]string, len(environ))
	for _, entry := range environ {
		if name, value, ok := strings.Cut(entry, "="); ok {
			host[name] = value
		}
	}
	for _, pair := range credentialMountProxyCasePairs {
		var value string
		claimed := false
		for _, s := range pair {
			if _, ok := forwarded[s]; !ok {
				continue
			}
			if v, set := host[s]; set {
				value, claimed = v, true
				break
			}
		}
		if !claimed {
			continue
		}
		for _, s := range pair {
			_, fwd := forwarded[s]
			_, set := host[s]
			if fwd && set {
				continue
			}
			names = append(names, s)
			synth = append(synth, s+"="+value)
		}
	}
	return names, synth
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
		// The loader names are refused to run_args but never pinned: an
		// image's own LD_* values are the same trust class as its binaries,
		// and the inner env filter keeps them off the agent either way.
		if _, loader := credentialMountProgramLoaderNames[name]; loader {
			continue
		}
		// The first-party names take their real default value, not an empty
		// pin: the agents that read them treat defined-empty as a real
		// endpoint rather than falling back. The shared program selectors
		// re-assert a working default for the same reason — defined-empty
		// names the empty string as the program and breaks the tool.
		if _, firstParty := credentialMountFirstPartyNames[agent][name]; firstParty {
			continue
		}
		if _, program := credentialMountSharedProgramEntries[name]; program {
			continue
		}
		// af's session markers are af's own values, stamped at the pane
		// boundary — an empty pin would erase them at the exec shim after
		// tmux installed them (#7531).
		if _, marker := credentialMountSessionMarkerNames[name]; marker {
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

// credentialMountReassertEntries returns the sorted NAME=value re-asserts
// runContainer emits after run_args: the agent's first-party endpoint
// names plus the shared program-selector defaults — the names af cannot
// pin empty because a defined empty value is itself a real (broken)
// configuration. A name the operator forwards is skipped: their forwarded
// -e NAME already carries their approved value.
func credentialMountReassertEntries(agent string, passthrough []string) []string {
	forwarded := make(map[string]struct{}, len(passthrough))
	for _, name := range passthrough {
		forwarded[name] = struct{}{}
	}
	values := make(map[string]string, len(credentialMountSharedProgramEntries)+len(credentialMountFirstPartyNames[agent]))
	for name, value := range credentialMountSharedProgramEntries {
		values[name] = value
	}
	for name, value := range credentialMountFirstPartyNames[agent] {
		values[name] = value
	}
	out := make([]string, 0, len(values))
	for name, value := range values {
		if _, ok := forwarded[name]; ok {
			continue
		}
		out = append(out, name+"="+value)
	}
	sort.Strings(out)
	return out
}

// dockerContainerImageEnv reads the Config.Env of the image the container
// ACTUALLY runs: the image reference resolved off the created container,
// then inspected — not whatever p.image's tag inspected as before docker
// run, which may have pulled a newer image or never existed locally (#7531).
// The bool reports whether the environment could be read; the blanked-name
// warning degrades to run_args names and says so when it could not.
func (p *dockerProvisioner) dockerContainerImageEnv() ([]string, bool) {
	ref, err := p.docker(dockerShortStepTimeout,
		"inspect", p.containerID, "--format", "{{.Image}}")
	if err != nil {
		return nil, false
	}
	image := strings.TrimSpace(string(ref))
	if image == "" {
		return nil, false
	}
	out, err := p.docker(dockerShortStepTimeout,
		"image", "inspect", image, "--format", "{{json .Config.Env}}")
	if err != nil {
		return nil, false
	}
	var env []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &env); err != nil {
		return nil, false
	}
	return env, true
}

// dockerImageEnv runs `docker image inspect` for the session's CONFIGURED
// image and returns its Config.Env as NAME=value entries — the source for
// the PATH re-assertion, which must be computed before docker run fixes the
// argv. A failure to inspect returns nil: it is a best-effort layer on top
// of the run_args refusal, and the mount path's own guards do not depend on
// it. The blanked-name warning deliberately does NOT use this: it reads the
// image the container actually runs through dockerContainerImageEnv, because
// a pre-run tag inspect can resolve to a different (or no) image than the
// one docker run pulls (#7531).
func (p *dockerProvisioner) dockerImageEnv() []string {
	out, err := p.docker(dockerShortStepTimeout,
		"image", "inspect", p.image, "--format", "{{json .Config.Env}}")
	if err != nil {
		return nil
	}
	var env []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &env); err != nil {
		return nil
	}
	return env
}

// credentialMountImagePATH extracts PATH's value from an image's Config.Env
// entries — the value af re-asserts after run_args on the credential-mount
// path. Returns "" when the image configures no PATH; runContainer then emits
// nothing, because with run_args PATH refused the container inherits the
// image's own PATH (or the runtime's built-in default) anyway — the same
// trusted source, never a repo-chosen one (#7529 R7).
func credentialMountImagePATH(imageEnv []string) string {
	path := ""
	for _, entry := range imageEnv {
		if name, value, ok := strings.Cut(entry, "="); ok && name == "PATH" {
			path = value
		}
	}
	return path
}

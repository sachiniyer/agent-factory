package session

// Split from agent_credentials_env.go to keep that file under the
// production length limit (#1145): the names an agent's own dotenv loader
// can fill, the shared git environment-config names the same fill channel
// can set, and the refusal's because-clause for every classified name.

import (
	"slices"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// credentialMountLoaderFilledNames are per-agent names the agent's own
// dotenv loader fills when they are unset — gemini walks up from the
// workspace applying <dir>/.gemini/.env and <dir>/.env fill-if-unset, so a
// checked-in file can claim any name af does not define. Each is pinned
// defined-and-empty: the loader treats the name as claimed and skips it,
// while the agent's own read — measured on @google/gemini-cli 0.42.0 —
// falls back to its safe default on empty. A class counts as pinned only
// when EVERY name the read consults is pinned — a chain
// A || B || default leaves A free for the .env to claim if only B is
// held (#7544).
var credentialMountLoaderFilledNames = map[string]map[string]struct{}{
	// The gemini set is DERIVED from the measured bundle artifact
	// (internal/sessionenv/gemini_bundle_env.go, @google/gemini-cli
	// 0.42.0): every entry whose account class is keep-if-set — the
	// auth-source selectors, the metadata-endpoint pair, the gRPC proxy
	// and trust-root names the #7659 review found (R1), the project-id
	// chain twins (R2), and the rest of the .env-fillable surface (#7662).
	// Completeness lives in that artifact and its tests, not here.
	tmux.ProgramGemini: geminiBundleLoaderFilled(),
}

// credentialMountSharedGitEnvNames inject git configuration into every git
// run in the container through the environment. Any agent's dotenv loader
// can fill them when unset — and a repo image ENV or run_args can set them
// directly regardless of a loader — so the class is shared across agents.
// Both are pinned defined-and-empty by measurement: git reads
// GIT_CONFIG_COUNT through strtoul, where "" yields 0 with no error —
// count 0 makes every indexed GIT_CONFIG_KEY_*/GIT_CONFIG_VALUE_* pair
// inert, so pinning the count kills the whole unbounded family — and an
// empty GIT_CONFIG_PARAMETERS parses to zero command-line -c pairs.
// The remaining family member is GIT_CONFIG, which an empty pin would
// break: git treats defined-empty as a real (empty) config file path and
// `git config` fails, so it is documented rather than pinned (#7544).
var credentialMountSharedGitEnvNames = map[string]struct{}{
	"GIT_CONFIG_COUNT":      {},
	"GIT_CONFIG_PARAMETERS": {},
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
	if _, ok := credentialMountPaneBoundaryNames[name]; ok {
		return "it falsifies af's session membership or pane identity — the real value is stamped at the pane boundary", true
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
	if _, ok := credentialMountLoaderFilledNames[agent][name]; ok {
		return "the agent's own .env loader fills it when unset, and af pins it defined-and-empty", true
	}
	if _, ok := credentialMountSharedGitEnvNames[name]; ok {
		return "it injects git configuration through the environment for every git run in the container, and af pins it defined-and-empty", true
	}
	if strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_") {
		return "it supplies the indexed pairs a forwarded GIT_CONFIG_COUNT would activate — the family stays af's or the operator's own through session_env_passthrough, never the repository's", true
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

// accountLoaderBlankEntries returns the `-e NAME=` entries a docker account
// session emits for loader-filled names the operator forwarded but did not
// set: -e admits the name through the wrap's passthrough extras, and an
// image ENV value that survived would reach ScopeAgentServerAccount reading
// exactly like the operator's claim (#7555). The indexed pair names get the
// same treatment because a forwarded GIT_CONFIG_COUNT animates whatever
// GIT_CONFIG_KEY_*/GIT_CONFIG_VALUE_* the image declared (#7582 review).
// Every other forwarded name keeps master's contract: unset in the daemon
// env means no -e at all, so the image's own value stands — blanking those
// would eat legitimate image-supplied values like locale settings.
//
// On gemini the proxy spellings are loader-filled keep-if-set names now
// (#7665), so a forwarded-but-unset HTTPS_PROXY does emit the blanking -e
// and the image's proxy cannot reach the credential-bearing transport —
// with one exception: a forwarded-but-unset proxy spelling whose case twin
// is forwarded AND host-set reseats to the twin's value instead, because a
// `NAME=` here reaches applyAccount indistinguishable from an explicit
// empty and a lowercase-preferring reader would then disable the
// operator's proxy (Codex P2 on a46bdbf91). A genuinely explicit empty —
// the operator exported `NAME=` themselves — is "set" and keeps the
// ordinary contract above. An unforwarded sibling's value is never af's to
// copy.
func accountLoaderBlankEntries(agent string, passthrough, source []string) []string {
	values := make(map[string]string, len(source))
	for _, entry := range source {
		if name, v, ok := strings.Cut(entry, "="); ok {
			values[name] = v
		}
	}
	loaderNames := make(map[string]struct{})
	for _, name := range sessionenv.AccountLoaderFilledNames(agent) {
		loaderNames[name] = struct{}{}
	}
	var out []string
	for _, name := range passthrough {
		if _, set := values[name]; set {
			continue
		}
		_, loader := loaderNames[name]
		pair := len(loaderNames) > 0 &&
			(strings.HasPrefix(name, "GIT_CONFIG_KEY_") ||
				strings.HasPrefix(name, "GIT_CONFIG_VALUE_"))
		if !loader && !pair {
			continue
		}
		if sib, twin := sessionenv.ProxyEnvCaseTwin(name); twin &&
			slices.Contains(passthrough, sib) {
			if v, set := values[sib]; set {
				out = append(out, name+"="+v)
				continue
			}
		}
		out = append(out, name+"=")
	}
	// A forwarded GIT_CONFIG_COUNT that resolves to N>0 in the daemon
	// environment animates GIT_CONFIG_KEY_0..N-1 / GIT_CONFIG_VALUE_0..N-1.
	// When the operator forwarded the count without the pairs it requires —
	// including a pair that would arrive defined-but-empty for a KEY, which
	// git fails with "empty config key" exactly as it fails a missing one —
	// the bare -e GIT_CONFIG_COUNT above rides the daemon's value into the
	// container while no usable pair travels with it, so every git
	// invocation fails fatally ("missing config key GIT_CONFIG_KEY_0"). Pin
	// the count empty here — the blank is emitted after the bare forward,
	// so docker's last-wins -e resolves to empty, rendering the whole
	// family inert the way an unforwarded count would.
	if missing, total := sessionenv.GitConfigCountForwardMissingPairs(agent, passthrough, source); total > 0 {
		log.WarningLog.Printf("backend=docker: session_env_passthrough forwards GIT_CONFIG_COUNT without its required indexed pairs (%s); git would fail the count without them, so af pinned the count empty — also forward the pair names through session_env_passthrough",
			sessionenv.FormatGitConfigMissingPairs(missing, total))
		out = append(out, "GIT_CONFIG_COUNT=")
	}
	return out
}

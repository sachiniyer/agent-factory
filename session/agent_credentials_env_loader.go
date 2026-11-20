package session

// Split from agent_credentials_env.go to keep that file under the
// production length limit (#1145): the names an agent's own dotenv loader
// can fill, the shared git environment-config names the same fill channel
// can set, and the refusal's because-clause for every classified name.

import (
	"strings"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
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
	tmux.ProgramGemini: {
		// getAuthTypeFromEnv is CLOUD_SHELL === "true" ||
		// GEMINI_CLI_USE_COMPUTE_ADC === "true" — the auth source is
		// pinned only when both selectors are; a defined-empty value fails
		// each compare and the default source stands.
		"GEMINI_CLI_USE_COMPUTE_ADC": {},
		"CLOUD_SHELL":                {},
		// The metadata host is GCE_METADATA_IP || GCE_METADATA_HOST ||
		// HOST_ADDRESS — pinning only the second link leaves IP free for
		// the .env to retarget the endpoint the ADC probe and token fetch
		// dial; empty is falsy on either env link and falls back to
		// HOST_ADDRESS, which is gcp-metadata's compiled constant
		// 169.254.169.254 — a module constant, not an env read, so there is
		// no third link for a .env to claim and nothing more to pin.
		"GCE_METADATA_HOST": {},
		"GCE_METADATA_IP":   {},
		// DEBUG_AUTH, DETECT_GCP_RETRIES, and METADATA_SERVER_DETECTION are
		// read by the bundled gcp-metadata / google-auth-library transports
		// inside @google/gemini-cli 0.42.0, not gemini-cli's own source —
		// so a source-tree measurement of the CLI misses them. None routes
		// the credential, but each is dotenv-fillable and af left it
		// unclaimed, so a .env setting DEBUG_AUTH would log the session's
		// OAuth material. A defined-empty pin claims each for af (the .env
		// loader finds it defined) and is inert: debug is off on "", and
		// the gcp-metadata reads fall to their compiled defaults rather
		// than dialing a repo-chosen host. An operator's forwarded value
		// still survives (keep-if-set). GOOGLE_CLOUD_UNIVERSE_DOMAIN — the
		// sibling bundled-transport read that DOES route the credential and
		// reads through a nullish ?? — is re-asserted to its first-party
		// default in credentialMountFirstPartyNames, not pinned empty here.
		"DEBUG_AUTH":                {},
		"DETECT_GCP_RETRIES":        {},
		"METADATA_SERVER_DETECTION": {},
	},
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

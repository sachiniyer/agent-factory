package session

// Split from agent_credentials_env.go to keep that file under the
// production length limit (#1145): the names an agent's own dotenv loader
// can fill, plus the shared git environment-config names the same fill
// channel can set.

import "github.com/sachiniyer/agent-factory/session/tmux"

// credentialMountLoaderFilledNames are per-agent names the agent's own
// dotenv loader fills when they are unset — gemini walks up from the
// workspace applying <dir>/.gemini/.env and <dir>/.env fill-if-unset, so a
// checked-in file can claim any name af does not define. Each is pinned
// defined-and-empty: the loader treats the name as claimed and skips it,
// while the agent's own read — measured on @google/gemini-cli 0.42.0 —
// falls back to its safe default on empty (#7544).
var credentialMountLoaderFilledNames = map[string]map[string]struct{}{
	tmux.ProgramGemini: {
		// Selects compute-ADC as the auth source when === "true": a
		// defined-empty value fails the compare and the default source
		// stands, so the pin cannot move the session's identity.
		"GEMINI_CLI_USE_COMPUTE_ADC": {},
		// Read as env || the compiled metadata.google.internal: empty is
		// falsy and falls back to the real metadata endpoint, while a
		// filled value retargets the endpoint the ADC probe and token
		// fetch use.
		"GCE_METADATA_HOST": {},
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

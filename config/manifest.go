package config

import (
	"sort"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The config manifest: the single source of truth describing the union of every
// user-facing configuration key — its shape, supported locations and formats,
// precedence and merge policy, and editor-facing description.
//
// It exists to be READ BY AN AGENT. A config agent gets RenderBriefing(cfg) as
// its briefing document (manifest_briefing.go), so every Purpose here is written
// for someone who has never seen this codebase: one plain-language line, no
// internal vocabulary, no issue numbers. The struct's own doc comments stay the
// contributor-facing reference — they are multi-line prose about rationale and
// history, which is the wrong shape for a briefing.
//
// WHY A CURATED TABLE, not struct tags plus codegen: Go does not expose doc
// comments at runtime, so deriving this from config_types.go would need a
// build-time go/ast generator, and it would still emit the multi-line prose a
// briefing cannot use. The anti-drift guarantee that generation would buy is
// bought instead by TestManifestCoversEveryConfigKey, which reflects over both
// Config and InRepoConfig and fails when a decoded field has no correctly
// sourced entry here (and when an entry names no field). Adding a config key or
// admitting it at another source without touching this file is therefore a test
// failure, not a silent omission — the same guarantee, a fraction of the
// machinery.
//
// Two further locks keep the table honest: TestManifestAgreesWithSettableKeys
// pins Settable against the real `af config set` allowlist (settableKeySpecs),
// and TestManifestDefaultsMatchDefaultConfig pins Default against
// DefaultConfig() for every key whose default is deterministic.

// ConfigTier ranks a config key by how likely a user is to need it, so an agent
// briefing (and any future `af config` surface) can lead with what matters
// instead of listing every key flat.
type ConfigTier int

const (
	// TierCore is the onboarding core: the handful of keys a new user is
	// likely to want to set, and the ones a config agent should offer first.
	TierCore ConfigTier = 1
	// TierCommon is the occasional-but-normal tier: real preferences a user
	// reaches for deliberately rather than on day one.
	TierCommon ConfigTier = 2
	// TierAdvanced is everything else user-facing: tuning knobs, opt-in
	// behaviors, and structural tables that are correct by default and rarely
	// touched.
	TierAdvanced ConfigTier = 3
)

// ManifestEntry describes one user-facing config key across every location in
// which that key is supported. A key shared by Config and InRepoConfig has one
// entry with multiple Sources, never one entry per struct.
type ManifestEntry struct {
	// Key is the toml key as written in config.toml (e.g. "default_program").
	// It must match a decoded field in at least one supported schema —
	// TestManifestCoversEveryConfigKey rejects a key that names no field.
	Key string
	// Type is the value's shape: "string", "bool", "int", "table", or "list".
	Type string
	// Default is the default value rendered for a human. For a key whose default
	// is deterministic, TestManifestDefaultsMatchDefaultConfig pins this against
	// DefaultConfig(); the rest (a username-derived prefix, a detected binary
	// path) are described rather than quoted.
	Default string
	// Purpose is ONE plain-language line for a non-expert. It is briefing copy:
	// sentence case, no CAPS for emphasis, "…" not "...", " · " joining
	// fragments (the repo copy conventions in CLAUDE.md).
	Purpose string
	// Tier ranks the key for ordering and for what an agent surfaces first.
	Tier ConfigTier
	// Settable reports whether the current global `af config set` accepts this
	// key — for a dynamic family (program_overrides, limit_patterns) that means
	// its leaves, e.g.
	// `af config set program_overrides.claude …`. It is pinned against the real
	// allowlist by TestManifestAgreesWithSettableKeys, so it can never become a
	// claim the CLI does not honor. A false here means the key must be changed
	// through another writer or by hand in an allowed config location.
	Settable bool
	// Enum is the allowed values, when they are enumerated. Nil when the value
	// is free-form (a path, a duration, an address) or a plain bool.
	Enum []string
	// Sources is the set of supported user-facing locations that may declare
	// this key. Built-in and read-only compatibility candidates belong only in
	// Precedence. Zero is invalid.
	Sources SourceSet
	// Precedence lists this key's resolver candidates from lowest to highest.
	// It always starts with SourceBuiltIn and may include a compatibility source
	// that is deliberately not an allowed write target. Zero/empty is invalid.
	Precedence []ConfigSource
	// Merge says how present values from successive sources combine. Zero is
	// invalid, including for keys that currently have only one configured source.
	Merge MergePolicy
	// Formats lists the encodings recognized across this key's supported
	// Sources. Zero is invalid.
	Formats FormatSet
}

// manifestSkippedKeys are the toml-tagged Config fields deliberately absent
// from the manifest. Every entry carries a reason: this is the ONLY escape
// hatch from the reflective coverage check, so an unexplained addition is how
// drift starts. Unexported fields (keyOverrides) are skipped structurally by
// the test, not listed here — they carry no toml tag and are not config keys at
// all, just the validated in-memory form of one.
//
// This mirrors configEntriesInternalKeys in commands/configcmd_test.go, which
// makes the same schema_version exclusion for the same reason on the read path.
var manifestSkippedKeys = map[string]string{
	// Written by config_save.go and configset.go and read by the migration
	// machinery — never by a user. There is nothing a user can usefully do with
	// the number, and briefing an agent on it would invite setting a field that
	// must only ever be moved by a migration.
	"schema_version": "machine-managed migration bookkeeping, never user-set",
}

// configManifest is the curated union table, in tier order (and, within a tier,
// in the order a user meets the keys). AllManifest returns the whole table;
// Manifest preserves its historical global-only view for the config agent and
// editors. Nothing outside the manifest implementation should touch this slice
// directly.
var configManifest = []ManifestEntry{
	// ---- Tier 1: the onboarding core ----
	{
		Key:        "default_program",
		Type:       "string",
		Default:    "claude",
		Purpose:    "The coding agent a new session starts.",
		Tier:       TierCore,
		Settable:   true,
		Enum:       tmux.SupportedPrograms,
		Sources:    sourceGlobalRepo,
		Precedence: precedenceGlobalRepo,
		Merge:      MergeReplace,
		Formats:    formatTOMLJSON,
	},
	{
		Key:        "listen_addr",
		Type:       "string",
		Default:    "127.0.0.1:8443",
		Purpose:    "Address the browser interface and HTTP API are served on · set it to \"\" to turn the web server off entirely.",
		Tier:       TierCore,
		Settable:   true,
		Sources:    sourceGlobalOnly,
		Precedence: precedenceGlobal,
		Merge:      MergeReplace,
		Formats:    formatTOMLJSON,
	},
	{
		Key:        "require_token",
		Type:       "bool",
		Default:    "false",
		Purpose:    "Require an access token from other machines on the network · off by default, so the browser interface opens with no login.",
		Tier:       TierCore,
		Settable:   true,
		Sources:    sourceGlobalOnly,
		Precedence: precedenceGlobal,
		Merge:      MergeReplace,
		Formats:    formatTOMLJSON,
	},
	{
		Key:        "require_loopback_token",
		Type:       "bool",
		Default:    "false",
		Purpose:    "Also require the token from browsers on this same machine · has no effect on its own, since it only tightens a token that require_token must first turn on.",
		Tier:       TierCore,
		Settable:   true,
		Sources:    sourceGlobalOnly,
		Precedence: precedenceGlobal,
		Merge:      MergeReplace,
		Formats:    formatTOMLJSON,
	},
	{
		Key:        "update_channel",
		Type:       "string",
		Default:    "stable",
		Purpose:    "Which releases updates come from · stable, or preview for early builds cut every few hours.",
		Tier:       TierCore,
		Settable:   true,
		Enum:       []string{UpdateChannelStable, UpdateChannelPreview},
		Sources:    sourceGlobalOnly,
		Precedence: precedenceGlobal,
		Merge:      MergeReplace,
		Formats:    formatTOMLJSON,
	},
	{
		Key:        "auto_update",
		Type:       "bool",
		Default:    "true",
		Purpose:    "Check for a newer version on launch and install it automatically.",
		Tier:       TierCore,
		Settable:   true,
		Sources:    sourceGlobalOnly,
		Precedence: precedenceGlobal,
		Merge:      MergeReplace,
		Formats:    formatTOMLJSON,
	},

	// ---- Tier 2 ----
	{
		Key:        "theme",
		Type:       "table",
		Default:    "the Zenburn palette",
		Purpose:    "Colors the terminal interface uses · one #RRGGBB value per slot, and any slot you leave out keeps its default.",
		Tier:       TierCommon,
		Settable:   false,
		Sources:    sourceGlobalOnly,
		Precedence: precedenceGlobal,
		Merge:      MergeTableByField,
		Formats:    formatTOMLOnly,
	},
	{
		Key:        "vscode_server_binary",
		Type:       "string",
		Default:    `""`,
		Purpose:    "Which editor program a VS Code tab runs · empty finds code-server or openvscode-server on your PATH.",
		Tier:       TierCommon,
		Settable:   true,
		Sources:    sourceGlobalOnly,
		Precedence: precedenceGlobal,
		Merge:      MergeReplace,
		Formats:    formatTOMLJSON,
	},

	// ---- Tier 3: everything else user-facing ----
	{
		Key:        "daemon_poll_interval",
		Type:       "int",
		Default:    "1000",
		Purpose:    "How often the background service checks sessions for new output, in milliseconds.",
		Tier:       TierAdvanced,
		Settable:   true,
		Sources:    sourceGlobalOnly,
		Precedence: precedenceGlobal,
		Merge:      MergeReplace,
		Formats:    formatTOMLJSON,
	},
	{
		Key:        "log_max_size_mb",
		Type:       "int",
		Default:    "50",
		Purpose:    "How large the log may grow, in MB, before it is rotated into a backup file.",
		Tier:       TierAdvanced,
		Settable:   true,
		Sources:    sourceGlobalOnly,
		Precedence: precedenceGlobal,
		Merge:      MergeReplace,
		Formats:    formatTOMLJSON,
	},
	{
		Key:        "log_max_backups",
		Type:       "int",
		Default:    "2",
		Purpose:    "How many rotated log files to keep before the oldest is deleted · 0 keeps none.",
		Tier:       TierAdvanced,
		Settable:   true,
		Sources:    sourceGlobalOnly,
		Precedence: precedenceGlobal,
		Merge:      MergeReplace,
		Formats:    formatTOMLJSON,
	},
	{
		Key:        "branch_prefix",
		Type:       "string",
		Default:    "your username, followed by a slash",
		Purpose:    "Prefix for the git branch each new session creates.",
		Tier:       TierAdvanced,
		Settable:   true,
		Sources:    sourceGlobalOnly,
		Precedence: precedenceGlobal,
		Merge:      MergeReplace,
		Formats:    formatTOMLJSON,
	},
	{
		Key:        "worktree_root",
		Type:       "string",
		Default:    WorktreeRootSibling,
		Purpose:    "Where a session's copy of the repository is created · sibling puts it next to the repository, subdirectory puts it under the agent-factory home.",
		Tier:       TierAdvanced,
		Settable:   true,
		Enum:       []string{WorktreeRootSubdirectory, WorktreeRootSibling},
		Sources:    sourceGlobalOnly,
		Precedence: precedenceGlobal,
		Merge:      MergeReplace,
		Formats:    formatTOMLJSON,
	},
	{
		Key:        "detach_keys",
		Type:       "string",
		Default:    "ctrl-w",
		Purpose:    "The key that leaves a session you are attached to and returns you to the session list.",
		Tier:       TierAdvanced,
		Settable:   true,
		Sources:    sourceGlobalOnly,
		Precedence: precedenceGlobal,
		Merge:      MergeReplace,
		Formats:    formatTOMLJSON,
	},
	{
		Key:        "program_overrides",
		Type:       "table",
		Default:    "claude, pointed at the claude command found when af first ran",
		Purpose:    "The full command to run for an agent, when it needs a specific path or extra flags · one entry per agent.",
		Tier:       TierAdvanced,
		Settable:   true,
		Enum:       tmux.SupportedPrograms,
		Sources:    sourceGlobalRepo,
		Precedence: precedenceGlobalRepo,
		Merge:      MergeMapByKey,
		Formats:    formatTOMLJSON,
	},
	{
		Key:        "session_env_passthrough",
		Type:       "list",
		Default:    "none",
		Purpose:    "Extra environment variable names an agent session may inherit · exact names only, values stay out of config, and each name explicitly trusts a repo-selected Docker image.",
		Tier:       TierAdvanced,
		Settable:   false,
		Sources:    sourceGlobalOnly,
		Precedence: precedenceGlobal,
		Merge:      MergeReplace,
		Formats:    formatTOMLJSON,
	},
	{
		Key:        "limit_patterns",
		Type:       "table",
		Default:    "none · the built-in patterns are used",
		Purpose:    "A custom regular expression for recognizing an agent's usage-limit message, replacing the built-in one · one entry per agent.",
		Tier:       TierAdvanced,
		Settable:   true,
		Enum:       tmux.SupportedPrograms,
		Sources:    sourceGlobalOnly,
		Precedence: precedenceGlobal,
		Merge:      MergeMapByKey,
		Formats:    formatTOMLJSON,
	},
	{
		Key:        "limit_auto_resume",
		Type:       "bool",
		Default:    "false",
		Purpose:    "Let a session that hit its usage limit resume on its own once the limit resets.",
		Tier:       TierAdvanced,
		Settable:   true,
		Sources:    sourceGlobalOnly,
		Precedence: precedenceGlobal,
		Merge:      MergeReplace,
		Formats:    formatTOMLJSON,
	},
	{
		Key:        "global_agent_skills",
		Type:       "bool",
		Default:    "false",
		Purpose:    "Let af add its af-usage skill to your own codex/gemini/amp config folders · off means those agents are not told about af.",
		Tier:       TierAdvanced,
		Settable:   true,
		Sources:    sourceGlobalOnly,
		Precedence: precedenceGlobal,
		Merge:      MergeReplace,
		Formats:    formatTOMLJSON,
	},
	{
		Key:        "limit_retry_interval",
		Type:       "string",
		Default:    "30m",
		Purpose:    "How long to wait before retrying a usage-limited session whose message gave no reset time · empty or 0 means never retry it.",
		Tier:       TierAdvanced,
		Settable:   true,
		Sources:    sourceGlobalOnly,
		Precedence: precedenceGlobal,
		Merge:      MergeReplace,
		Formats:    formatTOMLJSON,
	},
	{
		Key:        "cors_allowed_origins",
		Type:       "list",
		Default:    "none",
		Purpose:    "Other websites allowed to call this machine's API from a browser · empty blocks every one of them.",
		Tier:       TierAdvanced,
		Settable:   false,
		Sources:    sourceGlobalOnly,
		Precedence: precedenceGlobal,
		Merge:      MergeListReplace,
		Formats:    formatTOMLJSON,
	},
	{
		Key:        "root_agents",
		Type:       "table",
		Default:    "none",
		Purpose:    "Repositories that always keep a session named root running · one entry per repository path.",
		Tier:       TierAdvanced,
		Settable:   false,
		Sources:    sourceGlobalOnly,
		Precedence: precedenceGlobal,
		Merge:      MergeMapByKey,
		Formats:    formatTOMLJSON,
	},
	{
		Key:        "docker_env_trusted_images",
		Type:       "list",
		Default:    "none",
		Purpose:    "Immutable container image digests allowed to receive values from the daemon's environment · empty keeps every Docker image outside that credential boundary.",
		Tier:       TierAdvanced,
		Settable:   false,
		Sources:    sourceGlobalOnly,
		Precedence: precedenceGlobal,
		Merge:      MergeListReplace,
		Formats:    formatTOMLOnly,
	},
	{
		Key:        "keys",
		Type:       "table",
		Default:    "none · the built-in bindings are used",
		Purpose:    "Which key triggers each action in the terminal interface · one entry per action, replacing that action's default.",
		Tier:       TierAdvanced,
		Settable:   false,
		Sources:    sourceGlobalOnly,
		Precedence: precedenceGlobal,
		Merge:      MergeMapByKey,
		Formats:    formatTOMLOnly,
	},
	{
		Key:        "backend",
		Type:       "string",
		Default:    BackendLocal,
		Purpose:    "The runtime used for this repository's sessions.",
		Tier:       TierAdvanced,
		Settable:   false,
		Enum:       SupportedBackends,
		Sources:    sourceRepoOnly,
		Precedence: precedenceRepo,
		Merge:      MergeReplace,
		Formats:    formatTOMLJSON,
	},
	{
		Key:        "docker",
		Type:       "table",
		Default:    "none",
		Purpose:    "Container image and run arguments for repositories that use the docker backend.",
		Tier:       TierAdvanced,
		Settable:   false,
		Sources:    sourceRepoOnly,
		Precedence: precedenceRepo,
		Merge:      MergeTableByField,
		Formats:    formatTOMLJSON,
	},
	{
		Key:        "post_worktree_commands",
		Type:       "list",
		Default:    "none",
		Purpose:    "Commands run after af creates a worktree for this repository.",
		Tier:       TierAdvanced,
		Settable:   false,
		Sources:    sourceRepoOnly,
		Precedence: precedenceLegacyRepo,
		Merge:      MergeListReplace,
		Formats:    formatTOMLJSON,
	},
	{
		Key:        "remote_hooks",
		Type:       "table",
		Default:    "none",
		Purpose:    "Commands that provision and tear down this repository's hook-backed remote workspace.",
		Tier:       TierAdvanced,
		Settable:   false,
		Sources:    sourceRepoOnly,
		Precedence: precedenceLegacyRepo,
		Merge:      MergeReplace,
		Formats:    formatTOMLJSON,
	},
	{
		Key:        "ssh",
		Type:       "table",
		Default:    "none",
		Purpose:    "Connection details for repositories that run sessions through the SSH backend.",
		Tier:       TierAdvanced,
		Settable:   false,
		Sources:    sourceRepoOnly,
		Precedence: precedenceRepo,
		Merge:      MergeTableByField,
		Formats:    formatTOMLJSON,
	},
}

// Manifest returns every user-facing GLOBAL config key in tier order. It keeps
// the historical global-only contract used by the config agent and editors;
// callers that need the schema union use AllManifest.
//
// The result is DEEP-copied, including Enum and Precedence slices. A shallow
// copy would let an ordinary consumer sort corrupt the process-wide agent list
// or the manifest's resolution policy.
func Manifest() []ManifestEntry {
	return manifestForSource(SourceGlobal)
}

// AllManifest returns the union of user-facing keys across every supported
// config schema, in tier order. Like Manifest, its result is deep-copied.
func AllManifest() []ManifestEntry {
	return cloneManifest(configManifest)
}

func manifestForSource(source ConfigSource) []ManifestEntry {
	entries := make([]ManifestEntry, 0, len(configManifest))
	for _, entry := range configManifest {
		if entry.Sources.Has(source) {
			entries = append(entries, entry)
		}
	}
	return cloneManifest(entries)
}

func cloneManifest(entries []ManifestEntry) []ManifestEntry {
	out := make([]ManifestEntry, len(entries))
	copy(out, entries)
	for i := range out {
		if out[i].Enum != nil {
			enum := make([]string, len(out[i].Enum))
			copy(enum, out[i].Enum)
			out[i].Enum = enum
		}
		if out[i].Precedence != nil {
			precedence := make([]ConfigSource, len(out[i].Precedence))
			copy(precedence, out[i].Precedence)
			out[i].Precedence = precedence
		}
	}
	return out
}

// manifestKeysForSource is the sorted key projection used by source-specific
// decoders. Sorting here keeps errors deterministic even though table order is
// presentation order rather than alphabetical order.
func manifestKeysForSource(source ConfigSource) []string {
	var keys []string
	for _, entry := range configManifest {
		if entry.Sources.Has(source) {
			keys = append(keys, entry.Key)
		}
	}
	sort.Strings(keys)
	return keys
}

func manifestGlobalOnlyKeySet() map[string]bool {
	keys := make(map[string]bool)
	for _, entry := range configManifest {
		if entry.Sources.Has(SourceGlobal) && !entry.Sources.Has(SourceRepoShared) {
			keys[entry.Key] = true
		}
	}
	return keys
}

func manifestTOMLOnlyGlobalKeySet() map[string]bool {
	keys := make(map[string]bool)
	for _, entry := range configManifest {
		if entry.Sources.Has(SourceGlobal) && !entry.Formats.Has(FormatJSON) {
			keys[entry.Key] = true
		}
	}
	return keys
}

// ManifestTiers is the tier order a briefing (and any future `af config`
// surface) walks.
var ManifestTiers = []ConfigTier{TierCore, TierCommon, TierAdvanced}

// TierName is the short one-word label for a tier ("core", "common",
// "advanced"), for a UI section heading and the config editor's wire payload
// (ConfigEntry.TierName).
//
// It is deliberately NOT the briefing's tierHeading ("Core settings"), which is
// full-sentence prose for an agent to read. An editor wants a compact section
// label; the two surfaces legitimately want different words for the same tier,
// which is why this is its own accessor rather than a reuse.
func TierName(t ConfigTier) string {
	switch t {
	case TierCore:
		return "core"
	case TierCommon:
		return "common"
	case TierAdvanced:
		return "advanced"
	default:
		return "other"
	}
}

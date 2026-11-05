package config

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// This file holds the schema half of `af config set`: which keys are settable,
// the value kind each accepts, and the per-leaf writer contract. configset.go
// holds the write machinery itself — see its file comment for the three
// properties the surgical edit guarantees.

// cfgValueKind is the type a settable key accepts.
type cfgValueKind int

const (
	cfgString cfgValueKind = iota
	cfgInt
	// cfgDuration accepts the preferred Go duration spelling and the legacy
	// integer-millisecond spelling used by daemon_poll_interval.
	cfgDuration
	cfgBool
	// cfgStringList is a []string key written as a single-line TOML array. The CLI
	// value is comma-separated and REPLACES the whole list (the MergeListReplace
	// semantic the manifest already declares) — an empty value clears the key. It
	// is generic: any []string key whose elements cannot contain a comma can adopt
	// it by adding a spec entry (see cors_allowed_origins). A list whose elements
	// MAY contain commas (post_worktree_commands, shell commands) cannot, and stays
	// hand-edited.
	cfgStringList
	// cfgFloat is a fractional scalar — today the [web] terminal font sizes
	// (#5500), whose 6.5 phone pitch no integer kind can carry. It is written as a
	// real TOML float (8 → "8.0") so the file stays unambiguous about the type.
	cfgFloat
)

// settableKeySpec describes one settable key (or one dynamic family such as
// program_overrides.<name>).
type settableKeySpec struct {
	kind cfgValueKind
	// section is the TOML table the key lives under ("" = the root, pre-section
	// block).
	section string
	// dynamic marks a family whose leaf is user-supplied (program_overrides.<x>,
	// limit_patterns.<x>); the registry key is then the section/prefix name.
	dynamic bool
	// structured admits the bare key as one whole compact-JSON value. A dynamic
	// family may support both forms: program_overrides accepts a whole JSON map,
	// while program_overrides.<agent> keeps its convenient scalar leaf writer.
	structured bool
	// subkeys admits a fixed set of scalar leaves below a structured table. It is
	// distinct from dynamic: root_agent.enabled and root_agent.program are real
	// schema fields, while an arbitrary root_agent.<name> must still be rejected.
	subkeys map[string]settableLeafSpec
	// parent is populated only on a resolved fixed subkey. It carries the
	// manifest/scoping key through the shared scalar writer without pretending
	// the parent is a dynamic family.
	parent string
	// validate runs the loader's own validation on the parsed value before the
	// write, returning the loader's error verbatim where possible. leaf is the
	// sub-key for a dynamic family (the program name), else the key itself.
	validate func(leaf, value string) error
}

// settableLeafSpec is the typed writer contract for one fixed leaf of a
// structured table.
type settableLeafSpec struct {
	kind     cfgValueKind
	validate func(leaf, value string) error
}

// settableKeySpecs is the global writer registry. TestManifestAgreesWithSettableKeys
// pins it bidirectionally to Manifest(), so a new pane row without a real writer
// is a test failure rather than a read-only row. Repo-only manifest entries are
// written through their repository config surface and never enter the global panes.
var settableKeySpecs = map[string]settableKeySpec{
	"default_program": {kind: cfgString, validate: func(_, v string) error {
		return ValidateProgramEnum("default_program", "default_program", v, "")
	}},
	"appearance":  {kind: cfgString, validate: func(_, value string) error { return ValidateAppearance(value) }},
	"auto_update": {kind: cfgBool},
	// The staged-artifact escape hatch (#3864). A plain bool: it only widens what
	// an upgrade may set aside when it cannot read the owning home, and there is
	// nothing in the value for a validator to catch.
	"upgrade_clear_unverifiable_artifacts": {kind: cfgBool},
	// The opt-in profiling endpoint (#3651). A plain bool: the daemon decides where
	// it is served (unix socket only) and there is nothing here for a value to get
	// wrong, so no validator.
	"debug_pprof":                    {kind: cfgBool},
	"network.require_token":          {kind: cfgBool, section: "network"},
	"network.require_loopback_token": {kind: cfgBool, section: "network"},
	// The #5137 risk-acceptance valve. A plain bool — every guard for it lives
	// in the resulting-posture check (listenerWriteRefusal), which refuses the
	// write when the edit would leave a tokenless non-loopback bind without it.
	"network.allow_unauthenticated_network": {kind: cfgBool, section: "network"},
	"network.listen_addr": {kind: cfgString, section: "network", validate: func(_, v string) error {
		return validateListenAddrValue(v)
	}},
	// The preview origin's bind address (#1856). Same address grammar as
	// listen_addr, so the identical validator — an empty value disables the
	// second listener, any host:port binds it.
	"network.preview_listen_addr": {kind: cfgString, section: "network", validate: func(_, v string) error { return validateListenAddrValue(v) }},
	"network.editor_origin_ports": {kind: cfgString, section: "network", validate: validateEditorOriginPorts},
	// A []string written as a single-line TOML array, comma-separated on the CLI
	// (empty clears it). Each element must be a well-formed browser origin
	// (scheme://host[:port]); the writer rejects a malformed one eagerly so a typo
	// cannot become an entry that then silently never matches a request Origin. The
	// loader stays lenient on a hand-edit — validation lives here at the typed-input
	// boundary, the same asymmetry #2562 and #2565 preserved. There is no default
	// origin, so no fallback value to keep valid.
	"network.cors_allowed_origins": {kind: cfgStringList, section: "network", validate: func(_, v string) error {
		for _, origin := range splitListValue(v) {
			if err := validateCORSOrigin(origin); err != nil {
				return err
			}
		}
		return nil
	}},
	"network.allowed_hosts": {kind: cfgStringList, section: "network", validate: validateAllowedHostsValue},
	// Empty is meaningful (detect code-server/openvscode-server on PATH) and any
	// non-empty value is a path the daemon resolves — including a "~" it expands
	// and a binary that may not exist yet — so there is nothing to validate here
	// that would not reject a legitimate value. The executability check belongs
	// where the binary is actually run, and already lives there.
	"vscode_server_binary":           {kind: cfgString},
	"limit_auto_resume":              {kind: cfgBool},
	"limit_account_candidates":       {kind: cfgStringList, validate: validateLimitAccountCandidatesValue},
	"global_agent_skills":            {kind: cfgBool},
	"docker.mount_agent_credentials": {kind: cfgBool, section: "docker"},
	"ssh.host_key_verification": {kind: cfgString, section: "ssh", validate: func(_, v string) error {
		if !IsValidSSHHostKeyVerification(v) {
			return fmt.Errorf("ssh.host_key_verification must be one of [%s, %s, %s], got %q",
				SSHHostKeyStrict, SSHHostKeyAcceptNew, SSHHostKeyInsecure, v)
		}
		return nil
	}},
	// Free-form: any ssh invocation the operator already uses. Its usability is
	// checked at create time (BackendConfigError), like ssh.host — not here.
	"sandbox.ssh": {kind: cfgString, section: "sandbox"},
	// Daemon-local path streamed to remote hosts; a bad file surfaces at create.
	"sandbox.binary": {kind: cfgString, section: "sandbox"},
	"limit_retry_interval": {kind: cfgString, validate: func(_, v string) error {
		return validateLimitRetryIntervalValue(v)
	}},
	"daemon_poll_interval":      {kind: cfgDuration, validate: func(_, v string) error { return validateDaemonPollIntervalValue(v) }},
	"watcher_events_per_minute": {kind: cfgInt, validate: func(_, v string) error { return requirePositiveInt("watcher_events_per_minute", v) }},
	"log_max_size_mb":           {kind: cfgInt, validate: func(_, v string) error { return requirePositiveInt("log_max_size_mb", v) }},
	"log_max_backups":           {kind: cfgInt, validate: func(_, v string) error { return requireNonNegativeInt("log_max_backups", v) }},
	"branch_prefix":             {kind: cfgString},
	"on_archive_command":        {kind: cfgString},
	"move_cmd":                  {kind: cfgString},
	"projects_root":             {kind: cfgString, validate: validateProjectsRoot},
	"worktree_root": {kind: cfgString, validate: func(_, v string) error {
		if !validateWorktreeRootValue(v) {
			return fmt.Errorf("worktree_root must be one of [%s, %s], got %q", WorktreeRootSubdirectory, WorktreeRootSibling, v)
		}
		return nil
	}},
	"detach_keys": {kind: cfgString, validate: func(_, v string) error {
		if _, err := ParseDetachKey(v); err != nil {
			return fmt.Errorf("detach_keys: %w", err)
		}
		return nil
	}},
	"update_channel": {kind: cfgString, validate: func(_, v string) error {
		if v != UpdateChannelStable && v != UpdateChannelPreview {
			return fmt.Errorf("update_channel must be one of [%s, %s], got %q", UpdateChannelStable, UpdateChannelPreview, v)
		}
		return nil
	}},
	"program_overrides": {kind: cfgString, section: "program_overrides", dynamic: true, structured: true, validate: func(leaf, v string) error {
		return ValidateProgramEnum("program_overrides key", "program_overrides key", leaf, v)
	}},
	// The project-scoped credential identity (#3386). A dynamic family keyed by
	// AGENT, so `af config set default_accounts.codex work --project <path>` is the
	// gesture, and the whole map is still writable as compact JSON by the config
	// panes. The validator is the loader's own, so a shape a hand-edit would be
	// rejected for is rejected at the command that took it — existence of the
	// account is checked at create time instead, where a missing one matters.
	"default_accounts": {kind: cfgString, section: "default_accounts", dynamic: true, structured: true, validate: func(leaf, v string) error {
		return ValidateDefaultAccountEntry("default_accounts key", leaf, v)
	}},
	"limit_patterns": {kind: cfgString, section: "limit_patterns", dynamic: true, structured: true, validate: func(leaf, v string) error {
		if err := ValidateProgramEnum("limit_patterns key", "limit_patterns key", leaf, v); err != nil {
			return err
		}
		if _, err := regexp.Compile(v); err != nil {
			return fmt.Errorf("limit_patterns.%s is not a valid regular expression: %w", leaf, err)
		}
		return nil
	}},
	// The named host list (#5571 W6.1): a whole compact-JSON table of
	// name → {host,user,port,identity_file,workspace_root}, and the default's name.
	// A default is only meaningful against the list, so the cross-check (a default
	// naming a listed host) runs where both are known — the loader and the whole-
	// table writer; the leaf writer checks the name's shape.
	"hosts": {structured: true},
	"default_host": {kind: cfgString, validate: func(_, v string) error {
		return validateOptionalHostName(v)
	}},
	"session_env_passthrough": {structured: true},
	"sandbox_agent_config":    {structured: true},
	"root_agents":             {structured: true},
	"root_agent": {structured: true, subkeys: map[string]settableLeafSpec{
		"enabled": {kind: cfgBool},
		"program": {kind: cfgString},
	}},
	// The browser client's display preferences (#5500). The leaf form —
	// `af config set web.terminal_font_size_phone 8` — is what the web terminal's
	// +/- controls write through SetConfigValue, and the compact-JSON whole-table
	// form is what the config panes post back.
	"web": {structured: true, subkeys: map[string]settableLeafSpec{
		"terminal_font_size_phone":   {kind: cfgFloat, validate: validateWebTerminalFontSize},
		"terminal_font_size_desktop": {kind: cfgFloat, validate: validateWebTerminalFontSize},
	}},
	"keys": {structured: true},
	// The staged [account_handoff] table (#6776) — parser/schema only, nothing
	// consumes the values yet. Every leaf is settable so the staged surface is
	// complete, and each leaf validator is the loader's own, so a write cannot
	// produce a table the next load would refuse.
	"account_handoff": {structured: true, subkeys: map[string]settableLeafSpec{
		"mode":                 {kind: cfgString, validate: validateAccountHandoffLeaf},
		"include_pinned":       {kind: cfgBool},
		"target_order":         {kind: cfgString, validate: validateAccountHandoffLeaf},
		"confirm_wall":         {kind: cfgBool},
		"wall_percent":         {kind: cfgInt, validate: validateAccountHandoffLeaf},
		"move_idle_at_percent": {kind: cfgInt, validate: validateAccountHandoffLeaf},
		"prefer_below_percent": {kind: cfgInt, validate: validateAccountHandoffLeaf},
		"probe_ttl":            {kind: cfgString, validate: validateAccountHandoffLeaf},
		"default_login":        {kind: cfgString, validate: validateAccountHandoffLeaf},
		"require_carry":        {kind: cfgBool},
		"exclude_sessions":     {kind: cfgStringList, validate: validateAccountHandoffLeaf},
		"max_moves":            {kind: cfgInt, validate: validateAccountHandoffLeaf},
		"max_moves_window":     {kind: cfgString, validate: validateAccountHandoffLeaf},
		"refused_backoff":      {kind: cfgString, validate: validateAccountHandoffLeaf},
		"on_failure":           {kind: cfgString, validate: validateAccountHandoffLeaf},
		"report_to":            {kind: cfgString},
	}},
}

func requirePositiveInt(name, v string) error {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return fmt.Errorf("%s must be an integer, got %q", name, v)
	}
	if n <= 0 {
		return fmt.Errorf("%s must be a positive integer, got %d", name, n)
	}
	return nil
}

func requireNonNegativeInt(name, v string) error {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return fmt.Errorf("%s must be an integer, got %q", name, v)
	}
	if n < 0 {
		return fmt.Errorf("%s must be a non-negative integer, got %d", name, n)
	}
	return nil
}

// splitListValue parses a comma-separated CLI list value into its trimmed,
// non-empty elements. Whitespace around a comma and a trailing comma are
// tolerated (an empty element is dropped, not an error), so "a, b," and "a,b"
// both yield [a b]. An empty or whitespace-only input yields no elements, which
// the caller reads as "clear the list".
func splitListValue(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// validateCORSOrigin rejects anything that is not a well-formed browser origin: a
// bare scheme://host[:port] with no path, query, fragment, userinfo, opaque data,
// or trailing slash. originAllowed (daemon/httpauth.go) matches the request Origin
// EXACTLY, so a value carrying a path or a trailing slash parses fine yet can
// never match a real browser Origin — a silent dead entry. Catching it here, at
// `af config set`, makes that an immediate, actionable error; the loader stays
// lenient for a hand-edit.
func validateCORSOrigin(origin string) error {
	shape := fmt.Errorf("network.cors_allowed_origins entry %q is not a valid browser origin; use scheme://host[:port] with no path or trailing slash (e.g. https://af.example.com)", origin)
	u, err := url.Parse(origin)
	if err != nil {
		return shape
	}
	if u.Scheme == "" || u.Host == "" || u.User != nil ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return shape
	}
	// The canonical scheme://host serialization must reproduce the input exactly,
	// which rejects a trailing slash or anything url.Parse would otherwise tolerate.
	if u.Scheme+"://"+u.Host != origin {
		return shape
	}
	return nil
}

// isCommaListKey reports whether key is settable as a comma-separated list
// (its spec kind is cfgStringList). It is the SINGLE per-key opt-in for the comma
// syntax: the writer (canonicalizeScalar) comma-splits input for exactly these
// keys, and the editor/get renderer (CurrentValue) comma-joins exactly these keys.
// Both gate on this one property so they cannot drift, and a []string key that has
// NOT opted in is never comma-split or comma-joined by its type alone — which
// would silently mangle a value whose elements can contain a comma. A dynamic
// family is never a comma list (its leaves are scalars).
func isCommaListKey(key string) bool {
	key = canonicalConfigKey(key)
	spec, ok := settableKeySpecs[key]
	return ok && !spec.dynamic && spec.kind == cfgStringList
}

// SettableKeys returns the sorted, human-facing list of keys `config set`
// accepts; dynamic families are rendered as prefix.<name>.
func SettableKeys() []string {
	out := make([]string, 0, len(settableKeySpecs)+4)
	for k, s := range settableKeySpecs {
		if !s.dynamic || s.structured {
			out = append(out, k)
		}
		if s.dynamic {
			out = append(out, k+".<name>")
		}
		for leaf := range s.subkeys {
			out = append(out, k+"."+leaf)
		}
	}
	sort.Strings(out)
	return out
}

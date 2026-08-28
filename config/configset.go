package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// This file implements `af config set` (#1192). It writes one config key
// into the global config.toml with three deliberate properties:
//
//  1. Comment/ordering-preserving: it does a SURGICAL in-place edit of the file
//     text (see setTOMLScalar) rather than re-marshaling the Config struct.
//     toml.Marshal regenerates the file and would strip the comments, blank
//     lines, and key ordering of a file the README tells users to hand-edit —
//     an external-user footgun. Only the target value's bytes change.
//  2. Manifest-complete: every global manifest key is settable. Scalars retain
//     their native CLI syntax; tables and non-comma lists use compact JSON, the
//     exact form CurrentValue returns to both config panes (#3345).
//  3. Validated with the loader's own rules BEFORE writing: the same
//     ValidateProgramEnum / enum / range checks the loader applies, plus a final
//     parseConfigTOML gate on the edited bytes, so `config set` can never write
//     a config that then fails to load.
//
// config.toml is user-hand-editable state read by the daemon and TUI at
// startup (not daemon-exclusively-owned like instances.json), so the write is a
// file write guarded by WithFileLock (mirroring the pre-daemon tasks path) — not
// a daemon RPC. Changes apply exactly as a hand-edit does: on the next af /
// daemon start (SetResult.RequiresRestart is always true).

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
	// validate runs the loader's own validation on the parsed value before the
	// write, returning the loader's error verbatim where possible. leaf is the
	// sub-key for a dynamic family (the program name), else the key itself.
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
	"auto_update":                    {kind: cfgBool},
	"network.require_token":          {kind: cfgBool, section: "network"},
	"network.require_loopback_token": {kind: cfgBool, section: "network"},
	"network.listen_addr": {kind: cfgString, section: "network", validate: func(_, v string) error {
		return validateListenAddrValue(v)
	}},
	// The preview origin's bind address (#1856). Same address grammar as
	// listen_addr, so the identical validator — an empty value disables the
	// second listener, any host:port binds it.
	"network.preview_listen_addr": {kind: cfgString, section: "network", validate: func(_, v string) error {
		return validateListenAddrValue(v)
	}},
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
	// Empty is meaningful (detect code-server/openvscode-server on PATH) and any
	// non-empty value is a path the daemon resolves — including a "~" it expands
	// and a binary that may not exist yet — so there is nothing to validate here
	// that would not reject a legitimate value. The executability check belongs
	// where the binary is actually run, and already lives there.
	"vscode_server_binary":           {kind: cfgString},
	"limit_auto_resume":              {kind: cfgBool},
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
	"limit_retry_interval": {kind: cfgString, validate: func(_, v string) error {
		return validateLimitRetryIntervalValue(v)
	}},
	"daemon_poll_interval": {kind: cfgDuration, validate: func(_, v string) error { return validateDaemonPollIntervalValue(v) }},
	"log_max_size_mb":      {kind: cfgInt, validate: func(_, v string) error { return requirePositiveInt("log_max_size_mb", v) }},
	"log_max_backups":      {kind: cfgInt, validate: func(_, v string) error { return requireNonNegativeInt("log_max_backups", v) }},
	"branch_prefix":        {kind: cfgString},
	"on_archive_command":   {kind: cfgString},
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
	"limit_patterns": {kind: cfgString, section: "limit_patterns", dynamic: true, structured: true, validate: func(leaf, v string) error {
		if err := ValidateProgramEnum("limit_patterns key", "limit_patterns key", leaf, v); err != nil {
			return err
		}
		if _, err := regexp.Compile(v); err != nil {
			return fmt.Errorf("limit_patterns.%s is not a valid regular expression: %w", leaf, err)
		}
		return nil
	}},
	"theme":                   {structured: true},
	"session_env_passthrough": {structured: true},
	"root_agents":             {structured: true},
	"root_agent":              {structured: true},
	"keys":                    {structured: true},
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
	out := make([]string, 0, len(settableKeySpecs)+2)
	for k, s := range settableKeySpecs {
		if !s.dynamic || s.structured {
			out = append(out, k)
		}
		if s.dynamic {
			out = append(out, k+".<name>")
		}
	}
	sort.Strings(out)
	return out
}

// SetResult reports a successful `config set`.
type SetResult struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	Path  string `json:"path"`
	// RequiresRestart is always true: config.toml is read at startup, so a
	// change applies to af and the daemon on their next start, exactly like a
	// hand-edit.
	RequiresRestart bool `json:"requires_restart"`
	// Warnings are non-fatal notes about what the write actually means, printed
	// after the echo. The write SUCCEEDED — a warning never blocks or changes the
	// value. Today the only one is the tokenless-network-listener exposure
	// (exposureWarning).
	Warnings []string `json:"warnings,omitempty"`
}

// exposureWarning returns a warning when cfg — the config that RESULTS from
// this set, parsed from the bytes about to be written — serves an
// unauthenticated control plane to the network, i.e. the combination of a
// non-loopback listen_addr and require_token = false.
//
// cfg is the OUTCOME, not the starting point: it already carries the value
// being written, so nothing is spliced in here. That is deliberate (#2412) —
// splicing meant taking the other half of the pairing from a config loaded
// before the file lock, which two racing writers could each read stale. See
// scalarWrite.apply.
//
// It exists because this is now easy to do by accident. Before `listen_addr`
// became settable, exposing the listener took a deliberate hand-edit; now it is
// one command that exits 0 and prints nothing. The daemon already knows this
// pairing is dangerous and warns about exactly it (daemon/httpserver.go) — but
// only into the log, at the next daemon start, which is neither where nor when
// the user is looking. This says it at the moment they type it.
//
// It WARNS and nothing more: it does not refuse (that would break scripting) and
// does not auto-set require_token (silently changing a key the user did not name
// is worse than the surprise it prevents). The user stays in control; they just
// stop being surprised.
//
// Warning is now the ONLY response anywhere: #2090 briefly made the daemon
// refuse to start on this pairing, and #2168 Phase 0 reversed that by owner
// decision ("assume users are safe and will do the right thing"). #2168 Phase 2
// had proposed escalating THIS warning into a refusal as well; that was dropped
// with the rest. So this is the notice a user gets when they type the command,
// and the daemon repeats it once when the listener binds (startHTTPServer) — it
// no longer forecasts a failure, because there is not going to be one.
//
// Both directions of the pairing warn, because either key can create the
// exposure: pointing listen_addr at the network while the token is off, or
// turning the token off while listen_addr is already on the network. Setting
// any OTHER key stays silent even on an already-exposed config — this speaks to
// the change the user just made, and warning on every unrelated `config set`
// would train them to ignore it.
//
// The exposure test is ListenerServesUnauthenticatedNetwork — the SAME predicate
// the daemon's refusal uses, itself built on the IsLoopbackListenAddr the token
// gate derives from. Two definitions of "is this exposed" drifting apart is
// precisely how a security check rots, so there is only one.
func exposureWarning(cfg *Config, key string) string {
	if cfg == nil {
		return ""
	}
	key = canonicalConfigKey(key)
	if key != "network.listen_addr" && key != "network.require_token" {
		return ""
	}
	addr := cfg.ListenAddr
	if !ListenerServesUnauthenticatedNetwork(addr, cfg.RequireToken) {
		return ""
	}
	return fmt.Sprintf("WARNING: network.listen_addr %q is reachable from the network and network.require_token is false, which puts a "+
		"plain-HTTP control plane with no authentication in front of anyone who can reach it — including "+
		"DeliverPrompt, which runs instructions through your agents. The daemon will serve this on its next start. "+
		"Run `af config set network.require_token true` to require a token (`af token show` prints it), or set network.listen_addr "+
		"back to a loopback address such as 127.0.0.1:8443, or \"\" to turn the web server off.", addr)
}

// resolveSettable maps a user key ("default_program" or "program_overrides.claude")
// to its spec, section, and leaf. ok is false for anything not on the allowlist.
func resolveSettable(key string) (section, leaf string, spec settableKeySpec, ok bool) {
	key = canonicalConfigKey(key)
	if s, found := settableKeySpecs[key]; found {
		if s.dynamic {
			if s.structured {
				return "", key, s, true
			}
		} else {
			leaf := key
			if s.section != "" {
				prefix := s.section + "."
				leaf = strings.TrimPrefix(key, prefix)
			}
			return s.section, leaf, s, true
		}
	}
	if i := strings.IndexByte(key, '.'); i > 0 {
		prefix, rest := key[:i], key[i+1:]
		if s, found := settableKeySpecs[prefix]; found && s.dynamic && rest != "" && !strings.Contains(rest, ".") {
			return prefix, rest, s, true
		}
	}
	return "", "", settableKeySpec{}, false
}

// SetGlobalConfigValue validates key+rawValue against the settable-key allowlist
// and the loader's validators, then surgically writes the value into the global
// config.toml under a file lock, preserving unrelated comments and ordering. It
// guarantees the written file still loads. Returns an actionable error for an
// unknown key, a wrong-typed or invalid value, or an I/O failure.
func SetGlobalConfigValue(key, rawValue string) (*SetResult, error) {
	if key == "auto_yes" {
		return nil, RemovedAutoYesError()
	}
	section, leaf, spec, ok := resolveSettable(key)
	if !ok {
		return nil, fmt.Errorf("%q is not a settable config key. Settable keys: %s",
			key, strings.Join(SettableKeys(), ", "))
	}
	key = canonicalConfigKey(key)
	structured := spec.structured && section == ""
	canonical, encoded, err := canonicalizeConfigValue(key, spec, structured, rawValue)
	if err != nil {
		return nil, fmt.Errorf("invalid value for %s: %w", key, err)
	}
	if !structured && spec.validate != nil {
		if err := spec.validate(leaf, canonical); err != nil {
			return nil, err
		}
	}

	// Ensure config.toml exists (migrating a legacy config.json if needed) and
	// that the current config actually loads, so a later parse failure is
	// unambiguously our edit's fault, not a pre-existing broken file. This is a
	// PRECONDITION only — the loaded values are deliberately not carried into the
	// write. See scalarWrite.apply for why (#2412).
	if _, err := LoadConfig(); err != nil {
		return nil, fmt.Errorf("refusing to write: the current config does not load: %w", err)
	}
	configDir, err := GetConfigDir()
	if err != nil {
		return nil, err
	}
	tomlPath := filepath.Join(configDir, TomlConfigFileName)
	prettyPath := prettyHomePath(tomlPath)

	write := scalarWrite{key: key, section: section, leaf: leaf, canonical: canonical, encoded: encoded, structured: structured,
		rawStructured: rawValue, clear: spec.kind == cfgStringList && canonical == ""}

	var result *SetResult
	writeErr := WithFileLock(tomlPath, func() error {
		var err error
		result, err = write.apply(tomlPath, prettyPath)
		return err
	})
	if writeErr != nil {
		return nil, writeErr
	}
	return result, nil
}

// SetProjectConfigValue is the per-project counterpart of SetGlobalConfigValue
// (#2216 Phase 5). It resolves selector (a prj_ id or a repository path) to a
// registered project, validates key against BOTH the settable-key allowlist and
// the manifest's personal-project scope, then surgically writes the value into
// that project's machine-local config.toml under a file lock — the same
// validated, comment/order-preserving writer, aimed at a different destination.
// A key that is settable but does not admit the personal layer (a global-only or
// repo-contract key) is rejected with an actionable message, never written.
func SetProjectConfigValue(selector, key, rawValue string) (*SetResult, error) {
	if key == "auto_yes" {
		return nil, RemovedAutoYesError()
	}
	project, err := ResolveProjectSelector(selector)
	if err != nil {
		return nil, err
	}
	section, leaf, spec, err := resolveProjectSettable(key)
	if err != nil {
		return nil, err
	}

	structured := spec.structured && section == ""
	canonical, encoded, err := canonicalizeConfigValue(key, spec, structured, rawValue)
	if err != nil {
		return nil, fmt.Errorf("invalid value for %s: %w", key, err)
	}
	if !structured && spec.validate != nil {
		if err := spec.validate(leaf, canonical); err != nil {
			return nil, err
		}
	}

	path, err := ProjectConfigTomlPath(project.ID)
	if err != nil {
		return nil, err
	}
	prettyPath := prettyHomePath(path)
	write := scalarWrite{key: key, section: section, leaf: leaf, canonical: canonical, encoded: encoded, structured: structured}

	var result *SetResult
	writeErr := WithFileLock(path, func() error {
		var err error
		result, err = write.applyProject(path, prettyPath)
		return err
	})
	if writeErr != nil {
		return nil, writeErr
	}
	return result, nil
}

// resolveProjectSettable maps a user key to its settable spec AND enforces that
// the key admits the personal-project layer in the manifest. The manifest is the
// single authority on which keys may live where, so the write path checks it
// before editing rather than maintaining a second per-project allowlist.
func resolveProjectSettable(key string) (section, leaf string, spec settableKeySpec, err error) {
	key = canonicalConfigKey(key)
	section, leaf, spec, ok := resolveSettable(key)
	if !ok {
		return "", "", settableKeySpec{}, fmt.Errorf("%q is not a settable config key. Settable keys: %s",
			key, strings.Join(SettableKeys(), ", "))
	}
	scopeKey := key
	if spec.dynamic && section != "" {
		scopeKey = section
	}
	if !isProjectPersonalKey(scopeKey) {
		return "", "", settableKeySpec{}, projectScopeError(scopeKey)
	}
	return section, leaf, spec, nil
}

// projectScopeError explains why a settable key cannot be a per-project personal
// override, pointing the user at the location that key actually admits.
func projectScopeError(key string) error {
	if manifestGlobalOnlyKeySet()[key] {
		return fmt.Errorf("%q is a global setting and cannot be set per project; set it globally with `af config set %s <value>`", key, key)
	}
	return fmt.Errorf("%q describes the repository and cannot be a personal per-project override; set it in the repository's %s file",
		key, filepath.Join(InRepoConfigDirName, TomlConfigFileName))
}

// scalarWrite is one validated, canonicalized `config set` edit, ready to apply
// to config.toml.
type scalarWrite struct {
	// key is the user-facing key ("listen_addr", "program_overrides.claude").
	key string
	// section is the TOML table the key lives under ("" = the root block).
	section string
	// leaf is the key within that section.
	leaf string
	// canonical is the scalar's canonical string form, echoed back to the user.
	canonical string
	// encoded is its TOML encoding — the bytes that actually land in the file.
	encoded string
	// structured replaces a whole table/list value rather than one scalar line.
	structured bool
	// rawStructured retains the user's structured JSON until the global file
	// lock is held. Theme omissions must merge with the palette current inside
	// that lock, and default program-removal tombstones are encoded there too.
	rawStructured string
	// clear removes the key line instead of writing it. Set for an empty list
	// value (`af config set cors_allowed_origins ""`): a nil/absent list and an
	// empty one mean the same thing, so clearing keeps config.toml free of a
	// `key = []` line and round-trips an unset list back to unset.
	clear bool
}

// apply is SetGlobalConfigValue's critical section: re-read config.toml, make
// the surgical edit, prove the result still loads, write it, and judge the
// exposure of the config that results. Callers must hold the config.toml file
// lock.
//
// Everything here reads the file as it exists INSIDE the lock, including the
// exposure judgment. That last part is the #2412 fix and it is load-bearing:
// the warning used to be computed from a *Config loaded before the lock was
// taken, while the bytes being edited were re-read inside it. Since the
// exposure is a PAIRING — a non-loopback listen_addr together with
// require_token = false — judging it needs the value of the key the caller is
// NOT setting, and that value came from the stale pre-lock snapshot.
//
// Two processes racing could therefore each turn on one half of the exposure
// and each see the other half as it was before the race: `af config set
// listen_addr 0.0.0.0:8443` reads require_token = true, `af config set
// require_token false` reads a loopback listen_addr, both exit 0 silently, and
// the config left on disk serves an unauthenticated control plane with nobody
// having been told. The daemon is not a backstop — it emits its own notice only
// when it binds, so an already-running daemon says nothing until the next
// restart.
//
// Judging the RESULT rather than reconstructing it also removes the
// reconstruction: exposureWarning no longer has to splice the written value
// into a snapshot of the other one, because parseConfigTOML has already
// produced the exact config this write lands on. Whichever racer writes second
// now sees the full pairing and warns.
func (w scalarWrite) apply(tomlPath, prettyPath string) (*SetResult, error) {
	current, err := os.ReadFile(tomlPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to read %s: %w", prettyPath, err)
	}
	current = stripUTF8BOM(current)
	updated := string(current)
	if w.structured {
		base := DefaultConfig()
		if !isEffectivelyEmptyToml(current) {
			base, err = parseConfigTOML(current, prettyPath)
			if err != nil {
				return nil, fmt.Errorf("refusing to write: the current config does not load: %w", err)
			}
		}
		if w.key == "theme" && !strings.HasPrefix(strings.TrimSpace(w.rawStructured), "{") {
			w.canonical, w.encoded, err = canonicalizeConfigValue(w.key, settableKeySpec{}, true, w.rawStructured)
		} else {
			w.canonical, w.encoded, err = canonicalizeStructuredValueAgainst(w.key, w.rawStructured, base, true)
		}
		if err != nil {
			return nil, fmt.Errorf("invalid value for %s: %w", w.key, err)
		}
	}
	switch {
	case w.structured:
		updated, err = setTOMLStructured(updated, w.key, w.encoded)
		if err != nil {
			return nil, fmt.Errorf("failed to edit %s in %s: %w", w.key, prettyPath, err)
		}
	case w.clear:
		updated, _ = deleteTOMLScalar(updated, w.section, w.leaf)
	default:
		updated = setTOMLScalar(updated, w.section, w.leaf, w.encoded)
	}
	// If this file already carries the flat compatibility spelling, keep it in
	// sync inside the same lock. A rolled-back binary ignores the new table, so
	// leaving a conflicting old value behind would make a rollback change the
	// operator's setting. Grouped-only files stay grouped-only; no deprecated key
	// is invented for a new install.
	if alias, ok := configAliasForCanonical(w.key); ok {
		if metadata, err := metadataForSource(current, tomlPath, FormatTOML); err == nil {
			if _, present := metadata.shape[alias.legacy]; present {
				if w.clear {
					updated, _ = deleteTOMLScalar(updated, "", alias.legacy)
				} else {
					updated = setTOMLScalar(updated, "", alias.legacy, w.encoded)
				}
			}
		}
	}
	updated = setTOMLScalar(updated, "", SchemaVersionField, strconv.Itoa(GlobalConfigSchemaVersion))

	// Final gate: the edited bytes must parse and validate exactly as the loader
	// would, so `config set` can never leave an unloadable config. The parsed
	// result is also the config this write RESULTS in — the same values
	// LoadConfig will return for these bytes, since it reaches this very
	// function (parseLoadedConfigTOML adds provenance, not values) — so it is
	// what the exposure judgment below is made against.
	resulting, err := parseConfigTOML([]byte(updated), prettyPath)
	if err != nil {
		return nil, fmt.Errorf("internal error: edited config would not load (no changes written): %w", err)
	}
	if err := AtomicWriteFile(tomlPath, []byte(updated), 0644); err != nil {
		return nil, err
	}
	value := w.canonical
	if w.structured {
		value, _ = CurrentValue(resulting, w.key)
	}
	result := &SetResult{Key: w.key, Value: value, Path: tomlPath, RequiresRestart: true}
	if warn := exposureWarning(resulting, w.key); warn != "" {
		result.Warnings = append(result.Warnings, warn)
	}
	return result, nil
}

// applyProject is the personal-project counterpart of apply. It reuses the same
// surgical setTOMLScalar edit and re-parse-before-write discipline, but against
// a project's config.toml: it does NOT inject the global schema_version marker
// (that is global bookkeeping), gates on the personal-project loader rather than
// the global one, and produces no listener-exposure warning (network-surface
// keys are global-only and can never reach this path). Callers must hold the
// project config file lock.
func (w scalarWrite) applyProject(path, prettyPath string) (*SetResult, error) {
	current, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to read %s: %w", prettyPath, err)
	}
	current = stripUTF8BOM(current)
	updated := string(current)
	if w.structured {
		updated, err = setTOMLStructured(updated, w.key, w.encoded)
		if err != nil {
			return nil, fmt.Errorf("failed to edit %s in %s: %w", w.key, prettyPath, err)
		}
	} else {
		updated = setTOMLScalar(updated, w.section, w.leaf, w.encoded)
	}
	if _, err := parseProjectConfig([]byte(updated), path); err != nil {
		return nil, fmt.Errorf("internal error: edited personal project config would not load (no changes written): %w", err)
	}
	if err := AtomicWriteFile(path, []byte(updated), 0644); err != nil {
		return nil, err
	}
	return &SetResult{Key: w.key, Value: w.canonical, Path: path, RequiresRestart: true}, nil
}

// setTOMLScalar returns content with [section] leaf set to encoded, changing only
// the target value's bytes. If the key exists its value (and only its value) is
// replaced, preserving any trailing inline comment. It recognizes both TOML
// spellings of a table entry — a leaf under a [section] header AND a top-level
// dotted key (section.leaf = …) — and edits whichever is present, so a
// hand-edited dotted-key file is never left with a duplicate. If the key is
// absent it is inserted with minimal disturbance — appended to the end of its
// section's content block, i.e. after the section's last non-blank line
// (comments included) and before any trailing blanks preceding the next section
// or EOF (#1687), or for a root key the pre-section block; if the section itself
// is absent a new [section] block is appended. section == "" targets the root
// block.
func setTOMLScalar(content, section, leaf, encoded string) string {
	newLine := leaf + " = " + encoded

	if strings.TrimSpace(content) == "" {
		if section == "" {
			return newLine + "\n"
		}
		return "[" + section + "]\n" + newLine + "\n"
	}

	hadTrailingNewline := strings.HasSuffix(content, "\n")
	ls := strings.Split(content, "\n")
	if hadTrailingNewline && len(ls) > 0 && ls[len(ls)-1] == "" {
		ls = ls[:len(ls)-1]
	}

	keyRe := regexp.MustCompile(`^(\s*` + regexp.QuoteMeta(leaf) + `\s*=\s*)(.*)$`)

	// TOML also lets a hand-editor write a table entry as a top-level dotted key
	// (program_overrides.claude = "…") instead of under a [program_overrides]
	// header. For a dynamic key we must recognize that form too, or we would
	// miss the existing key and append a duplicate — corrupting the file (a
	// valid config never has both forms, so at most one matches). dotted whitespace
	// around the '.' is allowed by TOML, so tolerate it.
	var dottedKeyRe *regexp.Regexp
	if section != "" {
		dottedKeyRe = regexp.MustCompile(`^(\s*` + regexp.QuoteMeta(section) + `\s*\.\s*` + regexp.QuoteMeta(leaf) + `\s*=\s*)(.*)$`)
	}

	curSection := ""
	firstHeaderIdx := -1
	targetHeaderIdx := -1
	// lastContentIdxInTarget tracks the last non-blank line of the target
	// section INCLUDING comment lines, so a missing key is appended at the END
	// of the section's content block (the documented contract). Tracking only
	// key=value lines (the pre-#1687 behavior) left it at -1 for a comment-only
	// section, which inserted the new key immediately after the [section] header
	// and ABOVE the section's comments (#1687). Blank lines never update it, so
	// trailing blanks before the next header / EOF are excluded and the insert
	// lands at the end of the content, not spilling past it.
	lastContentIdxInTarget := -1

	rebuild := func() string {
		out := strings.Join(ls, "\n")
		if hadTrailingNewline {
			out += "\n"
		}
		return out
	}

	for i, line := range ls {
		if name, ok := tomlHeaderName(line); ok {
			if firstHeaderIdx == -1 {
				firstHeaderIdx = i
			}
			if name == section && targetHeaderIdx == -1 {
				targetHeaderIdx = i
			}
			curSection = name
			continue
		}
		// Top-level dotted form (section.leaf = …). Only valid at the root: the
		// same text under another header would name a different key.
		if dottedKeyRe != nil && curSection == "" {
			if m := dottedKeyRe.FindStringSubmatch(line); m != nil {
				if updated, ok := replaceTOMLAssignmentLines(ls, i, encoded); ok {
					ls = updated
					return rebuild()
				}
			}
			if tomlScalarLineMatches(line, section, leaf) {
				if updated, ok := replaceTOMLAssignmentLines(ls, i, encoded); ok {
					ls = updated
					return rebuild()
				}
			}
			if updated, ok := setTOMLInlineTableMember(line, section, leaf, encoded); ok {
				ls[i] = updated
				return rebuild()
			}
		}
		if curSection != section {
			continue
		}
		if m := keyRe.FindStringSubmatch(line); m != nil {
			if updated, ok := replaceTOMLAssignmentLines(ls, i, encoded); ok {
				ls = updated
				return rebuild()
			}
		}
		if tomlScalarLineMatches(line, "", leaf) {
			if updated, ok := replaceTOMLAssignmentLines(ls, i, encoded); ok {
				ls = updated
				return rebuild()
			}
		}
		if strings.TrimSpace(line) != "" {
			lastContentIdxInTarget = i
		}
	}

	// Key not found — insert.
	insertAt := func(idx int, s string) {
		ls = append(ls, "")
		copy(ls[idx+1:], ls[idx:])
		ls[idx] = s
	}

	switch {
	case section == "":
		switch {
		case lastContentIdxInTarget != -1:
			insertAt(lastContentIdxInTarget+1, newLine)
		case firstHeaderIdx != -1:
			insertAt(firstHeaderIdx, newLine)
		default:
			ls = append(ls, newLine)
		}
	case targetHeaderIdx == -1:
		// Section absent: append a fresh block, separated by one blank line.
		if len(ls) > 0 && ls[len(ls)-1] != "" {
			ls = append(ls, "")
		}
		ls = append(ls, "["+section+"]", newLine)
	default:
		if lastContentIdxInTarget != -1 {
			insertAt(lastContentIdxInTarget+1, newLine)
		} else {
			insertAt(targetHeaderIdx+1, newLine)
		}
	}
	return rebuild()
}

// deleteTOMLScalar removes the [section] leaf line from content, changing only
// that one line and preserving every other comment, blank line, and key. It is
// the inverse of setTOMLScalar and recognizes the same two spellings of a table
// entry — a leaf under a [section] header AND a top-level dotted key
// (section.leaf = …) — removing whichever is present. section == "" targets a
// root-block key. Returns the edited content and whether a line was removed; an
// absent key leaves content untouched and reports false. An emptied [section]
// header is left in place (a present-but-empty table resolves to no leaves).
func deleteTOMLScalar(content, section, leaf string) (string, bool) {
	if strings.TrimSpace(content) == "" {
		return content, false
	}

	hadTrailingNewline := strings.HasSuffix(content, "\n")
	ls := strings.Split(content, "\n")
	if hadTrailingNewline && len(ls) > 0 && ls[len(ls)-1] == "" {
		ls = ls[:len(ls)-1]
	}

	keyRe := regexp.MustCompile(`^(\s*` + regexp.QuoteMeta(leaf) + `\s*=\s*)(.*)$`)
	var dottedKeyRe *regexp.Regexp
	if section != "" {
		dottedKeyRe = regexp.MustCompile(`^(\s*` + regexp.QuoteMeta(section) + `\s*\.\s*` + regexp.QuoteMeta(leaf) + `\s*=\s*)(.*)$`)
	}

	curSection := ""
	removeAt := -1
	removeThrough := -1
	rebuild := func() string {
		out := strings.Join(ls, "\n")
		if hadTrailingNewline && out != "" {
			out += "\n"
		}
		return out
	}
	for i, line := range ls {
		if name, ok := tomlHeaderName(line); ok {
			curSection = name
			continue
		}
		// Top-level dotted form (section.leaf = …), valid only at the root.
		if dottedKeyRe != nil && curSection == "" && (dottedKeyRe.MatchString(line) || tomlScalarLineMatches(line, section, leaf)) {
			removeAt = i
			removeThrough = tomlAssignmentEnd(ls, i)
			break
		}
		if curSection == "" && section != "" {
			if updated, ok := deleteTOMLInlineTableMember(line, section, leaf); ok {
				ls[i] = updated
				return rebuild(), true
			}
		}
		if curSection != section {
			continue
		}
		if keyRe.MatchString(line) || tomlScalarLineMatches(line, "", leaf) {
			removeAt = i
			removeThrough = tomlAssignmentEnd(ls, i)
			break
		}
	}
	if removeAt < 0 {
		return content, false
	}

	kept := preservedTOMLAssignmentComments(ls, removeAt, removeThrough)
	kept = append(kept, ls[removeThrough+1:]...)
	ls = append(ls[:removeAt], kept...)
	return rebuild(), true
}

// TOML's two multiline string delimiters. They are scanned as three-byte UNITS
// rather than as three individual quotes, which is the whole of #3455: a
// per-quote toggle reads a triple quote as open-close-open and leaves the
// scanner believing it is inside a string, so a '#' after the delimiter never
// registers as a comment.
const (
	tomlMultilineBasic   = `"""`
	tomlMultilineLiteral = `'''`
)

// splitTrailingComment separates a TOML value from a trailing inline comment,
// tracking string state so a '#' inside a string is not mistaken for a comment.
// It returns the value part and the comment part (including the whitespace that
// preceded the '#'), so the comment can be reattached byte-for-byte.
//
// The scan begins OUTSIDE every string, which is right for a line that opens
// whatever strings it contains: a single-line assignment's value, or an inline
// table. The closing line of a MULTILINE assignment is the other case — the
// string it ends was opened on an earlier line — and that one needs
// scanTrailingComment with the delimiter still open.
func splitTrailingComment(rest string) (value, comment string) {
	value, comment, _ = scanTrailingComment(rest, "")
	return value, comment
}

// scanTrailingComment splits one line of a TOML value, resuming inside the
// string that `open` closes ("" when the line begins outside every string), and
// reports the delimiter still open when the line ends so the caller can carry
// the state to the next line.
//
// Carrying that state is what makes the closing delimiter of a multiline string
// read as the CLOSE it is rather than the open of a new string. It also covers
// the same defect one level down, where a multiline string is an ELEMENT of a
// multiline array and holds its state open across the element boundary.
func scanTrailingComment(rest, open string) (value, comment, stillOpen string) {
	for i := 0; i < len(rest); {
		if open != "" {
			// Basic strings process backslash escapes, so an escaped quote is
			// content and cannot close anything. Literal strings process none
			// at all, which is exactly what lets a lone backslash sit inside
			// one.
			if open[0] == '"' && rest[i] == '\\' {
				i += 2
				continue
			}
			if !strings.HasPrefix(rest[i:], open) {
				i++
				continue
			}
			i += len(open)
			if len(open) == 3 {
				// TOML lets a multiline string's content end with one or two of
				// its own quote characters, so the delimiter is the LAST three
				// of the run: `"""a""""` is the string `a"`. Consuming the whole
				// run keeps a stray quote from opening a phantom string that
				// would swallow the comment after it.
				for i < len(rest) && rest[i] == open[0] {
					i++
				}
			}
			open = ""
			continue
		}
		switch {
		case strings.HasPrefix(rest[i:], tomlMultilineBasic):
			open = tomlMultilineBasic
			i += len(tomlMultilineBasic)
		case strings.HasPrefix(rest[i:], tomlMultilineLiteral):
			open = tomlMultilineLiteral
			i += len(tomlMultilineLiteral)
		case rest[i] == '"':
			open = `"`
			i++
		case rest[i] == '\'':
			open = `'`
			i++
		case rest[i] == '#':
			j := i
			for j > 0 && (rest[j-1] == ' ' || rest[j-1] == '\t') {
				j--
			}
			return rest[:j], rest[j:], ""
		default:
			i++
		}
	}
	return rest, "", open
}

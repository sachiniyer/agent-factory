package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// This file implements `af config set` (#1192). It writes a single scalar key
// into the global config.toml with three deliberate properties:
//
//  1. Comment/ordering-preserving: it does a SURGICAL in-place edit of the file
//     text (see setTOMLScalar) rather than re-marshaling the Config struct.
//     toml.Marshal regenerates the file and would strip the comments, blank
//     lines, and key ordering of a file the README tells users to hand-edit —
//     an external-user footgun. Only the target value's bytes change.
//  2. Allowlisted: only a curated set of safe, documented scalar keys is
//     settable (settableKeySpecs). Unknown or structural keys (root_agents, the
//     [keys] rebinds) are rejected with the settable list — they stay
//     hand-edited.
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

// cfgValueKind is the scalar type a settable key accepts.
type cfgValueKind int

const (
	cfgString cfgValueKind = iota
	cfgInt
	cfgBool
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
	// validate runs the loader's own validation on the parsed value before the
	// write, returning the loader's error verbatim where possible. leaf is the
	// sub-key for a dynamic family (the program name), else the key itself.
	validate func(leaf, value string) error
}

// settableKeySpecs is the allowlist. Scalars + the two simple string maps only.
//
// Every EXCLUSION is deliberate and stated here, because an unexplained gap is
// indistinguishable from drift — and was: listen_addr, require_loopback_token,
// vscode_server_binary, limit_auto_resume and limit_retry_interval were all
// missing for no stated reason, which left the config agent unable
// to set two of the keys a new user most needs (listen_addr,
// require_loopback_token). TestManifestAgreesWithSettableKeys now pins this map
// against the config manifest, so a key can no longer go missing quietly: adding
// a Settable entry to the manifest without an entry here fails the test, and
// vice versa.
//
// Structural values stay hand-edited, and the manifest marks them Settable:false:
//   - root_agents — a nested table (path → {program}); no scalar shape.
//   - theme — a color table (see ThemeConfig); setting one slot at a time through
//     the CLI is not how anyone edits a palette.
//   - keys — array-capable rebinds (an action may map to a list of keys).
//   - cors_allowed_origins — a LIST of origins, and the only excluded key that is
//     otherwise scalar-ish. It needs real design first, not a spec entry: this
//     file's machinery is scalar-only end to end (cfgValueKind has no list kind,
//     canonicalizeScalar returns one encoded scalar, setTOMLScalar writes one
//     `key = value` line), and a list also needs a decided WRITE SEMANTIC —
//     whether `set` replaces the whole list, appends to it, or gets add/remove
//     verbs — which is a CLI contract choice, not an implementation detail.
//     Deferred rather than guessed.
var settableKeySpecs = map[string]settableKeySpec{
	"default_program": {kind: cfgString, validate: func(_, v string) error {
		return ValidateProgramEnum("default_program", "default_program", v, "")
	}},
	"auto_update":            {kind: cfgBool},
	"require_token":          {kind: cfgBool},
	"require_loopback_token": {kind: cfgBool},
	"listen_addr": {kind: cfgString, validate: func(_, v string) error {
		return validateListenAddrValue(v)
	}},
	// Empty is meaningful (detect code-server/openvscode-server on PATH) and any
	// non-empty value is a path the daemon resolves — including a "~" it expands
	// and a binary that may not exist yet — so there is nothing to validate here
	// that would not reject a legitimate value. The executability check belongs
	// where the binary is actually run, and already lives there.
	"vscode_server_binary": {kind: cfgString},
	"limit_auto_resume":    {kind: cfgBool},
	"global_agent_skills":  {kind: cfgBool},
	"limit_retry_interval": {kind: cfgString, validate: func(_, v string) error {
		return validateLimitRetryIntervalValue(v)
	}},
	"daemon_poll_interval": {kind: cfgInt, validate: func(_, v string) error { return requirePositiveInt("daemon_poll_interval", v) }},
	"log_max_size_mb":      {kind: cfgInt, validate: func(_, v string) error { return requirePositiveInt("log_max_size_mb", v) }},
	"log_max_backups":      {kind: cfgInt, validate: func(_, v string) error { return requireNonNegativeInt("log_max_backups", v) }},
	"branch_prefix":        {kind: cfgString},
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
	"program_overrides": {kind: cfgString, section: "program_overrides", dynamic: true, validate: func(leaf, v string) error {
		return ValidateProgramEnum("program_overrides key", "program_overrides key", leaf, v)
	}},
	"limit_patterns": {kind: cfgString, section: "limit_patterns", dynamic: true, validate: func(leaf, v string) error {
		if err := ValidateProgramEnum("limit_patterns key", "limit_patterns key", leaf, v); err != nil {
			return err
		}
		if _, err := regexp.Compile(v); err != nil {
			return fmt.Errorf("limit_patterns.%s is not a valid regular expression: %w", leaf, err)
		}
		return nil
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

// SettableKeys returns the sorted, human-facing list of keys `config set`
// accepts; dynamic families are rendered as prefix.<name>.
func SettableKeys() []string {
	out := make([]string, 0, len(settableKeySpecs))
	for k, s := range settableKeySpecs {
		if s.dynamic {
			out = append(out, k+".<name>")
		} else {
			out = append(out, k)
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
	if key != "listen_addr" && key != "require_token" {
		return ""
	}
	addr := cfg.ListenAddr
	if !ListenerServesUnauthenticatedNetwork(addr, cfg.RequireToken) {
		return ""
	}
	return fmt.Sprintf("WARNING: %s is reachable from the network and require_token is false, which puts a "+
		"plain-HTTP control plane with no authentication in front of anyone who can reach it — including "+
		"DeliverPrompt, which runs instructions through your agents. The daemon will serve this on its next start. "+
		"Run `af config set require_token true` to require a token (`af token show` prints it), or set listen_addr "+
		"back to a loopback address such as 127.0.0.1:8443, or \"\" to turn the web server off.", addr)
}

// resolveSettable maps a user key ("default_program" or "program_overrides.claude")
// to its spec, section, and leaf. ok is false for anything not on the allowlist.
func resolveSettable(key string) (section, leaf string, spec settableKeySpec, ok bool) {
	if s, found := settableKeySpecs[key]; found && !s.dynamic {
		return "", key, s, true
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
// config.toml under a file lock, preserving all comments and ordering. It
// guarantees the written file still loads. Returns an actionable error for an
// unknown key, a wrong-typed or invalid value, or an I/O failure.
func SetGlobalConfigValue(key, rawValue string) (*SetResult, error) {
	if key == "auto_yes" {
		return nil, RemovedAutoYesError()
	}
	section, leaf, spec, ok := resolveSettable(key)
	if !ok {
		return nil, fmt.Errorf("%q is not a settable config key. Settable keys: %s. "+
			"Structural keys (root_agents, [theme], [keys] rebinds) are edited directly in config.toml",
			key, strings.Join(SettableKeys(), ", "))
	}

	canonical, encoded, err := canonicalizeScalar(spec.kind, rawValue)
	if err != nil {
		return nil, fmt.Errorf("invalid value for %s: %w", key, err)
	}
	if spec.validate != nil {
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

	write := scalarWrite{key: key, section: section, leaf: leaf, canonical: canonical, encoded: encoded}

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

	canonical, encoded, err := canonicalizeScalar(spec.kind, rawValue)
	if err != nil {
		return nil, fmt.Errorf("invalid value for %s: %w", key, err)
	}
	if spec.validate != nil {
		if err := spec.validate(leaf, canonical); err != nil {
			return nil, err
		}
	}

	path, err := ProjectConfigTomlPath(project.ID)
	if err != nil {
		return nil, err
	}
	prettyPath := prettyHomePath(path)
	write := scalarWrite{key: key, section: section, leaf: leaf, canonical: canonical, encoded: encoded}

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

// UnsetResult reports a successful `af config unset --project`. Removed is false
// when there was no override to clear — the command is a clean no-op then, not an
// error.
type UnsetResult struct {
	Key             string `json:"key"`
	Path            string `json:"path"`
	Removed         bool   `json:"removed"`
	RequiresRestart bool   `json:"requires_restart"`
}

// UnsetProjectConfigValue removes key's personal override for a project so the
// value falls back to the lower layers again (#2216 Phase 5). Clearing an
// override is deliberately distinct from setting a value equal to the lower
// layer, which would still be a present, winning override. Unsetting a key that
// is not present is a clean no-op. It is scoped to a project: there is no global
// unset today (remove the line from config.toml by hand, or set a new value).
func UnsetProjectConfigValue(selector, key string) (*UnsetResult, error) {
	if key == "auto_yes" {
		return nil, RemovedAutoYesError()
	}
	project, err := ResolveProjectSelector(selector)
	if err != nil {
		return nil, err
	}
	section, leaf, _, err := resolveProjectSettable(key)
	if err != nil {
		return nil, err
	}
	path, err := ProjectConfigTomlPath(project.ID)
	if err != nil {
		return nil, err
	}
	prettyPath := prettyHomePath(path)

	var result *UnsetResult
	writeErr := WithFileLock(path, func() error {
		var err error
		result, err = applyProjectUnset(path, prettyPath, section, leaf, key)
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
	section, leaf, spec, ok := resolveSettable(key)
	if !ok {
		return "", "", settableKeySpec{}, fmt.Errorf("%q is not a settable config key. Settable keys: %s. "+
			"Structural keys (root_agents, [theme], [keys] rebinds) are edited directly in config.toml",
			key, strings.Join(SettableKeys(), ", "))
	}
	scopeKey := key
	if spec.dynamic {
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

// applyProjectUnset removes the target key line from a project's config.toml
// under the caller-held lock. A missing file or absent key is a clean no-op. If
// the removal empties the file it is deleted, so the project falls fully back to
// the lower layers rather than leaving a contentless file the loader rejects.
func applyProjectUnset(path, prettyPath, section, leaf, key string) (*UnsetResult, error) {
	current, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &UnsetResult{Key: key, Path: path, Removed: false}, nil
		}
		return nil, fmt.Errorf("failed to read %s: %w", prettyPath, err)
	}
	updated, removed := deleteTOMLScalar(string(current), section, leaf)
	if !removed {
		return &UnsetResult{Key: key, Path: path, Removed: false}, nil
	}
	if projectConfigHasNoTopLevelKeys(updated) {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("failed to remove emptied %s: %w", prettyPath, err)
		}
		return &UnsetResult{Key: key, Path: path, Removed: true, RequiresRestart: true}, nil
	}
	// The edited bytes must still parse and validate exactly as a read would, so
	// unset can never leave an unloadable personal config.
	if _, err := parseProjectConfig([]byte(updated), path); err != nil {
		return nil, fmt.Errorf("internal error: edited personal project config would not load (no changes written): %w", err)
	}
	if err := AtomicWriteFile(path, []byte(updated), 0644); err != nil {
		return nil, err
	}
	return &UnsetResult{Key: key, Path: path, Removed: true, RequiresRestart: true}, nil
}

// projectConfigHasNoTopLevelKeys reports whether content decodes to zero
// top-level keys (blank, comments-only, or whitespace). An emptied [section]
// header still counts as a key and keeps the file, which is harmless: a
// present-but-empty table contributes no leaves to resolution.
func projectConfigHasNoTopLevelKeys(content string) bool {
	if strings.TrimSpace(content) == "" {
		return true
	}
	var shape map[string]any
	if err := toml.Unmarshal([]byte(content), &shape); err != nil {
		// Leave the decision to the caller's parse gate rather than deleting a
		// file we could not understand.
		return false
	}
	return len(shape) == 0
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
	updated := setTOMLScalar(string(current), w.section, w.leaf, w.encoded)
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
	result := &SetResult{Key: w.key, Value: w.canonical, Path: tomlPath, RequiresRestart: true}
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
	updated := setTOMLScalar(string(current), w.section, w.leaf, w.encoded)
	if _, err := parseProjectConfig([]byte(updated), path); err != nil {
		return nil, fmt.Errorf("internal error: edited personal project config would not load (no changes written): %w", err)
	}
	if err := AtomicWriteFile(path, []byte(updated), 0644); err != nil {
		return nil, err
	}
	return &SetResult{Key: w.key, Value: w.canonical, Path: path, RequiresRestart: true}, nil
}

// canonicalizeScalar parses rawValue per kind and returns both the canonical
// string form (for echo/validation) and its TOML encoding (for the file).
func canonicalizeScalar(kind cfgValueKind, raw string) (canonical, encoded string, err error) {
	switch kind {
	case cfgBool:
		b, perr := strconv.ParseBool(strings.TrimSpace(raw))
		if perr != nil {
			return "", "", fmt.Errorf("expected a boolean (true/false), got %q", raw)
		}
		s := strconv.FormatBool(b)
		return s, s, nil
	case cfgInt:
		n, perr := strconv.Atoi(strings.TrimSpace(raw))
		if perr != nil {
			return "", "", fmt.Errorf("expected an integer, got %q", raw)
		}
		s := strconv.Itoa(n)
		return s, s, nil
	default:
		return raw, encodeTOMLString(raw), nil
	}
}

// encodeTOMLString renders s as a TOML string, preferring a literal single-quoted
// string (matching go-toml's output style and leaving backslashes in paths
// untouched). It falls back to a basic double-quoted string only when s contains
// a single quote or a newline, which a literal string cannot represent.
func encodeTOMLString(s string) string {
	if !strings.ContainsAny(s, "'\n\r") {
		return "'" + s + "'"
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

var (
	tomlHeaderRe = regexp.MustCompile(`^\s*\[([^\[\]]+)\]\s*(#.*)?$`)
)

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
		if m := tomlHeaderRe.FindStringSubmatch(line); m != nil {
			if firstHeaderIdx == -1 {
				firstHeaderIdx = i
			}
			name := strings.TrimSpace(m[1])
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
				_, comment := splitTrailingComment(m[2])
				ls[i] = m[1] + encoded + comment
				return rebuild()
			}
		}
		if curSection != section {
			continue
		}
		if m := keyRe.FindStringSubmatch(line); m != nil {
			_, comment := splitTrailingComment(m[2])
			ls[i] = m[1] + encoded + comment
			return rebuild()
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
	for i, line := range ls {
		if m := tomlHeaderRe.FindStringSubmatch(line); m != nil {
			curSection = strings.TrimSpace(m[1])
			continue
		}
		// Top-level dotted form (section.leaf = …), valid only at the root.
		if dottedKeyRe != nil && curSection == "" && dottedKeyRe.MatchString(line) {
			removeAt = i
			break
		}
		if curSection != section {
			continue
		}
		if keyRe.MatchString(line) {
			removeAt = i
			break
		}
	}
	if removeAt < 0 {
		return content, false
	}

	ls = append(ls[:removeAt], ls[removeAt+1:]...)
	out := strings.Join(ls, "\n")
	if hadTrailingNewline && out != "" {
		out += "\n"
	}
	return out, true
}

// splitTrailingComment separates a TOML value from a trailing inline comment,
// tracking quote state so a '#' inside a string is not mistaken for a comment.
// It returns the value part and the comment part (including the whitespace that
// preceded the '#'), so the comment can be reattached byte-for-byte.
func splitTrailingComment(rest string) (value, comment string) {
	inSingle, inDouble := false, false
	escape := false
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		switch {
		case inSingle:
			if c == '\'' {
				inSingle = false
			}
		case inDouble:
			if escape {
				escape = false
			} else if c == '\\' {
				escape = true
			} else if c == '"' {
				inDouble = false
				escape = false
			}
		case c == '\'':
			inSingle = true
			escape = false
		case c == '"':
			inDouble = true
			escape = false
		case c == '#':
			j := i
			for j > 0 && (rest[j-1] == ' ' || rest[j-1] == '\t') {
				j--
			}
			return rest[:j], rest[j:]
		}
	}
	return rest, ""
}

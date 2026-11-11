package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// This file implements `af config set` (#1192). It writes one config key
// into the global config.toml with three deliberate properties:
//
//  1. Comment/ordering-preserving: it does a SURGICAL in-place edit of the file
//     text (see setTOMLScalar in toml_scalar_edit.go) rather than re-marshaling
//     the Config struct.
//     toml.Marshal regenerates the file and would strip the comments, blank
//     lines, and key ordering of a file the README tells users to hand-edit —
//     an external-user footgun. Only the target value's bytes change.
//  2. Manifest-complete: every global manifest key is settable. Scalars retain
//     their native CLI syntax; tables and non-comma lists use compact JSON, the
//     exact form CurrentValue returns to both config panes (#3345).
//  3. Validated with the loader's own rules BEFORE writing: the same
//     ValidateProgramEnum / enum / range checks the loader applies, plus a final
//     parseConfigTOML gate on the edited bytes, so `config set` can never write
//     a config that then fails to load — and, since #3662, a value-drift gate
//     next to it, because a config that loads is not yet a config that still
//     MEANS what it meant (see rewrite_guard.go).
//
// config.toml is user-hand-editable state read by the daemon and TUI at
// startup (not daemon-exclusively-owned like instances.json), so the write is a
// file write guarded by WithFileLock (mirroring the pre-daemon tasks path) — not
// a daemon RPC. Changes apply exactly as a hand-edit does: on the next af /
// daemon start (SetResult.RequiresRestart is always true).

// SetResult reports a successful `config set`.
type SetResult struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	Path  string `json:"path"`
	// RequiresRestart is true for every ordinary write: config.toml is read at
	// startup, so a change applies to af and the daemon on their next start,
	// exactly like a hand-edit. The exception is a project-scoped branch_prefix
	// (#4539): stored but never applied, so no restart can apply it and the
	// field reports false rather than promising one.
	RequiresRestart bool `json:"requires_restart"`
	// Warnings are non-fatal notes about what the write actually means, printed
	// after the echo. The write SUCCEEDED — a warning never blocks or changes the
	// value. Today they are the tokenless-network-listener exposure
	// (exposureWarning) and, on a project-scoped write, the stored-but-ignored
	// branch_prefix notice (#4539).
	Warnings []string `json:"warnings,omitempty"`
}

// listenerWriteRefusal names the one `af config` refusal that is not a syntax
// or drift problem: a write that would LEAVE the control-plane listener in the
// #5137 refused posture — non-loopback listen_addr, require_token false, and
// no allow_unauthenticated_network opt-in.
//
// cfg is the config the edit RESULTS in — parsed from the bytes about to be
// written inside the file lock — so the judgment covers the pairing, never one
// key alone (#2412: two racing writers can each turn on one half of the
// exposure). A write that lands the refused combination is refused with the
// SAME message the daemon would emit at bind (ListenerBindRefusal), and the
// file is left untouched: refusing the write is refusing to create a config
// the daemon must then refuse to serve.
//
// The gate is scoped to the two keys a write can use to CREATE the refused
// posture — network.listen_addr pointed at the network, and require_token
// turned off. An unrelated write on an already-refused config (a hand-edit
// left one) is allowed: it did not create the posture, it leaves it no worse,
// and refusing it would strand every other config edit behind an unrelated
// fix — while every remediation write lands a safe resulting config and is
// therefore allowed.
//
// network.allow_unauthenticated_network is deliberately NOT gated. The only
// write of it that lands on a refused resulting posture is the REVOCATION —
// setting it false, or unsetting it, under a non-loopback tokenless bind —
// and refusing that write would keep the very listener it is revoking serving
// unauthenticated. `af doctor`'s own remediation for an opted-in exposure is
// `af config set network.allow_unauthenticated_network false`; it must land so
// the daemon's next apply can retire the listener into the refused state the
// operator just asked for.
//
// network.require_loopback_token is deliberately absent too: it is inert while
// require_token is false (tokenDisabled short-circuits the gate), so it neither
// creates nor rescues the exposure. network.preview_listen_addr is a separate
// listener that never serves the control plane, so it is governed by its own
// notice (PreviewListenerExposureNotice) and stays warning-only.
func listenerWriteRefusal(key string, resulting *Config) string {
	switch canonicalConfigKey(key) {
	case "network.listen_addr", "network.require_token":
		return ListenerBindRefusal(resulting)
	}
	return ""
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
// Since #5137 this warning only ever describes an OPTED-IN exposure: a write
// whose result would be refused outright never reaches it — listenerWriteRefusal
// already refused that write with the shared remediation message. What is left
// for the warning is the combination that IS going to serve unauthenticated —
// non-loopback listen_addr, token off, and allow_unauthenticated_network = true
// — so the operator sees the risk they just accepted stated plainly, once.
//
// Both directions of the pairing warn, because either key can create the
// exposure: pointing listen_addr at the network while the token is off, or
// turning the token off while listen_addr is already on the network. Setting
// any OTHER key stays silent even on an already-exposed config — this speaks to
// the change the user just made, and warning on every unrelated `config set`
// would train them to ignore it. Setting network.allow_unauthenticated_network
// itself stays silent too: opting in IS the acknowledgement, so warning about
// it would be noise.
//
// network.preview_listen_addr is the parallel surface for the web-tab preview
// listener (#1856). It has NO "other half" pairing: the preview origin never
// serves the control plane, and its own credential is the per-tab hostname, so
// the notice depends on preview_listen_addr ALONE and does NOT gate on
// network.require_token (see PreviewListenerExposureNotice). That notice replaces
// the control-plane text and the ListenerServesUnauthenticatedNetwork gate for
// this case, and warns whenever the resulting preview bind is network-reachable
// — matching this emitter's "warn on every exposed write of the changed key"
// discipline, so an operator moving an ALREADY network-bound preview between
// two non-loopback addresses is still warned (the apply-time transition emitter
// in daemon/config_apply.go handles the once-per-transition INTO exposure, and
// is not transition-gated for "still exposed, just moving"). The flat alias
// spelling "preview_listen_addr" canonicalizes to "network.preview_listen_addr"
// before the switch, so both CLI spellings warn.
//
// The exposure test is ListenerServesUnauthenticatedNetwork — the SAME predicate
// the daemon's refusal builds on, itself built on the IsLoopbackListenAddr the
// token gate derives from. Two definitions of "is this exposed" drifting apart
// is precisely how a security check rots, so there is only one.
func exposureWarning(cfg *Config, key string) string {
	if cfg == nil {
		return ""
	}
	key = canonicalConfigKey(key)
	switch key {
	case "network.listen_addr", "network.require_token":
		return ListenerExposureNotice(cfg)
	case "network.preview_listen_addr":
		// PreviewListenerExposureNotice is itself non-transition-gated: it returns
		// a notice whenever cfg.PreviewListenAddr is non-empty and non-loopback,
		// so a write that moves an already-exposed preview between two non-loopback
		// addresses still warns — parity with the control-plane case above, which
		// warns on every exposed write of the changed key.
		return PreviewListenerExposureNotice(cfg)
	default:
		return ""
	}
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
		if s, found := settableKeySpecs[prefix]; found && rest != "" && !strings.Contains(rest, ".") {
			if s.dynamic {
				return prefix, rest, s, true
			}
			if leafSpec, allowed := s.subkeys[rest]; allowed {
				return prefix, rest, settableKeySpec{
					kind: leafSpec.kind, section: prefix, parent: prefix, validate: leafSpec.validate,
				}, true
			}
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
	result, _, err := SetGlobalConfigValueWithDigest(key, rawValue)
	return result, err
}

// SetGlobalConfigValueWithDigest is SetGlobalConfigValue plus the digest of the
// config.toml bytes this save left on disk, for the callers that then ask a
// running daemon to apply them: comparing this against the digest the apply's
// own load reports is how a save decides whether it may claim the daemon is
// serving THIS save's value (#4247).
//
// The digest is returned BESIDE SetResult rather than on it. SetResult crosses
// the net/rpc control socket and is the `af config set --json` payload, and gob
// encodes every exported field whatever its json tag says — so a digest hung on
// SetResult would silently become part of two wire contracts to serve a
// comparison that never leaves this process. config.ConfigDigest's own fields
// are unexported for the same reason.
func SetGlobalConfigValueWithDigest(key, rawValue string) (*SetResult, ConfigDigest, error) {
	if err := RetiredThemeKeyError(key); err != nil {
		return nil, ConfigDigest{}, err
	}
	if err := removedKeyError(key); err != nil {
		return nil, ConfigDigest{}, err
	}
	section, leaf, spec, ok := resolveSettable(key)
	if !ok {
		return nil, ConfigDigest{}, unsettableConfigKeyError(key, "")
	}
	key = canonicalConfigKey(key)
	structured := spec.structured && section == ""
	canonical, encoded, err := canonicalizeConfigValue(key, spec, structured, rawValue)
	if err != nil {
		return nil, ConfigDigest{}, fmt.Errorf("invalid value for %s: %w", key, err)
	}
	if !structured && spec.validate != nil {
		if err := spec.validate(leaf, canonical); err != nil {
			return nil, ConfigDigest{}, err
		}
	}

	// Ensure config.toml exists (migrating a legacy config.json if needed) and
	// that the current config actually loads, so a later parse failure is
	// unambiguously our edit's fault, not a pre-existing broken file. This is a
	// PRECONDITION only — the loaded values are deliberately not carried into the
	// write. See scalarWrite.apply for why (#2412).
	if _, err := LoadConfig(); err != nil {
		return nil, ConfigDigest{}, fmt.Errorf("refusing to write: the current config does not load: %w", err)
	}
	configDir, err := GetConfigDir()
	if err != nil {
		return nil, ConfigDigest{}, err
	}
	tomlPath := filepath.Join(configDir, TomlConfigFileName)
	prettyPath := prettyHomePath(tomlPath)

	write := scalarWrite{key: key, section: section, leaf: leaf, canonical: canonical, encoded: encoded, structured: structured,
		rawStructured: rawValue, clear: spec.kind == cfgStringList && canonical == ""}

	var result *SetResult
	var digest ConfigDigest
	writeErr := withFollowedFileLock(tomlPath, func(locked lockedTarget) error {
		var err error
		result, digest, err = write.apply(locked, prettyPath)
		return err
	})
	if writeErr != nil {
		return nil, ConfigDigest{}, writeErr
	}
	if warn := defaultAccountWriteWarning(key, leaf, canonical); warn != "" {
		result.Warnings = append(result.Warnings, warn)
	}
	return result, digest, nil
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
	if err := RetiredThemeKeyError(key); err != nil {
		return nil, err
	}
	if err := removedKeyError(key); err != nil {
		return nil, err
	}
	project, err := ResolveProjectSelector(selector)
	if err != nil {
		return nil, err
	}
	section, leaf, spec, err := resolveProjectSettable(selector, key)
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
	write := scalarWrite{key: key, section: section, leaf: leaf, canonical: canonical, encoded: encoded, structured: structured,
		rawStructured: rawValue}

	var result *SetResult
	writeErr := WithFileLock(path, func() error {
		var err error
		result, err = write.applyProject(path, prettyPath)
		return err
	})
	if writeErr != nil {
		return nil, writeErr
	}
	// The same note the global write produces (#3386). A per-project account
	// default is the shape this key exists for, so it is the one that most needs
	// to say "that account is not registered yet" at the moment it is typed.
	if warn := defaultAccountWriteWarning(key, leaf, canonical); warn != "" {
		result.Warnings = append(result.Warnings, warn)
	}
	// A project-scoped branch_prefix is written and kept — existing configs stay
	// valid — but it is never applied (#4539); say so on the write itself. And
	// because no restart can ever apply it (the daemon names branches from its
	// frozen global prefix), the write bypasses the generic restart
	// result/notice: printing "restart them to apply" for a value that can
	// never take effect would contradict the warning.
	if warn := projectBranchPrefixWriteWarning(key); warn != "" {
		result.Warnings = append(result.Warnings, warn)
		result.RequiresRestart = false
	}
	return result, nil
}

// resolveProjectSettable maps a user key to its settable spec AND enforces that
// the key admits the personal-project layer in the manifest. The manifest is the
// single authority on which keys may live where, so the write path checks it
// before editing rather than maintaining a second per-project allowlist.
func resolveProjectSettable(selector, key string) (section, leaf string, spec settableKeySpec, err error) {
	key = canonicalConfigKey(key)
	section, leaf, spec, ok := resolveSettable(key)
	if !ok {
		return "", "", settableKeySpec{}, unsettableConfigKeyError(key, selector)
	}
	scopeKey := key
	if spec.dynamic && section != "" {
		scopeKey = section
	}
	if spec.parent != "" {
		scopeKey = spec.parent
	}
	if !isProjectPersonalKey(scopeKey) {
		return "", "", settableKeySpec{}, projectScopeError(scopeKey)
	}
	return section, leaf, spec, nil
}

// unsettableConfigKeyError keeps the complete allowlist while making the
// useful recovery local: if the user tried an unsupported leaf of a writable
// table, name the parent table's accepted compact-JSON form.
func unsettableConfigKeyError(key, projectSelector string) error {
	key = canonicalConfigKey(key)
	hint := ""
	if i := strings.IndexByte(key, '.'); i > 0 {
		parent := key[:i]
		if spec, ok := settableKeySpecs[parent]; ok && spec.structured && manifestKeyIsTable(parent) {
			projectFlag := ""
			if projectSelector != "" && isProjectPersonalKey(parent) {
				projectFlag = " --project " + ShellQuotePath(projectSelector)
			}
			hint = fmt.Sprintf(" Set the whole table with `af config set %s '<compact-json>'%s`.", parent, projectFlag)
		}
	}
	return fmt.Errorf("%q is not a settable config key.%s Settable keys: %s",
		key, hint, strings.Join(SettableKeys(), ", "))
}

func manifestKeyIsTable(key string) bool {
	for _, entry := range Manifest() {
		if entry.Key == key {
			return entry.Type == "table"
		}
	}
	return false
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
	// lock is held, so program-removal tombstones use the current snapshot.
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
//
// The second return is the digest of the bytes this write left on disk — the
// evidence a save needs to tell whether a later apply loaded ITS file or one a
// competing writer replaced (#4247). It is taken from `updated` at the moment
// locked.write commits it, so nothing can have re-read or re-encoded it in
// between.
func (w scalarWrite) apply(locked lockedTarget, prettyPath string) (*SetResult, ConfigDigest, error) {
	// Every byte read and written here is the file the caller's lock covers,
	// reached through the directory it holds open. Reading by path instead would
	// let a retarget between acquisition and write compute the edit against one
	// file and land it on another (#3688, #3697); locked.link is only what the
	// user is shown.
	current, err := locked.read()
	if err != nil && !os.IsNotExist(err) {
		return nil, ConfigDigest{}, fmt.Errorf("failed to read %s: %w", prettyPath, err)
	}
	current = stripUTF8BOM(current)
	updated := string(current)
	// What this file MEANS today, read inside the lock. A structured write merges
	// its new table onto it; the value-drift guard below compares the rewrite
	// against it. A file with no settings has no meaning to preserve — nothing to
	// merge onto and nothing to protect — so it stays unguarded on the defaults.
	before := DefaultConfig()
	guarded := !isEffectivelyEmptyToml(current)
	if guarded {
		if before, err = parseConfigTOML(current, prettyPath); err != nil {
			return nil, ConfigDigest{}, fmt.Errorf("refusing to write: the current config does not load: %w", err)
		}
	}
	var existingOverrides map[string]string
	if before != nil {
		existingOverrides = before.ProgramOverrides
	}
	if w.structured {
		w.canonical, w.encoded, err = canonicalizeStructuredValueAgainst(w.key, w.rawStructured, existingOverrides, true)
		if err != nil {
			return nil, ConfigDigest{}, fmt.Errorf("invalid value for %s: %w", w.key, err)
		}
	}
	switch {
	case w.structured:
		updated, err = setTOMLStructured(updated, w.key, w.encoded)
		if err != nil {
			return nil, ConfigDigest{}, fmt.Errorf("failed to edit %s in %s: %w", w.key, prettyPath, err)
		}
	case w.clear:
		updated, _ = deleteTOMLScalar(updated, w.section, w.leaf)
	default:
		// If the target table is already opened at the root by a top-level
		// dotted key (program_overrides.codex = …) rather than a [section]
		// header, TOML forbids re-opening it with a header, so a NEW leaf
		// has to join the table in the same dotted form. Without this guard
		// the insert appends a [section] block over the dotted table and the
		// pre-write parse gate refuses bytes that were valid before the edit
		// (mirroring the migrate guard in migrate.go). section == "" targets
		// the root block and is left untouched. When the leaf already exists
		// as a dotted key we skip the rerouting: setTOMLScalar's own
		// dottedKeyRe and tomlScalarLineMatches handle updates — including
		// keys written with whitespace or quotes around the dot — and calling
		// in with section="" would disable that syntax-aware matching.
		if w.section != "" && tomlRootDottedTable(updated, w.section) &&
			!tomlRootDottedLeafExists(updated, w.section, w.leaf) {
			updated = insertTOMLDottedLeaf(updated, w.section, w.leaf, w.encoded)
		} else {
			updated = setTOMLScalar(updated, w.section, w.leaf, w.encoded)
		}
	}
	// If this file already carries the flat compatibility spelling, keep it in
	// sync inside the same lock. A rolled-back binary ignores the new table, so
	// leaving a conflicting old value behind would make a rollback change the
	// operator's setting. Grouped-only files stay grouped-only; no deprecated key
	// is invented for a new install.
	if alias, ok := configAliasForCanonical(w.key); ok {
		if metadata, err := metadataForSource(current, locked.link, FormatTOML); err == nil {
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

	// A host list and its default are one fact. The loader drops a default that
	// names no listed host (sanitizeHosts), so a write that leaves one dangling
	// would report success for a setting that never takes effect; refuse it here,
	// against the bytes this write produces, before sanitization hides it.
	if w.key == "hosts" || w.key == "default_host" {
		if err := ValidateHostsInTOML([]byte(updated)); err != nil {
			return nil, ConfigDigest{}, fmt.Errorf("invalid value for %s: %w", w.key, err)
		}
	}
	// Final gate: the edited bytes must parse and validate exactly as the loader
	// would, so `config set` can never leave an unloadable config. The parsed
	// result is also the config this write RESULTS in — the same values
	// LoadConfig will return for these bytes, since it reaches this very
	// function (parseLoadedConfigTOML adds provenance, not values) — so it is
	// what the exposure judgment below is made against.
	resulting, err := parseConfigTOML([]byte(updated), prettyPath)
	if err != nil {
		return nil, ConfigDigest{}, fmt.Errorf("internal error: edited config would not load (no changes written): %w", err)
	}
	// resulting is the config this write produces, and CurrentValue reads it
	// below to echo the post-write refresh form for a structured key. For
	// root_agent that render is presence-aware — it consults the on-disk shape
	// so an absent [root_agent] table shows {} rather than the zero-struct
	// {"enabled":false} — but parseConfigTOML keeps no source (it is a
	// values-only parse, not a loader), so attach the post-edit bytes' shape
	// here. This makes resulting match what LoadConfig will return for these
	// bytes — the property the comment above already claims for the field
	// values; the shape is the half CurrentValue additionally needs. Only
	// structured writes reach CurrentValue below, so the attach is gated to
	// them; metadataForSource cannot fail on the bytes parseConfigTOML just
	// accepted.
	if w.structured {
		if err := attachConfigSource(resulting, []byte(updated), locked.link, FormatTOML); err != nil {
			return nil, ConfigDigest{}, fmt.Errorf("internal error: edited config would not load (no changes written): %w", err)
		}
	}
	// Second gate, and the one the parse cannot give: the edit must have landed
	// on the right line. A surgical edit that hit a decoy inside somebody's
	// multiline value produces valid TOML that MEANS something else (#3662), so
	// anything but the key asked for — plus the machine-managed schema marker
	// written just above — is a refusal that names what would have moved.
	if guarded {
		if drift := configRewriteDrift(before, resulting, w.key, SchemaVersionField); drift != "" {
			return nil, ConfigDigest{}, fmt.Errorf("internal error: setting %s in %s would change %s (no changes written)", w.key, prettyPath, drift)
		}
	}
	// #5137: refuse to leave the file in a posture the daemon will refuse to
	// bind — a non-loopback listen_addr with the token off and no explicit
	// opt-in. Judged on `resulting` (the locked bytes' meaning), so two racing
	// writers each touching one half of the pairing still converge on the
	// refusal rather than each seeing a stale safe half (#2412). The message is
	// ListenerBindRefusal's own, so the write refusal and the daemon's bind
	// refusal read identically. Nothing is written.
	if refusal := listenerWriteRefusal(w.key, resulting); refusal != "" {
		return nil, ConfigDigest{}, fmt.Errorf("refusing to write: %s", refusal)
	}
	if err := locked.write([]byte(updated), 0644); err != nil {
		return nil, ConfigDigest{}, err
	}
	// Hashed from the same bytes that were just committed, not from a re-read of
	// the file: a re-read would be a readback, and could only report what some
	// LATER writer left there.
	digest := digestConfigBytes([]byte(updated))
	value := w.canonical
	if w.structured {
		value, _ = CurrentValue(resulting, w.key)
	}
	result := &SetResult{Key: w.key, Value: value, Path: locked.link, RequiresRestart: true}
	if warn := exposureWarning(resulting, w.key); warn != "" {
		result.Warnings = append(result.Warnings, warn)
	}
	return result, digest, nil
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
	var before *ProjectConfig
	guarded := !isEffectivelyEmptyToml(current)
	if guarded {
		if before, err = parseProjectConfig(current, path); err != nil {
			return nil, fmt.Errorf("refusing to write: the current personal project config does not load: %w", err)
		}
	}
	var existingOverrides map[string]string
	if before != nil {
		existingOverrides = before.ProgramOverrides
	}
	if w.structured {
		w.canonical, w.encoded, err = canonicalizeStructuredValueAgainst(w.key, w.rawStructured, existingOverrides, false)
		if err != nil {
			return nil, fmt.Errorf("invalid value for %s: %w", w.key, err)
		}
		updated, err = setTOMLStructured(updated, w.key, w.encoded)
		if err != nil {
			return nil, fmt.Errorf("failed to edit %s in %s: %w", w.key, prettyPath, err)
		}
	} else {
		// See apply: a table opened at the root by a dotted key cannot be
		// re-opened with a [section] header, so a NEW leaf must join it in
		// the dotted form the migrate path uses. Skip the rerouting when the
		// leaf already exists: setTOMLScalar's own dottedKeyRe handles
		// updates even when the existing key uses whitespace or quoting.
		if w.section != "" && tomlRootDottedTable(updated, w.section) &&
			!tomlRootDottedLeafExists(updated, w.section, w.leaf) {
			updated = insertTOMLDottedLeaf(updated, w.section, w.leaf, w.encoded)
		} else {
			updated = setTOMLScalar(updated, w.section, w.leaf, w.encoded)
		}
	}
	resulting, err := parseProjectConfig([]byte(updated), path)
	if err != nil {
		return nil, fmt.Errorf("internal error: edited personal project config would not load (no changes written): %w", err)
	}
	if guarded {
		if drift := projectRewriteDrift(before, resulting, w.key); drift != "" {
			return nil, fmt.Errorf("internal error: setting %s in %s would change %s (no changes written)", w.key, prettyPath, drift)
		}
	}
	// An account_handoff write commits a MERGED table, not just this file:
	// the leaves passed their own checks, but ordering binds the effective
	// values the resolver produces. Validate the overlay onto the global
	// layer now rather than persist a file every subsequent resolve fails
	// on (Codex on #6793).
	if IsAccountHandoffKey(w.key) {
		if err := validatePersonalAccountHandoffMerge(resulting); err != nil {
			return nil, err
		}
	}
	value := w.canonical
	if w.structured {
		_, err = canonicalStructuredTOMLValue(updated, w.key)
		if err != nil {
			return nil, fmt.Errorf("internal error: cannot read back %s from %s (no changes written): %w", w.key, prettyPath, err)
		}
		var ok bool
		value, ok = projectStructuredCurrentValue(resulting, w.key)
		if !ok {
			return nil, fmt.Errorf("internal error: cannot read back %s from %s (no changes written)", w.key, prettyPath)
		}
	}
	if err := AtomicWriteFile(path, []byte(updated), 0644); err != nil {
		return nil, err
	}
	return &SetResult{Key: w.key, Value: value, Path: path, RequiresRestart: true}, nil
}

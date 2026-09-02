package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/aymanbagabas/go-udiff"
)

// MigrateGlobalConfig rewrites the deprecated spellings in the global config
// to their current ones, in place (#3624). It is the verb every deprecated-key
// warning now names.
//
// The contract, in the order it matters:
//
//   - It changes SPELLING, never meaning. Every migrated value is carried over
//     byte-for-byte where the source wrote it on one line, and the rewritten
//     file is re-parsed and compared against the original config before
//     anything is written: a migration that would change a single effective
//     value is an internal error, not a write.
//   - It is idempotent. A second run finds no deprecated flat spelling and
//     reports that there was nothing to migrate.
//   - It refuses rather than choose. A key written in BOTH spellings with
//     DIFFERENT values has no migration that does not pick a value for the
//     reader, so the whole run refuses, names the key, and writes nothing.
//   - It leaves what it cannot rewrite, and says so. root_agents' successor is
//     a registered project's personal [root_agent], so migrating it would mean
//     inventing project registrations — durable state outside this file. It is
//     reported with the manual step and left exactly where it is, and the keys
//     that CAN move still move: one un-rewritable key must not block the
//     migration that is available.
//   - The readers of the old spellings stay, so an older config keeps loading and
//     the running configuration is untouched. The reverse is narrower and worth
//     stating plainly: the GROUPED spellings have only been read since #3354
//     (2026-08-14), so an af older than that falls back to the built-in default
//     for a migrated key instead of reading it. For every key in the table that
//     default is the conservative one — a loopback listener, strict host-key
//     verification, no credential mount — so a downgrade past that boundary
//     loses the setting in the safe direction, and the .bak restores it.
//
// A legacy config.json is converted to config.toml on the way in (the ordinary
// LoadConfig conversion), which is what makes the flat JSON spellings — the
// permanent, correct spelling in that format — reachable by this at all.
func MigrateGlobalConfig() (*MigrationResult, error) {
	configDir, err := GetConfigDir()
	if err != nil {
		return nil, err
	}
	tomlPath := filepath.Join(configDir, TomlConfigFileName)

	// Whether the precondition below is about to CONVERT a legacy config.json
	// has to be observed before it runs. Reporting "nothing to migrate" after
	// silently rewriting the user's config into a different file and moving the
	// original aside would be false in the way that matters most — it describes
	// a run that changed nothing when the run changed which file af reads
	// (#3624 review).
	converting := !fileExists(tomlPath) && fileExists(filepath.Join(configDir, ConfigFileName))

	// Precondition, exactly as `af config set` uses it: materialize or convert
	// so config.toml exists, and prove the current file loads, so a later parse
	// failure is unambiguously this migration's fault.
	if _, err := LoadConfig(); err != nil {
		return nil, fmt.Errorf("refusing to migrate: the current config does not load: %w", err)
	}

	var result *MigrationResult
	if err := WithFollowedFileLock(tomlPath, func() error {
		var err error
		result, err = migrateConfigFile(tomlPath)
		return err
	}); err != nil {
		return nil, err
	}
	result.ConvertedFromJSON = converting && fileExists(tomlPath)
	return result, nil
}

// MigrationResult is one `af config migrate` run.
type MigrationResult struct {
	// Path is the file that was examined, whether or not it changed.
	Path string `json:"path"`
	// Backup is the copy of the pre-migration file, empty when nothing was
	// written.
	Backup string `json:"backup,omitempty"`
	// Migrated lists the keys that moved to their current spelling.
	Migrated []MigratedKey `json:"migrated"`
	// Left lists the deprecated keys that have no in-file migration and so
	// stayed exactly where they were, each with the step that ends it.
	Left []UnmigratedKey `json:"left,omitempty"`
	// Diff is the unified diff of the rewrite, empty when nothing changed.
	Diff string `json:"diff,omitempty"`
	// ConvertedFromJSON records that a legacy config.json was converted to
	// config.toml on the way in — af's ordinary conversion, but it moves the
	// user's original aside, so a run that reports no migrated keys still has
	// something to say for itself.
	ConvertedFromJSON bool `json:"converted_from_json,omitempty"`
	// Cautions are consequences of THIS migration the reader has to know about,
	// not general advice — today, the one downgrade that costs a security
	// setting rather than a convenience one.
	Cautions []string `json:"cautions,omitempty"`
}

// Changed reports whether the run rewrote the file.
func (r *MigrationResult) Changed() bool { return r != nil && len(r.Migrated) > 0 }

// MigratedKey is one deprecated key rewritten to its current spelling.
type MigratedKey struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Value is the setting's effective value, in the canonical form `af config
	// get` prints — the same string whichever spelling the file happened to use,
	// so a --json caller can compare two migrations of one key. It is reported so
	// a caller can show the migration moved a value rather than changed one; the
	// raw TOML token that was actually relocated is deliberately not exposed.
	Value string `json:"value"`
	// Redundant marks a key that was already written in BOTH spellings with the
	// same value: nothing moved, the stale flat line was simply dropped.
	Redundant bool `json:"redundant,omitempty"`
}

// UnmigratedKey is one deprecated key `af config migrate` deliberately left in
// place, with the manual step that ends its warning.
type UnmigratedKey struct {
	Key  string `json:"key"`
	Step string `json:"step"`
	// Detail names the specific instances the step applies to (the root_agents
	// map's paths), so the reader does not have to open the file to find them.
	Detail []string `json:"detail,omitempty"`
}

// migrateConfigFile is MigrateGlobalConfig's body, minus the lock and the
// load precondition, so tests can drive it against a file directly.
func migrateConfigFile(tomlPath string) (*MigrationResult, error) {
	prettyPath := prettyHomePath(tomlPath)
	raw, err := os.ReadFile(tomlPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", prettyPath, err)
	}
	// The loader supports a leading BOM, so a migration must not quietly strip
	// one. Edits run on the stripped text — every surgical helper expects that —
	// and the prefix goes back on at the write (#3624 review).
	stripped := stripUTF8BOM(raw)
	bom := string(raw[:len(raw)-len(stripped)])
	before := string(stripped)
	metadata, err := metadataForSource(raw, tomlPath, FormatTOML)
	if err != nil {
		return nil, tomlParseError("config file "+prettyPath, err)
	}
	beforeCfg, err := parseConfigTOML([]byte(before), prettyPath)
	if err != nil {
		return nil, fmt.Errorf("refusing to migrate: the current config does not load: %w", err)
	}

	result := &MigrationResult{Path: tomlPath, Migrated: []MigratedKey{}}
	content := before
	for _, deprecation := range configDeprecations() {
		if !deprecation.migratable() {
			if left, ok := unmigratedKey(deprecation, metadata.shape); ok {
				result.Left = append(result.Left, left)
			}
			continue
		}
		alias := *deprecation.alias
		flat, present := metadata.shape[alias.legacy]
		if !present {
			continue
		}
		grouped, groupedPresent := aliasGroupedValue(metadata.shape, alias)
		if groupedPresent {
			if !reflect.DeepEqual(flat, grouped) {
				return nil, ambiguousSpellingError(prettyPath, alias, beforeCfg)
			}
			// Same value in both spellings: the flat line carries no information
			// the grouped one does not, so dropping it changes nothing at all.
			updated, removed := deleteTOMLScalar(content, "", alias.legacy)
			if !removed {
				// The decoded shape says the key is there, so a delete that finds
				// no line means the two disagree about the file. Reporting success
				// here would tell the reader the warning is over while the key —
				// and the warning — are still in the file.
				return nil, unremovableKeyError(alias.legacy, prettyPath)
			}
			content = updated
			value, _ := CurrentValue(beforeCfg, alias.canonical)
			result.Migrated = append(result.Migrated, MigratedKey{
				From: alias.legacy, To: alias.canonical, Value: value, Redundant: true,
			})
			continue
		}
		encoded, ok := tomlRootScalarRawValue(content, alias.legacy)
		if !ok {
			// A value spread over several lines (an array) is re-encoded from the
			// decoded config rather than relocated as raw bytes.
			if encoded, err = encodeAliasValue(beforeCfg, alias); err != nil {
				return nil, err
			}
		}
		// REMOVE BEFORE INSERT, and the order is the fix rather than a preference.
		// Both surgical helpers find a section by scanning for [header] lines, and
		// neither tracks TOML multiline-string state — so a deprecated value that
		// happens to CONTAIN its own destination header, as a free-form
		// `sandbox_ssh = '''…\n[sandbox]\n…'''` command legitimately can, makes
		// the insert believe that section is already open and place the leaf inside
		// the string. Deleting the source first takes that text out of the document
		// before anything scans it. The value was lifted above, so nothing is lost
		// by removing the line early (#3624 review).
		updated, removed := deleteTOMLScalar(content, "", alias.legacy)
		if !removed {
			return nil, unremovableKeyError(alias.legacy, prettyPath)
		}
		content = updated
		if tomlRootDottedTable(content, alias.section) {
			// The destination table is already open as a dotted key, and TOML will
			// not let a [header] re-open it. Join it in the same form.
			content = setTOMLScalar(content, "", alias.section+"."+alias.leaf, encoded)
		} else {
			content = setTOMLScalar(content, alias.section, alias.leaf, encoded)
		}
		// Value is the EFFECTIVE value, never the raw TOML token: a --json caller
		// comparing two migrations of the same setting must not get "0.0.0.0:8443"
		// from one and "'0.0.0.0:8443'" from the other purely because one file
		// happened to write the redundant spelling (#3624 review). The source's
		// own bytes are what got written to the file; they are not the report.
		effective, _ := CurrentValue(beforeCfg, alias.canonical)
		result.Migrated = append(result.Migrated, MigratedKey{From: alias.legacy, To: alias.canonical, Value: effective})
	}
	if len(result.Migrated) == 0 {
		return result, nil
	}

	// The rewritten file must load, and must load to the SAME configuration.
	// This is the whole safety claim of a spelling migration, so it is asserted
	// against the parsed result rather than argued from the edit.
	afterCfg, err := parseConfigTOML([]byte(content), prettyPath)
	if err != nil {
		return nil, fmt.Errorf("internal error: the migrated config would not load (no changes written): %w", err)
	}
	if diff := configValueDrift(beforeCfg, afterCfg); diff != "" {
		return nil, fmt.Errorf("internal error: migrating %s would change %s (no changes written)", prettyPath, diff)
	}

	// The backup belongs beside the file actually being rewritten. When
	// config.toml is a symlink that is the target, not the link's directory:
	// it is the copy a dotfiles `git status` will show, and putting it next to
	// the link would scatter the two halves across two repositories (#3660).
	realPath, err := resolveWriteTarget(tomlPath)
	if err != nil {
		return nil, err
	}
	backup, err := availableBackupPath(realPath + ".bak")
	if err != nil {
		return nil, err
	}
	// Both writes carry the ORIGINAL file's mode rather than a fresh default.
	// An operator who deliberately made config.toml owner-only must not find it
	// world-readable because they took a migration — widening a live file's
	// permissions is not something a spelling rewrite gets to do — and the
	// backup exists to be copied back over it, so a restore must not change
	// them either (#3624 review).
	mode := os.FileMode(0644)
	if info, statErr := os.Stat(tomlPath); statErr == nil {
		mode = info.Mode().Perm()
	}
	// The backup path is already derived from the RESOLVED config, and
	// availableBackupPath only returns a path that does not exist — so there is
	// no link to follow here and the plain writer says so.
	if err := AtomicWriteFile(backup, raw, mode); err != nil {
		return nil, fmt.Errorf("failed to write the backup %s (no changes written): %w", prettyHomePath(backup), err)
	}
	if err := AtomicWriteFileFollowingLink(tomlPath, []byte(bom+content), mode); err != nil {
		return nil, err
	}
	result.Backup = backup
	// go-udiff is x/tools' diff implementation, already in this module's graph.
	// The reader is being shown a rewrite of their own file, so the rendering
	// should be the ordinary unified diff they already know how to read — and one
	// this repo does not have to own a differ to produce.
	result.Diff = udiff.Unified(prettyPath, prettyPath, before, content)
	result.Cautions = downgradeCautions(result.Migrated, beforeCfg, prettyHomePath(backup))
	return result, nil
}

// downgradePosture is what a pre-#3354 af — one that reads no grouped spelling
// at all — sees for the three keys that decide whether af's control plane is
// reachable and whether reaching it needs a token.
type downgradePosture struct {
	listen               string
	requireToken         bool
	requireLoopbackToken bool
}

// reachable reports whether this posture serves the control plane. An empty
// listen address is the documented way to turn the web server off.
func (p downgradePosture) reachable() bool { return p.listen != "" }

// authenticated reports whether reaching that control plane actually costs a
// token. It mirrors daemon.webListenerPolicy rather than restating
// require_token: that key gates NON-loopback peers, and a loopback bind exempts
// its peers — which are then the only reachable ones — unless
// require_loopback_token withdraws the exemption.
//
// Without this, `require_token = true` on the default loopback listener looks
// like authentication when nothing is authenticated, and migrating it would
// warn about losing a protection that was never in effect (#3624 review).
func (p downgradePosture) authenticated() bool {
	if !p.reachable() || !p.requireToken {
		return false
	}
	return !IsLoopbackListenAddr(p.listen) || p.requireLoopbackToken
}

// downgradeCautions reports what THIS migration costs a reader who later runs an
// af predating the grouped spellings (#3354, 2026-08-14).
//
// It compares postures rather than testing keys one at a time, because two
// rounds of review found cases a per-key rule missed — an already-grouped
// require_token that made the advised restore useless, and a deliberately empty
// listen_addr whose loss turns the web server ON. Both fall out of the same
// question: what did an older binary see before this run, and what does it see
// after? A key this run migrated was FLAT, so that binary read the configured
// value then and reads the built-in default now. A key it did not migrate was
// already grouped or absent, so the default applied in both worlds and nothing
// changed for that binary.
//
// The values come from the PARSED config, never from MigratedKey.Value: that
// field is presentation, and a security decision must not match TOML source text.
func downgradeCautions(migrated []MigratedKey, cfg *Config, backup string) []string {
	moved := make(map[string]bool, len(migrated))
	for _, key := range migrated {
		moved[key.To] = true
	}

	defaults := DefaultConfig()
	// After the migration every one of these is grouped, so an older binary reads
	// the built-in defaults for all of them.
	after := downgradePosture{
		listen:               defaults.ListenAddr,
		requireToken:         defaults.RequireToken,
		requireLoopbackToken: defaults.RequireLoopbackToken,
	}
	before := after
	if moved["network.listen_addr"] {
		before.listen = cfg.ListenAddr
	}
	if moved["network.require_token"] {
		before.requireToken = cfg.RequireToken
	}
	if moved["network.require_loopback_token"] {
		before.requireLoopbackToken = cfg.RequireLoopbackToken
	}

	switch {
	case !before.reachable() && after.reachable():
		// The operator turned the web server OFF, in the one spelling an older
		// binary can read. Migrating hides that from it, and its default is a
		// LIVE listener — so the downgrade does not weaken the control plane, it
		// creates one where there was none.
		return []string{fmt.Sprintf(
			"network.listen_addr is empty — the web server is off — and moving it to the grouped spelling hides that from "+
				"an af older than 2026-08-14 (#3354) · such a binary would fall back to the default %s listener and serve "+
				"the control plane to every local user with no token · restore %s before downgrading past that release",
			after.listen, backup)}

	case before.authenticated() && !after.authenticated():
		// Reaching the control plane cost a token before and does not after.
		// network.require_loopback_token is only named when it travelled too: on
		// its own it is inert, since it tightens a token that require_token must
		// first turn on.
		lost := []string{"network.require_token"}
		if before.requireLoopbackToken {
			lost = append(lost, "network.require_loopback_token")
		}
		return []string{fmt.Sprintf(
			"%s moved to the grouped spelling, which af only began reading on 2026-08-14 (#3354) · an older af reads "+
				"neither spelling, falls back to false, and keeps serving the control plane on the default %s listener "+
				"with no token — restore %s before downgrading past that release",
			strings.Join(lost, " and "), after.listen, backup)}
	}
	return nil
}

// unmigratedKey builds the report for a deprecation with no in-file migration,
// or reports false when the file does not carry it. The presence test comes off
// the table entry, so it is by construction the same test the warning uses —
// migrate cannot stay silent about a key the loader just warned on.
func unmigratedKey(deprecation configDeprecation, shape map[string]any) (UnmigratedKey, bool) {
	if deprecation.present == nil {
		return UnmigratedKey{}, false
	}
	detail, ok := deprecation.present(shape)
	if !ok {
		return UnmigratedKey{}, false
	}
	return UnmigratedKey{Key: deprecation.key, Step: deprecation.migrationRemedy(), Detail: detail}, true
}

// ambiguousSpellingError refuses a run whose file writes one setting twice with
// two different values. Choosing between them is the reader's call — af has a
// documented winner at LOAD time (the grouped value), but silently making that
// tie-break permanent is exactly the sort of thing a migration must not do to a
// file it also backs up and rewrites.
func ambiguousSpellingError(prettyPath string, alias configKeyAlias, cfg *Config) error {
	effective, _ := CurrentValue(cfg, alias.canonical)
	return fmt.Errorf("refusing to migrate %s: %q and %q are both set, to different values — "+
		"af currently uses the grouped value (%s), and no migration should make that tie-break permanent for you; "+
		"delete whichever line is wrong, then run `af config migrate` again. Nothing was rewritten",
		prettyPath, alias.legacy, alias.canonical, echoMigrationValue(effective))
}

// unremovableKeyError reports a key the decoder found but the surgical edit
// could not locate. Both callers use it: an unusual spelling must never be
// reported as migrated, and must never be silently skipped either.
func unremovableKeyError(key, prettyPath string) error {
	return fmt.Errorf("internal error: %q is present in %s but could not be removed from the root block, "+
		"so nothing was rewritten; move it under its current spelling by hand, or open an issue with the file's shape", key, prettyPath)
}

func echoMigrationValue(v string) string {
	if v == "" {
		return `""`
	}
	return v
}

// encodeAliasValue renders an alias's current value in TOML, for the values
// tomlRootScalarRawValue declines to relocate as raw bytes.
func encodeAliasValue(cfg *Config, alias configKeyAlias) (string, error) {
	spec, ok := settableKeySpecs[alias.canonical]
	if !ok {
		return "", fmt.Errorf("config alias %q has no settable encoding", alias.canonical)
	}
	value, ok := CurrentValue(cfg, alias.canonical)
	if !ok {
		return "", fmt.Errorf("config alias %q has no runtime field", alias.canonical)
	}
	_, encoded, err := canonicalizeScalar(spec.kind, value)
	if err != nil {
		return "", fmt.Errorf("encode config alias %q: %w", alias.canonical, err)
	}
	return encoded, nil
}

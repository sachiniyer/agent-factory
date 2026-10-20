package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// UnsetResult reports a successful config unset. Removed is false when there
// was no override or alias spelling to clear, which is a clean no-op.
type UnsetResult struct {
	Key             string `json:"key"`
	Path            string `json:"path"`
	Removed         bool   `json:"removed"`
	RequiresRestart bool   `json:"requires_restart"`
}

// UnsetProjectConfigValue removes key's personal override for a project so the
// value falls back to the lower layers again (#2216 Phase 5).
func UnsetProjectConfigValue(selector, key string) (*UnsetResult, error) {
	if err := RetiredThemeKeyError(key); err != nil {
		return nil, err
	}
	if key == "auto_yes" {
		return nil, RemovedAutoYesError()
	}
	project, err := ResolveProjectSelector(selector)
	if err != nil {
		return nil, err
	}
	section, leaf, spec, err := resolveProjectSettable(selector, key)
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
		// A fixed struct-field subkey (root_agent.enabled, root_agent.program)
		// decodes onto its struct field case-insensitively (taggedFieldByKey),
		// so a case-variant on-disk spelling (Enabled = true) is the same leaf as
		// the canonical enabled but deleteTOMLScalar's exact-key line match would
		// miss it. A dynamic family (program_overrides.<name>) is a map key and is
		// case-sensitive, so it is NOT folded (#5397 review).
		caseFoldLeaf := spec.parent != "" && !spec.dynamic
		result, err = applyProjectUnset(path, prettyPath, section, leaf, key, spec.structured && section == "", caseFoldLeaf)
		return err
	})
	if writeErr != nil {
		return nil, writeErr
	}
	// Removing a stored per-project branch_prefix needs no restart either: the
	// value was never applied (#4539), so nothing a restart could do changes.
	if result.Removed && canonicalConfigKey(key) == "branch_prefix" {
		result.RequiresRestart = false
	}
	return result, nil
}

// applyProjectUnset removes the target key line from a project's config.toml
// under the caller-held lock. A missing file or absent key is a clean no-op.
// caseFoldLeaf reports whether leaf is a fixed struct-field subkey whose on-disk
// spelling may be a case variant of the canonical key the decoder matches (e.g.
// root_agent.Enabled for root_agent.enabled); deleteTOMLScalar must target the
// physical spelling on disk, or a case-variant [root_agent] Enabled = true would
// survive an unset that the loader treats as effective (#5397 review).
func applyProjectUnset(path, prettyPath, section, leaf, key string, structured, caseFoldLeaf bool) (*UnsetResult, error) {
	current, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &UnsetResult{Key: key, Path: path, Removed: false}, nil
		}
		return nil, fmt.Errorf("failed to read %s: %w", prettyPath, err)
	}
	current = stripUTF8BOM(current)
	updated := string(current)
	removed := false
	if structured {
		updated, err = removeTOMLTopLevelValue(updated, key)
		if err != nil {
			return nil, fmt.Errorf("failed to remove %s from %s: %w", key, prettyPath, err)
		}
		removed = updated != string(current)
	} else {
		// A fixed struct-field subkey (root_agent.enabled, root_agent.program)
		// can carry more than one case-equivalent on-disk spelling at once — a
		// hand-written [root_agent] Enabled = true plus a later enabled = false
		// set over it, or any pair the operator typed. Each one decodes onto the
		// same struct field, so deleting only the canonical (or any single)
		// spelling leaves the surviving variant setting the field and the unset
		// reports Removed: true while restoring nothing (#5397 review). Collect
		// every case-folded spelling and delete each, so the field is actually
		// cleared. Dynamic map keys never reach this branch (caseFoldLeaf gates
		// them out and stays exact-match).
		spellings := []string{leaf}
		if caseFoldLeaf {
			spellings = physicalLeafSpellingsForUnset(updated, section, leaf)
		}
		removed = false
		for _, sp := range spellings {
			var r bool
			updated, r = deleteTOMLScalar(updated, section, sp)
			if r {
				removed = true
			}
		}
	}
	if !removed {
		return &UnsetResult{Key: key, Path: path, Removed: false}, nil
	}
	if projectConfigHasNoTopLevelKeys(updated) {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("failed to remove emptied %s: %w", prettyPath, err)
		}
		return &UnsetResult{Key: key, Path: path, Removed: true, RequiresRestart: true}, nil
	}
	// Read after the no-op returns above, so an absent key on a file this loader
	// would reject stays the clean no-op it has always been.
	before, err := parseProjectConfig(current, path)
	if err != nil {
		return nil, fmt.Errorf("refusing to write: the current personal project config does not load: %w", err)
	}
	resulting, err := parseProjectConfig([]byte(updated), path)
	if err != nil {
		return nil, fmt.Errorf("internal error: edited personal project config would not load (no changes written): %w", err)
	}
	// The delete must have removed the target's line and nothing else. A parse
	// gate cannot tell those apart — a line lifted out of somebody's multiline
	// value leaves valid TOML behind (#3662) — so the values and the presence
	// map are compared instead, and anything else that moved is named.
	if drift := projectRewriteDrift(before, resulting, key); drift != "" {
		return nil, fmt.Errorf("internal error: unsetting %s in %s would change %s (no changes written)", key, prettyPath, drift)
	}
	if err := AtomicWriteFile(path, []byte(updated), 0o644); err != nil {
		return nil, err
	}
	return &UnsetResult{Key: key, Path: path, Removed: true, RequiresRestart: true}, nil
}

func projectConfigHasNoTopLevelKeys(content string) bool {
	if strings.TrimSpace(content) == "" {
		return true
	}
	var shape map[string]any
	if err := toml.Unmarshal([]byte(content), &shape); err != nil {
		return false
	}
	return len(shape) == 0
}

// UnsetGlobalConfigValue removes both storage spellings of one migrated alias
// under the global config lock. Removing only the grouped winner would silently
// resurrect a conflicting flat value, so an alias is one effective setting for
// unset just as it is for get and set.
func UnsetGlobalConfigValue(key string) (*UnsetResult, error) {
	result, _, err := UnsetGlobalConfigValueWithDigest(key)
	return result, err
}

// UnsetGlobalConfigValueWithDigest is UnsetGlobalConfigValue plus the digest of
// the config.toml bytes this unset left on disk, for the same reason and with
// the same placement rule as SetGlobalConfigValueWithDigest: it rides beside
// UnsetResult, never on it (#4247).
func UnsetGlobalConfigValueWithDigest(key string) (*UnsetResult, ConfigDigest, error) {
	if err := RetiredThemeKeyError(key); err != nil {
		return nil, ConfigDigest{}, err
	}
	if key == "auto_yes" {
		return nil, ConfigDigest{}, RemovedAutoYesError()
	}
	canonicalKey := canonicalConfigKey(key)
	alias, ok := configAliasForCanonical(canonicalKey)
	if !ok {
		return nil, ConfigDigest{}, fmt.Errorf("%q is not a globally unsettable migrated config key", key)
	}
	if _, err := LoadConfig(); err != nil {
		return nil, ConfigDigest{}, fmt.Errorf("refusing to write: the current config does not load: %w", err)
	}
	configDir, err := GetConfigDir()
	if err != nil {
		return nil, ConfigDigest{}, err
	}
	path := filepath.Join(configDir, TomlConfigFileName)
	prettyPath := prettyHomePath(path)

	var result *UnsetResult
	var digest ConfigDigest
	writeErr := withFollowedFileLock(path, func(locked lockedTarget) error {
		var err error
		result, digest, err = applyGlobalUnset(locked, prettyPath, canonicalKey, alias)
		return err
	})
	if writeErr != nil {
		return nil, ConfigDigest{}, writeErr
	}
	return result, digest, nil
}

// applyGlobalUnset removes both storage spellings of one alias from the global
// config.toml under the caller-held lock. It is separate from its caller so the
// value-drift guard below can be driven directly, with a canonicalKey and an
// alias that name different settings — which is exactly the shape of the bug the
// guard exists to catch: an edit that does not do what the command says it does.
//
// The second return is the digest of the bytes this unset left on disk, as
// described on UnsetGlobalConfigValueWithDigest. A no-op unset reports the bytes
// it READ rather than an unknown digest: nothing was written, so those bytes are
// what the file holds, and they are a file in which the key is already absent —
// exactly the state the no-op claims. A read that found no file at all reports
// unknown, because then there are no bytes to stand behind.
func applyGlobalUnset(locked lockedTarget, prettyPath, canonicalKey string, alias configKeyAlias) (*UnsetResult, ConfigDigest, error) {
	// The read is of the locked file, and so is the write below: neither a
	// config.toml that is a symlink nor an AF home that is one may be resolved a
	// second time inside the lock (#3688, #3697).
	raw, err := locked.read()
	readOK := err == nil
	if err != nil {
		// A missing config.toml under the lock is an empty document, exactly
		// as loadConfigLocked answers it: a zero-byte config.json can satisfy
		// the pre-lock LoadConfig with in-memory defaults while this file
		// stays absent (#4483 review), or the file was removed in the window
		// between that load and acquisition. Either way there is no key to
		// remove — the answer is "not set", not an ENOENT the user cannot act
		// on. Materializing a file just to report that would write where a
		// mid-flight rewrite may be landing.
		if !os.IsNotExist(err) {
			return nil, ConfigDigest{}, fmt.Errorf("failed to read %s: %w", prettyPath, err)
		}
	}
	// The digest is taken from raw, before the BOM is stripped: it must describe
	// the file's actual bytes, which is what a later load reads.
	current := stripUTF8BOM(raw)
	updated, groupedRemoved := deleteTOMLScalar(string(current), alias.section, alias.leaf)
	updated, legacyRemoved := deleteTOMLScalar(updated, "", alias.legacy)
	if !groupedRemoved && !legacyRemoved {
		// "Nothing to remove" is a claim about a file, so it has to be a claim
		// about the RIGHT file. Only the write path confirms otherwise, and a
		// no-op takes it: a link retargeted to a config that does have the key
		// would leave the user told it was already absent, which is the same
		// silent wrong answer in the shape of a report rather than a lost
		// update (#3696 review).
		if err := locked.confirm(); err != nil {
			return nil, ConfigDigest{}, err
		}
		var digest ConfigDigest
		if readOK {
			digest = digestConfigBytes(raw)
		}
		return &UnsetResult{Key: canonicalKey, Path: locked.link, Removed: false}, digest, nil
	}
	// Read after the no-op return above, for the same reason as its personal
	// twin: nothing is being written, so nothing needs vouching for.
	before, err := parseConfigTOML(current, prettyPath)
	if err != nil {
		return nil, ConfigDigest{}, fmt.Errorf("refusing to write: the current config does not load: %w", err)
	}
	updated = setTOMLScalar(updated, "", SchemaVersionField, strconv.Itoa(GlobalConfigSchemaVersion))
	resulting, err := parseConfigTOML([]byte(updated), prettyPath)
	if err != nil {
		return nil, ConfigDigest{}, fmt.Errorf("internal error: edited config would not load (no changes written): %w", err)
	}
	// Removing a line that only LOOKED like the key leaves valid TOML behind
	// (#3662), so the parse above cannot vouch for the delete. This can: the
	// unset key and the machine-managed schema marker are the only values the
	// rewrite may move.
	if drift := configRewriteDrift(before, resulting, canonicalKey, SchemaVersionField); drift != "" {
		return nil, ConfigDigest{}, fmt.Errorf("internal error: unsetting %s in %s would change %s (no changes written)", canonicalKey, prettyPath, drift)
	}
	if err := locked.write([]byte(updated), 0o644); err != nil {
		return nil, ConfigDigest{}, err
	}
	// Same rule as scalarWrite.apply: hashed from the committed bytes, never
	// from a re-read.
	return &UnsetResult{Key: canonicalKey, Path: locked.link, Removed: true, RequiresRestart: true}, digestConfigBytes([]byte(updated)), nil
}

// physicalLeafSpellingsForUnset returns every on-disk spelling of leaf under
// [section] that the typed decoder folds onto the same struct field as leaf, so
// the unset path can remove each one. A fixed struct-field subkey
// (root_agent.enabled, root_agent.program) decodes case-insensitively
// (taggedFieldByKey), so a personal [root_agent] Enabled = true and a
// hand-written enabled = false are the same effective leaf and both must be
// removed to restore inheritance — deleting only the canonical (or any single)
// spelling would report Removed: true while a surviving variant re-sets the
// field (#5397 review). Dynamic map keys are case-sensitive and never reach
// this path (caseFoldLeaf gates them out). Returns []string{leaf} when section
// is empty, the section is absent, or no case-variant is present, so callers
// that already use the canonical spelling are unaffected.
func physicalLeafSpellingsForUnset(content, section, leaf string) []string {
	if section == "" {
		return []string{leaf}
	}
	var root map[string]any
	if err := toml.Unmarshal([]byte(content), &root); err != nil {
		return []string{leaf}
	}
	sub, ok := root[section].(map[string]any)
	if !ok {
		return []string{leaf}
	}
	var spellings []string
	for k := range sub {
		if strings.EqualFold(k, leaf) {
			spellings = append(spellings, k)
		}
	}
	if len(spellings) == 0 {
		return []string{leaf}
	}
	return spellings
}

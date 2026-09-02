package config

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// A surgical config writer edits the file's TEXT: it finds the target key's line
// and changes that line's bytes, so the comments, blank lines and key ordering
// of a file the README tells users to hand-edit survive an `af config set`.
//
// The risk that buys is the one #3662 landed on. An edit that lands on the WRONG
// line still produces valid TOML — a decoy `branch_prefix = "…"` inside somebody's
// multiline on_archive_command is a shell script's text, and rewriting it changes
// only a string's contents — so the re-parse-before-write gate every writer
// already had reported success while an unrelated setting was silently rewritten
// and the key the user asked for was left untouched.
//
// "The result parses" is therefore not the invariant anyone wants. This file
// holds the one that is: the rewritten file must MEAN what the original meant,
// key for key, except for the settings the command was actually asked to change.
// `af config migrate` has asserted that since #3624 — it moves a spelling and
// must change nothing at all — and this generalizes it to writers that DO change
// something, by naming what they are allowed to change.
//
// The guard is a backstop, not the fix. tomlStringContentLines is what makes the
// edit land on the right line in the first place. This is what turns the NEXT
// scanner bug into a refusal that names the key, rather than into silent data
// loss discovered weeks later.

// configRewriteDrift names the first effective value a global-config rewrite
// would change that the caller did not ask it to change, or "" when the rewrite
// means exactly what the file meant. changed names the keys this write IS
// allowed to move, in any spelling taggedFieldByKey resolves (a flat legacy
// name, its dotted canonical alias, or one entry of a dynamic map).
func configRewriteDrift(before, after *Config, changed ...string) string {
	if before == nil || after == nil {
		return "the loaded configuration"
	}
	expected := snapshotConfig(before)
	if drift := exemptRewrittenKeys(reflect.ValueOf(expected), reflect.ValueOf(after), changed); drift != "" {
		return drift
	}
	return configValueDrift(expected, after)
}

// projectRewriteDrift is configRewriteDrift for a personal project config. It
// additionally compares PRESENCE: this layer distinguishes "set to an empty
// value" from "absent" (ProjectConfig.IsSet), so a rewrite that dropped a line
// whose value happened to equal the zero value would change how the key
// resolves even though every field still compares equal.
func projectRewriteDrift(before, after *ProjectConfig, changed ...string) string {
	if before == nil || after == nil {
		return "the personal project configuration"
	}
	expected := snapshotProjectConfig(before)
	observed := snapshotProjectConfig(after)
	if drift := exemptRewrittenKeys(reflect.ValueOf(expected), reflect.ValueOf(observed), changed); drift != "" {
		return drift
	}
	if drift := structValueDrift(reflect.ValueOf(*expected), reflect.ValueOf(*observed)); drift != "" {
		return drift
	}
	return projectPresenceDrift(before, after, changed)
}

// projectPresenceDrift names the first key whose PRESENCE in the file changed
// without being asked to. A dotted key is charged to its top-level table, which
// is the granularity presence is recorded at.
func projectPresenceDrift(before, after *ProjectConfig, changed []string) string {
	allowed := make(map[string]bool, len(changed))
	for _, key := range changed {
		top, _, _ := strings.Cut(canonicalConfigKey(key), ".")
		allowed[top] = true
	}
	keys := make([]string, 0, len(before.setKeys)+len(after.setKeys))
	for key := range before.setKeys {
		keys = append(keys, key)
	}
	for key := range after.setKeys {
		if !before.setKeys[key] {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		if allowed[key] || before.IsSet(key) == after.IsSet(key) {
			continue
		}
		if before.IsSet(key) {
			return fmt.Sprintf("%s from set to absent", key)
		}
		return fmt.Sprintf("%s from absent to set", key)
	}
	return ""
}

// exemptRewrittenKeys copies each changed key's value from after onto expected,
// so the comparison that follows passes over exactly the settings this write
// meant to change and nothing else. It returns "" on success, or a drift
// sentence for a key it could not resolve.
//
// An unresolvable key is reported as drift rather than skipped, and that
// direction is deliberate: failing to exempt a key the writer really did change
// costs a refusal on a write that was fine, while silently NOT exempting the
// key you thought you had exempted would blind the guard to that key's own
// drift — the failure mode this whole file exists to prevent. Every settable key
// resolves (TestCurrentValueCoversEveryManifestKey pins the reflection walk), so
// reaching that branch means a caller passed a key no writer should be writing.
func exemptRewrittenKeys(expected, after reflect.Value, changed []string) string {
	for _, key := range changed {
		if !exemptRewrittenKey(expected, after, key) {
			return fmt.Sprintf("values this guard cannot account for (%q names no config field)", key)
		}
	}
	return ""
}

// exemptRewrittenKey resolves the two spellings the writers use: a top-level
// field — including the flat field behind a dotted alias such as
// network.listen_addr, which taggedFieldByKey maps for us — and a SINGLE entry
// of a dynamic map (program_overrides.claude), whose siblings stay guarded.
// expected and after must be pointers, so the field found in expected is
// settable.
func exemptRewrittenKey(expected, after reflect.Value, key string) bool {
	if field, ok := taggedFieldByKey(expected, key); ok {
		source, sourceOK := taggedFieldByKey(after, key)
		if !sourceOK || !field.CanSet() || !source.Type().AssignableTo(field.Type()) {
			return false
		}
		field.Set(source)
		return true
	}
	table, leaf, dotted := strings.Cut(key, ".")
	if !dotted {
		return false
	}
	target, ok := taggedFieldByKey(expected, table)
	if !ok || target.Kind() != reflect.Map || target.Type().Key() != reflect.TypeOf("") || !target.CanSet() {
		return false
	}
	source, ok := taggedFieldByKey(after, table)
	if !ok || source.Type() != target.Type() {
		return false
	}
	entry := source.MapIndex(reflect.ValueOf(leaf))
	if !entry.IsValid() {
		// Gone after the write. Drop it from the expectation too, so unsetting
		// one map entry is exempted without exempting its siblings.
		if !target.IsNil() {
			target.SetMapIndex(reflect.ValueOf(leaf), reflect.Value{})
		}
		return true
	}
	if target.IsNil() {
		target.Set(reflect.MakeMap(target.Type()))
	}
	target.SetMapIndex(reflect.ValueOf(leaf), entry)
	return true
}

// configValueDrift names the first exported Config field whose value differs
// between two loads, or "" when the two configurations are identical. It is the
// migration's proof that it moved a spelling and nothing else.
func configValueDrift(before, after *Config) string {
	if before == nil || after == nil {
		return "the loaded configuration"
	}
	return structValueDrift(reflect.ValueOf(*snapshotConfig(before)), reflect.ValueOf(*snapshotConfig(after)))
}

// structValueDrift names the first exported field of two same-typed structs
// whose values differ, spelled with the field's toml name so the message names
// the key as the file writes it. Unexported fields are skipped: they carry
// provenance and decode bookkeeping, not configuration.
func structValueDrift(before, after reflect.Value) string {
	for i := 0; i < before.NumField(); i++ {
		field := before.Type().Field(i)
		if field.PkgPath != "" {
			continue
		}
		if reflect.DeepEqual(before.Field(i).Interface(), after.Field(i).Interface()) {
			continue
		}
		name := field.Name
		if tag, ok := field.Tag.Lookup("toml"); ok {
			name = strings.Split(tag, ",")[0]
		}
		return fmt.Sprintf("%s from %v to %v", name, before.Field(i).Interface(), after.Field(i).Interface())
	}
	return ""
}

// snapshotProjectConfig is snapshotConfig's personal-project twin: a deep copy
// of the exported state, so a comparison can mutate one side without touching
// the other. Both sides of a comparison go through it, so the unexported
// bookkeeping it drops is equal by construction rather than merely ignored.
func snapshotProjectConfig(cfg *ProjectConfig) *ProjectConfig {
	if cfg == nil {
		return nil
	}
	copied := cloneExportedValue(reflect.ValueOf(*cfg)).Interface().(ProjectConfig)
	return &copied
}

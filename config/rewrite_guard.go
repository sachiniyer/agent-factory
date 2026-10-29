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
	return configRewriteDriftFor(before, after, nil, changed...)
}

// configRewriteDriftFor is configRewriteDrift plus the value a whole-table
// rewrite MEANT to land for the changed key. intended carries, for a whole-map
// write, the write's own decoded payload, so the exemption for that field copies
// the intended value onto expected rather than the post-load value in after.
//
// A surgical writer that fails to remove a case-variant header ([HOSTS.box1]
// while the writer looked for "hosts") leaves the stale block on disk; the
// loader folds it into after.Hosts alongside the freshly appended canonical
// table, so after.Hosts already contains the "ghost". Copying the whole map
// from after onto expected — the original whole-field exemption — would paste
// the ghost in too and the comparison would pass, masking exactly the silent
// rewrite this guard exists to catch. Supplying the intended payload keeps the
// ghost's survival as a named drift: expected.Hosts is the payload, after.Hosts
// is the payload plus the ghost, and the two differ. Callers without an
// intended value (scalar writes, dotted dynamic-map entries, the tests below)
// pass nil so exemptRewrittenKey falls back to the original behavior.
func configRewriteDriftFor(before, after, intended *Config, changed ...string) string {
	if before == nil || after == nil {
		return "the loaded configuration"
	}
	expected := snapshotConfig(before)
	if drift := exemptRewrittenKeys(reflect.ValueOf(expected), reflect.ValueOf(after), reflect.ValueOf(intended), changed); drift != "" {
		return drift
	}
	return configValueDrift(expected, after)
}

// projectRewriteDrift is configRewriteDrift for a personal project config. It
// additionally compares PRESENCE: this layer distinguishes "set to an empty
// value" from "absent" (ProjectConfig.IsSet), so a rewrite that dropped a line
// whose value happened to equal the zero value would change how the key
// resolves even though every field still compares equal.
//
// The personal-project loader rejects a case-variant table header outright
// (its top-level-key allowlist is checked case-sensitively), so the
// case-variant ghost the global guard's intended-payload path exists for cannot
// arise here; intended is therefore passed as a zero Value and the original
// field-wide exemption applies.
func projectRewriteDrift(before, after *ProjectConfig, changed ...string) string {
	if before == nil || after == nil {
		return "the personal project configuration"
	}
	expected := snapshotProjectConfig(before)
	observed := snapshotProjectConfig(after)
	if drift := exemptRewrittenKeys(reflect.ValueOf(expected), reflect.ValueOf(observed), reflect.Value{}, changed); drift != "" {
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
// intended, when valid, supplies the value a whole-table rewrite MEANT to land
// for the changed key (see configRewriteDriftFor); exemptRewrittenKey uses it
// only for the whole-map field it names, and for every other key (and when it
// is absent) it copies from after as before. Passing a zero Value restores the
// original behavior, so the existing scalar/dotted callers and tests are
// unchanged.
//
// An unresolvable key is reported as drift rather than skipped, and that
// direction is deliberate: failing to exempt a key the writer really did change
// costs a refusal on a write that was fine, while silently NOT exempting the
// key you thought you had exempted would blind the guard to that key's own
// drift — the failure mode this whole file exists to prevent. Every settable key
// resolves (TestCurrentValueCoversEveryManifestKey pins the reflection walk), so
// reaching that branch means a caller passed a key no writer should be writing.
func exemptRewrittenKeys(expected, after, intended reflect.Value, changed []string) string {
	for _, key := range changed {
		if !exemptRewrittenKey(expected, after, intended, key) {
			return fmt.Sprintf("values this guard cannot account for (%q names no config field)", key)
		}
	}
	return ""
}

// exemptRewrittenKey resolves the two spellings the writers use: a top-level
// field — including the flat field behind a dotted alias such as
// network.listen_addr, which taggedFieldByKey maps for us — a SINGLE entry of a
// dynamic map (program_overrides.claude), or one fixed field of a structured
// table (root_agent.enabled). Sibling entries and fields stay guarded.
// expected and after must be pointers, so the field found in expected is
// settable.
//
// For a whole-table write of a dynamic MAP the top-level branch used to — and
// for every non-map field still does — copy the entire value from after, which
// is correct for a scalar (listen_addr) or a struct (root_agent) or a list
// (session_env_passthrough: a duplicate of a list is a parse error, not a
// silent merge). It is wrong for a dynamic map when the surgical writer left a
// stale case-variant block on disk: the loader folds that block into after as a
// "ghost", so copying after pastes the ghost into expected and the comparison
// passes. When intended supplies the write's own payload for such a field, the
// exemption copies THAT instead, so the ghost's survival shows as drift. The
// name-aware path is gated to typed string-keyed maps whose entries the loader
// never drops from a value the writer accepted, so after equals the payload for
// a correct write and the guard only fires on an actual ghost: map[string]any
// (keys) load-discards removed-PR and dead-shift bindings, and root_agents is
// pruned by registration, so both stay on the after-copy and retain the original
// field-wide exemption.
func exemptRewrittenKey(expected, after, intended reflect.Value, key string) bool {
	if field, ok := taggedFieldByKey(expected, key); ok {
		source, sourceOK := taggedFieldByKey(after, key)
		if !sourceOK || !field.CanSet() || !source.Type().AssignableTo(field.Type()) {
			return false
		}
		if payload, ok := intendedMapPayload(intended, key, field); ok {
			field.Set(payload)
			return true
		}
		field.Set(source)
		return true
	}
	table, leaf, dotted := strings.Cut(key, ".")
	if !dotted {
		return false
	}
	target, ok := taggedFieldByKey(expected, table)
	if !ok || !target.CanSet() {
		return false
	}
	source, ok := taggedFieldByKey(after, table)
	if !ok || source.Type() != target.Type() {
		return false
	}
	if target.Kind() == reflect.Struct {
		targetLeaf, targetOK := taggedFieldByKey(target, leaf)
		sourceLeaf, sourceOK := taggedFieldByKey(source, leaf)
		if !targetOK || !sourceOK || !targetLeaf.CanSet() || !sourceLeaf.Type().AssignableTo(targetLeaf.Type()) {
			return false
		}
		targetLeaf.Set(sourceLeaf)
		return true
	}
	if target.Kind() != reflect.Map || target.Type().Key() != reflect.TypeOf("") {
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

// intendedMapPayload returns the value a whole-map write intended to land for
// key, when intended carries it and using it is safe for this field — a typed
// string-keyed map whose element is not an interface and that is not pruned by
// the loader from a value the writer accepted (root_agents is, by
// registration). It exists to keep the whole-map drift exemption name-aware
// (only the entries the write actually named move) without loosening it for
// fields where a correct write's post-load value legitimately differs from its
// payload.
//
// A nil intended pointer (the common "caller had no payload" case: the public
// configRewriteDrift/projectRewriteDrift pass nil, and scalar/dotted writes pass
// a zero Value) must read as "no intended value": reflect.ValueOf of a nil
// *Config is a valid Pointer Value rather than the zero Value, so an IsValid
// check alone would resolve it to a zero Config's empty map and paste that in.
func intendedMapPayload(intended reflect.Value, key string, field reflect.Value) (reflect.Value, bool) {
	if !intended.IsValid() || intended.Kind() != reflect.Pointer || intended.IsNil() {
		return reflect.Value{}, false
	}
	if field.Kind() != reflect.Map {
		return reflect.Value{}, false
	}
	if field.Type().Key().Kind() != reflect.String || field.Type().Elem().Kind() == reflect.Interface || key == "root_agents" {
		return reflect.Value{}, false
	}
	payload, ok := taggedFieldByKey(intended, key)
	if !ok || !payload.IsValid() || !payload.Type().AssignableTo(field.Type()) {
		return reflect.Value{}, false
	}
	return payload, true
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
		if sameConfigValue(before.Field(i), after.Field(i)) {
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

// sameConfigValue compares two same-typed config field values, treating a NIL map
// and an EMPTY map as the same configuration — because for a config key they are:
// both decode from a file with no entries, both resolve to no override, and
// neither is a value a user can write differently.
//
// reflect.DeepEqual disagrees, and that disagreement was a REAL refusal rather
// than a nicety. Unsetting the LAST entry of a dynamic map — `af config unset
// program_overrides.claude --project <path>`, or default_accounts.<agent> —
// produced an expectation holding an emptied map while the re-parsed file decoded
// the removed table as nil. The guard then refused its own writer's edit with
// "would change program_overrides from map[] to map[]": two values it printed
// identically, because they ARE identical as configuration. The command it
// refused is the one `af config unset --help` documents, and the one this key's
// refusals hand a user to run.
//
// It stays strict about everything else: a map that gained, lost or changed any
// entry still differs, and so does every non-map field.
func sameConfigValue(before, after reflect.Value) bool {
	if before.Kind() == reflect.Map && after.Kind() == reflect.Map &&
		before.Len() == 0 && after.Len() == 0 {
		return true
	}
	return reflect.DeepEqual(before.Interface(), after.Interface())
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

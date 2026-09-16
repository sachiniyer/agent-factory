package config

import "reflect"

// CurrentSavedValue returns cfg's live value for any key a save surface can
// WRITE, in the same form SetResult.Value records and `af config set` accepts
// back.
//
// It is CurrentValue widened by exactly one case: the dotted dynamic-family
// leaves (program_overrides.<agent>, default_accounts.<agent>,
// limit_patterns.<agent>). Those are valid settable keys, but the full dotted
// key names no toml-tagged field — the reflection walk behind CurrentValue
// matches whole fields, and the field here is the containing map — so
// CurrentValue reports ok=false for them.
//
// That distinction matters because a post-apply readback must resolve exactly
// the key that was written. Reading an unresolvable key as "diverged" made every
// successful leaf save look like a lost race (#4247); reading it as "matched"
// would be the opposite lie. Resolving it is the only answer that is true in
// both directions.
//
// ok is false only for a key that is neither a manifest key nor a settable leaf,
// which leaves the caller to say "cannot verify" rather than guess. An ABSENT
// map entry is a resolved empty value, not a failure: "" is what a leaf reads as
// before its first set and after an unset removes it, so an unset that lands
// must compare equal to the empty default rather than unverifiable.
func CurrentSavedValue(cfg *Config, key string) (string, bool) {
	if value, ok := CurrentValue(cfg, key); ok {
		return value, true
	}
	section, leaf, _, ok := resolveSettable(key)
	// section == "" is the whole-table form (program_overrides on its own),
	// which CurrentValue already renders above; reaching here with it means the
	// table itself did not resolve, and indexing nothing would not help.
	if !ok || section == "" || leaf == "" {
		return "", false
	}
	field, ok := configFieldByTomlKey(cfg, section)
	if !ok || field.Kind() != reflect.Map {
		return "", false
	}
	// MapIndex on a nil or entry-less map yields the zero Value rather than
	// panicking, which is the absent-entry case documented above.
	entry := field.MapIndex(reflect.ValueOf(leaf))
	if !entry.IsValid() {
		return "", true
	}
	return editorValue(entry), true
}

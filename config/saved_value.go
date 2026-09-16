package config

import (
	"reflect"
	"strconv"
	"strings"
)

// SavedValueMatches reports whether cfg's stored value for key is the value this
// save wrote. match is only meaningful when ok is true; ok is false when the key
// does not resolve at all, which leaves the caller to say "cannot verify"
// instead of guessing in either direction.
//
// It exists because a raw string compare answers the wrong question for some
// keys. One instant has several accepted duration spellings ("1500ms", "2s", a
// bare millisecond count), and a duration key can land in an INT field, so the
// readback renders a different string for the very value this save wrote —
// making a successful save look like a lost race with no competing writer
// anywhere (#4247). Instants are therefore compared as instants.
func SavedValueMatches(cfg *Config, key, written string) (match bool, ok bool) {
	live, resolved := CurrentSavedValue(cfg, key)
	if !resolved {
		return false, false
	}
	if live == written {
		return true, true
	}
	if durationSpelledKey(key) {
		wroteMS, wroteOK := durationOrMillisecondCount(written)
		liveMS, liveOK := durationOrMillisecondCount(live)
		if wroteOK && liveOK {
			return wroteMS == liveMS, true
		}
	}
	return false, true
}

// durationSpelledKey reports whether key's writer accepts duration spellings, so
// only those keys take the numeric comparison above. The kind comes from the one
// allowlist the writer itself uses, rather than a second list that could drift.
func durationSpelledKey(key string) bool {
	_, _, spec, ok := resolveSettable(key)
	return ok && spec.kind == cfgDuration
}

// durationOrMillisecondCount parses either accepted spelling into milliseconds,
// trying the bare integer first exactly as validateDaemonPollIntervalValue does,
// so the comparison accepts precisely what the writer accepts.
func durationOrMillisecondCount(value string) (int, bool) {
	trimmed := strings.TrimSpace(value)
	if ms, err := strconv.Atoi(trimmed); err == nil {
		return ms, true
	}
	ms, err := durationMilliseconds(trimmed)
	if err != nil {
		return 0, false
	}
	return ms, true
}

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

package config

import (
	"reflect"
	"strconv"
)

// This file is the manifest's value-reading half: given a manifest key, what is
// the user's live value, and in what form?
//
// There are two forms, and the distinction is the whole reason this file is
// separate from manifest_briefing.go:
//
//   - The BRIEFING form (renderConfigValue) is prose for an agent to read. It
//     renders an unset string as `""` and an empty table as "none", because a
//     briefing that emitted a blank line there would read as a rendering bug.
//   - The EDITOR form (CurrentValue) is the value as `af config set` would
//     accept it back. An unset string is "", not `""` — an editor that
//     pre-filled a text field with two literal quote characters would write
//     those quotes into config.toml the moment the user pressed enter.
//
// Both read the SAME field by the SAME reflection walk (configFieldByTomlKey),
// so neither surface can drift from Config. That is the manifest's premise
// (see manifest.go): a second hand-written key → field map is the bug, not the
// fix.

// configFieldByTomlKey finds the Config field carrying toml tag `key`.
//
// It is the single reflection walk behind every value read — the briefing, the
// TUI editor, and the web editor all land here. ok is false for a key that
// names no toml-tagged field, which TestManifestCoversEveryConfigKey makes
// unreachable for a manifest key.
func configFieldByTomlKey(cfg *Config, key string) (reflect.Value, bool) {
	if cfg == nil {
		return reflect.Value{}, false
	}
	rv := reflect.ValueOf(*cfg)
	rt := rv.Type()
	for i := range rt.NumField() {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		if tomlTagName(f.Tag.Get("toml")) != key {
			continue
		}
		return rv.Field(i), true
	}
	return reflect.Value{}, false
}

// CurrentValue returns cfg's live value for one manifest key in the form an
// editor should show and `af config set` would accept back: a string bare, a
// bool as "true"/"false", an int in decimal, and a composite (a table or list)
// as compact JSON.
//
// ok is false for a nil cfg and for a key naming no toml-tagged field. Callers
// must not substitute a default on !ok: showing a default as though it were the
// user's live setting is the same lie RenderBriefing refuses to tell with its
// "unknown", and here it would be worse — the user could save it back.
//
// The round-trip property (what this returns is what `config set` accepts) is
// pinned by TestCurrentValueRoundTripsThroughConfigSet.
func CurrentValue(cfg *Config, key string) (string, bool) {
	field, ok := configFieldByTomlKey(cfg, key)
	if !ok {
		return "", false
	}
	return editorValue(field), true
}

// editorValue renders one config field in the editor form. It deliberately does
// NOT share renderConfigValue's briefing decorations (`""`, "none") — see the
// file comment.
func editorValue(v reflect.Value) string {
	switch v.Kind() {
	case reflect.String:
		return v.String()
	case reflect.Bool:
		return strconv.FormatBool(v.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10)
	case reflect.Map:
		if v.Len() == 0 {
			return "{}"
		}
		return compactJSON(v)
	case reflect.Slice:
		if v.Len() == 0 {
			return "[]"
		}
		return compactJSON(v)
	default:
		return compactJSON(v)
	}
}

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

// ConfigEntry is one manifest entry zipped with the user's live value: the form
// a config EDITOR renders a row from, and the form that crosses the wire to the
// web UI.
//
// It exists so the two editors render from one description of config rather than
// two. The TUI calls ManifestWithValues in-process; the daemon returns the same
// slice from GetConfig and the web UI renders that. Neither surface holds a key
// list, a type switch, or a copy of the defaults, so adding a key to
// config_types.go reaches both without either UI being touched — which is what
// TestBothEditorSurfacesRenderEveryManifestKey pins.
type ConfigEntry struct {
	Key      string `json:"key"`
	Type     string `json:"type"`
	Default  string `json:"default"`
	Purpose  string `json:"purpose"`
	Tier     int    `json:"tier"`
	TierName string `json:"tier_name"`
	// Settable reports whether this key can be edited from a UI at all. It is
	// the manifest's own field, pinned against the real `af config set`
	// allowlist by TestManifestAgreesWithSettableKeys — so a surface may trust
	// it rather than re-deriving one. A false renders read-only, with Purpose
	// explaining that the key is hand-edited.
	Settable bool `json:"settable"`
	// Enum drives a picker instead of a free-text field when non-empty. For a
	// table it constrains the entry NAMES, not the value (see the briefing's
	// same distinction), which is why a UI must not offer it as a value picker
	// for Type "table".
	Enum []string `json:"enum,omitempty"`
	// Value is the user's live value in editor form (see CurrentValue).
	Value string `json:"value"`
	// RequiresRestart reports that a change to this key reaches af and the
	// daemon only when they next start.
	//
	// It is true for EVERY key, which is not laziness: config.toml is read at
	// startup, so this mirrors SetResult.RequiresRestart and the note
	// `af config set` already prints. It is carried per-entry rather than as one
	// banner because the honest per-key answer is not uniform — some keys are
	// re-read per use (worktree_root, via LoadConfig on each worktree create),
	// while the daemon's own listener keys are captured once into manager.cfg at
	// startup and cannot change without a restart. Claiming the former apply
	// "live" would be the lie this field exists to avoid, and nothing pins such
	// a claim today, so every key reports the conservative truth. Over-warning
	// costs a needless restart; under-warning silently ignores the user's edit.
	RequiresRestart bool `json:"requires_restart"`
}

// ManifestWithValues returns the manifest zipped with cfg's live values: the
// single description of config that BOTH editor surfaces render from.
//
// A nil cfg (or a key that will not resolve) yields an empty Value rather than a
// default, for the reason CurrentValue documents: an editor that pre-filled a
// default as though it were the user's setting invites saving it back.
func ManifestWithValues(cfg *Config) []ConfigEntry {
	entries := Manifest()
	out := make([]ConfigEntry, 0, len(entries))
	for _, e := range entries {
		value, _ := CurrentValue(cfg, e.Key)
		out = append(out, ConfigEntry{
			Key:      e.Key,
			Type:     e.Type,
			Default:  e.Default,
			Purpose:  e.Purpose,
			Tier:     int(e.Tier),
			TierName: TierName(e.Tier),
			Settable: e.Settable,
			Enum:     e.Enum,
			Value:    value,
			// Uniformly true — see the field's comment.
			RequiresRestart: true,
		})
	}
	return out
}

// RestartNotice is the one sentence every surface uses to tell a user their edit
// is not live yet, and what to do about it.
//
// It names `af daemon restart` because "restart them to apply" (what the CLI
// prints) leaves a user to guess the command — and a UI that changes a value the
// running daemon then ignores, without saying so, is the failure this feature is
// specifically not allowed to ship.
const RestartNotice = "af and the daemon read config.toml at startup · run `af daemon restart` and restart af to apply"

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

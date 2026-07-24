package config

import (
	"reflect"
	"strconv"
)

// This file is the GLOBAL manifest view's value-reading half: given a key from
// Manifest(), what is the user's live global value, and in what form? Repo-only
// entries from AllManifest() resolve through the per-repo resolver instead.
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
// names no toml-tagged field, which
// TestGlobalManifestCoversEveryGlobalConfigKey makes unreachable for a key
// returned by Manifest().
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

// CurrentValue returns cfg's live value for one global manifest key in the form
// an editor should show and `af config set` would accept back: a string bare, a
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
// config_types.go reaches both without either UI being touched.
//
// No single test pins that end to end; one per link in the chain does:
//   - TestGlobalManifestCoversEveryGlobalConfigKey (config) — every toml-tagged
//     Config field is in the global Manifest() view.
//   - TestCurrentValueCoversEveryManifestKey (config) — every entry's Key
//     resolves through the reflection walk both surfaces read values with, so an
//     entry with a typo'd Key cannot render as "unknown" in both UIs.
//   - TestConfigPaneRendersEveryManifestKey (ui) — the TUI editor renders every
//     manifest key.
//   - "the control for a key is decided by the manifest, so an unknown key still
//     renders" (web/src/config.test.ts) — the web control is chosen from the
//     entry's own description, so a key the bundle predates still renders.
type ConfigEntry struct {
	Key      string `json:"key"`
	Type     string `json:"type"`
	Default  string `json:"default"`
	Purpose  string `json:"purpose"`
	Tier     int    `json:"tier"`
	TierName string `json:"tier_name"`
	// Settable is the MANIFEST's claim: `af config set` accepts this key — or,
	// for a dynamic family, its LEAVES (`af config set program_overrides.claude
	// …`). It is pinned against the real allowlist by
	// TestManifestAgreesWithSettableKeys.
	//
	// A UI must NOT drive a control off this field. "The CLI accepts this key's
	// leaves" is not "this row can be edited as one value", and conflating the
	// two makes the editor offer program_overrides as a text field pre-filled
	// with the map's JSON — which the writer then refuses, because the bare key
	// is not settable. Use Editable.
	Settable bool `json:"settable"`
	// Editable is the EDITOR's question: can this row be edited directly, as a
	// single scalar value the write path will accept?
	//
	// It is Settable minus the dynamic families, derived from the real
	// settableKeySpecs allowlist rather than restated, so it cannot promise a
	// shape `af config set` does not take. A false renders read-only with
	// EditHint, which is the honest outcome: an editable-looking field whose save
	// can only ever be refused is a dead end the user finds by pressing save.
	Editable bool `json:"editable"`
	// EditHint says how to change a key that is not directly editable. It is not
	// always "hand-edit the file": a dynamic family's leaves ARE settable from
	// the CLI, so the hint names that command instead of sending the user to a
	// text editor for something af can do.
	EditHint string `json:"edit_hint,omitempty"`
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

// ManifestWithValues returns the global Manifest() view zipped with cfg's live
// values: the single description of config that BOTH current editor surfaces
// render from.
//
// A nil cfg (or a key that will not resolve) yields an empty Value rather than a
// default, for the reason CurrentValue documents: an editor that pre-filled a
// default as though it were the user's setting invites saving it back.
func ManifestWithValues(cfg *Config) []ConfigEntry {
	entries := Manifest()
	out := make([]ConfigEntry, 0, len(entries))
	for _, e := range entries {
		value, _ := CurrentValue(cfg, e.Key)
		editable, hint := editability(e)
		out = append(out, ConfigEntry{
			Key:      e.Key,
			Type:     e.Type,
			Default:  e.Default,
			Purpose:  e.Purpose,
			Tier:     int(e.Tier),
			TierName: TierName(e.Tier),
			Settable: e.Settable,
			Editable: editable,
			EditHint: hint,
			Enum:     e.Enum,
			Value:    value,
			// Uniformly true — see the field's comment.
			RequiresRestart: true,
		})
	}
	return out
}

// editability answers, for one manifest entry, whether an editor may offer it as
// a single editable value — and if not, what to tell the user instead.
//
// It reads settableKeySpecs (the REAL `af config set` allowlist) rather than
// restating which keys are dynamic, so it cannot drift from what the writer
// actually accepts. That matters because the two "not editable" cases are
// different, and telling a user the wrong one wastes their time:
//
//   - A dynamic family (program_overrides, limit_patterns) holds a table. The
//     bare key is NOT settable, but its leaves are — so the honest hint names
//     the command that works, rather than sending someone to a text editor for
//     something af can do for them.
//   - A structural key (theme, root_agents, keys, session_env_passthrough,
//     cors_allowed_origins) has no single-scalar `af config set` shape. The hint
//     points at the config assistant, which edits these in the file for the user
//     — the whole reason "hand-edit the file yourself" is no longer the answer
//     (#2453 / #2454).
//
// assistantEditHint is the one string for that second case, so both editor
// surfaces (TUI pane, web pane) say the same thing and TestStructuralKeysPointAtTheAssistant
// pins it. It is surface-neutral on purpose: the TUI opens the assistant with a
// key and the web with a button, so the hint names neither and describes the
// capability instead.
const assistantEditHint = "the config assistant can change this for you"

func editability(e ManifestEntry) (editable bool, hint string) {
	if !e.Settable {
		return false, assistantEditHint
	}
	spec, ok := settableKeySpecs[e.Key]
	if !ok {
		// Unreachable while TestManifestAgreesWithSettableKeys passes; fail
		// closed rather than offer a field the writer has no spec for.
		return false, assistantEditHint
	}
	if spec.dynamic {
		return false, "set one entry: af config set " + e.Key + ".<name> <value>"
	}
	return true, ""
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

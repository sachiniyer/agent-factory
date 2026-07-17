package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// DefaultConfigPathLabel is the config file location named when the real path
// cannot be resolved. It matches the documented default; it is a fallback label
// for prose, never a path anything opens.
const DefaultConfigPathLabel = "~/.agent-factory/config.toml"

// RenderBriefing renders the config manifest (manifest.go) as markdown for an
// agent to read: tier by tier, every user-facing global config key with its
// purpose, type, default, allowed values, current value, and how to set it.
//
// It is the briefing document a config agent is handed — hence markdown, hence
// the plain-language Purpose lines, and hence CURRENT values: an agent asked to
// "turn off the web server" or "make my theme darker" needs to know what the
// setting is now, not merely what it defaults to.
//
// configPath is the config file this describes, and it is a PARAMETER rather
// than a literal for a reason: this text used to hardcode
// "~/.agent-factory/config.toml", so under a relocated AGENT_FACTORY_HOME it
// told the agent to read and edit a file that was not the user's config. An
// agent that confidently operates on the wrong file is worse than one that
// fails. There is one path value, supplied by the caller, interpolated
// everywhere. An empty configPath falls back to the documented default location.
//
// It emits NO top-level heading. The sections start at H2 so this can be
// embedded inside a larger document (configagent.BuildBriefing) without nesting
// an H1 under an H2, and a standalone caller can title it however it likes.
//
// Current values are read by REFLECTION over Config's toml tags rather than a
// hand-written key → field switch. That is the whole point of this phase: a
// second hand-maintained key list is exactly the drift the manifest exists to
// kill (it is how `af config set` silently lost five keys, and how
// configEntries lost six). Reflection cannot fall behind the struct, and the
// manifest's own coverage test guarantees every toml-tagged field has an entry
// to render.
//
// A nil cfg renders every current value as "unknown" rather than panicking or
// substituting defaults — a briefing that quietly showed defaults as if they
// were the user's live settings would be worse than one that admits it does not
// know.
func RenderBriefing(cfg *Config, configPath string) string {
	var b strings.Builder

	if strings.TrimSpace(configPath) == "" {
		configPath = DefaultConfigPathLabel
	}

	fmt.Fprintf(&b, "These are the settings in `%s`, which apply to every repository. ", configPath)
	b.WriteString("Keys are grouped by how likely you are to need them.\n\n")
	b.WriteString("Most keys can be changed with `af config set <key> <value>`, which edits that one value in place ")
	b.WriteString("and leaves every comment and the file's ordering untouched. ")
	b.WriteString("The rest are tables you edit in the file by hand — each one says so below. ")
	b.WriteString("Either way the change applies the next time af and its background service start, not immediately.\n")

	for _, tier := range ManifestTiers {
		entries := manifestEntriesForTier(tier)
		if len(entries) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n## %s\n\n%s\n", tierHeading(tier), tierBlurb(tier))
		for _, e := range entries {
			b.WriteString(renderBriefingEntry(cfg, e))
		}
	}

	return b.String()
}

// manifestEntriesForTier returns the manifest entries in one tier, in table
// order.
func manifestEntriesForTier(tier ConfigTier) []ManifestEntry {
	var out []ManifestEntry
	for _, e := range configManifest {
		if e.Tier == tier {
			out = append(out, e)
		}
	}
	return out
}

// tierHeading is the section title for a tier.
func tierHeading(tier ConfigTier) string {
	switch tier {
	case TierCore:
		return "Core settings"
	case TierCommon:
		return "Common settings"
	case TierAdvanced:
		return "Advanced settings"
	default:
		return "Other settings"
	}
}

// tierBlurb is the one-line orientation under a tier heading.
func tierBlurb(tier ConfigTier) string {
	switch tier {
	case TierCore:
		return "The settings most people want. Start here."
	case TierCommon:
		return "Preferences worth knowing about, but not day-one decisions."
	case TierAdvanced:
		return "Tuning and opt-in behavior · correct by default, and rarely worth changing."
	default:
		return ""
	}
}

// renderBriefingEntry renders one key's markdown block.
func renderBriefingEntry(cfg *Config, e ManifestEntry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n### `%s`\n\n%s\n\n", e.Key, e.Purpose)
	fmt.Fprintf(&b, "- type: %s\n", e.Type)
	fmt.Fprintf(&b, "- default: %s\n", e.Default)
	fmt.Fprintf(&b, "- current: %s\n", currentConfigValue(cfg, e.Key))
	if len(e.Enum) > 0 {
		quoted := make([]string, 0, len(e.Enum))
		for _, v := range e.Enum {
			quoted = append(quoted, "`"+v+"`")
		}
		label := "allowed values"
		if e.Type == "table" {
			// For a table the enum constrains the KEYS (one entry per agent),
			// not the value, and saying "allowed values" there would read as a
			// claim about the command strings.
			label = "allowed entry names"
		}
		fmt.Fprintf(&b, "- %s: %s\n", label, strings.Join(quoted, ", "))
	}
	fmt.Fprintf(&b, "- to change: %s\n", briefingSetHint(e))
	return b.String()
}

// briefingSetHint tells the agent how to change a key: the `af config set`
// invocation, or that it is hand-edited.
//
// The settable FORM is derived from settableKeySpecs, not restated here, so the
// hint cannot promise a command shape the CLI does not accept. A dynamic family
// renders its leaf form (`af config set program_overrides.claude …`) because the
// bare key is not settable on its own.
func briefingSetHint(e ManifestEntry) string {
	spec, ok := settableKeySpecs[e.Key]
	if !ok || !e.Settable {
		hint := "edit `" + e.Key + "` in `config.toml` by hand"
		// Name the actual shape — calling the cors_allowed_origins list a
		// "table" would be a small lie in the one sentence telling a reader
		// what to go and do.
		switch e.Type {
		case "table", "list":
			hint += " · it is a " + e.Type + ", not a single value"
		}
		return hint
	}
	if spec.dynamic {
		return "`af config set " + e.Key + ".<name> <value>`"
	}
	return "`af config set " + e.Key + " <value>`"
}

// currentConfigValue renders the live value of one toml key from cfg in the
// BRIEFING form, found by reflecting over Config's toml tags (never a
// hand-written key → field map, see RenderBriefing). Returns "unknown" for a nil
// cfg and for a key that names no field — the latter is unreachable while
// TestManifestCoversEveryConfigKey passes, since it rejects a manifest key with
// no matching field.
//
// The field lookup is shared with CurrentValue (manifest_value.go); only the
// rendering differs, because a briefing and an editable field want different
// things from an unset value.
func currentConfigValue(cfg *Config, key string) string {
	field, ok := configFieldByTomlKey(cfg, key)
	if !ok {
		return "unknown"
	}
	return renderConfigValue(field)
}

// renderConfigValue renders one config field for the briefing: scalars bare, an
// empty string as `""` (so "unset" is visible rather than a blank line), an
// empty map/list as "none", and any composite as compact JSON.
func renderConfigValue(v reflect.Value) string {
	switch v.Kind() {
	case reflect.String:
		if v.String() == "" {
			return `""`
		}
		return v.String()
	case reflect.Bool:
		return strconv.FormatBool(v.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10)
	case reflect.Map, reflect.Slice:
		if v.Len() == 0 {
			return "none"
		}
		return compactJSON(v)
	default:
		return compactJSON(v)
	}
}

// compactJSON renders a composite value as one line of JSON, falling back to Go
// formatting if it will not marshal (nothing in Config today, but a briefing
// must never fail to render over a formatting detail).
func compactJSON(v reflect.Value) string {
	data, err := json.Marshal(v.Interface())
	if err != nil {
		return fmt.Sprintf("%v", v.Interface())
	}
	return string(data)
}

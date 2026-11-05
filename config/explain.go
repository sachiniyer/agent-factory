package config

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// The explain surface (#2216): which config layers supplied an effective value.
// These are the ONE implementation of `af config get/list --explain`'s
// resolution, exported so every surface that answers the question — the CLI,
// the daemon's ExplainConfig route (which the web posts and the remote-target
// TUI reads), and the local TUI editor in-process — describes the same
// precedence rather than growing a second implementation of it.
//
// The subtlety they all share is root_agent: it resolves through FOUR layers in
// the daemon (built-in < global < legacy root_agents < personal-project), but
// the generic manifest resolver only knows its singleton layers. Answering it
// through the generic path would show a precedence model that did not decide
// the value, so every caller routes root_agent keys to the specialized trace.

// IsRootAgentExplainKey reports whether keyPath names the root_agent table or
// one of its leaves. It matches "root_agent" and "root_agent.<leaf>" but
// deliberately NOT "root_agents" (the legacy map is a distinct key resolved by
// the generic path).
func IsRootAgentExplainKey(keyPath string) bool {
	return keyPath == "root_agent" || strings.HasPrefix(keyPath, "root_agent.")
}

// RootAgentExplainValue returns the specialized four-layer root_agent
// resolution: the whole table, or a projected leaf for a dotted key. It mirrors
// what the daemon resolves (built-in/global/legacy/personal), unlike the
// generic global<personal resolver. A dotted leaf is projected through the same
// ResolvedValuePath machinery every other key uses, by wrapping the specialized
// table in a throwaway ResolvedConfig — so concise and explain reads cannot
// disagree about the effective value.
//
// strictProjectLookup makes an unreadable project registry a hard error rather
// than a degraded "no personal layer" answer; global-scope callers pass false.
func RootAgentExplainValue(projectSelector, keyPath string, strictProjectLookup bool) (ResolvedValue, error) {
	parent, err := ResolveRootAgentForInspection(projectSelector, strictProjectLookup)
	if err != nil {
		return ResolvedValue{}, err
	}
	if keyPath == "root_agent" {
		return parent, nil
	}
	// A fail-closed table (#3264) has no Origins for the generic projection to
	// key on — no config source decided it — so its leaves project through the
	// dedicated path that keeps every candidate's cause verbatim.
	if RootAgentValueFailsClosed(parent) {
		projected, ok := ProjectFailClosedRootAgentLeaf(parent, keyPath)
		if !ok {
			return ResolvedValue{}, UnknownConfigKeyError(keyPath)
		}
		return projected, nil
	}
	// An ignored-global table (markRootAgentGlobalIneligible) marks the present
	// global [root_agent] row Allowed:false to encode the registry cause
	// ("project is not registered and has no legacy root_agents entry"). The
	// generic ResolvedValuePath projector relabels every Allowed:false
	// candidate as "disallowed · manifest policy does not allow this key at
	// this source" — a factually false claim here, since the root_agent
	// manifest entry DOES admit the global layer — which clobbers the true
	// cause and contradicts the full-table `root_agent --explain` trace
	// (the very "confidently wrong trace" this surface exists to prevent). The
	// fail-closed shape already routes around that relabeling via
	// ProjectFailClosedRootAgentLeaf (the #3264 / #3305 precedent); the
	// ignored-global shape has the same claim on its own cause-preserving
	// projector so the dotted read cannot disagree with its full-table sibling.
	if RootAgentValueHasIgnoredGlobal(parent) {
		projected, ok := ProjectIgnoredGlobalRootAgentLeaf(parent, keyPath)
		if !ok {
			return ResolvedValue{}, UnknownConfigKeyError(keyPath)
		}
		return projected, nil
	}
	synthetic := &ResolvedConfig{Resolution: []ResolvedValue{parent}}
	projected, ok := synthetic.ResolvedValuePath(keyPath)
	if !ok {
		return ResolvedValue{}, UnknownConfigKeyError(keyPath)
	}
	return projected, nil
}

// RootAgentAwareResolution returns resolved.Resolution with the root_agent row
// replaced by the specialized four-layer trace, so a whole-manifest explain
// describes what the daemon actually resolves for that key rather than the
// generic two-layer approximation.
func RootAgentAwareResolution(resolved *ResolvedConfig, projectSelector string, strictProjectLookup bool) ([]ResolvedValue, error) {
	values := append([]ResolvedValue(nil), resolved.Resolution...)
	rootAgent, err := ResolveRootAgentForInspection(projectSelector, strictProjectLookup)
	if err != nil {
		return nil, err
	}
	for i := range values {
		if values[i].Key == "root_agent" {
			values[i] = rootAgent
			break
		}
	}
	return values, nil
}

// ExplainGlobalValue resolves one key's global-scope explanation — the same
// ResolvedValue `af config get <key> --explain` prints outside a project. It is
// the single entry point shared by the daemon's ExplainConfig route and the
// TUI's local read path: root_agent keys route to the specialized four-layer
// trace, every other key to the generic two-layer resolution, and a key no
// layer can explain reports as unknown.
func ExplainGlobalValue(keyPath string) (ResolvedValue, error) {
	if IsRootAgentExplainKey(keyPath) {
		return RootAgentExplainValue("", keyPath, false)
	}
	resolved, err := ResolveGlobalConfig()
	if err != nil {
		return ResolvedValue{}, err
	}
	value, ok := resolved.ResolvedValuePath(keyPath)
	if !ok {
		return ResolvedValue{}, UnknownConfigKeyError(keyPath)
	}
	return value, nil
}

// FormatValue renders a resolved config value for human output: scalars bare
// (so `af config get default_program` prints exactly `claude`, script-friendly),
// composites as compact JSON. It is the one renderer the CLI's get/list output,
// --explain's values, and the TUI explain view share, so no surface spells the
// same value differently.
//
// FormatValueForKey is the key-aware counterpart: a cfgStringList leaf
// (isCommaListKey) renders as the comma-joined form `af config set` accepts back
// rather than JSON, so a get→set round-trip does not silently corrupt the list.
// Use FormatValueForKey anywhere the key is known; FormatValue remains the
// fallback when it is not (and for non-comma-list keys the two are identical).
func FormatValue(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case int:
		return strconv.Itoa(x)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(b)
	}
}

// FormatValueForKey renders a resolved config value for human output, keyed off
// the config key so a cfgStringList leaf (isCommaListKey) renders as the
// comma-joined form `af config set` accepts back rather than JSON. For every
// other key it is identical to FormatValue. It is the renderer `af config get`
// and `af config list` use for a typed value whose key is known, so a get→set
// round-trip is byte-identical for comma-list keys (cors_allowed_origins,
// allowed_hosts, limit_account_candidates, account_handoff.exclude_sessions).
func FormatValueForKey(key string, v any) string {
	if isCommaListKey(key) {
		if joined, ok := commaJoinStringList(key, v); ok {
			return joined
		}
	}
	return FormatValue(v)
}

// FormatExplainValue renders a value inside an explanation: an explicitly
// configured empty string stays visible as `""` — a bare blank would read as
// absent — while everything else prints as FormatValue.
func FormatExplainValue(value any) string {
	if text, ok := value.(string); ok && text == "" {
		return `""`
	}
	return FormatValue(value)
}

// FormatExplainValueForKey is the key-aware counterpart of FormatExplainValue: a
// cfgStringList leaf (isCommaListKey) renders as the comma-joined form (with an
// empty list shown as `""` to distinguish it from an absent value) rather than
// JSON, so the explain trace and the concise get output agree. For every other
// key it is identical to FormatExplainValue.
func FormatExplainValueForKey(key string, value any) string {
	if isCommaListKey(key) {
		if joined, ok := commaJoinStringList(key, value); ok {
			if joined == "" {
				return `""`
			}
			return joined
		}
	}
	return FormatExplainValue(value)
}

// commaJoinStringList renders a []string or []any-of-strings as a comma-joined
// string. The explain trace's candidate values come from map[string]any (TOML
// decode) which produces []any rather than []string, so both spellings are
// accepted. ok is false for any other type, leaving the caller to its fallback.
//
// ok is also false when the joined form would not round-trip back through the
// leaf setter exactly: an element containing a comma (the setter splits on
// commas), an element that splitListValue would alter (it trims surrounding
// whitespace and drops empty elements, so " work " or "" would silently change
// the list on the way back), or an element the per-key leaf validator rejects
// (e.g. exclude_sessions titles containing brackets or quotes). The comma form
// is returned only when re-splitting and re-validating the joined string
// reproduces every original element; otherwise the caller falls back to JSON —
// the unambiguous form that signals "use the whole-table form to set this back"
// rather than a value that looks round-trippable but is not.
func commaJoinStringList(key string, v any) (string, bool) {
	var list []string
	switch l := v.(type) {
	case []string:
		list = l
	case []any:
		parts := make([]string, len(l))
		for i, elem := range l {
			s, ok := elem.(string)
			if !ok {
				return "", false
			}
			parts[i] = s
		}
		list = parts
	default:
		return "", false
	}
	for _, s := range list {
		if strings.ContainsRune(s, ',') {
			return "", false
		}
	}
	joined := strings.Join(list, ",")
	roundTripped := splitListValue(joined)
	if len(roundTripped) != len(list) {
		return "", false
	}
	for i := range list {
		if roundTripped[i] != list[i] {
			return "", false
		}
	}
	if _, leaf, spec, ok := resolveSettable(key); ok && spec.validate != nil {
		if err := spec.validate(leaf, joined); err != nil {
			return "", false
		}
	}
	return joined, true
}

// UnknownConfigKeyError reports a key no manifest entry or root_agent leaf can
// explain, keeping the retired-key hints alongside the generic message so a
// user typing a removed spelling is told where it went rather than just "unknown".
func UnknownConfigKeyError(key string) error {
	if err := RetiredThemeKeyError(key); err != nil {
		return err
	}
	if key == "auto_yes" {
		return RemovedAutoYesError()
	}
	return fmt.Errorf("unknown config key %q; run `af config list` to see all keys", key)
}

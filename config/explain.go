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

// FormatExplainValue renders a value inside an explanation: an explicitly
// configured empty string stays visible as `""` — a bare blank would read as
// absent — while everything else prints as FormatValue.
func FormatExplainValue(value any) string {
	if text, ok := value.(string); ok && text == "" {
		return `""`
	}
	return FormatValue(value)
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

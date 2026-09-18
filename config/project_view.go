package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
)

// The PROJECT config view: what `af config get/list --repo` resolves, zipped
// into the manifest-row form the UI editors already render (ConfigEntry — see
// manifest_value.go). The CLI prints ResolvedValue directly; the UIs need the
// same answer in the shape their rows are built from, so this file is the one
// projection both the daemon's GetProjectConfig route and the TUI's local
// read call — a UI that rendered the resolution its own way would disagree
// with `af config list --repo` in exactly the place the surfaces exist to
// agree.
//
// Three conventions the projection inherits rather than re-decides:
//
//   - The VALUE is the effective resolved value, formatted like the CLI's
//     human output: scalars bare, composites compact JSON. An unset composite
//     is "" so the pane's existing "(unset)" convention answers instead of a
//     literal "null" — which would read as a configured value.
//   - An explicitly-empty configured value stays visible ("", "{}") — the
//     same "use the default" vs "I configured the empty value" distinction
//     the CLI's "(unset)" label makes, decided by the same configured scan.
//   - root_agent comes from ResolveRootAgentForInspection, the specialized
//     four-layer resolution (built-in < global < legacy root_agents <
//     personal-project), swapped in for the generic one — exactly what
//     `af config list` does (rootAgentAwareResolution), so the surfaces
//     cannot report different effective root agents for the same repo.

// ResolveProjectConfigView resolves a repository path selector to the
// project-effective config view the UIs render: every AllManifest key with
// its effective value, in manifest order. The selector is the same contract
// as `af config get --repo` — an existing repository path, resolution only:
// it never registers a project and never writes identity state.
//
// The returned root is the resolved repository root, for the header a UI
// shows so the view names what it describes.
func ResolveProjectConfigView(selector string) ([]ConfigEntry, string, error) {
	if selector == "" {
		return nil, "", fmt.Errorf("a project path is required")
	}
	abs, err := ResolveUserPath(selector)
	if err != nil {
		return nil, "", fmt.Errorf("failed to resolve project path %q: %w", selector, err)
	}
	repo, err := RepoFromPath(abs)
	if err != nil {
		return nil, "", fmt.Errorf("failed to resolve project path %q: %w", selector, err)
	}
	resolved, err := ResolveConfigForRepoInspection(repo)
	if err != nil {
		return nil, "", err
	}
	// The selector is always explicit here — the UI named a project — so the
	// strict lookup is the one that matches `af config get --repo`: a registry
	// read failure is an answer of "unknown", never a silent absent layer.
	rootAgent, err := ResolveRootAgentForInspection(selector, true)
	if err != nil {
		return nil, "", err
	}
	return ProjectEntries(resolved, rootAgent), resolved.ProjectRoot, nil
}

// ProjectEntries renders a repo's resolved config as ConfigEntry rows: one
// per AllManifest key, in the resolution's own manifest order, with the
// effective value in display form. rootAgent is the specialized
// ResolveRootAgentForInspection answer; it replaces the generic resolution's
// root_agent entry so the view describes the decision the daemon actually
// makes.
func ProjectEntries(resolved *ResolvedConfig, rootAgent ResolvedValue) []ConfigEntry {
	manifestByKey := make(map[string]ManifestEntry, len(resolved.Resolution))
	for _, entry := range AllManifest() {
		manifestByKey[entry.Key] = entry
	}
	entries := make([]ConfigEntry, 0, len(resolved.Resolution))
	for _, value := range resolved.Resolution {
		if value.Key == "root_agent" {
			value = rootAgent
		}
		manifestEntry, ok := manifestByKey[value.Key]
		if !ok {
			continue
		}
		entries = append(entries, ConfigEntry{
			Key:             manifestEntry.Key,
			Type:            manifestEntry.Type,
			AcceptedTypes:   manifestEntry.AcceptedTypes,
			Default:         manifestEntry.Default,
			Purpose:         manifestEntry.Purpose,
			Tier:            int(manifestEntry.Tier),
			TierName:        TierName(manifestEntry.Tier),
			Editable:        true,
			Settable:        manifestEntry.Settable,
			Enum:            manifestEntry.Enum,
			Value:           ProjectDisplayValue(value),
			RequiresRestart: true,
		})
	}
	return entries
}

// ProjectDisplayValue renders a resolved value for the project view: the
// effective value formatted as `af config list` prints it, or "" when no
// source configured it — which the pane's "(unset)" convention then labels
// instead of showing a literal "null" as though it were a value.
func ProjectDisplayValue(value ResolvedValue) string {
	if IsEmptyConfigValue(value.Value) && !ResolvedValueConfigured(value) {
		return ""
	}
	return FormatConfigValue(value.Value)
}

// ResolvedValueConfigured reports whether any non-built-in source supplied
// the effective value — the "configured" half of the CLI's "(unset)" vs
// explicit-empty distinction (configEntryFromResolvedValue). An empty value
// that a real source deliberately set is configured; one nothing set is not.
func ResolvedValueConfigured(value ResolvedValue) bool {
	if value.Winner != nil && value.Winner.Layer != SourceBuiltIn.String() {
		return true
	}
	// Empty composites have no leaf origin or winner. Their candidate result
	// still distinguishes an intentionally empty/replacing value from a
	// nonempty value that validation discarded as "ignored".
	for _, candidate := range value.Candidates {
		if candidate.Layer != SourceBuiltIn.String() && candidate.Allowed && candidate.Present &&
			(candidate.Result == "empty" || candidate.Result == "replaced") {
			return true
		}
	}
	return false
}

// FormatConfigValue renders a resolved value for display: scalars bare (so
// `af config get default_program` prints exactly `claude`, script-friendly),
// composites as compact JSON. Lifted from the CLI's formatConfigValue so the
// daemon's project view formats identically rather than growing a second
// renderer.
func FormatConfigValue(v any) string {
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

// IsEmptyConfigValue reports whether a resolved value is absent-shaped: nil,
// an empty string, or an empty map/slice/nil-pointer — the check the CLI's
// "(unset)" label makes before deciding a bare default needs the marker.
func IsEmptyConfigValue(value any) bool {
	if value == nil {
		return true
	}
	if text, ok := value.(string); ok {
		return text == ""
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Map, reflect.Slice:
		return reflected.Len() == 0
	case reflect.Pointer:
		return reflected.IsNil()
	default:
		return false
	}
}

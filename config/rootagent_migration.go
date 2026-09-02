package config

import (
	"sort"
	"sync"

	"github.com/sachiniyer/agent-factory/log"
)

// legacyRootAgentsWarned keeps the permanent compatibility adapter from
// flooding a long-lived daemon. The source path is the identity: one file gets
// one actionable migration notice per process, however often config is loaded.
var legacyRootAgentsWarned sync.Map

// legacyRootAgentsAdvice is the migration recipe both root_agents notices open
// with. Only their tail differs: what happened to the file.
//
// Its recipe IS legacyRootAgentsManualStep, spliced in rather than restated.
// The two were separate strings for one commit and immediately drifted — the
// review caught the migrate report omitting the "remove the entry" step, and
// fixing only that would have left the WARNING telling readers to do work that
// does not silence it. Sharing the literal is what makes that class impossible
// rather than merely fixed once (#3624 review).
const legacyRootAgentsAdvice = "root_agents is the legacy path map; use [root_agent], the current project profile, for new configuration; for exact per-path equivalence, " + legacyRootAgentsManualStep

// legacyRootAgentsInShape reports whether shape carries a configured legacy
// path map. An empty table is not one, so the presence test and the migration's
// are the same test.
func legacyRootAgentsInShape(shape map[string]any) bool {
	_, ok := legacyRootAgentsPaths(shape)
	return ok
}

// legacyRootAgentsPaths is this deprecation's presence test for the shared
// table: the configured legacy paths, sorted, or false when the map is absent
// or empty. The warning and `af config migrate` both read it, so neither can
// find a legacy entry the other does not.
func legacyRootAgentsPaths(shape map[string]any) ([]string, bool) {
	rootAgents, ok := shape[LegacyRootAgentsKey].(map[string]any)
	if !ok || len(rootAgents) == 0 {
		return nil, false
	}
	paths := make([]string, 0, len(rootAgents))
	for path := range rootAgents {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, true
}

// legacyRootAgentsDeprecation is this key's entry in the shared deprecation
// table, which owns the remedy clause the warning ends with (#3624).
func legacyRootAgentsDeprecation() configDeprecation {
	for _, deprecation := range configDeprecations() {
		if deprecation.key == LegacyRootAgentsKey {
			return deprecation
		}
	}
	// Unreachable while configDeprecations lists the key, and the table's
	// coverage test pins that. Degrade to the bare advice rather than panic in
	// a config loader.
	return configDeprecation{key: LegacyRootAgentsKey}
}

func warnLegacyRootAgents(shape map[string]any, prettyPath string) {
	if !legacyRootAgentsInShape(shape) {
		return
	}
	if _, seen := legacyRootAgentsWarned.LoadOrStore(prettyPath, struct{}{}); seen {
		return
	}
	// The tail used to read "no file was rewritten", which named the obstruction
	// and no remedy. It now names the verb AND why that verb stops short of this
	// one key, so a reader who runs it is not surprised (#3624).
	log.WarningLog.Printf("config %s: %s; %s", prettyPath, legacyRootAgentsAdvice, legacyRootAgentsDeprecation().tomlRemedy(false))
}

func warnConvertedLegacyRootAgents(rootAgents map[string]RootAgentConfig, legacyPath, tomlPath string) {
	if len(rootAgents) == 0 {
		return
	}
	// Key the notice by the canonical TOML path. The immediate reparse and every
	// later load in this process then suppress the generic no-write wording.
	if _, seen := legacyRootAgentsWarned.LoadOrStore(tomlPath, struct{}{}); seen {
		return
	}
	log.WarningLog.Printf("config %s: %s; conversion wrote %s and preserved every legacy entry", legacyPath, legacyRootAgentsAdvice, tomlPath)
}

func resetLegacyRootAgentsWarnings() {
	legacyRootAgentsWarned.Clear()
}

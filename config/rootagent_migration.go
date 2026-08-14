package config

import (
	"sync"

	"github.com/sachiniyer/agent-factory/log"
)

// legacyRootAgentsWarned keeps the permanent compatibility adapter from
// flooding a long-lived daemon. The source path is the identity: one file gets
// one actionable migration notice per process, however often config is loaded.
var legacyRootAgentsWarned sync.Map

func warnLegacyRootAgents(shape map[string]any, prettyPath string) {
	rootAgents, ok := shape["root_agents"].(map[string]any)
	if !ok || len(rootAgents) == 0 {
		return
	}
	if _, seen := legacyRootAgentsWarned.LoadOrStore(prettyPath, struct{}{}); seen {
		return
	}
	log.WarningLog.Printf("config %s: root_agents is the legacy path map; use [root_agent], the current project profile, for new configuration; for exact per-path equivalence, register the project and set enabled = true plus the optional program in its personal [root_agent] config; no file was rewritten", prettyPath)
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
	log.WarningLog.Printf("config %s: root_agents is the legacy path map; use [root_agent], the current project profile, for new configuration; for exact per-path equivalence, register the project and set enabled = true plus the optional program in its personal [root_agent] config; conversion wrote %s and preserved every legacy entry", legacyPath, tomlPath)
}

func resetLegacyRootAgentsWarnings() {
	legacyRootAgentsWarned.Clear()
}

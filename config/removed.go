package config

import (
	"errors"

	"github.com/sachiniyer/agent-factory/log"
)

// RemovedAutoYesMessage is shared by both compatibility warnings and new-input
// rejection for the removed auto_yes feature. Keeping one message prevents
// config, CLI, and HTTP callers from receiving different migration advice.
const RemovedAutoYesMessage = "auto_yes was removed (including --autoyes and --auto-yes); configure approval behavior directly in the agent command via program_overrides (for example, program_overrides.codex = \"codex --ask-for-approval never\")"

// RemovedAutoYesError returns the actionable migration error for a new write,
// removed CLI flag, or removed request field. Existing config files are a
// compatibility input: loaders ignore their stale key with a warning so an
// upgrade cannot prevent af or its daemon from starting.
func RemovedAutoYesError() error {
	return errors.New(RemovedAutoYesMessage)
}

func warnRemovedAutoYes(shape map[string]any, source string) {
	if removedAutoYesInShape(shape) {
		warnRemovedAutoYesAt(source)
	}
}

func warnRemovedAutoYesAt(source string) {
	log.WarningLog.Printf("%s: %s; the stale setting is ignored during upgrade", source, RemovedAutoYesMessage)
}

func removedAutoYesInShape(shape map[string]any) bool {
	if shape == nil {
		return false
	}
	if _, present := shape["auto_yes"]; present {
		return true
	}
	rootAgents, ok := shape["root_agents"].(map[string]any)
	if !ok {
		return false
	}
	for _, rawProfile := range rootAgents {
		profile, ok := rawProfile.(map[string]any)
		if !ok {
			continue
		}
		if _, present := profile["auto_yes"]; present {
			return true
		}
	}
	return false
}

func removedAutoYesKeyPath(key []string) bool {
	if len(key) == 1 {
		return key[0] == "auto_yes"
	}
	return len(key) >= 3 && key[0] == "root_agents" && key[len(key)-1] == "auto_yes"
}

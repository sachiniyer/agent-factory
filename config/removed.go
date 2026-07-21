package config

import "errors"

// RemovedAutoYesMessage is shared by every compatibility tripwire for the
// removed auto_yes feature. Keeping one message prevents config, CLI, doctor,
// and HTTP callers from receiving different migration advice.
const RemovedAutoYesMessage = "auto_yes was removed (including --autoyes and --auto-yes); configure approval behavior directly in the agent command via program_overrides (for example, program_overrides.codex = \"codex --ask-for-approval never\")"

// RemovedAutoYesError returns the actionable migration error for a stale
// auto_yes setting, flag, or request field.
func RemovedAutoYesError() error {
	return errors.New(RemovedAutoYesMessage)
}

func rejectRemovedAutoYes(data []byte, path string, format ConfigFormat) error {
	metadata, err := metadataForSource(data, path, format)
	if err != nil {
		// The real typed decoder owns syntax and type errors. This compatibility
		// probe only replaces a valid stale key with migration guidance.
		return nil
	}
	if removedAutoYesInShape(metadata.shape) {
		return RemovedAutoYesError()
	}
	return nil
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

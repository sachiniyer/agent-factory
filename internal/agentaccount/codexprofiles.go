package agentaccount

import "fmt"

// codexSelectedProfile is shared by provider checks and runtime inheritance.
// Unknown selections (including external-file profiles) are not guessed or read
// from another location. Only the selected inline profile can override the root.
func codexSelectedProfile(doc map[string]any) (map[string]any, string) {
	selection, selected := doc["profile"]
	if !selected {
		return nil, ""
	}
	name, ok := selection.(string)
	if !ok || name == "" {
		return nil, "selected profile could not be verified"
	}
	profiles, ok := doc["profiles"].(map[string]any)
	if !ok {
		return nil, fmt.Sprintf("selected profile %q could not be verified", name)
	}
	profile, ok := profiles[name].(map[string]any)
	if !ok {
		return nil, fmt.Sprintf("selected profile %q could not be verified", name)
	}
	return profile, ""
}

// codexEffectiveSettings overlays only the selected profile. Callers still use
// an explicit allowlist; profile selectors and provider configuration never copy.
func codexEffectiveSettings(doc map[string]any) (map[string]any, string) {
	profile, reason := codexSelectedProfile(doc)
	if reason != "" {
		return nil, reason
	}
	effective := make(map[string]any, len(doc))
	for key, value := range doc {
		effective[key] = value
	}
	for key, value := range profile {
		effective[key] = value
	}
	return effective, ""
}

func codexModelSeedBlockReason(doc map[string]any) string {
	if _, exists := doc["model_provider"]; exists {
		return "selects a custom model_provider"
	}
	profile, reason := codexSelectedProfile(doc)
	if reason != "" {
		return reason
	}
	if _, exists := profile["model_provider"]; exists {
		return fmt.Sprintf("selected profile %q sets model_provider", doc["profile"])
	}
	return ""
}

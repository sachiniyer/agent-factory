package agentaccount

import "fmt"

// codexModelSeedBlockReason keeps models from crossing provider contexts. Codex
// applies the selected profile over top-level configuration; inspecting only
// model_provider at the root therefore cannot establish the model's provider.
// Profiles and provider settings are never copied. An unresolved selection (for
// example a newer external-file profile) blocks model seeding rather than making
// af guess or read configuration outside these two documents.
func codexModelSeedBlockReason(doc map[string]any) string {
	if _, exists := doc["model_provider"]; exists {
		return "selects a custom model_provider"
	}
	selection, selected := doc["profile"]
	if !selected {
		return ""
	}
	name, ok := selection.(string)
	if !ok || name == "" {
		return "selected profile could not be verified"
	}
	profiles, ok := doc["profiles"].(map[string]any)
	if !ok {
		return fmt.Sprintf("selected profile %q could not be verified", name)
	}
	profile, ok := profiles[name].(map[string]any)
	if !ok {
		return fmt.Sprintf("selected profile %q could not be verified", name)
	}
	if _, exists := profile["model_provider"]; exists {
		return fmt.Sprintf("selected profile %q sets model_provider", name)
	}
	return ""
}

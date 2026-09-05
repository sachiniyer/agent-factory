package agentaccount

import (
	"fmt"
	"path/filepath"
)

const codexApprovalPolicyNotice = "Accepted scalar values: on-request · never. A validated granular map is also accepted."

// validCodexApprovalPolicy follows AskForApproval and GranularApprovalConfig in
// https://github.com/openai/codex/blob/main/codex-rs/core/config.schema.json.
// Unknown policies are unverified, not silently treated as healthy; the notice
// asks the operator to check their installed version rather than rewriting it.
func validCodexApprovalPolicy(value any) bool {
	if policy, ok := value.(string); ok {
		return policy == "on-request" || policy == "never"
	}
	policy, ok := value.(map[string]any)
	if !ok || len(policy) != 1 {
		return false
	}
	granular, ok := policy["granular"].(map[string]any)
	if !ok {
		return false
	}
	for _, key := range []string{"sandbox_approval", "rules", "mcp_elicitations"} {
		if _, ok := granular[key].(bool); !ok {
			return false
		}
	}
	for _, key := range []string{"request_permissions", "skill_approval"} {
		if field, exists := granular[key]; exists {
			if _, ok := field.(bool); !ok {
				return false
			}
		}
	}
	return true
}

// CodexApprovalWarning diagnoses legacy accounts without changing their policy.
// An explicitly chosen interactive policy stands just like any existing key.
func CodexApprovalWarning(name, dir string) string {
	path := filepath.Join(dir, "config.toml")
	_, doc, err := readCodexSettings(path)
	if err == nil && doc != nil {
		effective, reason := codexEffectiveSettings(doc)
		if reason != "" {
			return fmt.Sprintf("Codex account %q: approval_policy in %s could not be verified: %s. Check the selected profile before starting a session.", name, path, reason)
		}
		doc = effective
		if policy, exists := doc["approval_policy"]; exists {
			if validCodexApprovalPolicy(policy) {
				return ""
			}
			return fmt.Sprintf("Codex account %q: approval_policy in %s could not be verified (unsupported value or type); Codex may reject it at startup. %s Check the policy against your installed Codex version. Registration preserves existing keys and will not repair this value.", name, path, codexApprovalPolicyNotice)
		}
	}
	reason := "has no effective approval_policy"
	if err != nil || doc == nil {
		reason = "could not be read or parsed to verify approval_policy"
	}
	return fmt.Sprintf("Codex account %q: %s %s; sessions can stop on Codex's approval picker. Set approval_policy in that file or its selected profile, or run `af accounts add codex %s` to seed missing runtime settings from ~/.codex/config.toml.", name, path, reason, name)
}

// codexApprovalSeedValue excludes ignored fields from a validated granular map:
// schema compatibility does not authorize copying arbitrary configuration.
func codexApprovalSeedValue(value any) any {
	policy, ok := value.(map[string]any)
	if !ok {
		return value
	}
	granular := policy["granular"].(map[string]any) // Caller validated the shape.
	safe := make(map[string]any)
	for _, key := range []string{"sandbox_approval", "rules", "mcp_elicitations", "request_permissions", "skill_approval"} {
		if field, exists := granular[key]; exists {
			safe[key] = field
		}
	}
	return map[string]any{"granular": safe}
}

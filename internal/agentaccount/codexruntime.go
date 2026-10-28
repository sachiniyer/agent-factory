package agentaccount

// SandboxMode in codex-rs/core/config.schema.json is this exact string enum.
const codexSandboxModeNotice = "Accepted sandbox_mode values: read-only · workspace-write · danger-full-access."

func validCodexSandboxMode(value any) bool {
	mode, ok := value.(string)
	return ok && (mode == "read-only" || mode == "workspace-write" || mode == "danger-full-access")
}

// codexRuntimeSource refuses all seeding when the selected runtime cannot be
// understood, so registration never partially inherits an invalid configuration.
// The same reason is printed by the login and add notices.
func codexRuntimeSource(doc map[string]any) (map[string]any, string) {
	effective, reason := codexEffectiveSettings(doc)
	if reason != "" {
		return nil, reason
	}
	if policy, exists := effective["approval_policy"]; exists && !validCodexApprovalPolicy(policy) {
		return nil, "approval_policy could not be verified. " + codexApprovalPolicyNotice
	}
	if mode, exists := effective["sandbox_mode"]; exists && !validCodexSandboxMode(mode) {
		return nil, "sandbox_mode could not be verified. " + codexSandboxModeNotice
	}
	if mode, _ := effective["sandbox_mode"].(string); mode == "workspace-write" {
		if options, exists := effective["sandbox_workspace_write"]; exists && !validCodexWorkspaceOptions(options) {
			return nil, "sandbox_workspace_write could not be verified. Expected boolean network_access · exclude_tmpdir_env_var · exclude_slash_tmp, and an array of strings for writable_roots"
		}
	}
	return effective, ""
}

// validCodexWorkspaceOptions checks the allowlisted fields against the upstream
// SandboxWorkspaceWrite schema. Unknown fields are never copied.
func validCodexWorkspaceOptions(value any) bool {
	table, ok := value.(map[string]any)
	if !ok {
		return false
	}
	for _, key := range []string{"network_access", "exclude_tmpdir_env_var", "exclude_slash_tmp"} {
		if value, exists := table[key]; exists {
			if _, ok := value.(bool); !ok {
				return false
			}
		}
	}
	if value, exists := table["writable_roots"]; exists {
		roots, ok := value.([]any)
		if !ok {
			return false
		}
		for _, root := range roots {
			if _, ok := root.(string); !ok {
				return false
			}
		}
	}
	return true
}

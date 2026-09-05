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
	return effective, ""
}

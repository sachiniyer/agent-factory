package session

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// agentPolicyFor resolves the operator policy an off-box agent-server runs under
// (#5571 W2.5) and renders it as the file the provisioner writes beside the
// session. It runs on the daemon: the repo layer is read from the working tree
// (uncommitted .agent-factory edits included) and the legacy per-repo keys the
// TUI saves hooks into, none of which the sandbox's clone holds. branchPrefix,
// when non-nil, is the daemon's frozen snapshot and wins over a fresh read.
//
// A repo config the repository answered as invalid degrades to the global
// layer with a warning rather than failing the create: the agent-server still
// never reads its own host's config. A repo config that could not be READ in
// time is different — the policy is the sandbox agent's whole configuration,
// and shipping the global layer in place of a project it could not learn
// launches that agent on the wrong default_program and program_overrides — so
// that case fails the create with the actionable unknown-config error (#7077).
func agentPolicyFor(repoPath string, branchPrefix *string) ([]byte, error) {
	global, err := config.ResolveGlobalConfig()
	if err != nil {
		return nil, fmt.Errorf("resolving the operator config for the sandbox failed: %w", err)
	}
	repo, rerr := resolveProjectConfig(repoPath, runtimeProjectConfig())
	if rerr != nil {
		if isProjectConfigUnknown(rerr) {
			return nil, fmt.Errorf("resolving the project config for the sandbox failed: %w", rerr)
		}
		log.WarningLog.Printf("sandbox policy: could not resolve the repo config for %q, shipping the global layer only: %v", repoPath, rerr)
	}
	if repo == nil {
		repo = global
	}
	policy := config.NewAgentPolicy(global, repo)
	if branchPrefix != nil {
		policy.Global.BranchPrefix = *branchPrefix
		policy.Repo.BranchPrefix = *branchPrefix
	}
	data, err := policy.Marshal()
	if err != nil {
		return nil, fmt.Errorf("encoding the sandbox policy failed: %w", err)
	}
	return data, nil
}

package config

import "github.com/sachiniyer/agent-factory/log"

const (
	// WorktreeRootSubdirectory stores worktrees under the global config directory.
	WorktreeRootSubdirectory = "subdirectory"
	// WorktreeRootSibling stores worktrees beside the repository.
	WorktreeRootSibling = "sibling"
)

func validateWorktreeRootValue(value string) bool {
	return value == WorktreeRootSubdirectory || value == WorktreeRootSibling
}

func normalizeWorktreeRoot(value, prettyConfigPath string) string {
	if validateWorktreeRootValue(value) {
		return value
	}
	log.WarningLog.Printf("config %s: worktree_root=%q is not one of [%s, %s]; using default %q",
		prettyConfigPath, value, WorktreeRootSubdirectory, WorktreeRootSibling, WorktreeRootSibling)
	return WorktreeRootSibling
}

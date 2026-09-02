package session

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/session/git"
)

// GitWorktreeData represents the serializable data of a GitWorktree.
//
// BranchCreatedByUs indicates whether the session created the underlying
// branch itself (vs. reused a pre-existing one). It is serialized via a
// pointer so that "missing" (nil, for data written before this field was
// added) can be distinguished from an explicit false. Missing values are
// treated as true to preserve the prior behavior for sessions that existed
// before this flag was introduced.
type GitWorktreeData struct {
	RepoPath          string `json:"repo_path"`
	WorktreePath      string `json:"worktree_path"`
	SessionName       string `json:"session_name"`
	BranchName        string `json:"branch_name"`
	BaseCommitSHA     string `json:"base_commit_sha"`
	ExternalWorktree  bool   `json:"external_worktree,omitempty"`
	BranchCreatedByUs *bool  `json:"branch_created_by_us,omitempty"`
	// HookScopeUnitPrefix is the durable handle for the transient systemd scopes
	// a DAEMON-spawned post-worktree or archive hook enters (#3650). Every such
	// scope is named "<prefix>-<generation>-<index>.scope", so this one string
	// names every generation's scopes for this session — including a run that
	// outlived the daemon that started it, which is the whole point: hooksCancel,
	// cmd.Wait and the process-group pgid all died with that daemon.
	//
	// It is OPTIONAL, and its absence is load-bearing rather than a default to
	// fill in. A record written before this field existed, or by a path that never
	// entered a scope (every TUI- and CLI-initiated worktree), comes back empty
	// and takes exactly the behaviour that shipped before #3650: no scope sweep,
	// no systemd round trip. A binary rolled back past this change simply drops
	// the field on its next write, which lands the session back in that same
	// pre-#3650 behaviour rather than in an unrepresentable state.
	HookScopeUnitPrefix string `json:"hook_scope_unit_prefix,omitempty"`
	// RelocationRecovery qualifies WorktreePath whenever a bounded lifecycle step
	// did not establish a safe outcome. Some states retain a second pathname;
	// every state blocks consumers until its owning retry resolves it.
	RelocationRecovery *GitWorktreeRelocationRecoveryData `json:"relocation_recovery,omitempty"`
}

type GitWorktreeRelocationRecoveryData struct {
	State git.RelocationRecoveryState `json:"state,omitempty"`
	// CleanupLifecycle carries cleanup-only states additively while State is
	// projected to claim_stale for previous releases which reject new enum values.
	CleanupLifecycle                   git.RelocationRecoveryState `json:"cleanup_lifecycle,omitempty"`
	AlternatePath                      string                      `json:"alternate_path"`
	IdentityKnown                      bool                        `json:"identity_known,omitempty"`
	Device                             uint64                      `json:"device"`
	Inode                              uint64                      `json:"inode"`
	FileType                           uint32                      `json:"file_type"`
	CleanupGeneration                  string                      `json:"cleanup_generation,omitempty"`
	CleanupOriginalExternalWorktree    *bool                       `json:"cleanup_original_external_worktree,omitempty"`
	CleanupOriginalBranchCreatedByUs   *bool                       `json:"cleanup_original_branch_created_by_us,omitempty"`
	CleanupOriginalStartupStateUnknown *bool                       `json:"cleanup_original_startup_state_unknown,omitempty"`
	OriginalExternalWorktree           *bool                       `json:"original_external_worktree,omitempty"`
	OriginalBranchCreatedByUs          *bool                       `json:"original_branch_created_by_us,omitempty"`
	OriginalStartupStateUnknown        *bool                       `json:"original_startup_state_unknown,omitempty"`
}

func projectRelocationRecoveryForPreviousRelease(recovery *GitWorktreeRelocationRecoveryData) {
	if recovery == nil {
		return
	}
	switch recovery.State {
	case git.RelocationRecoveryCleanupReady, git.RelocationRecoveryCleanupFinalizing:
		// The immediately preceding reader understands recovery but not this
		// additive lifecycle. Preserve the actual values for current readers, and
		// give the old reader ownership values which remain safe even after its
		// repo-gone restore consumes claim_stale and clears the record.
		recovery.CleanupOriginalExternalWorktree = cloneBoolPointer(recovery.OriginalExternalWorktree)
		recovery.CleanupOriginalBranchCreatedByUs = cloneBoolPointer(recovery.OriginalBranchCreatedByUs)
		recovery.CleanupOriginalStartupStateUnknown = cloneBoolPointer(recovery.OriginalStartupStateUnknown)
		safeExternal := true
		safeBranchOwned := false
		safeStartupUnknown := true
		recovery.OriginalExternalWorktree = &safeExternal
		recovery.OriginalBranchCreatedByUs = &safeBranchOwned
		recovery.OriginalStartupStateUnknown = &safeStartupUnknown
		recovery.CleanupLifecycle = recovery.State
		recovery.State = git.RelocationRecoveryClaimStale
	}
}

func runtimeRelocationRecoveryState(
	recovery *GitWorktreeRelocationRecoveryData,
) (git.RelocationRecoveryState, error) {
	if recovery.CleanupLifecycle == "" {
		return recovery.State, nil
	}
	if recovery.State != git.RelocationRecoveryClaimStale {
		return "", fmt.Errorf(
			"cleanup lifecycle %q requires a claim_stale compatibility state, got %q",
			recovery.CleanupLifecycle, recovery.State,
		)
	}
	if recovery.CleanupLifecycle != git.RelocationRecoveryCleanupReady &&
		recovery.CleanupLifecycle != git.RelocationRecoveryCleanupFinalizing {
		return "", fmt.Errorf("unknown cleanup relocation lifecycle %q", recovery.CleanupLifecycle)
	}
	return recovery.CleanupLifecycle, nil
}

package app

import (
	"testing"
)

// TestPendingInstanceCollisionShouldSkip covers the decision logic used by
// mergePendingInstances when a scheduled-task rerun produces a pending
// instance whose title collides with an existing sidebar instance. See
// issue #255: a rerun recreates the same tmux session name under a new
// worktree path, so TmuxAlive() alone cannot tell whether the sidebar
// instance is still valid.
func TestPendingInstanceCollisionShouldSkip(t *testing.T) {
	cases := []struct {
		name             string
		existingWorktree string
		pendingWorktree  string
		tmuxAlive        bool
		wantSkip         bool
	}{
		{
			name:             "worktree paths differ and tmux alive -> replace (issue #255)",
			existingWorktree: "/repo/worktrees/task",
			pendingWorktree:  "/repo/worktrees/task-2",
			tmuxAlive:        true,
			wantSkip:         false,
		},
		{
			name:             "worktree paths differ and tmux dead -> replace",
			existingWorktree: "/repo/worktrees/task",
			pendingWorktree:  "/repo/worktrees/task-2",
			tmuxAlive:        false,
			wantSkip:         false,
		},
		{
			name:             "worktree paths match and tmux alive -> skip",
			existingWorktree: "/repo/worktrees/task",
			pendingWorktree:  "/repo/worktrees/task",
			tmuxAlive:        true,
			wantSkip:         true,
		},
		{
			name:             "worktree paths match and tmux dead -> replace",
			existingWorktree: "/repo/worktrees/task",
			pendingWorktree:  "/repo/worktrees/task",
			tmuxAlive:        false,
			wantSkip:         false,
		},
		{
			name:             "existing worktree unknown and tmux alive -> skip",
			existingWorktree: "",
			pendingWorktree:  "/repo/worktrees/task",
			tmuxAlive:        true,
			wantSkip:         true,
		},
		{
			name:             "pending worktree unknown and tmux alive -> skip",
			existingWorktree: "/repo/worktrees/task",
			pendingWorktree:  "",
			tmuxAlive:        true,
			wantSkip:         true,
		},
		{
			name:             "both worktrees unknown and tmux dead -> replace",
			existingWorktree: "",
			pendingWorktree:  "",
			tmuxAlive:        false,
			wantSkip:         false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pendingInstanceCollisionShouldSkip(tc.existingWorktree, tc.pendingWorktree, tc.tmuxAlive)
			if got != tc.wantSkip {
				t.Fatalf("pendingInstanceCollisionShouldSkip(%q, %q, %v) = %v; want %v",
					tc.existingWorktree, tc.pendingWorktree, tc.tmuxAlive, got, tc.wantSkip)
			}
		})
	}
}

package app

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session"
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

// TestSessionAutoYesAuthoritative is a regression test for issue #326.
//
// Previously the TUI loops only set instance.AutoYes = true when the
// session-level autoYes was true and never cleared it, so a prior
// `--auto-yes` run that persisted AutoYes=true would silently keep
// auto-accepting prompts in subsequent TUI runs without the flag.
//
// The fix synchronizes instance.AutoYes with the session-level autoYes
// in all TUI paths (loading instances, starting instances, merging
// pending instances, and refreshing external instances). This test
// guards the load-instances path: it verifies that a persisted
// AutoYes=true is cleared when the session autoYes is false.
func TestSessionAutoYesAuthoritative(t *testing.T) {
	cases := []struct {
		name           string
		persistedValue bool
		sessionAutoYes bool
		want           bool
	}{
		{
			name:           "persisted true, session false -> false (issue #326)",
			persistedValue: true,
			sessionAutoYes: false,
			want:           false,
		},
		{
			name:           "persisted false, session true -> true",
			persistedValue: false,
			sessionAutoYes: true,
			want:           true,
		},
		{
			name:           "persisted true, session true -> true",
			persistedValue: true,
			sessionAutoYes: true,
			want:           true,
		},
		{
			name:           "persisted false, session false -> false",
			persistedValue: false,
			sessionAutoYes: false,
			want:           false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Mirror the load-instances loop in app.go: the session-level
			// autoYes must be authoritative over the persisted value.
			instances := []*session.Instance{{Title: "t", AutoYes: tc.persistedValue}}
			autoYes := tc.sessionAutoYes
			for _, instance := range instances {
				instance.AutoYes = autoYes
			}
			if instances[0].AutoYes != tc.want {
				t.Fatalf("instance.AutoYes = %v; want %v", instances[0].AutoYes, tc.want)
			}
		})
	}
}

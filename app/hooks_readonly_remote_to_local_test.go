package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
)

// TestHooksPaneEditableAfterRemoteToLocalSwitch guards the write-once
// read-only flag in the hooks pane. A TUI started against a remote target arms
// HooksPane.readOnly via SetReadOnly(remoteHooksReadOnlyReason()). The app
// re-resolves the daemon target live (e.g. after `af target clear` in another
// terminal), so isRemoteTarget() can flip true→false mid-session. A subsequent
// switchProject to a local repo loads that project's real, saveable hooks into
// the pane; the local branch must clear the stale reason via SetReadOnly(""),
// otherwise n/enter/D stay refused forever with a banner pointing at the
// no-longer-targeted daemon host while the save path (IsDirty() &&
// isRemoteTarget()) would happily commit an edit.
func TestHooksPaneEditableAfterRemoteToLocalSwitch(t *testing.T) {
	h := newTestHome(t)
	h.snapshotFetcher = func(string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{}, nil
	}
	// Stub the live target re-evaluation the rest of the app reads per call,
	// saving and restoring the package var around the two phases below.
	prev := isRemoteTarget
	t.Cleanup(func() { isRemoteTarget = prev })

	// Phase 1: remote target. switchProject lands on the remote branch, which
	// arms the read-only flag and leaves the pane empty — the preconditions
	// for the bug.
	isRemoteTarget = func() bool { return true }
	repoARoot := initTestGitRepo(t)
	repoA := &config.RepoContext{Root: repoARoot, ID: config.RepoIDFromRoot(repoARoot)}
	_, _ = h.switchProject(repoA)

	hp := h.hooksPane
	hp.SetSize(80, 12)
	require.Empty(t, hp.GetCommands(), "a remote switch leaves the hooks pane empty")
	require.Contains(t, hp.String(), "daemon host", "the pane shows the remote read-only banner")

	// The edit keys are consumed but have no effect: the pane is read-only.
	hp.SetFocus(true)
	for _, k := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune("n")},
		{Type: tea.KeyEnter},
		{Type: tea.KeyRunes, Runes: []rune("D")},
	} {
		require.True(t, hp.HandleKeyPress(k), "a read-only pane still consumes the edit key")
		require.False(t, hp.IsEditing(), "a read-only pane must not start an edit (key: %q)", k.String())
		require.False(t, hp.IsDirty(), "a read-only pane must not mark itself dirty (key: %q)", k.String())
	}

	// Phase 2: the target flipped to local mid-session (af target clear). The
	// switch to a second repo resolves its in-repo config and loads the real,
	// saveable hooks into the pane.
	isRemoteTarget = func() bool { return false }
	repoBRoot := initTestGitRepo(t)
	writeInRepoConfig(t, repoBRoot, "post_worktree_commands = [\"echo b1\", \"echo b2\"]\n")
	repoB := &config.RepoContext{Root: repoBRoot, ID: config.RepoIDFromRoot(repoBRoot)}
	_, _ = h.switchProject(repoB)

	require.Equal(t, []string{"echo b1", "echo b2"}, hp.GetCommands(),
		"the local switch loads the new project's hooks into the pane")
	require.NotContains(t, hp.String(), "daemon host",
		"the stale remote banner must be gone after the switch to a local target")

	// After the fix the pane is editable again, so each edit key takes effect.
	hp.SetFocus(true)

	// `n` starts an add.
	require.True(t, hp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")}))
	require.True(t, hp.IsEditing(), "after a remote→local switch, n must start an add — the pane is no longer read-only")
	require.True(t, hp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEsc}), "esc cancels the add")

	// `enter` opens an inline edit on the selected hook.
	require.True(t, hp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter}))
	require.True(t, hp.IsEditing(), "enter must open an inline edit after the switch to local")
	require.True(t, hp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEsc}), "esc cancels the edit")

	// `D` deletes the selected hook and marks the pane dirty.
	require.True(t, hp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")}))
	require.True(t, hp.IsDirty(), "D must delete a hook after the switch to local — the pane is editable")
	require.Len(t, hp.GetCommands(), 1, "one of the two hooks was deleted")
}

// TestHooksPaneEditableAfterLocalSwitchWithConfigResolveFailure guards the
// other local branch of switchProject: when the target is local but config
// resolve fails, the pane is emptied (so the outgoing project's hooks cannot
// leak, #1686) and must ALSO be editable — a stale remote reason must not
// survive the switch, and a save from this checkout would still write to its
// in-repo config.
func TestHooksPaneEditableAfterLocalSwitchWithConfigResolveFailure(t *testing.T) {
	h := newTestHome(t)
	h.snapshotFetcher = func(string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{}, nil
	}
	prev := isRemoteTarget
	t.Cleanup(func() { isRemoteTarget = prev })

	// Arm the read-only flag with a remote switch.
	isRemoteTarget = func() bool { return true }
	repoARoot := initTestGitRepo(t)
	repoA := &config.RepoContext{Root: repoARoot, ID: config.RepoIDFromRoot(repoARoot)}
	_, _ = h.switchProject(repoA)
	hp := h.hooksPane
	hp.SetSize(80, 12)
	require.Contains(t, hp.String(), "daemon host", "precondition: the remote switch armed the read-only flag")

	// Flip to local and switch to a project whose config cannot be parsed.
	isRemoteTarget = func() bool { return false }
	repoBRoot := initTestGitRepo(t)
	writeInRepoConfig(t, repoBRoot, "invalid toml {{{\n[broken")
	repoB := &config.RepoContext{Root: repoBRoot, ID: config.RepoIDFromRoot(repoBRoot)}
	_, err := config.ResolveConfig(repoBRoot)
	require.Error(t, err, "precondition: ResolveConfig must fail for the corrupt in-repo config")
	_, _ = h.switchProject(repoB)

	require.Empty(t, hp.GetCommands(), "the failed resolve clears the hooks pane (#1686)")
	require.NotContains(t, hp.String(), "daemon host",
		"the stale remote banner must be cleared even when the local config fails to resolve")

	// The pane is editable: an add can be started (and a save would write to
	// this checkout's in-repo config).
	hp.SetFocus(true)
	require.True(t, hp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")}))
	require.True(t, hp.IsEditing(), "n must start an add after the local switch with a failed resolve")
}

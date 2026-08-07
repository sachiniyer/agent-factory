package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// remoteCaps is the off-box row of the capability table, as
// remoteAgentBackend declares it.
func remoteCaps() Capabilities { return (&remoteAgentBackend{}).Capabilities() }

func localCaps() Capabilities { return (&LocalBackend{}).Capabilities() }

// TestTabKindRequiresClassifiesEveryKind pins the classification itself. The
// default is the conservative one on purpose: a kind nobody classified must be
// treated as spawning a process, not silently admitted everywhere.
func TestTabKindRequiresClassifiesEveryKind(t *testing.T) {
	for _, tc := range []struct {
		kind TabKind
		want TabKindNeed
	}{
		{TabKindWeb, TabNeedsMetadataOnly},
		{TabKindVSCode, TabNeedsLocalWorktreeRead},
		{TabKindShell, TabNeedsLocalProcess},
		{TabKindProcess, TabNeedsLocalProcess},
		{TabKindAgent, TabNeedsLocalProcess},
		{TabKind(9999), TabNeedsLocalProcess},
	} {
		assert.Equal(t, tc.want, TabKindRequires(tc.kind), "kind %v classified wrong", tc.kind)
	}
}

// TestRefuseTabKindAdmitsMetadataTabsOffBox is the #3053 regression. A web tab
// is a name and a URL: it spawns nothing and reads nothing, so no backend has
// grounds to refuse it. Before the fix the single TabManagement bit refused it
// alongside the kinds that genuinely need a worktree.
func TestRefuseTabKindAdmitsMetadataTabsOffBox(t *testing.T) {
	require.False(t, remoteCaps().TabManagement,
		"fixture drifted: the off-box row is supposed to lack TabManagement")

	require.NoError(t, remoteCaps().RefuseTabKind(TabKindWeb),
		"an off-box session refused a web tab, which needs nothing from the workspace")
	require.NoError(t, localCaps().RefuseTabKind(TabKindWeb))
}

// TestRefuseTabKindNamesTheUnmetRequirement is the other half of #3053: a
// refusal that survives must say what it actually is. "Not supported on this
// backend" cannot be told apart from a kind that could have worked.
func TestRefuseTabKindNamesTheUnmetRequirement(t *testing.T) {
	t.Run("vscode names the editor, not a spawn", func(t *testing.T) {
		err := remoteCaps().RefuseTabKind(TabKindVSCode)
		require.Error(t, err, "an off-box session must still refuse a vscode tab")
		msg := strings.ToLower(err.Error())
		assert.Contains(t, msg, "editor", "the refusal must name the editor requirement")
		assert.NotContains(t, msg, "spawn",
			"a vscode tab needs the worktree to READ, not to spawn in; saying spawn is the #3053 defect")
	})

	t.Run("process names the spawn", func(t *testing.T) {
		for _, kind := range []TabKind{TabKindShell, TabKindProcess} {
			err := remoteCaps().RefuseTabKind(kind)
			require.Error(t, err)
			assert.Contains(t, strings.ToLower(err.Error()), "spawn",
				"a process-backed refusal must name the spawn it cannot do")
		}
	})

	t.Run("local admits every kind", func(t *testing.T) {
		for _, kind := range []TabKind{TabKindWeb, TabKindVSCode, TabKindShell, TabKindProcess} {
			require.NoError(t, localCaps().RefuseTabKind(kind), "kind %v refused on a local backend", kind)
		}
	})
}

// TestTabSpawnPreconditionIsPerKind pins the SECOND gate, one layer under the
// capability check. It demanded a local tmux AND worktree for every kind, so a
// web tab would have failed here even with the capability gate opened — the
// plain struct proved nothing about the wiring.
func TestTabSpawnPreconditionIsPerKind(t *testing.T) {
	const (
		started     = true
		noTmux      = false
		noWorktree  = false
		hasTmux     = true
		hasWorktree = true
	)

	require.NoError(t, tabSpawnPreconditionErr(started, noTmux, noWorktree, TabKindWeb),
		"a web tab needs neither tmux nor a worktree, but the precondition demanded both")

	vscodeErr := tabSpawnPreconditionErr(started, noTmux, noWorktree, TabKindVSCode)
	require.Error(t, vscodeErr)
	assert.Contains(t, strings.ToLower(vscodeErr.Error()), "editor")
	require.NoError(t, tabSpawnPreconditionErr(started, noTmux, hasWorktree, TabKindVSCode),
		"a vscode tab needs the worktree to read; it owns no PTY, so tmux is not its requirement")

	shellErr := tabSpawnPreconditionErr(started, noTmux, noWorktree, TabKindShell)
	require.Error(t, shellErr)
	assert.Contains(t, strings.ToLower(shellErr.Error()), "spawn")
	require.NoError(t, tabSpawnPreconditionErr(started, hasTmux, hasWorktree, TabKindShell))

	// "Not started" outranks every kind: there is no session to attach to yet.
	for _, kind := range []TabKind{TabKindWeb, TabKindVSCode, TabKindShell} {
		err := tabSpawnPreconditionErr(false, hasTmux, hasWorktree, kind)
		require.Error(t, err, "kind %v admitted on a session that is not started", kind)
		assert.Contains(t, err.Error(), "not started")
	}
}

// TestOffBoxRefusalsStayClosed guards the #3054 coupling. Opening #3053 must not
// make the vscode proxy route reachable off-box: its editor is served by the
// daemon from a local worktree path, and an off-box session has none.
func TestOffBoxRefusalsStayClosed(t *testing.T) {
	require.Error(t, remoteCaps().RefuseTabKind(TabKindVSCode),
		"#3054 would be live: an off-box session may not create a vscode tab until the editor is served from inside the workspace")
	require.NotEqual(t, WorkspaceLocalWorktree, remoteCaps().Workspace,
		"fixture drifted: the off-box row must not claim a local worktree")
}

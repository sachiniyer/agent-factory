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

// TestRefuseTabKindGivesEachKindItsOwnReason is the #3053 regression. The three
// kinds are refused off-box for THREE DIFFERENT reasons, and before the fix all
// three were refused for one — the worktree a web tab never needed. That the
// reasons are distinct is the property under test: it is what lets a user tell a
// tab kind that could work from one that cannot.
func TestRefuseTabKindGivesEachKindItsOwnReason(t *testing.T) {
	require.False(t, remoteCaps().TabManagement,
		"fixture drifted: the off-box row is supposed to lack TabManagement")

	web := remoteCaps().RefuseTabKind(TabKindWeb, "http://localhost:3000")
	vscode := remoteCaps().RefuseTabKind(TabKindVSCode, "")
	shell := remoteCaps().RefuseTabKind(TabKindShell, "")
	require.Error(t, web, "a web tab cannot be SERVED off-box yet (#3062)")
	require.Error(t, vscode)
	require.Error(t, shell)

	msgs := map[string]string{"web": web.Error(), "vscode": vscode.Error(), "shell": shell.Error()}
	for a, ma := range msgs {
		for bName, mb := range msgs {
			if a < bName {
				assert.NotEqual(t, ma, mb, "%s and %s share a refusal message; the whole point is that they differ", a, bName)
			}
		}
	}

	// And each names its OWN missing requirement, not another kind's.
	assert.NotContains(t, strings.ToLower(msgs["web"]), "spawn",
		"a web tab spawns nothing; saying spawn is the #3053 defect")
	assert.Contains(t, strings.ToLower(msgs["web"]), "proxied",
		"the web refusal must name the serving problem that actually blocks it")
	assert.Contains(t, msgs["web"], "3062", "the web refusal must point at the work that lifts it")
}

// TestWebRefusalNamesOnlyTheBlockersThatApply is this PR's own thesis applied to
// its own message. Only LOOPBACK targets are reverse-proxied, so citing the
// daemon-host routing gap for an external URL states a requirement that tab does
// not have — the same defect one level down. Both targets are refused, for
// different and individually true reasons.
func TestWebRefusalNamesOnlyTheBlockersThatApply(t *testing.T) {
	loopback := remoteCaps().RefuseTabKind(TabKindWeb, "http://localhost:3000")
	external := remoteCaps().RefuseTabKind(TabKindWeb, "https://example.com")
	require.Error(t, loopback)
	require.Error(t, external, "an off-box web tab is still not restored across a restart, whatever it points at")

	assert.Contains(t, strings.ToLower(loopback.Error()), "proxied",
		"a loopback target IS reverse-proxied from the daemon host; the refusal must say so")
	assert.NotContains(t, strings.ToLower(external.Error()), "proxied",
		"an external URL is iframed directly and never proxied; claiming otherwise is the #3053 defect recreated")

	// The blocker they DO share is the one that is unconditionally true.
	for name, err := range map[string]error{"loopback": loopback, "external": external} {
		assert.Contains(t, strings.ToLower(err.Error()), "restored",
			"%s refusal must name the restore gap, which applies to every off-box web tab", name)
		assert.Contains(t, err.Error(), "3062", "%s refusal must point at the work that lifts it", name)
	}

	// A local session takes neither branch.
	require.NoError(t, localCaps().RefuseTabKind(TabKindWeb, "http://localhost:3000"))
	require.NoError(t, localCaps().RefuseTabKind(TabKindWeb, "https://example.com"))
}

// TestRefuseTabKindNamesTheUnmetRequirement is the other half of #3053: a
// refusal that survives must say what it actually is. "Not supported on this
// backend" cannot be told apart from a kind that could have worked.
func TestRefuseTabKindNamesTheUnmetRequirement(t *testing.T) {
	t.Run("vscode names the editor, not a spawn", func(t *testing.T) {
		err := remoteCaps().RefuseTabKind(TabKindVSCode, "")
		require.Error(t, err, "an off-box session must still refuse a vscode tab")
		msg := strings.ToLower(err.Error())
		assert.Contains(t, msg, "editor", "the refusal must name the editor requirement")
		assert.NotContains(t, msg, "spawn",
			"a vscode tab needs the worktree to READ, not to spawn in; saying spawn is the #3053 defect")
	})

	t.Run("process names the spawn", func(t *testing.T) {
		for _, kind := range []TabKind{TabKindShell, TabKindProcess} {
			err := remoteCaps().RefuseTabKind(kind, "")
			require.Error(t, err)
			assert.Contains(t, strings.ToLower(err.Error()), "spawn",
				"a process-backed refusal must name the spawn it cannot do")
		}
	})

	t.Run("local admits every kind", func(t *testing.T) {
		for _, kind := range []TabKind{TabKindWeb, TabKindVSCode, TabKindShell, TabKindProcess} {
			require.NoError(t, localCaps().RefuseTabKind(kind, ""), "kind %v refused on a local backend", kind)
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

	// This layer answers MECHANICS — what the kind needs from the instance — and
	// a web tab needs nothing, which stays true. Whether the daemon can SERVE one
	// off-box is a separate question, answered by RefuseTabKind above (#3062).
	// Keeping them apart is what stops the serving gap from being re-encoded as a
	// fake worktree requirement.
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
	require.Error(t, remoteCaps().RefuseTabKind(TabKindVSCode, ""),
		"#3054 would be live: an off-box session may not create a vscode tab until the editor is served from inside the workspace")
	require.NotEqual(t, WorkspaceLocalWorktree, remoteCaps().Workspace,
		"fixture drifted: the off-box row must not claim a local worktree")
}

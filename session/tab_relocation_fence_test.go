package session

import (
	"testing"

	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRelocationFenceAppliesByTabNeed is the #5174 review fix: an unresolved
// af-managed worktree relocation must refuse only tab kinds that can touch the
// worktree — a local process spawn or a local worktree read — because those
// are the paths where the recorded directory is not yet authoritative. A web
// tab is pure metadata (a name and a URL): it spawns nothing and reads
// nothing, so refusing it protects no invariant and only wedges the roster.
func TestRelocationFenceAppliesByTabNeed(t *testing.T) {
	inst := &Instance{
		ID:       "reloc-fence",
		Title:    "reloc-fence",
		Path:     t.TempDir(),
		Program:  tmux.ProgramClaude,
		backend:  NewFakeBackend(),
		started:  true,
		liveness: LiveRunning,
	}
	gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, t.TempDir(), inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
	require.NoError(t, gw.RestoreRelocationRecovery(sessiongit.RelocationRecovery{
		State: sessiongit.RelocationRecoveryStalled,
	}))
	inst.Tabs = []*Tab{newAgentTab(tmux.NewTmuxSession("af_reloc_fence", tmux.ProgramClaude))}

	// Web is metadata-only: it must stay addable while the move is unsettled.
	tab, err := inst.AddWebTab("http://127.0.0.1:8080/", "")
	require.NoError(t, err,
		"a web tab is a name and a URL — an unresolved relocation is not its concern")
	require.Equal(t, TabKindWeb, tab.Kind)

	// VS Code reads the worktree at serve time; shell/process spawn inside it —
	// all three must wait for the move to settle.
	_, err = inst.AddVSCodeTab("")
	require.ErrorContains(t, err, "unresolved worktree relocation")
	_, err = inst.AddShellTab()
	require.ErrorContains(t, err, "unresolved worktree relocation")
	_, err = inst.AddProcessTab("sleep 60", "")
	require.ErrorContains(t, err, "unresolved worktree relocation")

	// The roster gained exactly the one tab it was owed.
	kinds := []TabKind{}
	for _, tb := range inst.Tabs {
		kinds = append(kinds, tb.Kind)
	}
	assert.Equal(t, []TabKind{TabKindAgent, TabKindWeb}, kinds)
}

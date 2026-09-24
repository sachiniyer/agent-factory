package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

// These tests pin the alignment between admission's cross-agent verdict and
// MissionBrief.Render's same-agent branch for an OPAQUE handoff target — a
// program_overrides command af cannot classify as an agent.
//
// Admission's opaque branch decides sameness from recorded enum == target enum
// (HandoffTargetIsCurrent). CaptureHandoffBrief sets the brief's To to the
// target enum and its From to swap.From.Agent (the resolved running identity).
// A two-step program_overrides chain can make the resolved running identity
// equal the target enum while the recorded enum differs, so admission admits a
// cross-agent handoff whose From == To would otherwise collapse Render onto the
// same-agent branch. The CrossAgent flag threads admission's verdict through so
// Render honors it instead of re-deriving it from From == To (#4430 review).

// setupBugInstance configures the two-step override chain that yields
// swap.From.Agent == target while admission is cross-agent, then proves each
// invariant through the real config I/O and admission predicate.
func setupBugInstance(t *testing.T) *Instance {
	t.Helper()
	const wrapper = "/home/dev/bin/agent-wrapper"
	saveProgramOverrides(t, map[string]string{
		tmux.ProgramClaude: tmux.ProgramCodex,
		tmux.ProgramCodex:  wrapper,
	})
	inst := handoffTestInstance(t, tmux.ProgramClaude)
	inst.Tabs[0].Conversation = AgentConversationData{}
	inst.SetTmuxSession(tmux.NewTmuxSession("af-bug", tmux.ProgramCodex))
	require.Equal(t, tmux.ProgramCodex, inst.CurrentAgentName(),
		"precondition: the pane runs the override target's provable agent (codex)")
	require.Equal(t, tmux.ProgramClaude, inst.AgentProgram(),
		"precondition: the recorded enum is still claude")
	require.Empty(t, HandoffEffectiveAgentForPath(inst.Path, tmux.ProgramCodex),
		"precondition: codex's own override is an unprovable wrapper — opaque target")
	require.False(t, HandoffTargetIsCurrent(inst.CurrentAgentName(), tmux.ProgramCodex, "", inst.AgentProgram()),
		"precondition: admission must classify this as cross-agent (recorded claude != target codex)")
	return inst
}

// attachRepo gives the instance a real worktree; withWork=true also commits once
// and leaves a dirty file so the brief's Work section is non-empty.
func attachRepo(t *testing.T, inst *Instance, withWork bool) {
	t.Helper()
	repo := initTempGitRepo(t)
	if withWork {
		gitOut(t, repo, "-c", "user.name=Bug", "-c", "user.email=bug@example.com",
			"commit", "--allow-empty", "-m", "base")
	}
	gw, err := git.NewGitWorktreeFromStorage(repo, repo, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
	if withWork {
		require.NoError(t, os.WriteFile(filepath.Join(gw.GetWorktreePath(), "wip.txt"), []byte("wip"), 0o644))
	}
}

// Pre-first-conversation capture path (Conversation.Agent == "" → fallback to
// currentAgentNameLocked). The opaque target makes admission cross-agent while
// From == To == "codex": Render must take the cross-agent branch and attribute
// the work to codex, not collapse onto the same-agent "account handoff" framing.
func TestCaptureHandoffBrief_OpaqueCrossAgent_PreFirstConversation_NonEmptyWork(t *testing.T) {
	inst := setupBugInstance(t)
	attachRepo(t, inst, true)
	require.NoError(t, inst.Transition(BeginHandoff()))
	entry, err := inst.RecordHandoffSwap(tmux.ProgramCodex, "", HandoffReasonManual, "", false)
	require.NoError(t, err)
	require.Equal(t, tmux.ProgramCodex, entry.From.Agent, "outgoing identity is the running codex")
	require.Equal(t, tmux.ProgramClaude, entry.previousProgram, "the pre-swap recorded enum is preserved")
	brief, err := inst.CaptureHandoffBrief(&entry, "finish the work")
	require.NoError(t, err)
	require.Equal(t, "codex", brief.From, "From is the resolved running identity, not the recorded enum")
	require.Equal(t, "codex", brief.To, "To is the target enum admission compared")
	require.True(t, brief.CrossAgent, "admission's cross-agent verdict is threaded onto the brief")
	require.False(t, brief.Work.Empty(), "precondition: the worktree carries work")
	rendered := brief.Render()
	t.Logf("render:\n%s", rendered)
	require.False(t, strings.Contains(rendered, "account handoff"),
		"a cross-agent handoff must not render the same-agent account-handoff framing")
	require.True(t, strings.Contains(rendered, "It was being done by codex"),
		"the cross-agent attribution sentence must name the running agent")
	require.True(t, strings.Contains(rendered, "Do not start over"),
		"the continue-don't-restart instruction must survive")
}

// Empty-work edge case: this is the functional loss the bug report calls out. A
// cross-agent handoff issued before any commits with a clean tree must render
// the full brief (attribution + goal + branch + "Nothing yet" + "Do not start
// over"), NOT collapse to the bare goal. Without the CrossAgent flag the
// same-agent empty-work shortcut returns m.Goal alone — the incoming agent has
// no signal it is continuing a predecessor's session (#2013 blended-context
// hazard).
func TestCaptureHandoffBrief_OpaqueCrossAgent_EmptyWorkKeepsFullContext(t *testing.T) {
	inst := setupBugInstance(t)
	attachRepo(t, inst, false)
	require.NoError(t, inst.Transition(BeginHandoff()))
	entry, err := inst.RecordHandoffSwap(tmux.ProgramCodex, "", HandoffReasonManual, "", false)
	require.NoError(t, err)
	brief, err := inst.CaptureHandoffBrief(&entry, "finish the work")
	require.NoError(t, err)
	require.True(t, brief.Work.Empty(), "precondition: no commits and a clean tree")
	rendered := brief.Render()
	t.Logf("render:\n%s", rendered)
	require.NotEqual(t, "finish the work", rendered,
		"an empty-work cross-agent handoff must NOT collapse to the bare goal")
	require.True(t, strings.Contains(rendered, "It was being done by codex"),
		"attribution must name the running agent even with no work")
	require.True(t, strings.Contains(rendered, "Nothing yet"),
		"the clean-tree notice must be present")
	require.True(t, strings.Contains(rendered, "Do not start over"),
		"the continue-don't-restart instruction must be present even with empty work")
	require.True(t, strings.Contains(rendered, "finish the work"),
		"the goal is still rendered inside the full brief")
}

// Regression guard: an opaque-target cross-agent handoff whose running agent
// DIFFERS from the target must keep the running agent as From. From != To
// already routes Render to the cross-agent branch, so this guards the honest
// attribution the CrossAgent approach preserves — the alternative
// previousProgram fix would have overwritten From to "claude" (the recorded
// enum), mis-attributing the work.
func TestCaptureHandoffBrief_OpaqueCrossAgent_DifferentTargetKeepsRunningAgent(t *testing.T) {
	const aiderWrapper = "/home/dev/bin/aider-wrapper"
	saveProgramOverrides(t, map[string]string{
		tmux.ProgramClaude: tmux.ProgramCodex,
		tmux.ProgramAider:  aiderWrapper,
	})
	inst := handoffTestInstance(t, tmux.ProgramClaude)
	inst.Tabs[0].Conversation = AgentConversationData{}
	inst.SetTmuxSession(tmux.NewTmuxSession("af-aider", tmux.ProgramCodex))
	require.Equal(t, tmux.ProgramCodex, inst.CurrentAgentName())
	require.Empty(t, HandoffEffectiveAgentForPath(inst.Path, tmux.ProgramAider),
		"precondition: aider's override is an opaque wrapper")
	require.False(t, HandoffTargetIsCurrent(inst.CurrentAgentName(), tmux.ProgramAider, "", inst.AgentProgram()),
		"precondition: admission must classify this as cross-agent")
	attachRepo(t, inst, true)
	require.NoError(t, inst.Transition(BeginHandoff()))
	entry, err := inst.RecordHandoffSwap(tmux.ProgramAider, "", HandoffReasonManual, "", false)
	require.NoError(t, err)
	require.Equal(t, tmux.ProgramCodex, entry.From.Agent)
	require.Equal(t, tmux.ProgramAider, entry.To)
	require.Equal(t, tmux.ProgramClaude, entry.previousProgram)
	brief, err := inst.CaptureHandoffBrief(&entry, "finish the work")
	require.NoError(t, err)
	require.Equal(t, tmux.ProgramCodex, brief.From, "From must stay the running codex, NOT the recorded claude")
	require.True(t, brief.CrossAgent)
	rendered := brief.Render()
	require.True(t, strings.Contains(rendered, "It was being done by codex"),
		"attribution names the process that actually ran")
	require.False(t, strings.Contains(rendered, "It was being done by claude"),
		"attribution must not fall back to the recorded enum")
}

// Same-agent (account-only) handoffs are unaffected: CrossAgent is false, so
// Render's sameAgent is unchanged and the account-handoff framing renders as
// before. These direct Render guards pin the non-regression for the genuine
// same-agent path that the account-swap machinery (SelectAccountForHandoff)
// exercises in production — RecordHandoffSwap always passes crossAgent=true
// and refuses same-target, so it cannot reach this branch.
func TestMissionBrief_Render_SameAgentPreservedWhenCrossAgentFalse(t *testing.T) {
	// Non-empty work: the account-handoff framing sentence renders.
	brief := MissionBrief{
		From: tmux.ProgramClaude,
		To:   tmux.ProgramClaude,
		Goal: "finish the work",
		Work: git.WorkSummary{Branch: "main", Commits: 1, DirtyFiles: 0, HeadSHA: "abc"},
	}
	rendered := brief.Render()
	require.True(t, strings.Contains(rendered, "account handoff"),
		"a genuine same-agent swap must still render the account-handoff framing")
	require.False(t, strings.Contains(rendered, "It was being done by"),
		"the cross-agent attribution sentence is not used for a same-agent swap")
	require.True(t, strings.Contains(rendered, "Do not start over"))

	// Empty work + no carry failure: the same-agent shortcut returns the bare
	// goal. This is the INTENTIONAL same-agent behavior; the bug is that the
	// cross-agent path reached it. CrossAgent=false keeps it here.
	empty := MissionBrief{From: tmux.ProgramClaude, To: tmux.ProgramClaude, Goal: "finish the work"}
	require.Equal(t, "finish the work", empty.Render(),
		"same-agent empty-work shortcut is preserved when CrossAgent is false")
}

// The flag is what routes a From == To brief off the same-agent branch: with
// CrossAgent=true the same empty brief renders the full cross-agent context
// instead of collapsing to the goal.
func TestMissionBrief_Render_CrossAgentFlagRoutesFromEqualsToOffSameAgent(t *testing.T) {
	brief := MissionBrief{
		From:       "codex",
		To:         "codex",
		CrossAgent: true,
		Goal:       "finish the work",
		Work:       git.WorkSummary{Branch: "main"},
	}
	rendered := brief.Render()
	require.NotEqual(t, "finish the work", rendered,
		"CrossAgent=true must keep the empty-work brief from collapsing to the goal")
	require.True(t, strings.Contains(rendered, "It was being done by codex"),
		"the cross-agent attribution sentence names the running agent")
	require.True(t, strings.Contains(rendered, "Nothing yet"))
	require.True(t, strings.Contains(rendered, "Do not start over"))
}

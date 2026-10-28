package app

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// #4848: pressing D ran the kill confirmation's git status/log reads inside
// Update, freezing the whole TUI for tens of ms normally and up to ~30s with a
// wedged git. These pin that the dialog opens without them, with its confirm
// withheld, and that only the result for the dialog still open completes it.

// settleKillCheck runs the cmd a kill keypress returned and delivers its loss
// check result through Update, as the event loop would. A dialog that opened
// complete (no worktree the kill would remove) must produce no result.
func settleKillCheck(t *testing.T, h *home, cmd tea.Cmd) {
	t.Helper()
	want := 0
	if h.confirmationOverlay != nil && h.confirmationOverlay.Pending() != "" {
		want = 1
	}
	delivered := 0
	for _, msg := range drainCmd(t, cmd, 10*time.Second) {
		if res, ok := msg.(killLossCheckedMsg); ok {
			_, _ = h.Update(res)
			delivered++
		}
	}
	require.Equal(t, want, delivered, "exactly one loss check result per pending kill dialog, none otherwise")
}

// stubKillLossCheck swaps killLossCheck for a fake that blocks on seam and then
// returns loss.
func stubKillLossCheck(t *testing.T, seam *blockingSeam, loss killLossAssessment) {
	t.Helper()
	orig := killLossCheck
	t.Cleanup(func() { killLossCheck = orig })
	killLossCheck = func(session.WorktreeCleanupImpact) killLossAssessment {
		seam.enter()
		return loss
	}
}

// killCheckHome builds a home whose selected row owns a real local worktree, so
// kill takes the path that needs the loss checks.
func killCheckHome(t *testing.T, title string) (*home, *session.Instance) {
	t.Helper()
	repoDir, baseSHA := initBaseRepo(t)
	wt := addWorktree(t, repoDir, baseSHA, "dev/"+title)
	inst := startedWorktreeInstance(t, title, repoDir, wt, "dev/"+title, baseSHA)
	h := newTestHome(t)
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)
	return h, inst
}

// killCheckResults runs cmd — which may still be parked in the seam — and
// returns the loss check results it produced once the seam is released.
func killCheckResults(t *testing.T, cmd tea.Cmd) []killLossCheckedMsg {
	t.Helper()
	var got []killLossCheckedMsg
	for _, msg := range drainCmd(t, cmd, 10*time.Second) {
		if res, ok := msg.(killLossCheckedMsg); ok {
			got = append(got, res)
		}
	}
	return got
}

var (
	keyKill    = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")}
	keyYes     = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}
	keyEscape  = tea.KeyMsg{Type: tea.KeyEsc}
	severeLoss = killLossAssessment{severe: "This branch has 1 commit that is not merged or pushed; killing permanently deletes it. This cannot be undone."}
)

// TestKill_UpdateDoesNotRunLossCheck is the core #4848 property: D returns from
// Update without having run the git checks, the dialog is already open with the
// static pending note, and the confirm is withheld until the result lands.
func TestKill_UpdateDoesNotRunLossCheck(t *testing.T) {
	h, inst := killCheckHome(t, "offloop")
	seam := newBlockingSeam(t)
	stubKillLossCheck(t, seam, killLossAssessment{})

	cmd := updateOffLoop(t, h, keyKill, seam)
	require.NotNil(t, cmd, "the loss checks must come back as a cmd")
	require.Equal(t, stateConfirm, h.state, "the key must register at once: the dialog opens in the same Update")
	require.NotNil(t, h.confirmationOverlay)
	assert.Equal(t, killCheckPendingNote, h.confirmationOverlay.Pending())
	assert.Contains(t, flatten(h.confirmationOverlay.Render()), killCheckPendingNote,
		"the pending note is the visible working affordance")

	// A confirm before the checks land would consent to copy the user never saw.
	_, _ = h.Update(keyYes)
	assert.Equal(t, stateConfirm, h.state, "y must not confirm a kill whose loss checks are pending")
	assert.NotEqual(t, session.Deleting, inst.GetStatus())

	seam.unblock()
	results := killCheckResults(t, cmd)
	require.Len(t, results, 1)
	assert.EqualValues(t, 1, seam.calls.Load(), "the check runs exactly once, inside the cmd")
	_, _ = h.Update(results[0])
	assert.Empty(t, h.confirmationOverlay.Pending(), "the result completes the dialog")

	_, confirmCmd := h.Update(keyYes)
	assert.Equal(t, stateDefault, h.state, "a clean assessment confirms on y")
	assert.Equal(t, session.Deleting, inst.GetStatus())
	assert.NotNil(t, confirmCmd)
}

// TestKill_LossCheckResultEscalates: a severe result replaces the pending dialog
// with the escalated one — the loss named in full and the distinct confirm key.
func TestKill_LossCheckResultEscalates(t *testing.T) {
	h, inst := killCheckHome(t, "severe")
	seam := newBlockingSeam(t)
	stubKillLossCheck(t, seam, severeLoss)

	cmd := updateOffLoop(t, h, keyKill, seam)
	seam.unblock()
	results := killCheckResults(t, cmd)
	require.Len(t, results, 1)
	_, _ = h.Update(results[0])

	require.Equal(t, stateConfirm, h.state)
	assert.Empty(t, h.confirmationOverlay.Pending())
	assert.Contains(t, flatten(h.confirmationOverlay.Render()), "cannot be undone")
	require.Equal(t, unmergedKillConfirmKey, h.confirmationOverlay.ConfirmKey)

	_, _ = h.Update(keyYes)
	assert.Equal(t, stateConfirm, h.state, "reflexive y must not confirm a data-loss kill")
	_, _ = h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(unmergedKillConfirmKey)})
	assert.Equal(t, stateDefault, h.state, "the named key confirms")
	assert.Equal(t, session.Deleting, inst.GetStatus())
}

// TestKill_StaleLossCheckResultIsDropped: a result for a dialog the user already
// cancelled must not reopen it, and a result for a cancelled dialog must not
// complete a newer one — the newer dialog stays pending until its own result.
func TestKill_StaleLossCheckResultIsDropped(t *testing.T) {
	h, _ := killCheckHome(t, "stale")
	seam := newBlockingSeam(t)
	stubKillLossCheck(t, seam, severeLoss)

	first := updateOffLoop(t, h, keyKill, seam)
	_, _ = h.Update(keyEscape)
	require.Equal(t, stateDefault, h.state)

	second := updateOffLoop(t, h, keyKill, seam)
	require.Equal(t, stateConfirm, h.state)
	seam.unblock()

	stale := killCheckResults(t, first)
	require.Len(t, stale, 1)
	_, _ = h.Update(stale[0])
	assert.Equal(t, killCheckPendingNote, h.confirmationOverlay.Pending(),
		"the cancelled dialog's result must not complete the newer dialog")
	assert.Equal(t, "y", h.confirmationOverlay.ConfirmKey)

	fresh := killCheckResults(t, second)
	require.Len(t, fresh, 1)
	_, _ = h.Update(fresh[0])
	assert.Empty(t, h.confirmationOverlay.Pending())
	assert.Equal(t, unmergedKillConfirmKey, h.confirmationOverlay.ConfirmKey)

	// And after a cancel with nothing reopened, a late result is a no-op.
	_, _ = h.Update(keyEscape)
	require.Equal(t, stateDefault, h.state)
	_, _ = h.Update(fresh[0])
	assert.Equal(t, stateDefault, h.state, "a late result must not reopen a cancelled dialog")
	assert.Nil(t, h.confirmationOverlay)
}

// TestKill_SecondPressWhilePendingStartsNoDuplicate: the pending dialog owns
// the keyboard, so a second D is swallowed instead of starting another check.
func TestKill_SecondPressWhilePendingStartsNoDuplicate(t *testing.T) {
	h, _ := killCheckHome(t, "dup")
	seam := newBlockingSeam(t)
	stubKillLossCheck(t, seam, killLossAssessment{})

	first := updateOffLoop(t, h, keyKill, seam)
	dialog := h.confirmationOverlay
	again := updateOffLoop(t, h, keyKill, seam)
	assert.Same(t, dialog, h.confirmationOverlay, "a second D must not replace the pending dialog")

	seam.unblock()
	assert.Empty(t, killCheckResults(t, again), "a second D must not start another loss check")
	assert.Len(t, killCheckResults(t, first), 1)
	assert.EqualValues(t, 1, seam.calls.Load(), "exactly one check for one dialog")
}

// TestKill_NoLocalWorktreeSkipsPending: a row with nothing to check gets its
// final dialog in the same Update, with no cmd and no pending note.
func TestKill_NoLocalWorktreeSkipsPending(t *testing.T) {
	h := newTestHome(t)
	inst := newKillableInstance(t, "noworktree")
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)
	seam := newBlockingSeam(t)
	stubKillLossCheck(t, seam, severeLoss)

	cmd := updateOffLoop(t, h, keyKill, seam)
	require.Equal(t, stateConfirm, h.state)
	assert.Empty(t, h.confirmationOverlay.Pending())
	for _, msg := range drainCmd(t, cmd, 2*time.Second) {
		_, isResult := msg.(killLossCheckedMsg)
		assert.False(t, isResult, "no worktree, no loss check")
	}
	assert.Zero(t, seam.calls.Load())
}

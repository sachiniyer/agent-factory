package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// ----------------------------------------------------------------------------
// #7551 — a row verb sent right after selection navigation must act on the row
// the navigation landed on. Two failures made it silently lose the press:
//
//  1. The cursor DRIFTED. rebuildVisibleItems keeps the cursor's flat index, so
//     an asynchronous store mutation landing between the painted selection
//     frame and the verb — a kill finalizing (handleInstanceKilled) or the
//     750ms snapshot reconcile's toRemove path, neither of which asserts a
//     selection — re-pointed that index at a different row. The verb then
//     opened its confirmation for a session the user never picked ("cursor
//     ended on beta"), or resolved nil when the index landed on a header and
//     died silently.
//  2. The row was transiently FENCED. The same reconcile adopts daemon-owned
//     in-flight ops onto the row (adoptSnapshotOp), and a fenced row answered
//     the verb with a bare `return m, nil` — a keypress handled zero times,
//     with no evidence it arrived.
//
// The invariant is: every keypress the TUI receives is handled exactly once,
// in arrival order, against the state the keys before it produced — and every
// press gets an answer, even when the answer is "not yet".
// ----------------------------------------------------------------------------

// TestNavThenVerb_ArchiveConfirmNamesLandedRow: j/k navigation followed
// immediately by `a` — the play-test's alpha/beta scan — must open the archive
// confirmation for the row the scan landed on.
func TestNavThenVerb_ArchiveConfirmNamesLandedRow(t *testing.T) {
	h := newTestHome(t)
	alpha := archiveActionInstance(t, "alpha", session.Ready)
	beta := archiveActionInstance(t, "beta", session.Ready)
	gamma := archiveActionInstance(t, "gamma", session.Ready)
	for _, inst := range []*session.Instance{alpha, beta, gamma} {
		h.store.AddInstance(inst)
	}
	h.sidebar.SetSize(40, 24)

	// j j — the driver's af_select scan. Each keypress is handled against the
	// state the key before it produced, landing the cursor on beta's tab row.
	h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	require.Same(t, beta, h.sidebar.GetSelectedInstance(),
		"precondition: the scan landed the cursor on beta")

	// `a`, sent with no settle delay, must name the row the scan landed on.
	h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	require.Equal(t, stateConfirm, h.state,
		"the archive verb must open its confirmation on the landed row (#7551)")
	require.NotNil(t, h.confirmationOverlay)
	require.Contains(t, h.confirmationOverlay.Render(), `Archive session "beta"`,
		"the confirmation must name the row navigation landed on")
}

// TestNavThenVerb_ArchiveAfterStoreMutationNamesLandedRow is the drift leg: an
// asynchronous store mutation removes the row ABOVE the cursor's session
// between the painted frame and the verb — the shape a finalizing kill or a
// snapshot reconcile's toRemove produces. The kept flat index then names the
// next session (gamma), and on the unfixed sidebar the confirmation names
// that row instead.
func TestNavThenVerb_ArchiveAfterStoreMutationNamesLandedRow(t *testing.T) {
	h := newTestHome(t)
	alpha := archiveActionInstance(t, "alpha", session.Ready)
	beta := archiveActionInstance(t, "beta", session.Ready)
	gamma := archiveActionInstance(t, "gamma", session.Ready)
	for _, inst := range []*session.Instance{alpha, beta, gamma} {
		h.store.AddInstance(inst)
	}
	h.sidebar.SetSize(40, 24)

	h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	require.Same(t, beta, h.sidebar.GetSelectedInstance(),
		"precondition: the scan landed the cursor on beta")

	// The mutation the play-test's snapshot reconcile can land between the
	// visible selection frame and the verb: alpha's row leaves the store, so
	// the cursor's flat index now sits one row past beta's tab row — on
	// gamma's instance row.
	require.True(t, h.store.RemoveInstance(alpha))

	h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	require.Equal(t, stateConfirm, h.state,
		"the archive verb must still open its confirmation (#7551)")
	require.NotNil(t, h.confirmationOverlay)
	require.Contains(t, h.confirmationOverlay.Render(), `Archive session "beta"`,
		"the confirmation must name the row the scan landed on, not the row the cursor index drifted to (#7551)")
}

// TestNavThenVerb_KillAfterStoreMutationNamesLandedRow: the `D` repro — same
// drift, resolved through RowVerbTarget instead of GetSelectedInstance.
func TestNavThenVerb_KillAfterStoreMutationNamesLandedRow(t *testing.T) {
	h := newTestHome(t)
	alpha := archiveActionInstance(t, "alpha", session.Ready)
	beta := archiveActionInstance(t, "beta", session.Ready)
	gamma := archiveActionInstance(t, "gamma", session.Ready)
	for _, inst := range []*session.Instance{alpha, beta, gamma} {
		h.store.AddInstance(inst)
	}
	h.sidebar.SetSize(40, 24)

	h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	require.Same(t, beta, h.sidebar.GetSelectedInstance(),
		"precondition: the scan landed the cursor on beta")

	require.True(t, h.store.RemoveInstance(alpha))

	h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})
	require.Equal(t, stateConfirm, h.state,
		"the kill verb must still open its confirmation (#7551)")
	require.NotNil(t, h.confirmationOverlay)
	require.Contains(t, h.confirmationOverlay.Render(), `Delete session "beta"`,
		"the confirmation must name the row the scan landed on, not the row the cursor index drifted to (#7551)")
}

// TestVerbOnInFlightRow_AnswersWithNotice is the fence leg: a row the snapshot
// reconcile has fenced with an in-flight op — the transient state #7551's
// hello-task/beta presses hit — must answer the verb by name. Before the fix
// both handlers returned a bare `m, nil`: the keypress was swallowed whole.
func TestVerbOnInFlightRow_AnswersWithNotice(t *testing.T) {
	for _, tc := range []struct {
		name    string
		op      session.InFlightOp
		verb    rune
		wantMsg string
	}{
		{"archive on creating row", session.OpCreating, 'a', "still being created"},
		{"kill on creating row", session.OpCreating, 'D', "still being created"},
		{"archive on restoring row", session.OpRestoring, 'a', "already being restored"},
		{"kill on restoring row", session.OpRestoring, 'D', "already being restored"},
		{"archive on respawning row", session.OpRespawning, 'a', "resuming"},
		{"kill on respawning row", session.OpRespawning, 'D', "resuming"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHome(t)
			inst := archiveActionInstance(t, "busy-row", session.Ready)
			inst.SetInFlightOpForTest(tc.op)
			h.store.AddInstance(inst)
			h.sidebar.SetSize(40, 24)
			h.sidebar.SetSelectedInstance(0)

			h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{tc.verb}})

			require.Equal(t, stateDefault, h.state,
				"a fenced row must not open a confirmation (#7551)")
			require.Nil(t, h.confirmationOverlay)
			require.Contains(t, h.errBox.FullError(), tc.wantMsg,
				"the verb must be answered by naming the op fencing it, not swallowed (#7551)")
		})
	}
}

// TestVerbWithNoSessionUnderCursor_AnswersWithNotice: the drifted-to-a-header
// end state — a verb whose cursor resolves no session must still answer
// visibly. Before the fix this was the second silent return.
func TestVerbWithNoSessionUnderCursor_AnswersWithNotice(t *testing.T) {
	h := newTestHome(t)
	// No instances at all: the cursor rests on the Instances header, which
	// resolves nil for both verb paths.
	for _, verb := range []rune{'a', 'D'} {
		h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{verb}})
		require.Equal(t, stateDefault, h.state)
		require.Contains(t, h.errBox.FullError(), "No session under the cursor",
			"every verb press must get an answer, even 'there is nothing here' (#7551)")
	}
}

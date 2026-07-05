package app

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// ----------------------------------------------------------------------------
// Regression tests for #1238: killing the daemon-managed root agent from the
// TUI must not look — or be dispatched — like killing a throwaway worktree.
//
// The 2026-07-05 outage was a muscle-memory D+y on root's row: the generic
// "[!] Kill session 'root'?" confirm gave no hint that root is the daemon-
// managed singleton, and the ordinary 'y' confirm tore it down, silently
// decapitating the inbound event pipeline. The guard gives root distinct,
// consequence-bearing copy AND a distinct confirm key so a reflexive 'y' is a
// no-op.
// ----------------------------------------------------------------------------

// TestKillConfirmMessage_RootIsDistinctAndNamesConsequence pins the copy on the
// IsReservedTitle branch: it must name the daemon-managed root, the delivery
// consequence, and the self-heal recovery (#1237) — and must NOT be the generic
// scratch-session prompt.
func TestKillConfirmMessage_RootIsDistinctAndNamesConsequence(t *testing.T) {
	generic := killConfirmMessage("scratch-1", "", false)
	assert.Equal(t, "[!] Kill session 'scratch-1'?", generic,
		"non-reserved copy must be unchanged")

	root := killConfirmMessage(session.RootSessionTitle, "", true)
	assert.NotEqual(t, "[!] Kill session 'root'?", root,
		"root must not show the generic scratch-session prompt")
	assert.Contains(t, root, "daemon-managed root agent",
		"root copy must identify the singleton")
	assert.Contains(t, strings.ToLower(root), "delivery",
		"root copy must name the delivery consequence")
	assert.Contains(t, strings.ToLower(root), "self-heal",
		"root copy must name the #1237 self-heal recovery, not a hard-coded restart")
}

// TestKillConfirmMessage_WarningAppendsForBothBranches verifies the uncommitted-
// changes warning is still appended for reserved and non-reserved alike.
func TestKillConfirmMessage_WarningAppendsForBothBranches(t *testing.T) {
	warn := "you have uncommitted changes"
	assert.Contains(t, killConfirmMessage("scratch-1", warn, false), warn)
	assert.Contains(t, killConfirmMessage(session.RootSessionTitle, warn, true), warn)
}

// TestHandleKill_RootRequiresDistinctConfirmKey is the core guard: selecting
// root and pressing kill must arm a dialog whose confirm key is NOT the ordinary
// 'y', so a reflexive 'y' cannot dispatch the kill — only the named key does.
func TestHandleKill_RootRequiresDistinctConfirmKey(t *testing.T) {
	h := newTestHome(t)
	root := newKillableInstance(t, session.RootSessionTitle)
	h.store.AddInstance(root)
	h.sidebar.SetSelectedInstance(0)

	model, _ := h.handleKill()
	hm := model.(*home)
	require.Equal(t, stateConfirm, hm.state, "kill must open the confirmation dialog")
	require.NotNil(t, hm.confirmationOverlay)
	require.Equal(t, rootKillConfirmKey, hm.confirmationOverlay.ConfirmKey,
		"root kill must demand the distinct confirm key, not the ordinary 'y'")
	require.NotEqual(t, "y", hm.confirmationOverlay.ConfirmKey)

	// A reflexive 'y' — the muscle-memory gesture from #1238 — must be ignored:
	// the dialog stays open, root is untouched.
	model, cmd := hm.handleStateConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	hm = model.(*home)
	assert.Equal(t, stateConfirm, hm.state, "reflexive 'y' must not confirm a root kill")
	assert.NotNil(t, hm.confirmationOverlay, "dialog must stay open after a rejected key")
	assert.Nil(t, cmd, "no kill must be dispatched by 'y'")
	assert.NotEqual(t, session.Deleting, root.GetStatus(), "root must not be marked Deleting")

	// The named key confirms.
	model, cmd = hm.handleStateConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(rootKillConfirmKey)})
	hm = model.(*home)
	assert.Equal(t, stateDefault, hm.state, "the named key must confirm")
	assert.Nil(t, hm.confirmationOverlay, "dialog must close on confirm")
	require.NotNil(t, cmd, "confirm must forward the start-kill message")
	assert.Equal(t, session.Deleting, root.GetStatus(),
		"root row must be marked Deleting once confirmed")
	startMsg, ok := cmd().(startKillMsg)
	require.True(t, ok, "confirm must emit startKillMsg, got a different msg")
	assert.Equal(t, session.RootSessionTitle, startMsg.title)
}

// TestHandleKill_NonReservedKeepsOrdinaryConfirm guards the acceptance criterion
// "no change to killing non-reserved sessions": a scratch session still uses the
// ordinary 'y' confirm.
func TestHandleKill_NonReservedKeepsOrdinaryConfirm(t *testing.T) {
	h := newTestHome(t)
	inst := newKillableInstance(t, "scratch-1")
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	model, _ := h.handleKill()
	hm := model.(*home)
	require.Equal(t, stateConfirm, hm.state)
	require.NotNil(t, hm.confirmationOverlay)
	assert.Equal(t, "y", hm.confirmationOverlay.ConfirmKey,
		"non-reserved kill must keep the ordinary 'y' confirm")

	model, cmd := hm.handleStateConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	assert.Equal(t, stateDefault, model.(*home).state, "'y' must confirm a scratch kill")
	require.NotNil(t, cmd)
	assert.Equal(t, session.Deleting, inst.GetStatus())
}

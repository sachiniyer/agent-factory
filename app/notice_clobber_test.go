package app

import (
	"os/exec"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/configagent"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/ui/layout"
)

// Notice-clobber regression coverage for the config-agent spawn.
//
// The TUI's status-bar notice system (errBox + the transientNoticeID
// generation token) hosts two kinds of notices that share one bar:
//   - deliberately-persistent spawn notices ("Starting the config agent…")
//     raised by handleConfigAgent with NO auto-clear, designed to stand until
//     the spawn reports back and retracts them under a generation guard
//     (if msg.noticeID == m.transientNoticeID) in handleConfigAgentSpawned;
//   - transient auto-hide notices ("X hidden — too narrow; resize…") raised
//     by setPaneAutoHideStatus when relayout can't fit every open pane.
//
// Before the fix, setPaneAutoHideStatus raised its notice UNCONDITIONALLY,
// so a window resize during the up-to-60s spawn readiness wait that crossed
// layout.MultiPaneMinWidth downward would overwrite the persistent
// "Starting…" notice and bump transientNoticeID past the spawn's captured
// noticeID. The spawn's later retraction is generation-guarded, so it was
// then silently SKIPPED and the original "Starting…" notice was permanently
// lost. The fix gates setPaneAutoHideStatus on configAgentSpawning so the
// auto-hide notice is suppressed for the duration of the spawn wait; because
// the suppressed path never sets pendingPaneAutoHideStatus,
// consumePaneAutoHideStatus's existing `pending == ""` early-return skips the
// second raise as well, so neither path bumps the generation token.
//
// These tests drive the real dispatch path (handleDefaultKeyPress →
// handleConfigAgent), the real resize path (updateHandleWindowSizeEvent →
// relayout → setPaneAutoHideStatus → consumePaneAutoHideStatus), and the real
// retraction (handleConfigAgentSpawned → enterConfigAgent). They fail on the
// unfixed tree (the auto-hide notice clobbers the spawn notice and the
// retraction is skipped) and pass once setPaneAutoHideStatus is gated on
// configAgentSpawning.

// TestPane_AutoHideDoesNotClobberConfigAgentSpawnNotice is the core regression:
// during the spawn readiness wait, a resize that auto-hides a pane must not
// replace the persistent "Starting the config agent…" notice, must not
// advance transientNoticeID past the spawn's captured noticeID, and must not
// arm a deferred auto-hide status. The spawn's retraction must then fire and
// clear the notice — the contract the clobber used to silently break.
func TestPane_AutoHideDoesNotClobberConfigAgentSpawnNotice(t *testing.T) {
	h := paneTestHome(t)
	t.Cleanup(SetConfigAgentSpawnerForTest(func(configagent.Mode, string) (string, string, error) {
		return "af-config-1", "", nil
	}))
	// enterConfigAgent shells out to tmux; stub the attach builder so the
	// retraction path builds a harmless cmd instead (the cmd is never run).
	prevExec := execConfigAgentAttach
	execConfigAgentAttach = func(string, string) *exec.Cmd { return exec.Command("true") }
	t.Cleanup(func() { execConfigAgentAttach = prevExec })

	// Two panes open at a width that fits both, so a downward resize crosses
	// layout.MultiPaneMinWidth and auto-hides one of them — the precondition
	// for setPaneAutoHideStatus to fire.
	alpha := h.store.GetInstanceByTitle("alpha")
	beta := h.store.GetInstanceByTitle("beta")
	require.NotNil(t, alpha)
	require.NotNil(t, beta)
	_, _ = h.openOrFocusPane(alpha, 0)
	_, _ = h.openOrFocusPane(beta, 0)
	require.Equal(t, 2, h.store.NumOpenPanes(), "two panes are open")
	require.Len(t, h.visiblePanes, 2, "both are visible at the wide width")

	// Reset any notice the open path left behind so the assertions isolate the
	// spawn notice and the resize that follows.
	h.errBox.Clear()
	h.pendingPaneAutoHideStatus = ""
	h.paneAutoHideNoticeID = 0

	// Press C: arms the in-flight guard and posts the persistent "Starting…"
	// notice with no auto-clear, exactly as handleConfigAgent does.
	_, cmd := h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'C'}}, keys.KeyConfigAgent)
	require.NotNil(t, cmd, "C must start the config-agent spawn")
	spawnMsg := cmd().(configAgentSpawnedMsg)
	require.NoError(t, spawnMsg.err)
	require.True(t, h.configAgentSpawning, "the in-flight guard is armed for the readiness wait")

	startingNoticeID := spawnMsg.noticeID
	require.Equal(t, startingNoticeID, h.transientNoticeID,
		"the spawn's notice is the current generation")
	require.Contains(t, h.errBox.FullError(), "Starting the config agent",
		"the persistent notice is on the bar")

	// Resize narrow enough to auto-hide a pane WHILE the spawn is in flight.
	// This is the clobber trigger: relayout auto-hides the LRU pane and
	// setPaneAutoHideStatus would raise the auto-hide notice.
	resizeHome(h, layout.MultiPaneMinWidth-1, 24)
	require.Len(t, h.visiblePanes, 1, "the narrow width hides one of the two panes")

	// The fix: the persistent config-agent notice must still be the one on the
	// bar, and the generation token must not have advanced past it.
	assert.Contains(t, h.errBox.FullError(), "Starting the config agent",
		"a resize during the spawn wait must not clobber the persistent notice")
	assert.NotContains(t, h.errBox.FullError(), "hidden",
		"the auto-hide guidance must not replace the spawn notice during the wait")
	assert.Equal(t, startingNoticeID, h.transientNoticeID,
		"transientNoticeID must not advance past the spawn's notice, or the "+
			"generation-guarded retraction would be silently skipped")
	assert.Empty(t, h.pendingPaneAutoHideStatus,
		"the auto-hide status is not armed while the spawn notice stands")

	// A resize wandering back and forth across the threshold during the wait
	// must not accumulate a deferred auto-hide status either: every narrow
	// crossing is suppressed, so the spawn's notice and generation survive.
	resizeHome(h, layout.MultiPaneMinWidth, 24) // wide enough to show both again
	require.Len(t, h.visiblePanes, 2, "both panes are visible again at the wide width")
	resizeHome(h, layout.MultiPaneMinWidth-1, 24) // narrow again
	require.Len(t, h.visiblePanes, 1, "narrow re-hides one pane")
	assert.Contains(t, h.errBox.FullError(), "Starting the config agent",
		"the persistent notice survives repeated resizes during the wait")
	assert.Equal(t, startingNoticeID, h.transientNoticeID,
		"the generation token stays at the spawn's notice across repeated resizes")
	assert.Empty(t, h.pendingPaneAutoHideStatus,
		"no deferred auto-hide status accumulates across resize crossings")

	// The spawn reports back. Because transientNoticeID is still the spawn's
	// own generation, the retraction fires and clears the "Starting…" notice —
	// the contract that the clobber used to silently break. (handleConfigAgent
	// captured noticeID; handleConfigAgentSpawned retracts under the guard.)
	h.handleConfigAgentSpawned(spawnMsg)
	assert.False(t, h.configAgentSpawning,
		"the in-flight guard clears when the spawn reports back")
	assert.Empty(t, h.errBox.FullError(),
		"the spawn retracts its own notice once it reports back — no clobber to defeat the retraction")
}

// TestPane_AutoHideNoticeResurfacesAfterSpawnSettles pins the other half of
// the contract: the auto-hide guidance is only SUPPRESSED while a config-agent
// spawn is in flight, not permanently disabled. Once the spawn reports back
// and configAgentSpawning clears, a narrow resize that auto-hides a pane must
// raise the "X hidden — too narrow…" notice exactly as before — so the fix
// does not regress the auto-hide feature (#1557 and its suite).
func TestPane_AutoHideNoticeResurfacesAfterSpawnSettles(t *testing.T) {
	h := paneTestHome(t)
	t.Cleanup(SetConfigAgentSpawnerForTest(func(configagent.Mode, string) (string, string, error) {
		return "af-config-1", "", nil
	}))
	prevExec := execConfigAgentAttach
	execConfigAgentAttach = func(string, string) *exec.Cmd { return exec.Command("true") }
	t.Cleanup(func() { execConfigAgentAttach = prevExec })

	alpha := h.store.GetInstanceByTitle("alpha")
	beta := h.store.GetInstanceByTitle("beta")
	require.NotNil(t, alpha)
	require.NotNil(t, beta)
	_, _ = h.openOrFocusPane(alpha, 0)
	_, _ = h.openOrFocusPane(beta, 0)
	require.Equal(t, 2, h.store.NumOpenPanes())

	h.errBox.Clear()
	h.pendingPaneAutoHideStatus = ""
	h.paneAutoHideNoticeID = 0

	_, cmd := h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'C'}}, keys.KeyConfigAgent)
	require.NotNil(t, cmd)
	spawnMsg := cmd().(configAgentSpawnedMsg)

	// A narrow resize during the spawn wait is suppressed.
	resizeHome(h, layout.MultiPaneMinWidth-1, 24)
	require.Contains(t, h.errBox.FullError(), "Starting the config agent",
		"the persistent notice survives the resize during the spawn wait")
	assert.NotContains(t, h.errBox.FullError(), "hidden",
		"the auto-hide notice is suppressed during the spawn wait")

	// The spawn settles, retracting the persistent notice and clearing the
	// in-flight guard.
	h.handleConfigAgentSpawned(spawnMsg)
	assert.False(t, h.configAgentSpawning, "the guard clears on settle")
	assert.Empty(t, h.errBox.FullError(), "the spawn notice is retracted on settle")

	// Grow back to a width that fits both panes, then shrink again — now that
	// no spawn is in flight, the auto-hide notice must fire normally.
	resizeHome(h, layout.MultiPaneMinWidth, 24)
	require.Len(t, h.visiblePanes, 2, "both panes are visible again at the wide width")
	h.errBox.Clear() // drop anything the grow-up left on the bar

	resizeHome(h, layout.MultiPaneMinWidth-1, 24)
	require.Len(t, h.visiblePanes, 1, "the narrow width re-hides a pane")
	assert.Contains(t, h.errBox.FullError(), "hidden",
		"the auto-hide notice resurfaces once no spawn is in flight — the guard is spawn-scoped, not permanent")
	assert.NotContains(t, h.errBox.FullError(), "Starting",
		"no stale spawn notice lingers after the spawn settled")
}

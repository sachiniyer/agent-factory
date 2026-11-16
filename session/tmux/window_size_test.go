package tmux

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// windowSize reads back the window-scope option Start's queue and reattach's
// pin both write — the only readback that proves the pin landed, because a
// session-scoped `-t =name` write sets nothing tmux ever reads (#7437 review:
// window-size is a WINDOW option on tmux 3.4, and -q swallows the miss).
// The readback is EMPTY for a window with no own setting: `latest` is the
// global default and only prints at -g scope, so an unpinned pane reads "",
// not "window-size latest" — the distinction the test assertions rely on.
func windowSize(t *testing.T, name string) string {
	t.Helper()
	out, err := exec.Command("tmux", "show-options", "-w", "-t", ExactTarget(name), "window-size").CombinedOutput()
	require.NoError(t, err, "show-options -w: %s", out)
	return strings.TrimSpace(string(out))
}

// TestWindowSizePinLandsAtWindowScope is the #7437-review proof the pin is
// real, not an argv string: Start's new-session queue must leave tmux's own
// window option at `manual`, or a plain `tmux attach` still resizes the pane —
// the reflow path the daemon cannot see (#7434).
func TestWindowSizePinLandsAtWindowScope(t *testing.T) {
	testguard.IsolateTmux(t)
	name := "af_winsize-pin-7437"
	session := NewTmuxSessionFromSanitizedNameWithDeps(name, "cat", MakePtyFactory(), cmd.MakeExecutor())

	require.NoError(t, session.Start(t.TempDir()))
	require.Equal(t, "window-size manual", windowSize(t, name),
		"the queued pin must land at the scope tmux reads it — a session-scoped =name write is silently nothing")
}

// TestSkippedPinPaneKeepsAttachSizing is the #7437-review R3 contract: a pane
// a REAL tmux client is the interface for — the account login flow, the
// config agent — must keep window-size `latest` so its window still follows
// the attaching client's terminal instead of rendering inside the pin's
// stored size.
func TestSkippedPinPaneKeepsAttachSizing(t *testing.T) {
	testguard.IsolateTmux(t)
	name := "af_winsize-skip-7437"
	session := NewTmuxSessionFromSanitizedNameWithDeps(name, "cat", MakePtyFactory(), cmd.MakeExecutor())
	session.SkipWindowSizePin()

	require.NoError(t, session.Start(t.TempDir()))
	require.Empty(t, windowSize(t, name),
		"a client-attached pane keeps tmux's attach-sizes behaviour: no window-scope window-size is set, so the global `latest` default still applies")
}

// TestReattachPinLandsAtWindowScope covers the heal half of M2: a session
// created before the pin existed (or here, by hand) still answers `latest`,
// so an answered non-fresh rebind must write the pin — at the same
// window-scoped target that makes it stick.
func TestReattachPinLandsAtWindowScope(t *testing.T) {
	testguard.IsolateTmux(t)
	name := "af_winsize-reattach-7437"
	out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "-x", "80", "-y", "24", "cat").CombinedOutput()
	require.NoError(t, err, "new-session: %s", out)
	require.Empty(t, windowSize(t, name), "the bare session starts unpinned — no window-scope setting")

	session := NewTmuxSessionFromSanitizedNameWithDeps(name, "cat", MakePtyFactory(), cmd.MakeExecutor())
	require.NoError(t, session.ReattachInert())
	require.Equal(t, "window-size manual", windowSize(t, name),
		"an answered, non-fresh reattach re-pins pre-pin sessions at the window scope")
}

package termpane

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// startScript runs a shell script on the TermPane's PTY in place of the tmux
// attach client — the hermetic "fake tmux": a scripted PTY exercises the
// whole PTY → emulator → grid path without any tmux server.
func startScript(t *testing.T, script string, width, height int) *TermPane {
	t.Helper()
	tp, err := NewWithCommand(exec.Command("/bin/sh", "-c", script), width, height)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tp.Close() })
	return tp
}

func startScriptWithStatusLayout(t *testing.T, script string, width, height, statusRows int, pos statusPosition) *TermPane {
	t.Helper()
	tp, err := newWithCommand(exec.Command("/bin/sh", "-c", script), width, height, statusRows, pos)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tp.Close() })
	return tp
}

// plainRender is Render with the styling stripped, for content assertions.
func plainRender(tp *TermPane, width, height int) string {
	return ansi.Strip(tp.Render(width, height, false))
}

func waitForRender(t *testing.T, tp *TermPane, width, height int, want string) {
	t.Helper()
	require.Eventuallyf(t, func() bool {
		return strings.Contains(plainRender(tp, width, height), want)
	}, 5*time.Second, 20*time.Millisecond, "grid never showed %q; last frame:\n%s", want, plainRender(tp, width, height))
}

func TestNewAttachCommandSocketParity(t *testing.T) {
	// Inside tmux, $TMUX (`socket_path,server_pid,session_id`) is the only
	// place the server's socket path lives. Stripping it from the child env
	// (nesting hygiene) must therefore hand the path back as -S, or on a
	// non-default socket (`tmux -L`/`-S`) the attach resolves
	// TMUX_TMPDIR/default and dies against the wrong server.
	cmd := newAttachCommand("mysess", "/private/dir/pt,12345,$0",
		[]string{"TMUX=/private/dir/pt,12345,$0", "TMUX_PANE=%1", "TERM=screen-256color", "HOME=/home/u", "TMUX_TMPDIR=/private/dir"})
	assert.Equal(t, []string{"tmux", "-S", "/private/dir/pt", "attach-session", "-t", "=mysess"}, cmd.Args)
	assert.NotContains(t, cmd.Env, "TMUX=/private/dir/pt,12345,$0", "child env must not carry $TMUX (nesting refusal)")
	assert.NotContains(t, cmd.Env, "TMUX_PANE=%1")
	assert.Contains(t, cmd.Env, "TERM=xterm-256color", "TERM pinned to what the vt emulator implements")
	assert.Contains(t, cmd.Env, "HOME=/home/u", "unrelated env passes through")
	assert.Contains(t, cmd.Env, "TMUX_TMPDIR=/private/dir", "TMUX_TMPDIR passes through")

	// Outside tmux ($TMUX unset/empty) default socket resolution already
	// matches every other af tmux call: no -S may be injected.
	cmd = newAttachCommand("mysess", "", []string{"HOME=/home/u"})
	assert.Equal(t, []string{"tmux", "attach-session", "-t", "=mysess"}, cmd.Args)
}

func TestParseTmuxStatusRows(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int
	}{
		{value: "", want: 1},
		{value: "on", want: 1},
		{value: "off", want: 0},
		{value: "0", want: 0},
		{value: "2", want: 2},
		{value: "5", want: 5},
		{value: "6", want: 5},
		{value: "bad", want: 1},
	} {
		assert.Equalf(t, tc.want, parseTmuxStatusRows(tc.value), "status %q", tc.value)
	}
}

// TestNewAttachesAcrossNonDefaultSocket is the end-to-end pin for the #1121
// play-test blocker: with af running inside a server on a non-default socket
// ($TMUX carries its path), New must reach THAT server — not auto-start a
// transient default-socket one and die.
func TestNewAttachesAcrossNonDefaultSocket(t *testing.T) {
	testguard.IsolateTmux(t)
	sock := filepath.Join(t.TempDir(), "pt-sock")
	run := func(args ...string) (string, error) {
		out, err := exec.Command("tmux", append([]string{"-S", sock}, args...)...).CombinedOutput()
		return string(out), err
	}
	t.Cleanup(func() { _, _ = run("kill-server") })
	out, err := run("new-session", "-d", "-s", "sockparity", "-x", "80", "-y", "24",
		"sh", "-c", "printf 'SOCK-PARITY-1121\\n'; sleep 120")
	require.NoError(t, err, "tmux new-session: %s", out)
	out, err = run("set-option", "-t", "sockparity", "status", "2")
	require.NoError(t, err, "tmux set status: %s", out)

	// What the TUI sees when it runs inside that server.
	t.Setenv("TMUX", sock+",12345,$0")

	tp, err := New("sockparity", 80, 24)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tp.Close() })
	assert.Equal(t, 2, tp.statusRows, "status query must use the non-default socket too")
	waitForRender(t, tp, 80, 24, "SOCK-PARITY-1121")
}

func TestScriptedPTYRendersIntoGrid(t *testing.T) {
	tp := startScript(t, "printf 'MARKER-1089-preview'; sleep 30", 40, 6)
	waitForRender(t, tp, 40, 6, "MARKER-1089-preview")

	// The width x height contract holds on live output too.
	lines := strings.Split(tp.Render(40, 6, false), "\n")
	require.Len(t, lines, 6)
	for i, line := range lines {
		require.Equalf(t, 40, ansi.StringWidth(line), "line %d width", i)
	}
}

func TestResizePropagatesPTYWinsize(t *testing.T) {
	// The script re-reports its terminal size forever; after Resize the PTY
	// slave must observe the new winsize (this is what makes tmux reflow).
	tp := startScript(t, "while :; do stty size; sleep 0.05; done", 80, 24)
	waitForRender(t, tp, 80, 24, "24 80")

	tp.Resize(100, 30)
	waitForRender(t, tp, 100, 30, "30 100")
}

// TestResizeBlanksStaleGrid pins the #1556 fix: the pinned x/vt emulator
// resizes by re-windowing the cell buffer, not by reflowing it, so a wrapped
// line is truncated to the new width and its continuation row is left behind —
// which renders as a command's tail merging straight into the next prompt.
// Real attachments self-heal on tmux's redraw, but until it lands the grid must
// not show that corrupted transcript. Resize therefore blanks the visible grid;
// here a scripted PTY (no tmux, so nothing ever redraws) makes that guarantee
// observable: after the resize the pane is empty, not merged.
func TestResizeBlanksStaleGrid(t *testing.T) {
	// "dev@host$ chmod +x todo.sh test.sh" is 34 cols, so it wraps at width 20:
	// row 0 is the prompt+command head, row 1 the "odo.sh test.sh" tail.
	tp := startScript(t, "printf 'dev@host$ chmod +x todo.sh test.sh\\r\\nnext-marker-1556\\r\\n'; sleep 30", 20, 6)
	waitForRender(t, tp, 20, 6, "next-marker-1556")
	require.Contains(t, plainRender(tp, 20, 6), "odo.sh test.sh", "setup: the command must wrap so a continuation row exists")

	// Shrink the pane. With no tmux behind the PTY nothing repaints, so whatever
	// the grid holds now is exactly what a pre-redraw frame would show.
	tp.Resize(14, 6)
	got := plainRender(tp, 14, 6)
	assert.Emptyf(t, strings.TrimSpace(got), "resize must blank the truncated, un-reflowed grid (#1556); got:\n%s", got)
}

// TestResizeBlanksStaleAltScreenGrid extends the #1556 fix to a full-screen
// program (vim/less/...) occupying the alternate screen when the resize lands.
// The blank must follow the ACTIVE screen: x/vt's ED 2 clears whichever screen
// is current (e.scr), so entering the alternate screen (?1049h) and resizing
// must leave that screen blank, not the stale, truncated alt grid — otherwise
// the pane would show a merged full-screen buffer until tmux repaints.
func TestResizeBlanksStaleAltScreenGrid(t *testing.T) {
	// Switch to the alternate screen, then draw a line there that wraps at 20.
	tp := startScript(t, "printf '\\033[?1049h'; printf 'ALT chmod +x todo.sh test.sh\\r\\nalt-marker-1556\\r\\n'; sleep 30", 20, 6)
	waitForRender(t, tp, 20, 6, "alt-marker-1556")

	tp.Resize(14, 6)
	got := plainRender(tp, 14, 6)
	assert.Emptyf(t, strings.TrimSpace(got), "resize must blank the ACTIVE (alternate) screen (#1556); got:\n%s", got)
}

func TestHiddenStatusRowsAreNotRendered(t *testing.T) {
	tp := startScriptWithStatusLayout(t, "printf 'VISIBLE-1425\\033[7;1HSTATUS-1425-A\\033[8;1HSTATUS-1425-B'; sleep 30", 30, 6, 2, statusBottom)
	waitForRender(t, tp, 30, 6, "VISIBLE-1425")

	out := plainRender(tp, 30, 6)
	assert.NotContains(t, out, "STATUS-1425-A", "first hidden bottom row must be cropped out of the pane")
	assert.NotContains(t, out, "STATUS-1425-B", "second hidden bottom row must be cropped out of the pane")
	gridLines(t, tp.Render(30, 6, false), 30, 6)
}

func TestTopStatusRowsAreNotRendered(t *testing.T) {
	tp := startScriptWithStatusLayout(t, "printf 'TOP-STATUS-1425-A\\033[2;1HTOP-STATUS-1425-B\\033[3;1HVISIBLE-1425'; sleep 30", 30, 6, 2, statusTop)
	waitForRender(t, tp, 30, 6, "VISIBLE-1425")

	out := plainRender(tp, 30, 6)
	assert.NotContains(t, out, "TOP-STATUS-1425-A", "first hidden top row must be cropped out of the pane")
	assert.NotContains(t, out, "TOP-STATUS-1425-B", "second hidden top row must be cropped out of the pane")
	gridLines(t, tp.Render(30, 6, false), 30, 6)
}

func TestHiddenStatusRowsAreIncludedInPTYWinsize(t *testing.T) {
	tp := startScriptWithStatusLayout(t, "while :; do stty size; sleep 0.05; done", 80, 24, 2, statusBottom)
	waitForRender(t, tp, 80, 24, "26 80")

	tp.Resize(100, 30)
	waitForRender(t, tp, 100, 30, "32 100")
}

func TestCloseKillsClientAndSignalsDone(t *testing.T) {
	tp := startScript(t, "printf 'up'; sleep 30", 20, 4)
	waitForRender(t, tp, 20, 4, "up")

	require.NoError(t, tp.Close())

	select {
	case <-tp.Done():
	default:
		t.Fatal("Done must be closed after Close")
	}

	// The client child must actually be gone (killed and reaped), not
	// orphaned behind the closed PTY. Signal 0 probes liveness: it errors
	// once the process is finished and waited on.
	require.Eventually(t, func() bool {
		return tp.cmd.Process.Signal(syscall.Signal(0)) != nil
	}, time.Second, 10*time.Millisecond, "attach client must be killed and reaped by Close")

	// The last frame stays renderable after Close (a hidden pane keeps its
	// final content until its owner swaps render sources).
	assert.Contains(t, plainRender(tp, 20, 4), "up")

	// Close is idempotent.
	require.NoError(t, tp.Close())
}

func TestClientExitClosesDoneWithoutClose(t *testing.T) {
	tp := startScript(t, "printf 'bye'", 20, 4)
	select {
	case <-tp.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done must close when the client exits on its own")
	}
	require.NoError(t, tp.Close())
}

func TestStartStopCyclesDoNotLeakGoroutines(t *testing.T) {
	// Warm up any lazily started runtime goroutines before baselining.
	tp := startScript(t, "printf warm; sleep 30", 20, 4)
	waitForRender(t, tp, 20, 4, "warm")
	require.NoError(t, tp.Close())

	runtime.GC()
	base := runtime.NumGoroutine()

	for i := 0; i < 5; i++ {
		cycle := startScript(t, fmt.Sprintf("printf 'cycle-%d'; sleep 30", i), 30, 5)
		waitForRender(t, cycle, 30, 5, fmt.Sprintf("cycle-%d", i))
		require.NoError(t, cycle.Close())
	}

	// Plain retry loop rather than assert.Eventually: Eventually polls its
	// condition from an extra goroutine of its own, which would inflate the
	// count it is asserting on.
	deadline := time.Now().Add(5 * time.Second)
	for {
		runtime.GC()
		if runtime.NumGoroutine() <= base {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines must drain back to the baseline after close cycles (base=%d, now=%d)", base, runtime.NumGoroutine())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestCloseLeavesTmuxSessionAlive is the one real-tmux test in the package:
// the whole point of Close is that it kills the attach CLIENT while the
// session keeps running server-side. It runs on a private isolated tmux
// server (testguard.IsolateTmux: private TMUX_TMPDIR, server killed and
// socket dir removed in cleanup) and is skipped when tmux is unavailable.
func TestCloseLeavesTmuxSessionAlive(t *testing.T) {
	testguard.IsolateTmux(t)

	run := func(args ...string) (string, error) {
		out, err := exec.Command("tmux", args...).CombinedOutput()
		return string(out), err
	}
	_, err := run("new-session", "-d", "-s", "termpane1089", "-x", "80", "-y", "24",
		"sh", "-c", "printf 'LIVE-AF-1089\\n'; sleep 120")
	require.NoError(t, err)

	tp, err := New("termpane1089", 80, 24)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tp.Close() })

	waitForRender(t, tp, 80, 24, "LIVE-AF-1089")

	require.NoError(t, tp.Close())

	// The session must survive the client teardown...
	_, err = run("has-session", "-t", "=termpane1089")
	require.NoError(t, err, "tmux session must still be alive after Close")
	// ...with no client left attached to it.
	assert.Eventually(t, func() bool {
		out, err := run("list-clients", "-t", "=termpane1089")
		return err == nil && strings.TrimSpace(out) == ""
	}, 3*time.Second, 50*time.Millisecond, "no attach client may survive Close")
}

func TestNewHidesNestedTmuxStatusLine(t *testing.T) {
	testguard.IsolateTmux(t)

	const (
		sessionName  = "termpane1425"
		statusMarker = "NESTED-STATUS-1425"
	)
	run := func(args ...string) (string, error) {
		out, err := exec.Command("tmux", args...).CombinedOutput()
		return string(out), err
	}
	t.Cleanup(func() { _, _ = run("kill-server") })
	out, err := run("new-session", "-d", "-s", sessionName, "-x", "40", "-y", "6",
		"sh", "-c", "printf 'LIVE-1425\\n'; sleep 120")
	require.NoError(t, err, "tmux new-session: %s", out)
	for _, args := range [][]string{
		{"set-option", "-t", sessionName, "status", "2"},
		{"set-option", "-t", sessionName, "status-format[0]", statusMarker + "-A"},
		{"set-option", "-t", sessionName, "status-format[1]", statusMarker + "-B"},
		{"set-option", "-t", sessionName, "status-interval", "0"},
	} {
		out, err = run(args...)
		require.NoError(t, err, "tmux %v: %s", args, out)
	}

	tp, err := New(sessionName, 40, 6)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tp.Close() })
	assert.Equal(t, 2, tp.statusRows, "New must query tmux's configured status row count")

	waitForRender(t, tp, 40, 6, "LIVE-1425")
	assert.Never(t, func() bool {
		return strings.Contains(plainRender(tp, 40, 6), statusMarker)
	}, 500*time.Millisecond, 20*time.Millisecond, "embedded pane must crop tmux's nested status line")
	gridLines(t, tp.Render(40, 6, false), 40, 6)
}

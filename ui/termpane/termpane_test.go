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

	// What the TUI sees when it runs inside that server.
	t.Setenv("TMUX", sock+",12345,$0")

	tp, err := New("sockparity", 80, 24)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tp.Close() })
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

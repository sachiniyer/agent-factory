package tmux

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// stallingTmuxOnPath puts a `tmux` earlier on PATH that never exits, standing in
// for a wedged tmux server: EVERY tmux invocation — including the
// ExistsOrUnknown probe the error paths would otherwise fall back to — hangs.
// The script sleeps in a CHILD process, so it also covers the case that makes a
// naive deadline useless: killing only the direct tmux process leaves the child
// holding the inherited capture pipe, and Output()/Run() block on pipe EOF until
// it dies. Passing therefore requires the process-group kill AND tmuxWaitDelay,
// not just exec.CommandContext.
func stallingTmuxOnPath(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nsleep 300 &\nwait\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// shortTmuxTimeout shortens the production deadline so the test asserts in
// milliseconds instead of tmuxCommandTimeout's generous production value.
func shortTmuxTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := tmuxCommandTimeout
	tmuxCommandTimeout = d
	t.Cleanup(func() { tmuxCommandTimeout = prev })
}

// TestBoundedTmuxCommandsDoNotHang is the #1787 regression: a stalled tmux must
// not hang the WS PTY subscribe path (CaptureVisiblePaneGrid / CursorPosition,
// which run before the 101 upgrade) or the capture transitions
// (EnablePipePane / DisablePipePane, which run while the broker holds captureMu).
// Before the fix each of these blocked forever on `exec.Command`; now each is
// bound by tmuxCommandTimeout and reports ErrTmuxTimeout.
func TestBoundedTmuxCommandsDoNotHang(t *testing.T) {
	cases := []struct {
		name string
		call func(ts *TmuxSession) error
	}{
		{"CaptureVisiblePaneGrid", func(ts *TmuxSession) error { _, err := ts.CaptureVisiblePaneGrid(); return err }},
		{"CursorPosition", func(ts *TmuxSession) error { _, _, err := ts.CursorPosition(); return err }},
		{"ReadTerminalState", func(ts *TmuxSession) error { _, err := ts.ReadTerminalState(); return err }},
		{"EnablePipePane", func(ts *TmuxSession) error { return ts.EnablePipePane("dd of=/dev/null") }},
		{"DisablePipePane", func(ts *TmuxSession) error { return ts.DisablePipePane() }},
		// Not named in #1787, but the same unbounded pattern on the same WS data
		// path — covered so the whole clientless channel keeps the invariant.
		{"SendRawKeys", func(ts *TmuxSession) error { return ts.SendRawKeys([]byte("hi")) }},
		{"ResizeWindow", func(ts *TmuxSession) error { return ts.ResizeWindow(80, 24) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stallingTmuxOnPath(t)
			shortTmuxTimeout(t, 200*time.Millisecond)
			ts := NewTmuxSessionWithDeps("bounded-1787", "sh", MakePtyFactory(), cmd.MakeExecutor())

			done := make(chan error, 1)
			go func() { done <- tc.call(ts) }()

			// Generous relative to the 200ms deadline, but far below the fake
			// tmux's 300s sleep: only a real bound can land inside it.
			select {
			case err := <-done:
				if !errors.Is(err, ErrTmuxTimeout) {
					t.Fatalf("want ErrTmuxTimeout against a stalled tmux, got %v", err)
				}
				// A timeout means the server is wedged and the session's state is
				// unknown — it must never be reported as a confirmed-gone session.
				if errors.Is(err, ErrSessionGone) {
					t.Fatalf("timeout must not be reported as ErrSessionGone: %v", err)
				}
			case <-time.After(30 * time.Second):
				t.Fatalf("%s hung against a stalled tmux (#1787): no return within 30s", tc.name)
			}
		})
	}
}

// TestBoundedTmuxCommandsSucceedWhenTmuxIsHealthy guards the other direction: the
// deadline must not break the normal path. A fake tmux that answers immediately
// stands in for a healthy server, so a regression that always trips the timeout
// (or that mis-reads a fast exit as a deadline) fails here.
func TestBoundedTmuxCommandsSucceedWhenTmuxIsHealthy(t *testing.T) {
	dir := t.TempDir()
	// display-message drives CursorPosition, which parses "row col"; every other
	// command just needs a clean exit.
	script := "#!/bin/sh\nif [ \"$1\" = \"display-message\" ]; then echo '3 7 0 0 0 0 0 0 0'; else echo 'pane line'; fi\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	shortTmuxTimeout(t, 10*time.Second)
	ts := NewTmuxSessionWithDeps("healthy-1787", "sh", MakePtyFactory(), cmd.MakeExecutor())

	if got, err := ts.CaptureVisiblePaneGrid(); err != nil || got != "pane line\n" {
		t.Fatalf("CaptureVisiblePaneGrid: got %q err %v", got, err)
	}
	row, col, err := ts.CursorPosition()
	if err != nil || row != 3 || col != 7 {
		t.Fatalf("CursorPosition: got (%d,%d) err %v, want (3,7)", row, col, err)
	}
	state, err := ts.ReadTerminalState()
	if err != nil || state.CursorRow != 3 || state.CursorCol != 7 {
		t.Fatalf("ReadTerminalState: got %+v err %v, want cursor (3,7)", state, err)
	}
	if err := ts.EnablePipePane("dd of=/dev/null"); err != nil {
		t.Fatalf("EnablePipePane: %v", err)
	}
	if err := ts.DisablePipePane(); err != nil {
		t.Fatalf("DisablePipePane: %v", err)
	}
	if err := ts.SendRawKeys([]byte("hi")); err != nil {
		t.Fatalf("SendRawKeys: %v", err)
	}
	if err := ts.ResizeWindow(80, 24); err != nil {
		t.Fatalf("ResizeWindow: %v", err)
	}
}

// TestRealPipePaneStreamsPastTheReap pins the invariant that makes the reap in
// runTmuxBounded/outputTmuxBounded safe: boundedTmuxCommand puts each tmux
// command in its OWN process group and SIGKILLs that group on every exit path —
// including SUCCESS — to collect any child holding the capture pipe. That is
// only correct because `pipe-pane`'s shell command (the broker's `dd`) is
// spawned by the tmux SERVER, not by the short-lived tmux CLIENT we exec, so it
// is not in the group we kill. If it ever were, EnablePipePane would destroy its
// own pipe the instant it succeeded and the WS stream would silently go dead —
// a far worse regression than the hang this all fixes, and one no mock can
// catch. So this drives a REAL tmux, on a private server (IsolateTmux) so it
// cannot touch the developer's sessions.
func TestRealPipePaneStreamsPastTheReap(t *testing.T) {
	// This is the minimal #1945 delivery gate — raw mkfifo + pipe-pane + dd + one
	// read, with no agent-server, daemon, or broker. It mirrors the post-#2300
	// production FIFO posture: a blocking read-only descriptor plus a private
	// keeper writer, never the O_RDWR descriptor implicated by the original
	// Darwin failures.
	testguard.IsolateTmux(t)

	const name = "af1787-reap-pipe"
	ex := cmd.MakeExecutor()
	if err := ex.Run(exec.Command("tmux", "new-session", "-d", "-s", name, "sh")); err != nil {
		t.Fatalf("new-session: %v", err)
	}
	t.Cleanup(func() { _ = ex.Run(exec.Command("tmux", "kill-session", "-t", "="+name)) })

	ts := NewTmuxSessionFromSanitizedNameWithDeps(name, "sh", MakePtyFactory(), ex)
	fifo := filepath.Join(t.TempDir(), "pane.out")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	fd, err := syscall.Open(fifo, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		t.Fatalf("open fifo: %v", err)
	}
	keeper, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		_ = syscall.Close(fd)
		t.Fatalf("open fifo keeper: %v", err)
	}
	t.Cleanup(func() { _ = keeper.Close() })
	if err := syscall.SetNonblock(fd, false); err != nil {
		_ = keeper.Close()
		_ = syscall.Close(fd)
		t.Fatalf("make fifo read blocking: %v", err)
	}
	rc := os.NewFile(uintptr(fd), fifo)
	t.Cleanup(func() { _ = rc.Close() })

	if err := ts.EnablePipePane("dd of=" + shellQuoteForTest(fifo) + " bs=4096 2>/dev/null"); err != nil {
		t.Fatalf("EnablePipePane: %v", err)
	}
	// Produce output AFTER the pipe is up: pipe-pane only streams future bytes.
	if err := ts.SendRawKeys([]byte("echo AF1787MARKER\n")); err != nil {
		t.Fatalf("SendRawKeys: %v", err)
	}
	// Bound the read from OUTSIDE rather than with SetReadDeadline. A FIFO opened
	// via os.OpenFile is non-pollable on darwin (golang/go#24164), so SetReadDeadline
	// there returns "file type does not support deadline" and the bound never arms —
	// it can never work for this fd type by Go's own design, on the very platform
	// this fixture mirrors. A goroutine plus a timeout arms identically everywhere.
	// The buffered channel keeps the reader from blocking on send if it does return
	// after the timeout.
	buf := make([]byte, 4096)
	type readResult struct {
		n   int
		err error
	}
	done := make(chan readResult, 1)
	go func() {
		n, err := rc.Read(buf)
		done <- readResult{n, err}
	}()
	var res readResult
	select {
	case res = <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("no bytes streamed within 10s after EnablePipePane — did the reap kill pipe-pane's own dd?")
	}
	if res.err != nil || res.n == 0 {
		t.Fatalf("no bytes streamed after EnablePipePane — did the reap kill pipe-pane's own dd? err=%v n=%d", res.err, res.n)
	}
	if err := ts.DisablePipePane(); err != nil {
		t.Fatalf("DisablePipePane: %v", err)
	}
}

func shellQuoteForTest(s string) string { return "'" + s + "'" }

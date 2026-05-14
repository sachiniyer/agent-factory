package tmux

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
)

// TestCloseDuringAttachRace is a regression test for issue #331.
//
// TmuxSession.Close used to nil out t.ptmx without coordinating with the
// monitorWindowSize goroutines spawned by Attach. A concurrent shutdown
// during attach raced and would panic dereferencing a nil PTY in
// updateWindowSize.
//
// We simulate a minimal Attach lifecycle (without invoking the real
// monitorWindowSize, which touches stdin/SIGWINCH and so is unsuitable
// for tests) by spawning a goroutine modeled on the real one: it watches
// ctx.Done and reads t.ptmx (the field) until cancelled. Close must
// cancel the context and wg.Wait the goroutine before clearing t.ptmx,
// otherwise the race detector will flag the read/write of t.ptmx and a
// real callsite would panic.
func TestCloseDuringAttachRace(t *testing.T) {
	for i := 0; i < 50; i++ {
		ptyFactory := NewMockPtyFactory(t)
		cmdExec := cmd_test.MockCmdExec{
			RunFunc:    func(cmd *exec.Cmd) error { return nil },
			OutputFunc: func(cmd *exec.Cmd) ([]byte, error) { return nil, nil },
		}

		session := newTmuxSession(toTmuxName("race-session", ""), "claude", ptyFactory, cmdExec)
		if err := session.Restore(""); err != nil {
			t.Fatalf("Restore: %v", err)
		}

		// Mimic the bookkeeping that Attach() sets up so Close has goroutines
		// to coordinate with.
		session.attachCh = make(chan struct{})
		session.wg = &sync.WaitGroup{}
		session.ctx, session.cancel = context.WithCancel(context.Background())

		// Stand-in for monitorWindowSize: reads t.ptmx (the field) until ctx
		// is cancelled. Without the Close fix, the pre-fix Close cleared
		// t.ptmx without cancelling the context first, so the read here
		// raced with the field write in Close. We deliberately read only
		// the field (not Fd()) to focus on the bug from #331 — the
		// nil-pointer panic in updateWindowSize.
		session.wg.Add(1)
		go func() {
			defer session.wg.Done()
			for {
				select {
				case <-session.ctx.Done():
					return
				default:
					if p := session.ptmx; p == nil {
						t.Errorf("monitor goroutine observed nil ptmx before ctx cancel")
						return
					}
				}
			}
		}()

		// Let the goroutine spin so Close races with active reads.
		time.Sleep(time.Microsecond)

		if err := session.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		if session.ptmx != nil {
			t.Fatalf("expected ptmx to be nil after Close")
		}
	}
}

// TestDetachClearsPtmxOnRestoreFailure is a regression test for issue #464.
//
// Detach used to close t.ptmx and then call Restore without clearing the
// field. If Restore failed (e.g. tmux session vanished between detach and
// re-attach), t.ptmx was left pointing at the closed file. A subsequent
// Attach would silently bind goroutines to the closed handle and hang.
//
// Detach must clear t.ptmx after closing so the state is unambiguously
// "no PTY", and Attach must surface a clear error rather than proceeding
// against a nil/closed handle.
func TestDetachClearsPtmxOnRestoreFailure(t *testing.T) {
	ptyFactory := NewMockPtyFactory(t)

	// First has-session (from the test's initial Restore) reports the
	// session exists so we can stand up a live ptmx. Subsequent calls
	// (from Detach's internal Restore("")) report missing so Restore
	// fails and exercises the bug.
	hasSessionCalls := 0
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "has-session") {
				hasSessionCalls++
				if hasSessionCalls > 1 {
					return fmt.Errorf("session vanished")
				}
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) { return nil, nil },
	}

	session := newTmuxSession(toTmuxName("detach-restore-fail", ""), "claude", ptyFactory, cmdExec)

	// Stand up the initial PTY the way Start would.
	if err := session.Restore(""); err != nil {
		t.Fatalf("initial Restore: %v", err)
	}

	// Mimic the bookkeeping that Attach() sets up so Detach has matching
	// state to tear down. The real Attach goroutines touch stdin/SIGWINCH
	// and so are unsuitable for tests; an empty wg is sufficient for
	// Detach to complete.
	session.attachCh = make(chan struct{})
	session.wg = &sync.WaitGroup{}
	session.ctx, session.cancel = context.WithCancel(context.Background())

	session.Detach()

	if session.ptmx != nil {
		t.Fatalf("expected ptmx to be nil after Detach with failed Restore, got %v", session.ptmx)
	}

	// A subsequent Attach must surface a clear error rather than hang on
	// a closed PTY.
	ch, err := session.Attach()
	if err == nil {
		t.Fatalf("expected Attach to error when ptmx is nil")
	}
	if ch != nil {
		t.Fatalf("expected nil channel when Attach errors, got %v", ch)
	}
	if !strings.Contains(err.Error(), "no PTY") {
		t.Fatalf("expected error to mention missing PTY, got %v", err)
	}
}

// TestDetachHappyPathReplacesPtmx confirms the normal Detach flow still
// installs a fresh PTY via Restore, so the issue #464 fix doesn't regress
// the path where the tmux session is alive after detach.
func TestDetachHappyPathReplacesPtmx(t *testing.T) {
	ptyFactory := NewMockPtyFactory(t)

	cmdExec := cmd_test.MockCmdExec{
		RunFunc:    func(cmd *exec.Cmd) error { return nil },
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) { return nil, nil },
	}

	session := newTmuxSession(toTmuxName("detach-happy", ""), "claude", ptyFactory, cmdExec)
	if err := session.Restore(""); err != nil {
		t.Fatalf("initial Restore: %v", err)
	}
	original := session.ptmx

	session.attachCh = make(chan struct{})
	session.wg = &sync.WaitGroup{}
	session.ctx, session.cancel = context.WithCancel(context.Background())

	session.Detach()

	if session.ptmx == nil {
		t.Fatalf("expected ptmx to be set after successful Detach -> Restore")
	}
	if session.ptmx == original {
		t.Fatalf("expected Detach to swap in a fresh ptmx, got the original handle")
	}
}

// TestDetachDuringResizeRace is a regression test for issue #512.
//
// PR #474 changed Detach to clear t.ptmx right after closing the PTY so a
// Restore failure couldn't leave a stale closed handle dangling on the
// struct. That nil-clear happened BEFORE t.wg.Wait(), so it raced with
// monitorWindowSize's resize goroutine — which reads t.ptmx via
// updateWindowSize and is tracked by t.wg. Under -race the read/write of
// t.ptmx is flagged; in production it could observe a nil ptmx and panic.
//
// Detach must drain t.wg.Wait() BEFORE touching t.ptmx (clear or replace
// via Restore). Mirrors TestCloseDuringAttachRace: a stand-in goroutine
// reads the t.ptmx field until ctx is cancelled. Many iterations to give
// the race detector room to fire.
func TestDetachDuringResizeRace(t *testing.T) {
	for i := 0; i < 100; i++ {
		ptyFactory := NewMockPtyFactory(t)
		cmdExec := cmd_test.MockCmdExec{
			RunFunc:    func(cmd *exec.Cmd) error { return nil },
			OutputFunc: func(cmd *exec.Cmd) ([]byte, error) { return nil, nil },
		}

		session := newTmuxSession(toTmuxName("detach-race", ""), "claude", ptyFactory, cmdExec)
		if err := session.Restore(""); err != nil {
			t.Fatalf("Restore: %v", err)
		}

		session.attachCh = make(chan struct{})
		session.wg = &sync.WaitGroup{}
		session.ctx, session.cancel = context.WithCancel(context.Background())

		// Stand-in for monitorWindowSize's resize goroutine: reads t.ptmx
		// (the field) until ctx is cancelled. The real goroutine calls
		// pty.Setsize(t.ptmx, ...) which reads the same field; we read the
		// field directly to focus on the data race without touching real
		// terminal state.
		session.wg.Add(1)
		go func() {
			defer session.wg.Done()
			for {
				select {
				case <-session.ctx.Done():
					return
				default:
					_ = session.ptmx
				}
			}
		}()

		// Let the goroutine spin so Detach races with active reads.
		time.Sleep(time.Microsecond)

		session.Detach()

		// Happy path: Detach calls Restore("") which succeeds and installs a
		// fresh ptmx. The point of this test is the race detector, but
		// asserting the happy-path invariant keeps the test honest about what
		// state Detach leaves behind.
		if session.ptmx == nil {
			t.Fatalf("expected ptmx to be set after successful Detach -> Restore")
		}
	}
}

// TestCloseWithoutAttach ensures Close is safe when Attach was never called
// (cancel/wg are nil).
func TestCloseWithoutAttach(t *testing.T) {
	ptyFactory := NewMockPtyFactory(t)
	cmdExec := cmd_test.MockCmdExec{
		RunFunc:    func(cmd *exec.Cmd) error { return nil },
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) { return nil, nil },
	}

	session := newTmuxSession(toTmuxName("no-attach-session", ""), "claude", ptyFactory, cmdExec)
	if err := session.Restore(""); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

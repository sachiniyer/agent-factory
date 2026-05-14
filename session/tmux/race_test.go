package tmux

import (
	"context"
	"errors"
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

// TestKeystrokeMethodsReturnErrSessionGoneAfterDetachRestoreFailure is a
// regression test for issue #510.
//
// PR #474 cleared t.ptmx in Detach when the follow-up Restore failed and
// gated Attach on the nil, but TapEnter/TapDAndEnter/SendKeys still
// dereferenced t.ptmx directly. A status-check (CheckAndHandleTrustPrompt)
// or autoyes daemon tick that fired after a failed Detach Restore would
// panic on the nil Write instead of surfacing ErrSessionGone.
//
// After this fix, every keystroke method that reads t.ptmx must short-
// circuit to ErrSessionGone when the PTY is nil so callers (preview pane,
// daemon, app key handler) can degrade gracefully — mirroring the gate
// already on Attach (#474) and SetDetachedSize (#499).
func TestKeystrokeMethodsReturnErrSessionGoneAfterDetachRestoreFailure(t *testing.T) {
	ptyFactory := NewMockPtyFactory(t)

	// Same has-session script as TestDetachClearsPtmxOnRestoreFailure: the
	// initial Restore succeeds, then Detach's internal Restore("") sees a
	// vanished session and fails — leaving t.ptmx nil.
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

	session := newTmuxSession(toTmuxName("keystroke-detach-fail", ""), "claude", ptyFactory, cmdExec)
	if err := session.Restore(""); err != nil {
		t.Fatalf("initial Restore: %v", err)
	}

	session.attachCh = make(chan struct{})
	session.wg = &sync.WaitGroup{}
	session.ctx, session.cancel = context.WithCancel(context.Background())

	session.Detach()

	if session.ptmx != nil {
		t.Fatalf("expected ptmx to be nil after Detach with failed Restore")
	}

	// Each keystroke method must return ErrSessionGone instead of panicking
	// on the nil Write. Use a table so a future addition can't silently skip
	// the regression check.
	cases := []struct {
		name string
		call func() error
	}{
		{"TapEnter", session.TapEnter},
		{"TapDAndEnter", session.TapDAndEnter},
		{"SendKeys", func() error { return session.SendKeys("hello") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("%s panicked on nil PTY: %v", c.name, r)
				}
			}()
			err := c.call()
			if err == nil {
				t.Fatalf("%s returned nil error, expected ErrSessionGone", c.name)
			}
			if !errors.Is(err, ErrSessionGone) {
				t.Fatalf("%s returned %v, expected ErrSessionGone", c.name, err)
			}
		})
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

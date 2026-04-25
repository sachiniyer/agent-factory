package tmux

import (
	"context"
	"os/exec"
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
		if err := session.Restore(); err != nil {
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

// TestCloseWithoutAttach ensures Close is safe when Attach was never called
// (cancel/wg are nil).
func TestCloseWithoutAttach(t *testing.T) {
	ptyFactory := NewMockPtyFactory(t)
	cmdExec := cmd_test.MockCmdExec{
		RunFunc:    func(cmd *exec.Cmd) error { return nil },
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) { return nil, nil },
	}

	session := newTmuxSession(toTmuxName("no-attach-session", ""), "claude", ptyFactory, cmdExec)
	if err := session.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
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

type blockingRestorePtyFactory struct {
	t *testing.T

	mu             sync.Mutex
	starts         int
	restoreStarted chan struct{}
	releaseRestore chan struct{}
}

func (pt *blockingRestorePtyFactory) Start(_ *exec.Cmd) (*os.File, error) {
	pt.mu.Lock()
	pt.starts++
	start := pt.starts
	if start == 2 {
		close(pt.restoreStarted)
	}
	pt.mu.Unlock()

	if start == 2 {
		<-pt.releaseRestore
	}
	return os.CreateTemp(pt.t.TempDir(), "pty-*")
}

func (pt *blockingRestorePtyFactory) Close() {}

// TestDetachCloseConcurrentAttachChClose is the #1477 regression test.
//
// The stdin-reader goroutine that calls Detach is intentionally outside
// t.wg. Before the fix, Close could run while Detach was still executing
// (here, blocked in the post-detach Restore) and close t.attachCh; Detach's
// deferred cleanup then closed the same channel again and panicked. This
// interleaving must be serialized so exactly one teardown path owns the
// attach channel close.
func TestDetachCloseConcurrentAttachChClose(t *testing.T) {
	ptyFactory := &blockingRestorePtyFactory{
		t:              t,
		restoreStarted: make(chan struct{}),
		releaseRestore: make(chan struct{}),
	}
	cmdExec := cmd_test.MockCmdExec{
		RunFunc:    func(cmd *exec.Cmd) error { return nil },
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) { return []byte(""), nil },
	}

	session := newTmuxSession(toTmuxName("detach-close-double-close", ""), "claude", ptyFactory, cmdExec)
	if err := session.Restore(""); err != nil {
		t.Fatalf("initial Restore: %v", err)
	}

	attachCh := make(chan struct{})
	session.attachCh = attachCh
	session.wg = &sync.WaitGroup{}
	session.ctx, session.cancel = context.WithCancel(context.Background())

	releaseWG := make(chan struct{})
	session.wg.Add(1)
	go func(wg *sync.WaitGroup) {
		defer wg.Done()
		<-releaseWG
	}(session.wg)

	detachDone := make(chan struct{})
	panicCh := make(chan any, 1)
	go func() {
		defer close(detachDone)
		defer func() {
			if r := recover(); r != nil {
				panicCh <- r
			}
		}()
		session.Detach()
	}()

	close(releaseWG)
	select {
	case <-ptyFactory.restoreStarted:
	case <-time.After(time.Second):
		t.Fatal("Detach did not reach blocked Restore")
	}

	closeErrCh := make(chan error, 1)
	go func() {
		closeErrCh <- session.Close()
	}()

	select {
	case <-attachCh:
		// Pre-fix Close wins here, then Detach panics when released.
	case <-time.After(200 * time.Millisecond):
		// Fixed path: Close is serialized behind Detach and cannot close
		// attachCh while Detach is still in progress.
	}
	close(ptyFactory.releaseRestore)

	select {
	case <-detachDone:
	case <-time.After(time.Second):
		t.Fatal("Detach did not return")
	}
	select {
	case err := <-closeErrCh:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not return")
	}
	select {
	case r := <-panicCh:
		t.Fatalf("Detach panicked closing attachCh: %v", r)
	default:
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

// TestDetachDuringResizeRace is a regression test for issue #512.
//
// TmuxSession.Detach used to clear t.ptmx (and call Restore which writes
// t.ptmx) before t.wg.Wait, racing those writes against monitorWindowSize
// goroutines tracked by t.wg — they read t.ptmx via updateWindowSize until
// t.ctx was observed cancelled. Under `go test -race`, the read/write of
// t.ptmx flagged a data race.
//
// Detach must now wg.Wait before mutating t.ptmx so the resize goroutines
// have drained, mirroring the coordination Close uses (#331).
func TestDetachDuringResizeRace(t *testing.T) {
	for i := 0; i < 100; i++ {
		ptyFactory := NewMockPtyFactory(t)
		cmdExec := cmd_test.MockCmdExec{
			RunFunc:    func(cmd *exec.Cmd) error { return nil },
			OutputFunc: func(cmd *exec.Cmd) ([]byte, error) { return nil, nil },
		}

		session := newTmuxSession(toTmuxName("detach-race-session", ""), "claude", ptyFactory, cmdExec)
		if err := session.Restore(""); err != nil {
			t.Fatalf("Restore: %v", err)
		}

		// Mimic the bookkeeping that Attach() sets up so Detach has matching
		// state to tear down. The real monitorWindowSize touches stdin and
		// SIGWINCH so we stand in for it with a goroutine that reads the
		// field — the bug is the unsynchronized read of t.ptmx, regardless
		// of whether the read goes on to Fd() or not.
		session.attachCh = make(chan struct{})
		session.wg = &sync.WaitGroup{}
		session.ctx, session.cancel = context.WithCancel(context.Background())

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

		// Let the goroutine spin so Detach races with active reads.
		time.Sleep(time.Microsecond)

		session.Detach()

		// After Detach completes, the goroutine must have exited (wg.Wait
		// in Detach drained it). t.ptmx may be non-nil here because Detach's
		// internal Restore("") succeeded against the mock — that's expected
		// and tested by TestDetachHappyPathReplacesPtmx.
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

// TestCloseAttachOnly_DoesNotKillSession verifies the non-destructive close
// added for sachiniyer/agent-factory#867: CloseAttachOnly releases the attach
// PTY this client opened in Restore but must NEVER run `tmux kill-session`.
// The daemon uses it to discard a duplicate Instance built from disk while the
// canonical, still-tracked Instance shares the same live tmux session — a
// kill-session there would tear that session out from under the canonical.
func TestCloseAttachOnly_DoesNotKillSession(t *testing.T) {
	ptyFactory := NewMockPtyFactory(t)
	var killSessionCalls int
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "kill-session") {
				killSessionCalls++
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) { return nil, nil },
	}

	session := newTmuxSession(toTmuxName("attach-only", ""), "claude", ptyFactory, cmdExec)
	// Restore opens the attach PTY — the resource a daemon duplicate holds (#867).
	if err := session.Restore(""); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if session.ptmx == nil {
		t.Fatalf("Restore should have opened a PTY")
	}

	if err := session.CloseAttachOnly(); err != nil {
		t.Fatalf("CloseAttachOnly: %v", err)
	}

	if killSessionCalls != 0 {
		t.Fatalf("CloseAttachOnly issued %d kill-session calls; want 0 (must not kill the shared session)", killSessionCalls)
	}
	if session.ptmx != nil {
		t.Fatalf("CloseAttachOnly must release the attach PTY (ptmx still set)")
	}
	// The PTY file must actually be closed (fd reclaimed), not just nil'd out.
	if len(ptyFactory.files) != 1 {
		t.Fatalf("expected exactly one PTY file, got %d", len(ptyFactory.files))
	}
	if err := ptyFactory.files[0].Close(); err == nil {
		t.Fatalf("PTY file should already be closed by CloseAttachOnly")
	}
}

// TestHasUpdatedRestoreRace is a regression test for issue #1528.
//
// TmuxSession.monitor is swapped by Restore() (t.setMonitor(newStatusMonitor()))
// on the restore/RPC/event-loop goroutines while the daemon's per-second poll
// reads the pointer — and mutates its dead/prevOutputHash fields — inside
// HasUpdated(). Before #1528 neither side took a lock, so the pointer write in
// Restore raced the read+field-mutations in HasUpdated (undefined behavior per
// Go's memory model). Both paths now serialize on monitorMu.
//
// Run under `go test -race` (bounded parallelism on shared boxes, see
// docs/container-testing.md): the poll goroutine hammers HasUpdated while the
// restore goroutine repeatedly swaps the monitor. Without the lock the race
// detector flags the t.monitor read/write.
func TestHasUpdatedRestoreRace(t *testing.T) {
	ptyFactory := NewMockPtyFactory(t)
	cmdExec := cmd_test.MockCmdExec{
		// has-session succeeds so Restore attaches (never re-spawns), and
		// capture-pane returns content so HasUpdated exercises the hash compare.
		RunFunc:    func(cmd *exec.Cmd) error { return nil },
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) { return []byte("pane content"), nil },
	}

	session := newTmuxSession(toTmuxName("monitor-race", ""), "claude", ptyFactory, cmdExec)
	if err := session.Restore(""); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Poll goroutine: reads t.monitor and mutates its fields on every tick.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				session.HasUpdated()
			}
		}
	}()

	// Restore goroutine: swaps the monitor pointer out from under the poll.
	// defer close(stop) so the poll goroutine is released on BOTH the normal
	// finish and an early error return — otherwise a transient mock-PTY failure
	// would leave the poll goroutine spinning and wg.Wait() would hang into a
	// test timeout instead of failing cleanly.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stop)
		for i := 0; i < 500; i++ {
			if err := session.Restore(""); err != nil {
				t.Errorf("Restore: %v", err)
				return
			}
		}
	}()

	wg.Wait()
}

package session

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// mockTmuxCaptureChannel is the product tmuxClientlessChannel over a tmux whose
// every command succeeds and does nothing: the capture is a REAL FIFO with its
// real private keeper, but pipe-pane spawns no writer. That is the exact shape
// of #7302's sighting, where TestSubscribeTab_RefusesStaleID's broker read a
// FIFO whose only writer was the keeper.
func mockTmuxCaptureChannel(t *testing.T) *tmuxClientlessChannel {
	t.Helper()
	exec := cmd_test.MockCmdExec{
		RunFunc:    func(*exec.Cmd) error { return nil },
		OutputFunc: func(*exec.Cmd) ([]byte, error) { return nil, nil },
	}
	return newTmuxClientlessChannel(tmux.NewTmuxSessionWithDeps("fifo-wake-7302", "claude", nil, exec))
}

// readUntilError runs the broker's read loop shape over rc and reports the
// error that ended it.
func readUntilError(rc io.Reader) <-chan error {
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 32*1024)
		for {
			if _, err := rc.Read(buf); err != nil {
				done <- err
				return
			}
		}
	}()
	return done
}

// TestCaptureReaderCloseAfterADrainStopWakesTheRead pins #7302's root cause. A
// successful StopCapture takes the drain path, which only closes the private
// keeper and waits for kernel EOF. The broker's Close that follows is the abort,
// and it must wake the read on its own. It used to be a no-op, because the drain
// had already spent the reader's single stop latch, so the abort's wake byte was
// never written and a read with no EOF coming stayed parked.
//
// A writer that never closes stands in for a pipe-pane child that outlived its
// pipe; with it open, EOF cannot arrive, so nothing but the abort's wake can end
// the read.
func TestCaptureReaderCloseAfterADrainStopWakesTheRead(t *testing.T) {
	ch := mockTmuxCaptureChannel(t)
	rc, err := ch.StartCapture()
	if err != nil {
		t.Fatalf("start capture: %v", err)
	}
	cr, ok := rc.(*captureReader)
	if !ok {
		t.Fatalf("StartCapture returned %T, want *captureReader", rc)
	}
	lingering, err := os.OpenFile(filepath.Join(cr.dir, "pane.out"), os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("open a lingering writer on the capture FIFO: %v", err)
	}
	t.Cleanup(func() { _ = lingering.Close() })

	ended := readUntilError(rc)
	time.Sleep(20 * time.Millisecond) // let the read park; the property holds either way
	if err := ch.StopCapture(); err != nil {
		t.Fatalf("stop capture: %v", err)
	}
	select {
	case err := <-ended:
		t.Fatalf("read ended (%v) after the drain stop with a writer still open; this test needs a read that only the abort can end", err)
	case <-time.After(100 * time.Millisecond):
	}

	if err := rc.Close(); err != nil {
		t.Fatalf("close capture reader: %v", err)
	}
	select {
	case err := <-ended:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("read ended with %v, want io.EOF from the abort", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read still parked after Close: an abort that follows a drain stop did not wake it")
	}
}

// TestCaptureReaderKeeperOnlyDrainStopEndsTheRead runs the sighting's shape many
// times: a capture whose only writer is the keeper, stopped by a drain. It
// asserts what the broker relies on, that every read ends once the capture is
// stopped and closed. It also MEASURES what the kernel does on its own, and logs
// it. #4319 and #7302 both saw a macOS read stay parked after the last writer
// closed, and nobody has pinned down the mechanism, so the log reports how often
// the drain's EOF arrived without the abort's help: with the read parked first,
// with the stop racing the read, and with fork/exec running alongside (a forked
// child briefly holds every descriptor, the keeper included).
func TestCaptureReaderKeeperOnlyDrainStopEndsTheRead(t *testing.T) {
	const perCell = 20
	const eofWait = 250 * time.Millisecond

	type cell struct {
		name          string
		parked, forks bool
		eof           int
		maxEOF        time.Duration
	}
	cells := []*cell{
		{name: "parked", parked: true},
		{name: "racing"},
		{name: "parked+forks", parked: true, forks: true},
		{name: "racing+forks", forks: true},
	}
	truePath, lookErr := exec.LookPath("true")
	for _, c := range cells {
		if c.forks && lookErr != nil {
			t.Logf("%s: no `true` binary (%v); running without fork load", c.name, lookErr)
		}
		var stopForks chan struct{}
		var forks sync.WaitGroup
		if c.forks && lookErr == nil {
			stopForks = make(chan struct{})
			forks.Add(1)
			go func() {
				defer forks.Done()
				for {
					select {
					case <-stopForks:
						return
					default:
					}
					_ = exec.Command(truePath).Run()
				}
			}()
		}
		for i := 0; i < perCell; i++ {
			ch := mockTmuxCaptureChannel(t)
			rc, err := ch.StartCapture()
			if err != nil {
				t.Fatalf("%s #%d: start capture: %v", c.name, i, err)
			}
			ended := readUntilError(rc)
			if c.parked {
				time.Sleep(10 * time.Millisecond)
			}
			stopped := time.Now()
			if err := ch.StopCapture(); err != nil {
				t.Fatalf("%s #%d: stop capture: %v", c.name, i, err)
			}
			select {
			case <-ended:
				c.eof++
				if d := time.Since(stopped); d > c.maxEOF {
					c.maxEOF = d
				}
				_ = rc.Close() // the broker always closes; after EOF it must be harmless
			case <-time.After(eofWait):
				_ = rc.Close()
				select {
				case <-ended:
				case <-time.After(2 * time.Second):
					t.Fatalf("%s #%d: read still parked after the drain stop AND Close", c.name, i)
				}
			}
		}
		if stopForks != nil {
			close(stopForks)
			forks.Wait()
		}
	}
	for _, c := range cells {
		t.Logf("keeper-only drain stop on %s, %s: kernel EOF ended the read in %d/%d iterations within %v (slowest %v); the abort's wake ended the other %d",
			runtime.GOOS, c.name, c.eof, perCell, eofWait, c.maxEOF, perCell-c.eof)
	}
}

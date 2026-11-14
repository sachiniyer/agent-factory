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

// finishedCaptureWithPlant runs a keeper-only capture to its own end — a drain
// stop and the kernel's EOF — so the reader has finished and released its
// directory. It then re-creates that directory and lets plant put something at
// the FIFO's old path. The reader comes back unclosed: whatever now sits at that
// path is not the FIFO this reader opened, so the Close that follows must not
// write to it.
func finishedCaptureWithPlant(t *testing.T, plant func(path string)) io.ReadCloser {
	t.Helper()
	ch := mockTmuxCaptureChannel(t)
	rc, err := ch.StartCapture()
	if err != nil {
		t.Fatalf("start capture: %v", err)
	}
	cr, ok := rc.(*captureReader)
	if !ok {
		t.Fatalf("StartCapture returned %T, want *captureReader", rc)
	}
	ended := readUntilError(rc)
	if err := ch.StopCapture(); err != nil {
		t.Fatalf("stop capture: %v", err)
	}
	select {
	case err := <-ended:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("read ended with %v, want the drain's io.EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a keeper-only drain stop did not reach EOF")
	}
	if _, err := os.Stat(cr.dir); !os.IsNotExist(err) {
		t.Fatalf("the finished reader's directory is still there (stat err %v); this test needs it released", err)
	}
	if err := os.Mkdir(cr.dir, 0o700); err != nil {
		t.Fatalf("re-create the capture directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(cr.dir) })
	plant(filepath.Join(cr.dir, "pane.out"))
	return rc
}

// TestCaptureReaderCloseAfterFinishLeavesALinkAtTheFIFOPathAlone pins the wake's
// ownership property (#7303 review): an abort's wake writes only into the FIFO
// this reader opened, and only while the reader still owns it. The broker closes
// every reader after its loop ends, so this Close is the ordinary teardown, not
// a corner case.
func TestCaptureReaderCloseAfterFinishLeavesALinkAtTheFIFOPathAlone(t *testing.T) {
	const content = "#!unchanged"
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte(content), 0o600); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	rc := finishedCaptureWithPlant(t, func(path string) {
		if err := os.Symlink(victim, path); err != nil {
			t.Fatalf("plant a link: %v", err)
		}
	})
	_ = rc.Close()
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("read victim: %v", err)
	}
	if string(got) != content {
		t.Fatalf("Close wrote through a link at the finished reader's FIFO path: %q -> %q", content, got)
	}
}

// TestCaptureReaderCloseAfterFinishLeavesAFileAtTheFIFOPathAlone is the same
// property for a plain file at the FIFO's old path.
func TestCaptureReaderCloseAfterFinishLeavesAFileAtTheFIFOPathAlone(t *testing.T) {
	const content = "unchanged"
	var planted string
	rc := finishedCaptureWithPlant(t, func(path string) {
		planted = path
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("plant a file: %v", err)
		}
	})
	_ = rc.Close()
	got, err := os.ReadFile(planted)
	if err != nil {
		t.Fatalf("read planted file: %v", err)
	}
	if string(got) != content {
		t.Fatalf("Close wrote into a file at the finished reader's FIFO path: %q -> %q", content, got)
	}
}

// TestCaptureReaderCloseNeverBlocksOnAFullFIFO pins the other half of the wake's
// contract (#7303 review): the wake never blocks. A byte the FIFO cannot take at
// once is dropped, which is safe because a full FIFO already holds data for the
// read to return, and abort is latched before the wake. Close runs inside a
// teardown that holds captureMu, between joinCapture's two bounded waits, so a
// blocking wake would be an unbounded step there.
func TestCaptureReaderCloseNeverBlocksOnAFullFIFO(t *testing.T) {
	// After a drain the wake takes the path route; with the keeper still open
	// (an abort with no drain before it) it writes through the keeper.
	t.Run("after a drain stop", func(t *testing.T) { closeOnAFullFIFO(t, true) })
	t.Run("with the keeper still open", func(t *testing.T) { closeOnAFullFIFO(t, false) })
}

func closeOnAFullFIFO(t *testing.T, drainFirst bool) {
	ch := mockTmuxCaptureChannel(t)
	rc, err := ch.StartCapture()
	if err != nil {
		t.Fatalf("start capture: %v", err)
	}
	cr := rc.(*captureReader)
	path := filepath.Join(cr.dir, "pane.out")
	// Fill the FIFO to capacity with nothing reading, through a raw nonblocking
	// descriptor that stays open, so the drain's EOF cannot arrive either.
	fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open a filler on the capture FIFO: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	chunk := make([]byte, 4096)
	for filled := 0; ; filled += len(chunk) {
		if filled > 64<<20 {
			t.Fatal("the FIFO never filled")
		}
		if _, err := syscall.Write(fd, chunk); err != nil {
			if errors.Is(err, syscall.EAGAIN) {
				break
			}
			t.Fatalf("fill the FIFO: %v", err)
		}
	}
	// One more byte may still fit after a short chunk is refused; take it too.
	for {
		if _, err := syscall.Write(fd, chunk[:1]); err != nil {
			break
		}
	}
	if drainFirst {
		if err := ch.StopCapture(); err != nil {
			t.Fatalf("stop capture: %v", err)
		}
	}

	closed := make(chan struct{})
	go func() {
		_ = rc.Close()
		close(closed)
	}()
	// However this test ends, release everything. A wake blocked on the full FIFO
	// (the bug) holds the reader's keeperMu, so first make room from a second
	// read end, which lets it finish without touching the reader. Then end the
	// read.
	t.Cleanup(func() {
		if drain, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0); err == nil {
			buf := make([]byte, 64<<10)
			for {
				if n, err := syscall.Read(drain, buf); n <= 0 || err != nil {
					break
				}
			}
			_ = syscall.Close(drain)
		}
		<-closed
		<-readUntilError(rc)
	})
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close is blocked: the abort's wake waited on a full FIFO instead of dropping its byte")
	}
	select {
	case err := <-readUntilError(rc):
		if !errors.Is(err, io.EOF) {
			t.Fatalf("read after the abort ended with %v, want io.EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read did not end after the abort")
	}
}

// testFIFO is a FIFO with its read end held open nonblocking, plus the identity
// a captureReader records for it.
type testFIFO struct {
	path     string
	fd       int
	dev, ino uint64
}

func newTestFIFO(t *testing.T) testFIFO {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open fifo read end: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		t.Fatalf("fstat fifo: %v", err)
	}
	return testFIFO{path: path, fd: fd, dev: uint64(st.Dev), ino: uint64(st.Ino)}
}

// pending reports how many bytes a nonblocking read finds waiting in the FIFO.
func (f testFIFO) pending(t *testing.T) int {
	t.Helper()
	buf := make([]byte, 16)
	n, err := syscall.Read(f.fd, buf)
	if err != nil && !errors.Is(err, syscall.EAGAIN) {
		t.Fatalf("read fifo: %v", err)
	}
	if n < 0 {
		return 0
	}
	return n
}

// wakeAfterDrain is a reader whose drain has closed its keeper, so its wake can
// only take the path route.
func wakeAfterDrain(path string, owner testFIFO) *captureReader {
	return &captureReader{fifo: path, fifoDev: owner.dev, fifoIno: owner.ino, keeperClosed: true}
}

// TestCaptureReaderPathWakeWritesOnlyIntoTheFIFOItOpened pins the wake's
// identity check on its own, without the finish latch's help (#7303 review). A
// path wake writes only to a descriptor that is this reader's own FIFO, and never
// through a link. The first case is the positive one: the right FIFO still gets
// its wake byte.
func TestCaptureReaderPathWakeWritesOnlyIntoTheFIFOItOpened(t *testing.T) {
	t.Run("its own FIFO gets the byte", func(t *testing.T) {
		own := newTestFIFO(t)
		wakeAfterDrain(own.path, own).wake()
		if got := own.pending(t); got != 1 {
			t.Fatalf("own FIFO holds %d bytes after the wake, want 1", got)
		}
	})
	t.Run("a different FIFO at its path gets nothing", func(t *testing.T) {
		own, other := newTestFIFO(t), newTestFIFO(t)
		wakeAfterDrain(other.path, own).wake()
		if got := other.pending(t); got != 0 {
			t.Fatalf("a FIFO the reader did not open received %d bytes", got)
		}
	})
	t.Run("a link to its own FIFO is not followed", func(t *testing.T) {
		own := newTestFIFO(t)
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(own.path, link); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		wakeAfterDrain(link, own).wake()
		if got := own.pending(t); got != 0 {
			t.Fatalf("the wake went through a link: %d bytes arrived", got)
		}
	})
	t.Run("a file at its path is left alone", func(t *testing.T) {
		own := newTestFIFO(t)
		path := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(path, []byte("unchanged"), 0o600); err != nil {
			t.Fatalf("write file: %v", err)
		}
		wakeAfterDrain(path, own).wake()
		if got, _ := os.ReadFile(path); string(got) != "unchanged" {
			t.Fatalf("the wake wrote into a file: %q", got)
		}
	})
}

// TestCaptureReaderWakeAfterFinishWritesNothing pins the finish latch on its
// own, without the identity check's help: once finish has begun, the reader owns
// nothing at its path, so no wake writes even where the path still names a FIFO
// that would pass the identity check.
func TestCaptureReaderWakeAfterFinishWritesNothing(t *testing.T) {
	own := newTestFIFO(t)
	r := wakeAfterDrain(own.path, own)
	r.finished = true
	r.wake()
	if got := own.pending(t); got != 0 {
		t.Fatalf("a finished reader's wake wrote %d bytes", got)
	}
}

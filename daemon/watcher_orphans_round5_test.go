package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Regressions from the fifth Codex round on #5337 (#5329).

// TestSweepSurvivesNonRegularRecordFile: the startup sweep reads the record
// file under s.mu, so a FIFO at that path with no writer must not stall it —
// nor the first reconcile, nor every reload and shutdown queued behind it. The
// record is refused and the sweep carries on with the environ channel alone.
func TestSweepSurvivesNonRegularRecordFile(t *testing.T) {
	s, home := newOrphanSweepSupervisor(t)
	s.reapOrphans = true
	fifo := filepath.Join(home, watchChildRecordName)
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	// On an unfixed tree the read blocks in open until a writer appears; open
	// and close one so the stuck reconcile drains instead of hanging the binary.
	// Registered after newTestSupervisor's Cleanup(s.Stop), so it runs first.
	t.Cleanup(func() {
		f, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			_ = f.Close()
		} else if !errors.Is(err, syscall.ENXIO) {
			t.Logf("open fifo for writing: %v", err)
		}
	})

	reloaded := make(chan error, 1)
	go func() { reloaded <- s.Reload() }()
	select {
	case err := <-reloaded:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first reconcile did not return: the sweep blocked reading a FIFO at the record path")
	}
}

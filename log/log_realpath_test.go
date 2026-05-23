package log

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// TestRealPathActualLoss exercises the production Initialize/Close path with
// the logFileName var pointed at a temp directory, and reports any Printf
// that lands in neither the log file nor captured stderr.
//
// Pre-fix (#642): the close-before-SetOutput ordering leaves a window where
// Printfs in flight when globalLogFile.Close() runs write to a closed fd and
// disappear. On the buggy code this test reports "actually lost (nowhere): N"
// with N > 0. Post-fix N is always zero.
//
// Burst pattern (one Printf per goroutine, gated by a shared channel) is
// what makes the window observable — it maximizes the number of in-flight
// Printfs at the moment Close races them.
func TestRealPathActualLoss(t *testing.T) {
	tmpDir := t.TempDir()
	origLogFileName := logFileName
	logFileName = filepath.Join(tmpDir, "actual.log")
	t.Cleanup(func() { logFileName = origLogFileName })

	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = origStderr })

	stderrCh := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(r)
		stderrCh <- b
	}()

	const burst = 3000

	Initialize(false)

	var totalWrites int64
	var wg sync.WaitGroup
	wg.Add(burst)
	startGate := make(chan struct{})
	for g := 0; g < burst; g++ {
		go func(gid int) {
			defer wg.Done()
			<-startGate
			// Hit all three loggers so the race covers Info / Warning / Error.
			InfoLog.Printf("REALPATH I g=%d", gid)
			WarningLog.Printf("REALPATH W g=%d", gid)
			ErrorLog.Printf("REALPATH E g=%d", gid)
			atomic.AddInt64(&totalWrites, 3)
		}(g)
	}
	close(startGate)
	Close()
	wg.Wait()

	_ = w.Close()
	capturedStderr := <-stderrCh

	fileContent, err := os.ReadFile(logFileName)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}

	inFile := int64(strings.Count(string(fileContent), "REALPATH "))
	inStderr := int64(strings.Count(string(capturedStderr), "REALPATH "))
	totalFound := inFile + inStderr

	lost := totalWrites - totalFound
	if lost != 0 {
		t.Fatalf("actually lost (nowhere): %d (wrote=%d, in file=%d, in stderr=%d)",
			lost, totalWrites, inFile, inStderr)
	}
}

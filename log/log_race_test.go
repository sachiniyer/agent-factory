package log

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// TestCloseRaceConcurrentPrintf reproduces the #642 race over many iterations.
// Each iteration: Initialize, spawn a burst of goroutines that each Printf
// once, call Close concurrently with the burst, then check that every Printf
// landed somewhere (log file or stderr). On the pre-fix code (close before
// SetOutput) a fraction of each burst is dropped to the closed fd.
//
// Burst pattern (every goroutine does exactly one Printf gated by a shared
// channel) is what makes the window observable — it maximizes the number of
// Printfs in-flight at the moment Close runs globalLogFile.Close().
func TestCloseRaceConcurrentPrintf(t *testing.T) {
	tmpDir := t.TempDir()
	origLogFileName := logFileName
	logFileName = filepath.Join(tmpDir, "race.log")
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

	const (
		iterations = 100
		burstSize  = 200
	)

	var totalWrites int64

	for iter := 0; iter < iterations; iter++ {
		Initialize(false)

		var wg sync.WaitGroup
		wg.Add(burstSize)
		startGate := make(chan struct{})
		for g := 0; g < burstSize; g++ {
			go func(iter, gid int) {
				defer wg.Done()
				<-startGate
				InfoLog.Printf("RACEMARK iter=%d g=%d", iter, gid)
				atomic.AddInt64(&totalWrites, 1)
			}(iter, g)
		}
		close(startGate)
		// Race the close against the in-flight burst.
		Close()
		wg.Wait()
	}

	_ = w.Close()
	capturedStderr := <-stderrCh

	fileContent, err := os.ReadFile(logFileName)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}

	combined := string(fileContent) + string(capturedStderr)
	found := int64(strings.Count(combined, "RACEMARK "))

	if found != totalWrites {
		t.Fatalf("message loss detected: wrote %d Printfs but only %d markers found in log+stderr (lost %d)",
			totalWrites, found, totalWrites-found)
	}
}

// TestCloseRaceSingleBurst is the smallest reproducer: one Initialize, one
// large burst of concurrent Printfs, one Close racing them. A failure here
// names specific missing ids, which is easier to debug than the iteration-
// summed loss reported by TestCloseRaceConcurrentPrintf.
func TestCloseRaceSingleBurst(t *testing.T) {
	tmpDir := t.TempDir()
	origLogFileName := logFileName
	logFileName = filepath.Join(tmpDir, "burst.log")
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

	Initialize(false)

	const total = 2000
	var wg sync.WaitGroup
	wg.Add(total)
	startGate := make(chan struct{})
	for i := 0; i < total; i++ {
		go func(id int) {
			defer wg.Done()
			<-startGate
			InfoLog.Printf("BURSTMARK %d", id)
		}(i)
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

	combined := string(fileContent) + string(capturedStderr)

	// Account for every Printf: each unique id must appear somewhere.
	var missing []int
	for i := 0; i < total; i++ {
		needle := fmt.Sprintf("BURSTMARK %d\n", i)
		if !strings.Contains(combined, needle) {
			missing = append(missing, i)
		}
	}
	if len(missing) > 0 {
		preview := missing
		if len(preview) > 10 {
			preview = preview[:10]
		}
		t.Fatalf("lost %d/%d messages (first missing ids: %v)", len(missing), total, preview)
	}
}

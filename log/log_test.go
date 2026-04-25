package log

import (
	"io"
	"os"
	"strings"
	"sync"
	"testing"
)

// TestInitializeRace spins multiple goroutines concurrently calling
// Initialize to make sure the package-level mutex prevents data races on
// globalLogFile and the exported logger pointers. Run with `go test -race`.
func TestInitializeRace(t *testing.T) {
	var wg sync.WaitGroup
	const n = 10
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			Initialize(false)
		}()
	}
	wg.Wait()

	// Clean up the file descriptor opened by the last Initialize call.
	Close()
}

// TestCloseRedirectsToStderr verifies that after Close(), the package-level
// loggers no longer point at the closed log file fd. Instead they should be
// redirected to stderr so late writes from background goroutines surface
// rather than being silently dropped.
func TestCloseRedirectsToStderr(t *testing.T) {
	// Redirect the process's stderr to a pipe so we can observe what Close()
	// and any subsequent log call actually write.
	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe failed: %v", err)
	}
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = origStderr })

	Initialize(false)
	Close()

	// Logging after Close should not panic and should reach stderr (the pipe).
	const msg = "post-close-message-xyz"
	InfoLog.Println(msg)
	WarningLog.Println(msg)
	ErrorLog.Println(msg)

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	captured, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}

	if !strings.Contains(string(captured), msg) {
		t.Fatalf("expected post-close log message %q to appear on stderr, got: %q", msg, string(captured))
	}

	// globalLogFile should be cleared so a follow-up Close is a no-op.
	if globalLogFile != nil {
		t.Fatalf("expected globalLogFile to be nil after Close")
	}
}

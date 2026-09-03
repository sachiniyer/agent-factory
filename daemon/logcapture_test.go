package daemon

import (
	"bytes"
	"io"
	stdlog "log"
	"strings"
	"sync"
	"testing"

	aflog "github.com/sachiniyer/agent-factory/log"
)

// The one sink daemon tests may install on a process-global logger (#3787).
//
// log.WarningLog / InfoLog / ErrorLog are process-global, so SetOutput does not
// scope anything to the calling test: EVERY goroutine alive in the test binary
// that logs at that level writes into whichever sink is currently installed.
// Seventeen test files pointed those globals at a bare, unsynchronized
// bytes.Buffer and then read it, which is a data race between the reader and
// any foreign writer — `Test (macOS)` went red on a PR containing no Go the
// daemon package can reach, with `reconcileLateGhostCleanup` (a background
// goroutine belonging to a Manager the failing test never created) writing into
// that test's assertion buffer.
//
// A mutex on both halves closes the race. It does NOT close the other half of
// #3787: the foreign warning still LANDS in the buffer, so an assertion of the
// form `contains(warnings, "…")` can still be satisfied by a warning some other
// test's subsystem emitted. Only per-subsystem log routing fixes that, and it
// is deliberately not attempted here — see part 2 of #3787.
//
// scripts/daemon_log_capture_test.go is the lint that keeps a new bare buffer
// from landing: this file is the only place in daemon/**/*_test.go allowed to
// call SetOutput on one of those loggers.

// logCapture is a synchronized log sink. Write is called under the logger's own
// mutex, but String/Reset are called from the test goroutine with no such
// protection, so both ends take this lock.
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

// String returns everything captured so far. Safe to call while a foreign
// goroutine is still logging.
func (c *logCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// Reset drops what has been captured, for tests that assert on one phase of a
// sequence at a time.
func (c *logCapture) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf.Reset()
}

// captureLogger installs a fresh logCapture on logger for the test's lifetime
// and restores the previous sink afterwards.
func captureLogger(t *testing.T, logger *stdlog.Logger) *logCapture {
	t.Helper()
	capture := &logCapture{}
	previous := logger.Writer()
	logger.SetOutput(capture)
	t.Cleanup(func() { logger.SetOutput(previous) })
	return capture
}

// captureWarnings routes the daemon WARNING log into a synchronized buffer for
// one test. Most daemon refusals are logged and never returned to a caller, so
// the log is the only place the diagnosis a user acts on survives.
func captureWarnings(t *testing.T) *logCapture {
	t.Helper()
	return captureLogger(t, aflog.WarningLog)
}

// captureInfo routes the daemon INFO log into a synchronized buffer for one
// test, so a test can assert what the daemon claims in its own success log.
func captureInfo(t *testing.T) *logCapture {
	t.Helper()
	return captureLogger(t, aflog.InfoLog)
}

// captureErrors routes the daemon ERROR log into a synchronized buffer for one
// test.
func captureErrors(t *testing.T) *logCapture {
	t.Helper()
	return captureLogger(t, aflog.ErrorLog)
}

// teeWarnings captures the WARNING log while still writing it through to the
// previous sink, so a failing test's output still shows the warnings it saw.
func teeWarnings(t *testing.T) *logCapture {
	t.Helper()
	capture := &logCapture{}
	previous := aflog.WarningLog.Writer()
	aflog.WarningLog.SetOutput(io.MultiWriter(previous, capture))
	t.Cleanup(func() { aflog.WarningLog.SetOutput(previous) })
	return capture
}

// silenceWarnings drops the WARNING log for one test. For tests that provoke
// warnings on purpose and assert on something else — the noise is expected, so
// it is not worth printing, and there is nothing to read back.
func silenceWarnings(t *testing.T) {
	t.Helper()
	previous := aflog.WarningLog.Writer()
	aflog.WarningLog.SetOutput(io.Discard)
	t.Cleanup(func() { aflog.WarningLog.SetOutput(previous) })
}

// TestLogCaptureSurvivesAForeignWriter is the #3787 regression, in the shape
// the race detector reported it: a goroutine this test did not start writes
// through the process-global WarningLog while the test reads its own capture.
//
// With the bare `var warnings bytes.Buffer` this file replaces, `go test -race`
// fails here with the report from the issue — a bytes.(*Buffer).String() read
// against a log.(*Logger).Printf() write. Verified by running exactly that
// before the helper existed.
//
// The Contains check at the end is the anti-vacuity half: without it, a capture
// that was never installed on WarningLog would race against nothing and pass.
func TestLogCaptureSurvivesAForeignWriter(t *testing.T) {
	warnings := captureWarnings(t)

	const writes = 200
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < writes; i++ {
			aflog.WarningLog.Printf("foreign goroutine warning %d", i)
		}
	}()

	reads := 0
	for {
		select {
		case <-done:
			if reads == 0 {
				t.Fatal("the reader never overlapped the writer, so nothing was exercised")
			}
			if got := warnings.String(); !strings.Contains(got, "foreign goroutine warning") {
				t.Fatalf("capture is not installed on WarningLog — it read %q", got)
			}
			return
		default:
			_ = warnings.String()
			reads++
		}
	}
}

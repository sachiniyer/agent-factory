package log

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestLoggersInitializedByDefault asserts that the package-level loggers are
// non-nil before any Initialize call. Regression for sachiniyer/agent-factory#514:
// `af upgrade` reaches the SIGTERM fallback in daemon/sigterm_fallback.go
// before runUpgrade has a chance to call Initialize, and the fallback writes
// through InfoLog/WarningLog. Without non-nil defaults those Printf calls
// nil-dereference and panic the upgrade.
func TestLoggersInitializedByDefault(t *testing.T) {
	if InfoLog == nil {
		t.Error("InfoLog is nil at package-init time; upgrade SIGTERM fallback would panic")
	}
	if WarningLog == nil {
		t.Error("WarningLog is nil at package-init time; upgrade SIGTERM fallback would panic")
	}
	if ErrorLog == nil {
		t.Error("ErrorLog is nil at package-init time")
	}
	// Exercise each logger to confirm no panic on a Printf path.
	InfoLog.Printf("default-logger-smoke-test")
	WarningLog.Printf("default-logger-smoke-test")
	ErrorLog.Printf("default-logger-smoke-test")
}

// TestLogFilePathMatchesResolvedFallback is the regression for
// sachiniyer/agent-factory#1604: when AGENT_FACTORY_HOME is set but its
// directory cannot be created, resolveLogPath falls back away from the home
// override, and LogFilePath must return that same resolved path — not the dead
// override — so `af bug-report`/`af doctor` tail the file logging actually
// writes to.
//
// The uncreatable-home shape mirrors the report's /tmp/blockdir repro: a
// regular file blocks MkdirAll of a subdir beneath it, so
// AGENT_FACTORY_HOME=<file>/subdir can never be created.
func TestLogFilePathMatchesResolvedFallback(t *testing.T) {
	t.Cleanup(func() {
		logFileName = ""
		globalLogFile = nil
	})

	blocker := filepath.Join(t.TempDir(), "blockdir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker file: %v", err)
	}
	afHome := filepath.Join(blocker, "subdir") // uncreatable: blocker is a file
	afHomeLog := filepath.Join(afHome, "agent-factory.log")
	t.Setenv("AGENT_FACTORY_HOME", afHome)

	Initialize(false)
	defer Close()

	// Sanity: the setup actually triggered the fallback (resolveLogPath could
	// not use the uncreatable home), otherwise the test proves nothing.
	if logFileName == afHomeLog {
		t.Fatalf("expected Initialize to fall back away from the uncreatable home %q, but logFileName is the home path", afHomeLog)
	}

	// The fix: LogFilePath returns exactly where logging landed, never the
	// abandoned override path.
	if got := LogFilePath(); got != logFileName {
		t.Fatalf("LogFilePath()=%q diverges from the written path logFileName=%q", got, logFileName)
	}
	if got := LogFilePath(); got == afHomeLog {
		t.Fatalf("LogFilePath() returned the uncreatable home path %q instead of the resolved fallback", afHomeLog)
	}
}

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

// TestCloseClaimsFileOnlyWhenOpened is the regression for
// sachiniyer/agent-factory#894: Close() must print "wrote logs to <file>" only
// when Initialize actually opened a log file. When the open fails (e.g. an
// unwritable path) logging falls back to stderr, globalLogFile stays nil, and
// claiming a file was written points the user at a file that does not exist.
func TestCloseClaimsFileOnlyWhenOpened(t *testing.T) {
	t.Cleanup(func() {
		logPathOverride = ""
		globalLogFile = nil
	})

	captureStderr := func(t *testing.T, fn func()) string {
		t.Helper()
		origStderr := os.Stderr
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe failed: %v", err)
		}
		os.Stderr = w
		fn()
		if err := w.Close(); err != nil {
			t.Fatalf("close pipe writer: %v", err)
		}
		os.Stderr = origStderr
		out, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("read pipe: %v", err)
		}
		return string(out)
	}

	t.Run("no file opened: no wrote-logs claim", func(t *testing.T) {
		// Parent directory does not exist, so OpenFile fails and Initialize
		// falls back to stderr without setting globalLogFile.
		logPathOverride = filepath.Join(t.TempDir(), "missing-dir", "agent-factory.log")
		globalLogFile = nil
		out := captureStderr(t, func() {
			Initialize(false)
			Close()
		})
		if globalLogFile != nil {
			t.Fatalf("expected globalLogFile to stay nil after a failed Initialize")
		}
		if strings.Contains(out, "wrote logs to") {
			t.Fatalf("Close() claimed logs were written though no file opened; stderr: %q", out)
		}
	})

	t.Run("file opened: wrote-logs claim present", func(t *testing.T) {
		logPathOverride = filepath.Join(t.TempDir(), "agent-factory.log")
		globalLogFile = nil
		out := captureStderr(t, func() {
			Initialize(false)
			Close()
		})
		want := "wrote logs to " + logFileName
		if !strings.Contains(out, want) {
			t.Fatalf("expected Close() to print %q; stderr: %q", want, out)
		}
	})
}

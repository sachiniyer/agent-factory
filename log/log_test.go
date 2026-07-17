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

// TestLogFilePathPreInitFallsBackFromUncreatableHome covers the OTHER divergence
// state #1604 can hit: LogFilePath called before Initialize (logFileName still
// "") with a set-but-uncreatable AGENT_FACTORY_HOME. Because both the resolver
// and LogFilePath now share the homeLogPath gate, the query must fall through to
// the UserConfigDir default here too — not return the dead override path — even
// though no Initialize has run to cache the resolved path.
func TestLogFilePathPreInitFallsBackFromUncreatableHome(t *testing.T) {
	mu.Lock()
	saved := logFileName
	logFileName = ""
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		logFileName = saved
		mu.Unlock()
	})

	// Deterministic UserConfigDir under a temp dir so the fallback path is
	// predictable and the test never touches the developer's real config.
	//
	// HOME as well as XDG_CONFIG_HOME: os.UserConfigDir consults XDG_CONFIG_HOME
	// only on unix-likes. On darwin it is unconditionally
	// $HOME/Library/Application Support and never reads XDG at all, so pinning
	// XDG alone left the fallback resolving against the runner's REAL home and
	// the assertion below comparing two unrelated paths (#1931).
	cfgHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	t.Setenv("HOME", cfgHome)

	blocker := filepath.Join(t.TempDir(), "blockdir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker file: %v", err)
	}
	afHome := filepath.Join(blocker, "subdir") // uncreatable: blocker is a file
	afHomeLog := filepath.Join(afHome, "agent-factory.log")
	t.Setenv("AGENT_FACTORY_HOME", afHome)

	got := LogFilePath()
	if got == afHomeLog {
		t.Fatalf("pre-Initialize LogFilePath returned the uncreatable home path %q instead of the fallback", afHomeLog)
	}
	// Re-derive the expected path from the OS's own config dir rather than
	// hard-coding the unix layout. This still pins the concrete shape the
	// fallback must produce — <config dir>/agent-factory/agent-factory.log — so a
	// regression to the dead override path (or anywhere else) still fails; only
	// the platform-chosen base is delegated. Deliberately NOT compared against
	// defaultLogPath(): asserting the fallback equals the function it calls would
	// pass no matter what that function returned.
	base, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("os.UserConfigDir: %v", err)
	}
	if want := filepath.Join(base, "agent-factory", "agent-factory.log"); got != want {
		t.Fatalf("pre-Initialize LogFilePath=%q, want resolved fallback %q", got, want)
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

	t.Run("file opened and dirtied: wrote-logs claim present", func(t *testing.T) {
		// #1749: the claim is printed only when the run actually logged
		// something at WARNING/ERROR level (a WarningLog write dirties the run).
		logPathOverride = filepath.Join(t.TempDir(), "agent-factory.log")
		globalLogFile = nil
		out := captureStderr(t, func() {
			Initialize(false)
			WarningLog.Print("something worth reporting")
			Close()
		})
		want := "wrote logs to " + logFileName
		if !strings.Contains(out, want) {
			t.Fatalf("expected Close() to print %q; stderr: %q", want, out)
		}
	})

	t.Run("file opened but clean: no wrote-logs claim", func(t *testing.T) {
		// #1749: a successful command that logged nothing at WARNING/ERROR ends
		// with its own output — Close() must not append the bookkeeping line.
		logPathOverride = filepath.Join(t.TempDir(), "agent-factory.log")
		globalLogFile = nil
		out := captureStderr(t, func() {
			Initialize(false)
			InfoLog.Print("routine chatter does not dirty the run")
			Close()
		})
		if strings.Contains(out, "wrote logs to") {
			t.Fatalf("Close() claimed logs were written on a clean run; stderr: %q", out)
		}
	})
}

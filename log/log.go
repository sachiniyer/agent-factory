package log

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	WarningLog *log.Logger
	InfoLog    *log.Logger
	ErrorLog   *log.Logger
)

// Default the package-level loggers to discard sinks so callers that reach a
// log call before Initialize do not nil-panic. Initialize replaces these with
// real file/stderr loggers. Without this, code paths like `af upgrade` that
// forget to call Initialize crash inside the SIGTERM fallback (#514).
func init() {
	InfoLog = log.New(io.Discard, "", 0)
	WarningLog = log.New(io.Discard, "", 0)
	ErrorLog = log.New(io.Discard, "", 0)
}

// mu guards writes to globalLogFile and the exported *log.Logger pointers
// performed by Initialize and Close. Readers (e.g. InfoLog.Printf) rely on
// init-once-before-use semantics: Initialize is expected to complete before
// any goroutine reads the logger pointers. *log.Logger itself is internally
// thread-safe, so we do not take this lock on the logging hot path.
var mu sync.Mutex

// logPathOverride, when non-empty, wins over all environment-based
// resolution. Only this package's tests set it, to point the log at a
// scratch file without touching the process environment.
var logPathOverride string

// expandTilde resolves a leading "~" or "~/" in dir. ok is false for the
// unsupported "~user" form or when the home directory cannot be resolved.
// Mirrors config.GetConfigDir's expansion; this package cannot import config
// (config imports log).
func expandTilde(dir string) (string, bool) {
	if !strings.HasPrefix(dir, "~") {
		return dir, true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	switch {
	case dir == "~":
		return home, true
	case strings.HasPrefix(dir, "~/"):
		return filepath.Join(home, dir[2:]), true
	default:
		return "", false
	}
}

// resolveLogPath picks the log destination, in priority order:
//
//  1. logPathOverride (this package's tests);
//  2. $AGENT_FACTORY_HOME/agent-factory.log — the same relocation override
//     config.GetConfigDir honors, so a relocated agent-factory home keeps its
//     log next to its config and state. This is the seam that lets sandboxed
//     tests, and the af binaries they exec, log into a temp home instead of
//     the developer's real log (#1056);
//  3. inside a `go test` binary with no home override: a scratch file under
//     os.TempDir(), never the real log — a test that forgot to sandbox its
//     home must not pollute the production daemon's log (#1056);
//  4. the historical default, os.UserConfigDir()/agent-factory/agent-factory.log.
//
// Resolved at Initialize time (not package init) so environment changes made
// by test setup are respected.
func resolveLogPath() string {
	if logPathOverride != "" {
		return logPathOverride
	}
	if home := os.Getenv("AGENT_FACTORY_HOME"); home != "" {
		if dir, ok := expandTilde(home); ok {
			if err := os.MkdirAll(dir, 0700); err == nil {
				return filepath.Join(dir, "agent-factory.log")
			}
		}
		// Unresolvable or uncreatable override: fall through to the defaults
		// below rather than not logging at all.
	}
	if testing.Testing() {
		return filepath.Join(os.TempDir(), "agent-factory-test.log")
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "agent-factory.log")
	}
	dir := filepath.Join(configDir, "agent-factory")
	os.MkdirAll(dir, 0700)
	return filepath.Join(dir, "agent-factory.log")
}

// logFileName is the path the most recent Initialize resolved and attempted
// to open; Close uses it for the "wrote logs to" message.
var logFileName string

var globalLogFile *os.File

// Initialize should be called once at the beginning of the program to set up logging.
// defer Close() after calling this function. It sets the go log output to the file in
// the user config directory.

func Initialize(daemon bool) {
	mu.Lock()
	defer mu.Unlock()

	logFileName = resolveLogPath()

	// Close any previously opened log file to avoid leaking file descriptors
	// when Initialize is called multiple times (e.g. af tasks trigger -> RunTask).
	// Redirect to stderr BEFORE closing for the same reason as Close (#642):
	// concurrent Printfs holding the old logger pointer would otherwise race
	// the file close and land on a closed fd. After reassignment below,
	// further Printfs see the new loggers; any still using old pointers fall
	// through to stderr instead of being dropped.
	if globalLogFile != nil {
		if InfoLog != nil {
			InfoLog.SetOutput(os.Stderr)
		}
		if WarningLog != nil {
			WarningLog.SetOutput(os.Stderr)
		}
		if ErrorLog != nil {
			ErrorLog.SetOutput(os.Stderr)
		}
		_ = globalLogFile.Close()
		globalLogFile = nil
	}

	f, err := os.OpenFile(logFileName, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not open log file %s: %v, logging to stderr\n", logFileName, err)
		fmtS := "%s"
		if daemon {
			fmtS = "[DAEMON] %s"
		}
		InfoLog = log.New(os.Stderr, fmt.Sprintf(fmtS, "INFO:"), log.Ldate|log.Ltime|log.Lshortfile)
		WarningLog = log.New(os.Stderr, fmt.Sprintf(fmtS, "WARNING:"), log.Ldate|log.Ltime|log.Lshortfile)
		ErrorLog = log.New(os.Stderr, fmt.Sprintf(fmtS, "ERROR:"), log.Ldate|log.Ltime|log.Lshortfile)
		return
	}

	// Set log format to include timestamp and file/line number
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	fmtS := "%s"
	if daemon {
		fmtS = "[DAEMON] %s"
	}
	InfoLog = log.New(f, fmt.Sprintf(fmtS, "INFO:"), log.Ldate|log.Ltime|log.Lshortfile)
	WarningLog = log.New(f, fmt.Sprintf(fmtS, "WARNING:"), log.Ldate|log.Ltime|log.Lshortfile)
	ErrorLog = log.New(f, fmt.Sprintf(fmtS, "ERROR:"), log.Ldate|log.Ltime|log.Lshortfile)

	globalLogFile = f
}

func Close() {
	mu.Lock()
	defer mu.Unlock()

	// Redirect to stderr BEFORE closing the file. SetOutput and Output share
	// each *log.Logger's internal mutex, so a concurrent Printf either fully
	// precedes the redirect (and completes its write to the still-open file)
	// or fully follows it (and writes to stderr). Closing after the redirect
	// guarantees no Printf ever lands on a closed fd. Reversing this order
	// loses messages logged in the gap between Close and SetOutput (#642).
	if InfoLog != nil {
		InfoLog.SetOutput(os.Stderr)
	}
	if WarningLog != nil {
		WarningLog.SetOutput(os.Stderr)
	}
	if ErrorLog != nil {
		ErrorLog.SetOutput(os.Stderr)
	}
	// Only claim a file was written when one was actually opened. When
	// Initialize fell back to stderr (or was never called), globalLogFile is
	// nil and there is no file to point at (#894).
	fileWasOpened := globalLogFile != nil
	if globalLogFile != nil {
		_ = globalLogFile.Close()
		globalLogFile = nil
	}
	if fileWasOpened {
		fmt.Fprintln(os.Stderr, "wrote logs to "+logFileName)
	}
}

// Every is used to log at most once every timeout duration.
type Every struct {
	mu       sync.Mutex
	duration time.Duration
	timer    *time.Timer
}

func NewEvery(timeout time.Duration) *Every {
	return &Every{duration: timeout}
}

// ShouldLog returns true if the timeout has passed since the last log.
func (e *Every) ShouldLog() bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.timer == nil {
		e.timer = time.NewTimer(e.duration)
		return true
	}

	select {
	case <-e.timer.C:
		e.timer.Reset(e.duration)
		return true
	default:
		return false
	}
}

package log

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
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

func getLogPath() string {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "agent-factory.log")
	}
	dir := filepath.Join(configDir, "agent-factory")
	os.MkdirAll(dir, 0700)
	return filepath.Join(dir, "agent-factory.log")
}

var logFileName = getLogPath()

var globalLogFile *os.File

// Initialize should be called once at the beginning of the program to set up logging.
// defer Close() after calling this function. It sets the go log output to the file in
// the user config directory.

func Initialize(daemon bool) {
	mu.Lock()
	defer mu.Unlock()

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

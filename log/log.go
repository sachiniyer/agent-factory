package log

import (
	"fmt"
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
	if globalLogFile != nil {
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

	if globalLogFile != nil {
		_ = globalLogFile.Close()
		globalLogFile = nil
	}
	// Redirect any further log writes to stderr so that messages from
	// goroutines that outlive Close() are not silently dropped to a closed
	// fd. log.Logger.SetOutput is documented as safe for concurrent use.
	if InfoLog != nil {
		InfoLog.SetOutput(os.Stderr)
	}
	if WarningLog != nil {
		WarningLog.SetOutput(os.Stderr)
	}
	if ErrorLog != nil {
		ErrorLog.SetOutput(os.Stderr)
	}
	fmt.Fprintln(os.Stderr, "wrote logs to "+logFileName)
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

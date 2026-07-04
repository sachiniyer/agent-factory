package log

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// Log-rotation defaults (#1059). The log is rotated when it exceeds
// DefaultMaxSizeMB; DefaultMaxBackups rotated files (agent-factory.log.1,
// .log.2, ...) are kept and older ones deleted. Both are overridable in the
// global config.json via "log_max_size_mb" and "log_max_backups"; the config
// package reuses these constants so the two packages cannot drift (config
// imports log, so the constants must live here).
const (
	DefaultMaxSizeMB  = 50
	DefaultMaxBackups = 2
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

// rotationPolicy resolves the log-rotation cap and backup count from the
// global config file ("log_max_size_mb" / "log_max_backups"), falling back to
// the package defaults. This package cannot import config (config imports
// log) and Initialize runs before config.LoadConfig, so the two keys are read
// here directly with a tolerant parse: a missing or unreadable file, invalid
// content, or out-of-range values all silently yield the defaults —
// config.LoadConfig is the layer that warns the user about bad values.
//
// Format resolution mirrors config.LoadConfig (#1030): a config.toml that
// exists is canonical (config.json is never consulted, even when the TOML is
// unparsable — falling back to json values that config.LoadConfig itself
// refuses to load would rotate on settings the rest of af is not using);
// otherwise config.json is read as before.
//
// The config file location mirrors config.GetConfigDir: $AGENT_FACTORY_HOME
// when set, else ~/.agent-factory. Inside a `go test` binary with no home
// override no file is read at all, so a developer's personal config can never
// change test behavior (#1056/#1057 hermeticity).
func rotationPolicy() (maxBytes int64, backups int) {
	maxMB := DefaultMaxSizeMB
	backups = DefaultMaxBackups

	configDir := ""
	if home := os.Getenv("AGENT_FACTORY_HOME"); home != "" {
		if dir, ok := expandTilde(home); ok {
			configDir = dir
		}
	} else if !testing.Testing() {
		if home, err := os.UserHomeDir(); err == nil {
			configDir = filepath.Join(home, ".agent-factory")
		}
	}
	if configDir != "" {
		// Pointers distinguish "key absent" from an explicit zero:
		// log_max_backups=0 (keep no rotated files) is valid, an absent key
		// means the default.
		var cfg struct {
			LogMaxSizeMB  *int `json:"log_max_size_mb" toml:"log_max_size_mb"`
			LogMaxBackups *int `json:"log_max_backups" toml:"log_max_backups"`
		}
		parsed := false
		if data, err := os.ReadFile(filepath.Join(configDir, "config.toml")); err == nil {
			parsed = toml.Unmarshal(data, &cfg) == nil
		} else if data, err := os.ReadFile(filepath.Join(configDir, "config.json")); err == nil {
			parsed = json.Unmarshal(data, &cfg) == nil
		}
		if parsed {
			if cfg.LogMaxSizeMB != nil && *cfg.LogMaxSizeMB > 0 {
				maxMB = *cfg.LogMaxSizeMB
			}
			if cfg.LogMaxBackups != nil && *cfg.LogMaxBackups >= 0 {
				backups = *cfg.LogMaxBackups
			}
		}
	}
	return int64(maxMB) * 1024 * 1024, backups
}

// rotateFiles shifts the rotated-log chain one step: path.<backups-1> ->
// path.<backups>, ..., path.1 -> path.2, path -> path.1. With backups == 0
// the current file is simply deleted. Afterwards any contiguous run of
// backups beyond the keep count (left over from a larger log_max_backups) is
// pruned. Everything is best-effort: rotation must never prevent logging, so
// rename/remove errors are ignored and the caller reopens path regardless.
//
// Cross-process note: rotation happens with no lock held against other af
// processes. os.Rename is atomic on the same filesystem, so concurrent
// writers never see a torn file; the worst race outcome is a writer holding
// an fd appending to a file that is now path.1 (its O_APPEND writes still
// land intact there). CLI invocations are short-lived so their fds close
// almost immediately, and the long-lived daemon rotates its own fd via
// rotatingWriter, so no process appends to a renamed file for long.
func rotateFiles(path string, backups int) {
	if backups <= 0 {
		_ = os.Remove(path)
	} else {
		for i := backups - 1; i >= 1; i-- {
			_ = os.Rename(fmt.Sprintf("%s.%d", path, i), fmt.Sprintf("%s.%d", path, i+1))
		}
		_ = os.Rename(path, path+".1")
	}
	for i := backups + 1; ; i++ {
		if err := os.Remove(fmt.Sprintf("%s.%d", path, i)); err != nil {
			break
		}
	}
}

// rotatingWriter is the io.Writer behind the exported loggers: an *os.File
// plus a size counter that triggers an in-place rotation when a write would
// push the file past maxBytes. Initialize's stat-and-rotate-on-open already
// bounds every short-lived CLI invocation; this write-path check is what
// bounds the always-on daemon, which otherwise holds one fd for weeks and —
// worse — would keep appending to the renamed (and eventually deleted) inode
// after another process rotated the file under it (#1059).
//
// The mutex serializes Write/Close because the three exported loggers share
// one writer and *log.Logger only serializes writes through itself. size is
// process-local: concurrent appends from another af process are not counted,
// so a file can transiently exceed the cap until either process next rotates.
// That slack is acceptable for a log; the cap is a bound, not an exact quota.
type rotatingWriter struct {
	mu       sync.Mutex
	file     *os.File
	path     string
	size     int64
	maxBytes int64
	backups  int
	mode     os.FileMode // permission for the fresh file each rotation creates
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		// Close redirects the loggers to stderr before closing this writer,
		// so nothing should land here; if something does (or a rotation
		// failed to reopen the file), surface it rather than dropping it —
		// the #642 tests pin "every Printf lands somewhere".
		return os.Stderr.Write(p)
	}
	// The size > 0 guard keeps a single oversized message from rotating an
	// empty file on every write.
	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		w.rotateLocked()
		if w.file == nil {
			return os.Stderr.Write(p)
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

// rotateLocked closes the current file, shifts the backup chain, and reopens
// a fresh file at path. On reopen failure w.file is left nil and Write falls
// back to stderr; the next Initialize will recover.
func (w *rotatingWriter) rotateLocked() {
	_ = w.file.Close()
	w.file = nil
	rotateFiles(w.path, w.backups)
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, w.mode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not reopen log file %s after rotation: %v, logging to stderr\n", w.path, err)
		return
	}
	w.file = f
	w.size = 0
}

func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// NewRotatingFile opens path for appending, wrapped in the same size-capped
// rotation as the main agent-factory.log (#1059): a file already over the
// configured cap is rotated before opening, and a write that would push it
// past the cap rotates in place first — the property that bounds callers who
// hold the returned writer long-term, like the daemon's per-task watch-script
// logs (#1062). The cap and keep-count come from the same "log_max_size_mb" /
// "log_max_backups" config keys as the main log, resolved once at open. perm
// applies both to the initial open and to the fresh file each rotation
// creates. The writer is safe for concurrent use; Close is idempotent.
func NewRotatingFile(path string, perm os.FileMode) (io.WriteCloser, error) {
	maxBytes, backups := rotationPolicy()
	if fi, err := os.Stat(path); err == nil && fi.Size() > maxBytes {
		rotateFiles(path, backups)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, perm)
	if err != nil {
		return nil, err
	}
	w := &rotatingWriter{file: f, path: path, maxBytes: maxBytes, backups: backups, mode: perm}
	if fi, err := f.Stat(); err == nil {
		w.size = fi.Size()
	}
	return w, nil
}

// logFileName is the path the most recent Initialize resolved and attempted
// to open; Close uses it for the "wrote logs to" message.
var logFileName string

var globalLogFile *rotatingWriter

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

	// Rotate before opening when the existing log already exceeds the cap
	// (#1059). This is the seam every af process passes through — the daemon,
	// the TUI, and each CLI invocation all call Initialize — so even a log
	// grown huge by an old binary is trimmed on the next af command.
	maxBytes, backups := rotationPolicy()
	if fi, err := os.Stat(logFileName); err == nil && fi.Size() > maxBytes {
		rotateFiles(logFileName, backups)
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

	w := &rotatingWriter{file: f, path: logFileName, maxBytes: maxBytes, backups: backups, mode: 0600}
	if fi, err := f.Stat(); err == nil {
		w.size = fi.Size()
	}

	fmtS := "%s"
	if daemon {
		fmtS = "[DAEMON] %s"
	}
	InfoLog = log.New(w, fmt.Sprintf(fmtS, "INFO:"), log.Ldate|log.Ltime|log.Lshortfile)
	WarningLog = log.New(w, fmt.Sprintf(fmtS, "WARNING:"), log.Ldate|log.Ltime|log.Lshortfile)
	ErrorLog = log.New(w, fmt.Sprintf(fmtS, "ERROR:"), log.Ldate|log.Ltime|log.Lshortfile)

	globalLogFile = w
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

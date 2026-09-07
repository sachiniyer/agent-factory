// Package hooklog gives operator hooks an output descriptor whose lifetime is
// independent of the process that launched them.
package hooklog

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// TailLimit is the most hook output retained in an error message. The complete
// output remains in the per-run log file named alongside that error.
const TailLimit = 64 * 1024

// Kind identifies the hook runner that owns a log. It is deliberately closed:
// Kind becomes part of a filename under the AF home and must never admit path
// separators from a caller.
type Kind string

const (
	PostWorktree Kind = "post-worktree"
	OnArchive    Kind = "on-archive"
)

// Open creates a private per-run log under $AF_HOME/logs/hooks. The returned
// *os.File is load-bearing: os/exec passes it straight to the child instead of
// creating a pipe and a copying goroutine in the launcher.
func Open(kind Kind) (*os.File, error) {
	return open(kind, syscall.Flock)
}

func open(kind Kind, flock func(int, int) error) (*os.File, error) {
	switch kind {
	case PostWorktree, OnArchive:
	default:
		return nil, fmt.Errorf("unknown hook log kind %q", kind)
	}
	home, err := config.GetConfigDir()
	if err != nil {
		return nil, fmt.Errorf("resolve AF home for hook log: %w", err)
	}
	dir := filepath.Join(home, "logs", "hooks")
	if err := config.MkdirAllUnderAFHome(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create hook log directory %s: %w", dir, err)
	}
	file, err := os.CreateTemp(dir, string(kind)+lockedLogMarker+"*.log")
	if err != nil {
		return nil, fmt.Errorf("create %s hook log in %s: %w", kind, dir, err)
	}
	// flock belongs to the open file description. The child's inherited
	// stdout/stderr keep it alive even after the launcher exits or closes its
	// copy. Never explicitly unlock: the last descriptor close releases it.
	if err := flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		// Retention must not make a previously working hook fail on a
		// filesystem without flock. An unmarked fallback is never pruned,
		// including if a later launcher can acquire locks on this storage.
		fallback, createErr := os.CreateTemp(dir, string(kind)+"-*.log")
		if createErr != nil {
			return nil, fmt.Errorf("create unlocked %s hook log in %s: %w", kind, dir, createErr)
		}
		log.WarningLog.Printf("hook log retention disabled for %s: cannot lock output: %v", fallback.Name(), err)
		return fallback, nil
	}
	pruned, pruneErr := prune(dir, file.Name(), time.Now())
	if pruned > 0 {
		log.InfoLog.Printf("hook logs: pruned %d kept logs from %s", pruned, dir)
	}
	if pruneErr != nil {
		log.WarningLog.Printf("hook log retention in %s: %v", dir, pruneErr)
	}
	return file, nil
}

// CloseAndReadTail closes the launcher's descriptor and returns a bounded tail
// of the file. After confirmed process-group/scope teardown the bytes are the
// final tail; a caller reporting failed teardown may still use the bounded
// snapshot while naming the full file for later inspection.
func CloseAndReadTail(file *os.File) (string, error) {
	path := file.Name()
	tail, readErr := readTail(file)
	closeErr := file.Close()
	if closeErr != nil {
		closeErr = fmt.Errorf("close hook log %s: %w", path, closeErr)
	}
	return tail, errors.Join(closeErr, readErr)
}

// readTail reads through the descriptor Open returned, not by reopening its
// pathname. Besides avoiding a path race with operator-authored code, ReadAt
// leaves the shared file offset alone if a failed scope teardown means a child
// still holds a duplicate descriptor and is writing.
func readTail(file *os.File) (string, error) {
	path := file.Name()
	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("inspect hook log %s: %w", path, err)
	}
	start := info.Size() - TailLimit
	truncated := start > 0
	if start < 0 {
		start = 0
	}
	data, err := io.ReadAll(io.NewSectionReader(file, start, TailLimit))
	if err != nil {
		return "", fmt.Errorf("read hook log %s: %w", path, err)
	}
	if !truncated {
		return string(data), nil
	}
	return fmt.Sprintf("[output truncated to last %d bytes]\n%s", TailLimit, data), nil
}

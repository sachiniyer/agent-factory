package git

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/systemdunit"
	"github.com/sachiniyer/agent-factory/log"
)

// SetHookResumeDisabled carries persisted terminal state through materialization.
// It performs no I/O; restore's adoption boundary retires the journal on disk.
func (g *GitWorktree) SetHookResumeDisabled(disabled bool) { g.hooksResumeDisabled = disabled }

var errInvalidHookProgress = errors.New("invalid hook journal")

// Test seam for a transient storage error during restore.
var hookProgressReadFile = BoundedReadFile

func readHookProgress(path string) (*hookProgress, error) {
	info, err := BoundedLstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: hook journal is not a regular file", errInvalidHookProgress)
	}
	data, err := hookProgressReadFile(path)
	if err != nil {
		return nil, err
	}
	var p hookProgress
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("%w: %v", errInvalidHookProgress, err)
	}
	p.Directory, err = hookReceiptDirectory(path, p.Directory)
	if err != nil {
		return nil, err
	}
	info, err = BoundedLstat(p.Directory)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: hook receipt directory is not a directory", errInvalidHookProgress)
	}
	return &p, nil
}

// A journal may have been written through another spelling of this AF home.
// Verify the parents identify the same directory, then operate only through
// the current journal parent. A foreign same-basename directory is not enough.
func hookReceiptDirectory(path, recorded string) (string, error) {
	base := filepath.Base(recorded)
	if !strings.HasPrefix(base, "entries-") {
		return "", fmt.Errorf("%w: hook journal has no receipt directory", errInvalidHookProgress)
	}
	parent, previousParent := filepath.Dir(path), filepath.Dir(recorded)
	if parent != previousParent {
		current, err := boundedResolveForCompare(parent)
		if err != nil {
			return "", err
		}
		previous, err := boundedResolveForCompare(previousParent)
		if err != nil {
			return "", err
		}
		currentInfo, err := BoundedLstat(current)
		if err != nil {
			return "", err
		}
		previousInfo, err := BoundedLstat(previous)
		if err != nil {
			return "", err
		}
		if !currentInfo.IsDir() || !previousInfo.IsDir() || !os.SameFile(currentInfo, previousInfo) {
			return "", fmt.Errorf("%w: hook receipt directory is outside its journal directory", errInvalidHookProgress)
		}
	}
	return filepath.Join(parent, base), nil
}

func (g *GitWorktree) ownedHookProgress() (*hookProgress, string, error) {
	if g.IsExternalWorktree() {
		return nil, "", fmt.Errorf("%w: external worktree", errInvalidHookProgress)
	}
	return readOwnedHookProgress(g.worktreePath, g.hookScopeSessionID)
}

func readOwnedHookProgress(worktreePath, sessionID string) (*hookProgress, string, error) {
	if sessionID == "" {
		return nil, "", fmt.Errorf("%w: worktree has no hook journal ownership", errInvalidHookProgress)
	}
	path, err := hookProgressPath(worktreePath)
	if err != nil {
		return nil, "", err
	}
	p, err := readHookProgress(path)
	if err != nil {
		return nil, path, err
	}
	if p.SessionID != sessionID || p.Worktree != worktreePath || p.Prefix != systemdunit.HookScopeUnitPrefix(sessionID) {
		return nil, path, fmt.Errorf("%w: hook journal belongs to a different session", errInvalidHookProgress)
	}
	return p, path, nil
}

// AbandonHookProgress commits terminal restore intent without adopting or
// stopping a survivor. Kill's existing safe teardown still owns that stop.
func (g *GitWorktree) AbandonHookProgress() {
	g.hooksResumeDisabled = true
	worktreePath, sessionID := g.worktreePath, g.hookScopeSessionID
	if err := abandonHookProgress(worktreePath, sessionID); err != nil {
		log.WarningLog.Printf("cannot abandon hook progress for %s: %v; scheduling bounded retry", worktreePath, err)
		done := make(chan struct{})
		g.hooksRetirementDone = done
		go func() {
			defer close(done)
			retryHookProgressAbandonment(worktreePath, sessionID)
		}()
	}
}

// Restore publishes terminal worktrees only after this asynchronous obligation
// is installed. The shared batch owns the bounded read; marker persistence and
// any retry continue without multiplying daemon startup by the row count.
func (g *GitWorktree) installHookProgressAbandonment(worktreePath, sessionID string, p *hookProgress, readErr error) {
	g.hooksResumeDisabled = true
	if noResumableHookProgress(readErr) {
		return
	}
	done := make(chan struct{})
	g.hooksRetirementDone = done
	go func() {
		defer close(done)
		if readErr == nil {
			if err := p.markFinished(); err == nil {
				return
			} else {
				readErr = err
			}
		}
		log.WarningLog.Printf("cannot abandon hook progress for %s: %v; scheduling bounded retry", worktreePath, readErr)
		retryHookProgressAbandonment(worktreePath, sessionID)
	}()
}

// retireHookProgress is called only AFTER cancellation, join and scope teardown
// have proved all writers gone. Mark terminal before attempting the lock so
// interrupted or deferred reclamation remains eligible for the orphan sweep.
func (g *GitWorktree) retireHookProgress() error {
	p, path, err := g.ownedHookProgress()
	if noResumableHookProgress(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot retire unreadable hook journal: %w", err)
	}
	if err := p.markFinished(); err != nil {
		return fmt.Errorf("cannot terminalize hook journal: %w", err)
	}
	acquired, err := retireHookProgressSnapshot(p, path)
	if err != nil || !acquired {
		if err != nil {
			log.WarningLog.Printf("cannot reclaim hook progress for %s: %v; scheduling bounded retry", p.Worktree, err)
		} else {
			log.WarningLog.Printf("deferring hook progress reclamation for %s: progress lock busy", p.Worktree)
		}
		// Archive may move g.worktreePath immediately after this returns. Retry
		// only the immutable journal identity, never mutable worktree fields.
		done := make(chan struct{})
		g.hooksRetirementDone = done
		go func() {
			defer close(done)
			retryHookProgressRetirement(p, path)
		}()
	}
	return nil
}

func (p *hookProgress) finished() bool {
	info, err := os.Lstat(filepath.Join(p.Directory, "finished"))
	return err == nil && info.Mode().IsRegular()
}

func (p *hookProgress) completed() bool {
	if !p.finished() {
		return false
	}
	for index := range p.Commands {
		info, err := os.Lstat(filepath.Join(p.receipt(index), "exit"))
		if err != nil || !info.Mode().IsRegular() {
			return false
		}
	}
	return true
}

// Retire the resumable name before deleting receipts. A crash leaves a
// non-resumable retired journal that the next creation sweep can reclaim.
func removeHookProgress(path string, p *hookProgress) error {
	retired := filepath.Join(filepath.Dir(path), "retired-"+filepath.Base(p.Directory)+".json")
	if path != retired {
		if err := os.Rename(path, retired); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(p.Directory); err != nil {
		return err
	}
	return os.Remove(retired)
}

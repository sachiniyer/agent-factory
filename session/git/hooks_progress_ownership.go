package git

import (
	"encoding/json"
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

func readHookProgress(path string) (*hookProgress, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("hook journal is not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p hookProgress
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
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
		return nil, fmt.Errorf("hook receipt directory is not a directory")
	}
	return &p, nil
}

// A journal may have been written through another spelling of this AF home.
// Verify the parents identify the same directory, then operate only through
// the current journal parent. A foreign same-basename directory is not enough.
func hookReceiptDirectory(path, recorded string) (string, error) {
	base := filepath.Base(recorded)
	if !strings.HasPrefix(base, "entries-") {
		return "", fmt.Errorf("hook journal has no receipt directory")
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
			return "", fmt.Errorf("hook receipt directory is outside its journal directory")
		}
	}
	return filepath.Join(parent, base), nil
}

func (g *GitWorktree) ownedHookProgress() (*hookProgress, string, error) {
	if g.IsExternalWorktree() || g.hookScopeSessionID == "" {
		return nil, "", fmt.Errorf("worktree has no hook journal ownership")
	}
	path, err := hookProgressPath(g.worktreePath)
	if err != nil {
		return nil, "", err
	}
	p, err := readHookProgress(path)
	if err != nil {
		return nil, path, err
	}
	if p.SessionID != g.hookScopeSessionID || p.Worktree != g.worktreePath || p.Prefix != systemdunit.HookScopeUnitPrefix(g.hookScopeSessionID) {
		return nil, path, fmt.Errorf("hook journal belongs to a different session")
	}
	return p, path, nil
}

// AbandonHookProgress commits terminal restore intent without adopting or
// stopping a survivor. Kill's existing safe teardown still owns that stop.
func (g *GitWorktree) AbandonHookProgress() {
	g.hooksResumeDisabled = true
	if p, _, err := g.ownedHookProgress(); err == nil {
		p.finish()
	}
}

// retireHookProgress is called only AFTER cancellation, join and scope teardown
// have proved all writers gone. Mark terminal before attempting the lock so
// interrupted or deferred reclamation remains eligible for the orphan sweep.
func (g *GitWorktree) retireHookProgress() {
	p, path, err := g.ownedHookProgress()
	if err != nil {
		return
	}
	p.finish()
	acquired, err := retireHookProgressSnapshot(p, path)
	if err != nil {
		log.WarningLog.Printf("cannot reclaim hook progress for %s: %v", p.Worktree, err)
	} else if !acquired {
		log.WarningLog.Printf("deferring hook progress reclamation for %s: progress lock busy", p.Worktree)
		// Archive may move g.worktreePath immediately after this returns. Retry
		// only the immutable journal identity, never mutable worktree fields.
		done := make(chan struct{})
		g.hooksRetirementDone = done
		go func() {
			defer close(done)
			retryHookProgressRetirement(p, path)
		}()
	}
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

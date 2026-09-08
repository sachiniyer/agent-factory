package git

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
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
	if filepath.Dir(p.Directory) != filepath.Dir(path) || !strings.HasPrefix(filepath.Base(p.Directory), "entries-") {
		return nil, fmt.Errorf("hook receipt directory is outside its journal directory")
	}
	info, err = os.Lstat(p.Directory)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("hook receipt directory is not a directory")
	}
	return &p, nil
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
// have proved all writers gone. Complete interrupted/unstarted receipts before
// deleting so an interrupted cleanup remains eligible for the orphan sweep.
func (g *GitWorktree) retireHookProgress() {
	p, path, err := g.ownedHookProgress()
	if err != nil {
		return
	}
	err = config.WithFileLock(filepath.Join(filepath.Dir(path), ".progress"), func() error {
		current, _, readErr := g.ownedHookProgress()
		if readErr != nil || current.Directory != p.Directory {
			return readErr
		}
		for index := range p.Commands {
			receipt := p.receipt(index)
			if err := os.MkdirAll(receipt, 0700); err != nil {
				return err
			}
			exit := filepath.Join(receipt, "exit")
			if _, err := os.Stat(exit); os.IsNotExist(err) {
				if err := os.WriteFile(exit, []byte("cancelled\n"), 0600); err != nil {
					return err
				}
			} else if err != nil {
				return err
			}
		}
		p.finish()
		if !p.completed() {
			return fmt.Errorf("hook journal completion could not be confirmed")
		}
		return removeHookProgress(path, p)
	})
	if err != nil {
		log.WarningLog.Printf("cannot reclaim hook progress for %s: %v", g.worktreePath, err)
	}
}

func (p *hookProgress) completed() bool {
	if info, err := os.Lstat(filepath.Join(p.Directory, "finished")); err != nil || !info.Mode().IsRegular() {
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

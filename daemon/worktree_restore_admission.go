package daemon

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// reserveLocalRestoreBranch validates and reserves the complete branch
// admission decision for one local recovery. The observed branch, not the
// cached record, is authoritative when the worktree is still registered.
// requireRegistered is true for archived restores, whose retained worktree must
// be associated with Git directly or through an identity-qualified relocation
// candidate; Lost recovery may legitimately rebuild a missing worktree and then
// uses its recorded branch as the candidate Git will bind. The per-repository /
// branch lock is held from the final Git observation through the live transition,
// so two archived lanes in an already-corrupted multiply-bound cohort cannot
// both pass while both still look archived.
func (m *Manager) reserveLocalRestoreBranch(
	repoID, title string,
	instance *session.Instance,
	requireRegistered bool,
	relocationAliases ...string,
) (func(), error) {
	row := instance.ToInstanceData()
	repoPath := strings.TrimSpace(row.Worktree.RepoPath)
	worktreePath := strings.TrimSpace(row.Worktree.WorktreePath)
	if repoPath == "" || worktreePath == "" {
		return nil, fmt.Errorf("cannot restore session %q: its repository or worktree identity is missing, so af cannot verify branch ownership", title)
	}

	initial, err := worktreeBranchBindings(repoPath)
	if err != nil {
		return nil, fmt.Errorf("cannot restore session %q: could not inspect worktree branch bindings; nothing was moved: %w", title, err)
	}
	branch, registered, err := restoreCandidateBranch(row, initial, relocationAliases...)
	if err != nil {
		return nil, fmt.Errorf("cannot restore session %q: %w", title, err)
	}
	if !registered && requireRegistered {
		return nil, fmt.Errorf("cannot restore session %q: its worktree %s is absent from Git's registration, so af cannot establish the branch it would activate; nothing was moved", title, config.ShellQuotePath(worktreePath))
	}
	if branch == "" {
		return func() {}, nil // positively observed detached HEAD
	}

	lock := m.restoreBranchLock(repoID, branch)
	lock.Lock()
	release := lock.Unlock
	fresh, err := worktreeBranchBindings(repoPath)
	if err != nil {
		release()
		return nil, fmt.Errorf("cannot restore session %q: could not revalidate worktree branch bindings; nothing was moved: %w", title, err)
	}
	freshBranch, freshRegistered, err := restoreCandidateBranch(row, fresh, relocationAliases...)
	if err != nil || freshBranch != branch || freshRegistered != registered {
		release()
		if err != nil {
			return nil, fmt.Errorf("cannot restore session %q: %w", title, err)
		}
		return nil, fmt.Errorf("cannot restore session %q: its worktree branch registration changed during admission; retry after it settles", title)
	}

	diskData, err := loadRepoInstanceData(repoID)
	if err != nil {
		release()
		return nil, fmt.Errorf("cannot restore session %q: could not read the persisted lane inventory needed to verify branch %q: %w", title, branch, err)
	}
	m.mu.Lock()
	current := m.instances[daemonInstanceKey(repoID, title)]
	for _, binding := range fresh {
		if binding.Branch != branch || sameWorktreePath(binding.Path, worktreePath) {
			continue
		}
		if lane := m.liveLaneHoldingWorktreeLocked(repoID, binding.Path, diskData); lane != "" && lane != title {
			m.mu.Unlock()
			release()
			handoff := shellsuggest.PositionalCommand("af", []string{"sessions", "handoff", "--to", "<agent>"}, lane)
			return nil, fmt.Errorf("cannot restore session %q: branch %q is already checked out by live lane %q at %s. Restoring would bind two live worktrees to one branch, so continue in that workspace with `%s` or release the branch yourself; af did not rename, detach, reset, or move either worktree",
				title, branch, lane, config.ShellQuotePath(binding.Path), handoff)
		}
	}
	m.mu.Unlock()
	if current != instance {
		release()
		return nil, fmt.Errorf("session %q changed state before its branch holders could be verified", title)
	}
	return release, nil
}

func restoreCandidateBranch(
	row session.InstanceData,
	bindings []sessiongit.WorktreeBranchBinding,
	relocationAliases ...string,
) (branch string, registered bool, err error) {
	paths := append([]string{row.Worktree.WorktreePath}, relocationAliases...)
	for _, binding := range bindings {
		matched := false
		for _, candidate := range paths {
			if sameWorktreePath(binding.Path, candidate) {
				matched = true
				break
			}
		}
		if matched {
			switch {
			case binding.HeadSHA == "":
				return "", true, fmt.Errorf("worktree %s has no observed HEAD, so its active branch is unknown", config.ShellQuotePath(row.Worktree.WorktreePath))
			case binding.Branch != "":
				return binding.Branch, true, nil
			case binding.Detached:
				return "", true, nil
			default:
				return "", true, fmt.Errorf("worktree %s has neither a branch nor a detached-HEAD marker, so its active branch is unknown", config.ShellQuotePath(row.Worktree.WorktreePath))
			}
		}
	}
	if _, statErr := os.Stat(row.Worktree.WorktreePath); statErr == nil {
		return "", false, fmt.Errorf("worktree %s exists but is absent from Git's registration, so its active branch is unknown", config.ShellQuotePath(row.Worktree.WorktreePath))
	} else if !os.IsNotExist(statErr) {
		return "", false, fmt.Errorf("worktree %s could not be identified: %w", config.ShellQuotePath(row.Worktree.WorktreePath), statErr)
	}
	branch = strings.TrimSpace(row.Worktree.BranchName)
	if branch == "" {
		return "", false, fmt.Errorf("the missing worktree has no recorded branch, so its recovery target is unknown")
	}
	return branch, false, nil
}

func sameWorktreePath(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	resolvedLeft := pathutil.ResolveForCompare(left)
	return resolvedLeft == pathutil.ResolveForCompare(right)
}

func (m *Manager) restoreBranchLock(repoID, branch string) *sync.Mutex {
	key := repoID + "\x00" + branch
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.restoreBranchLocks == nil {
		m.restoreBranchLocks = make(map[string]*sync.Mutex)
	}
	lock := m.restoreBranchLocks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		m.restoreBranchLocks[key] = lock
	}
	return lock
}

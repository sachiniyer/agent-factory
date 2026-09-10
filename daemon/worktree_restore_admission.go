package daemon

import (
	"fmt"
	"os"
	"sort"
	"strings"

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
// uses its recorded branch as the candidate Git will bind. The shared
// per-repository worktree-admission lock is held from the final Git observation
// through the live transition, so restores and creates cannot both pass while
// both still look archived or not yet published. admissionWorktreePath is the
// one source this restore will use. An archived caller passes the path selected
// by its identity-qualified relocation claim; the unselected recovery alternate
// is deliberately not branch authority.
func (m *Manager) reserveLocalRestoreBranch(
	repoID, title string,
	instance *session.Instance,
	requireRegistered bool,
	admissionWorktreePath string,
) (func(), error) {
	row := instance.ToInstanceData()
	repoPath := strings.TrimSpace(row.Worktree.RepoPath)
	worktreePath := strings.TrimSpace(admissionWorktreePath)
	if repoPath == "" || worktreePath == "" {
		return nil, fmt.Errorf("cannot restore session %q: its repository or worktree identity is missing, so af cannot verify branch ownership", title)
	}
	canonicalRepo, err := config.RepoFromPath(repoPath)
	if err != nil {
		return nil, fmt.Errorf("cannot restore session %q: af could not establish the repository identity needed to serialize worktree admission; nothing was moved: %w", title, err)
	}

	initial, err := worktreeBranchBindings(repoPath)
	if err != nil {
		return nil, fmt.Errorf("cannot restore session %q: could not inspect worktree branch bindings; nothing was moved: %w", title, err)
	}
	branch, registered, err := restoreCandidateBranch(row, initial, worktreePath)
	if err != nil {
		return nil, fmt.Errorf("cannot restore session %q: %w", title, err)
	}
	if !registered && requireRegistered {
		return nil, fmt.Errorf("cannot restore session %q: its worktree %s is absent from Git's registration, so af cannot establish the branch it would activate; nothing was moved", title, config.ShellQuotePath(worktreePath))
	}
	// repoID is the persisted inventory key and may survive a repository rename.
	// Admission is a property of the physical Git repository, so restores and
	// creates must rendezvous on the identity Git resolves now, not on whichever
	// historical key addresses this row.
	lock := m.worktreeAdmissionLockForRepo(canonicalRepo.ID)
	lock.Lock()
	release := lock.Unlock
	freshRepo, err := config.RepoFromPath(repoPath)
	if err != nil {
		release()
		return nil, fmt.Errorf("cannot restore session %q: af could not revalidate the repository identity under worktree admission; nothing was moved: %w", title, err)
	}
	if freshRepo.ID != canonicalRepo.ID {
		release()
		return nil, fmt.Errorf("cannot restore session %q: its repository identity changed during worktree admission; retry after it settles", title)
	}
	fresh, err := worktreeBranchBindings(repoPath)
	if err != nil {
		release()
		return nil, fmt.Errorf("cannot restore session %q: could not revalidate worktree branch bindings; nothing was moved: %w", title, err)
	}
	freshBranch, freshRegistered, err := restoreCandidateBranch(row, fresh, worktreePath)
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
		return nil, fmt.Errorf("cannot restore session %q: could not read the persisted lane inventory needed to verify worktree ownership: %w", title, err)
	}
	m.mu.Lock()
	current := m.instances[daemonInstanceKey(repoID, title)]
	if lanes := m.liveLanesHoldingWorktreeExceptLocked(worktreePath, instance, diskData); len(lanes) > 0 {
		m.mu.Unlock()
		release()
		lane := lanes[0]
		handoff := shellsuggest.PositionalCommand("af", []string{"sessions", "handoff", "--to", "<agent>"}, lane)
		if branch == "" {
			return nil, fmt.Errorf("cannot restore session %q: its detached HEAD worktree at %s is already used by live lane %q. Restoring could move the checkout out from under that lane or start another agent in its files and index, so continue there with `%s`; af did not rename, detach, reset, or move either worktree",
				title, config.ShellQuotePath(worktreePath), lane, handoff)
		}
		return nil, fmt.Errorf("cannot restore session %q: its worktree at %s is already used by live lane %q. Restoring could move the checkout out from under that lane or start another agent in its files and index, so continue there with `%s`; af did not rename, detach, reset, or move either worktree",
			title, config.ShellQuotePath(worktreePath), lane, handoff)
	}
	for _, binding := range fresh {
		if sameWorktreePath(binding.Path, worktreePath) || branch == "" || binding.Branch != branch {
			continue
		}
		if lanes := m.liveLanesHoldingWorktreeExceptLocked(binding.Path, instance, diskData); len(lanes) > 0 {
			m.mu.Unlock()
			release()
			lane := lanes[0]
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

// liveLanesHoldingWorktreeExceptLocked returns every live owner of holder across
// the complete manager roster except the exact instance being restored. A
// repository rename can leave that owner under a historical map key, and a
// retained worktree can already be shared by an archived lane and a live --here
// lane, so neither the map key nor path equality can identify the restoring
// owner; only pointer/stable-ID identity can exclude it safely.
func (m *Manager) liveLanesHoldingWorktreeExceptLocked(holder string, restoring *session.Instance, diskData []session.InstanceData) []string {
	target := pathutil.ResolveForCompare(holder)
	if target == "" {
		return nil
	}
	seen := make(map[string]struct{})
	lanes := make([]string, 0)
	add := func(title, id string) {
		key := "title:" + title
		if id != "" {
			key = "id:" + id
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		lanes = append(lanes, title)
	}
	for _, candidate := range m.instances {
		if candidate == nil || candidate == restoring || candidate.IsArchived() || pathutil.ResolveForCompare(candidate.GetWorktreePath()) != target {
			continue
		}
		add(candidate.Title, candidate.ID)
	}
	for _, data := range diskData {
		if session.IsArchivedData(data) || pathutil.ResolveForCompare(data.Worktree.WorktreePath) != target {
			continue
		}
		if restoring != nil && data.ID != "" && data.ID == restoring.ID {
			continue
		}
		add(data.Title, data.ID)
	}
	sort.Strings(lanes)
	return lanes
}

func restoreCandidateBranch(
	row session.InstanceData,
	bindings []sessiongit.WorktreeBranchBinding,
	worktreePath string,
) (branch string, registered bool, err error) {
	for _, binding := range bindings {
		if sameWorktreePath(binding.Path, worktreePath) {
			switch {
			case binding.HeadSHA == "":
				return "", true, fmt.Errorf("worktree %s has no observed HEAD, so its active branch is unknown", config.ShellQuotePath(worktreePath))
			case binding.Branch != "":
				return binding.Branch, true, nil
			case binding.Detached:
				return "", true, nil
			default:
				return "", true, fmt.Errorf("worktree %s has neither a branch nor a detached-HEAD marker, so its active branch is unknown", config.ShellQuotePath(worktreePath))
			}
		}
	}
	if _, statErr := os.Stat(worktreePath); statErr == nil {
		// A bounded relocation can move the directory before `git worktree
		// repair` completes. In that state the repository-wide listing still
		// names the now-absent old path, but the selected directory's own .git
		// link remains authoritative. Probe that exact identity-qualified path;
		// never borrow branch metadata from the unselected recovery alternate.
		branch, detached, directErr := sessiongit.WorktreeBranchAtPath(worktreePath)
		switch {
		case directErr != nil:
			return "", false, fmt.Errorf("worktree %s exists but its active branch could not be established directly: %w", config.ShellQuotePath(worktreePath), directErr)
		case branch != "":
			return branch, true, nil
		case detached:
			return "", true, nil
		default:
			return "", false, fmt.Errorf("worktree %s has neither an attached branch nor a detached-HEAD marker; worktree safety is unknown", config.ShellQuotePath(worktreePath))
		}
	} else if !os.IsNotExist(statErr) {
		return "", false, fmt.Errorf("worktree %s could not be identified: %w", config.ShellQuotePath(worktreePath), statErr)
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

package daemon

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/sachiniyer/agent-factory/session"
)

type killWorkStatus struct {
	branch           string
	commitsNotOnBase int
	comparisonLabel  string
	uncommitted      bool
	verifyErr        error
}

func (s killWorkStatus) hasRecoverableWork() bool {
	return s.commitsNotOnBase > 0 || s.uncommitted || s.verifyErr != nil
}

func (s killWorkStatus) refusal(title string) error {
	details := make([]string, 0, 3)
	if s.commitsNotOnBase > 0 {
		label := s.comparisonLabel
		if label == "" {
			label = "base"
		}
		details = append(details, fmt.Sprintf("%d commits not on %s", s.commitsNotOnBase, label))
	}
	if s.uncommitted {
		details = append(details, "uncommitted changes")
	}
	if s.verifyErr != nil {
		if len(details) == 0 {
			return fmt.Errorf("session %s may have unmerged work on branch %s (could not verify safe to destroy: %v). Keep it (restorable): af sessions archive %s. Destroy anyway: af sessions kill %s --force",
				title, s.branch, s.verifyErr, title, title)
		}
		details = append(details, fmt.Sprintf("could not verify safe to destroy: %v", s.verifyErr))
	}
	return fmt.Errorf("session %s has unmerged work on branch %s (%s). Keep it (restorable): af sessions archive %s. Destroy anyway: af sessions kill %s --force",
		title, s.branch, strings.Join(details, "; "), title, title)
}

func guardKillRecoverableWork(title string, instance *session.Instance, data *session.InstanceData) error {
	info, ok := killGuardInfo(instance, data)
	if !ok {
		return nil
	}
	status := inspectKillWork(info)
	if !status.hasRecoverableWork() {
		return nil
	}
	return status.refusal(title)
}

type killGuardWorktree struct {
	repoPath     string
	worktreePath string
	branch       string
}

func killGuardInfo(instance *session.Instance, data *session.InstanceData) (killGuardWorktree, bool) {
	if instance != nil {
		if instance.IsRemote() || instance.IsExternalWorktree() {
			return killGuardWorktree{}, false
		}
		d := instance.ToInstanceData()
		return killGuardInfoFromData(d)
	}
	if data == nil {
		return killGuardWorktree{}, false
	}
	return killGuardInfoFromData(*data)
}

func killGuardInfoFromData(data session.InstanceData) (killGuardWorktree, bool) {
	if data.BackendType == "remote" || data.Worktree.ExternalWorktree {
		return killGuardWorktree{}, false
	}
	branch := strings.TrimSpace(data.Worktree.BranchName)
	if branch == "" {
		branch = strings.TrimSpace(data.Branch)
	}
	info := killGuardWorktree{
		repoPath:     strings.TrimSpace(data.Worktree.RepoPath),
		worktreePath: strings.TrimSpace(data.Worktree.WorktreePath),
		branch:       branch,
	}
	if info.repoPath == "" && info.worktreePath == "" && info.branch == "" {
		return killGuardWorktree{}, false
	}
	return info, true
}

func inspectKillWork(info killGuardWorktree) killWorkStatus {
	status := killWorkStatus{branch: info.branch}
	if status.branch == "" {
		status.branch = "(unknown)"
	}

	head, checkedOutBranch, detached, headOK, err := worktreeHeadState(info.worktreePath)
	if err != nil {
		status.verifyErr = err
		return status
	}
	if checkedOutBranch != "" {
		status.branch = checkedOutBranch
	}
	if detached && head != "" {
		status.branch = "detached HEAD " + shortSHA(head)
	}
	var pendingVerifyErr error
	if checkedOutBranch != "" && info.branch != "" && info.branch != "HEAD" && checkedOutBranch != info.branch {
		pendingVerifyErr = fmt.Errorf("worktree is on branch %q but the stored session branch is %q", checkedOutBranch, info.branch)
	}

	if dirty, err := worktreeHasUncommittedChanges(info.worktreePath); err != nil {
		status.verifyErr = err
		return status
	} else {
		status.uncommitted = dirty
	}

	if pendingVerifyErr != nil {
		status.verifyErr = pendingVerifyErr
		return status
	}
	if strings.TrimSpace(info.branch) == "" || strings.TrimSpace(info.branch) == "HEAD" {
		status.verifyErr = fmt.Errorf("stored branch name is %q", info.branch)
		return status
	}

	if !headOK {
		status.verifyErr = fmt.Errorf("worktree HEAD is unavailable")
		return status
	}

	if _, err := runLocalGit(info.repoPath, "rev-parse", "--git-dir"); err != nil {
		status.verifyErr = err
		return status
	}

	baseRef, label, err := killComparisonBase(info.repoPath)
	if err != nil {
		status.verifyErr = err
		return status
	}
	count, err := revListCount(info.repoPath, baseRef+".."+head)
	if err != nil {
		status.verifyErr = err
		return status
	}
	status.commitsNotOnBase = count
	status.comparisonLabel = label

	if detached {
		status.verifyErr = fmt.Errorf("worktree HEAD is detached")
	}

	return status
}

func worktreeHeadState(worktreePath string) (head string, branch string, detached bool, ok bool, err error) {
	if worktreePath == "" {
		return "", "", false, false, nil
	}
	if stat, statErr := os.Stat(worktreePath); statErr != nil {
		if os.IsNotExist(statErr) {
			return "", "", false, false, nil
		}
		return "", "", false, false, statErr
	} else if !stat.IsDir() {
		return "", "", false, false, fmt.Errorf("worktree path %s is not a directory", worktreePath)
	}

	headOut, err := runLocalGit(worktreePath, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", "", false, false, err
	}
	head = strings.TrimSpace(headOut)

	out, err := runLocalGit(worktreePath, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err == nil {
		return head, strings.TrimSpace(out), false, true, nil
	}
	if _, gitErr := runLocalGit(worktreePath, "rev-parse", "--git-dir"); gitErr == nil {
		return head, "", true, true, nil
	}
	return "", "", false, false, err
}

func worktreeHasUncommittedChanges(worktreePath string) (bool, error) {
	if worktreePath == "" {
		return false, nil
	}
	if stat, err := os.Stat(worktreePath); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	} else if !stat.IsDir() {
		return false, fmt.Errorf("worktree path %s is not a directory", worktreePath)
	}
	out, err := runLocalGit(worktreePath, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

func revListCount(repoPath, revRange string) (int, error) {
	out, err := runLocalGit(repoPath, "rev-list", "--count", revRange)
	if err != nil {
		return 0, err
	}
	count, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("git rev-list returned non-numeric count %q: %w", strings.TrimSpace(out), err)
	}
	return count, nil
}

func shortSHA(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}

func killComparisonBase(repoPath string) (rev string, label string, err error) {
	if defaultRef, ok := originDefaultRef(repoPath); ok {
		return defaultRef, strings.TrimPrefix(defaultRef, "origin/"), nil
	}
	if defaultRef, ok := configuredDefaultRef(repoPath); ok {
		return defaultRef.rev, defaultRef.label, nil
	}
	if defaultRef, ok := masterDefaultRef(repoPath); ok {
		return defaultRef.rev, defaultRef.label, nil
	}
	// Intentional fail-closed behavior: stale or missing default refs may
	// over-refuse a safe kill, but that preserves work and --force remains the
	// explicit escape hatch.
	return "", "", fmt.Errorf("could not resolve origin/HEAD, configured default branch, or master")
}

func originDefaultRef(repoPath string) (string, bool) {
	out, err := runLocalGit(repoPath, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD")
	if err != nil {
		return "", false
	}
	ref := strings.TrimSpace(out)
	if ref == "" {
		return "", false
	}
	if _, err := runLocalGit(repoPath, "rev-parse", "--verify", ref+"^{commit}"); err != nil {
		return "", false
	}
	return ref, true
}

type defaultRef struct {
	rev   string
	label string
}

func configuredDefaultRef(repoPath string) (defaultRef, bool) {
	out, err := runLocalGit(repoPath, "config", "--get", "init.defaultBranch")
	if err != nil {
		return defaultRef{}, false
	}
	name := strings.TrimSpace(out)
	if name == "" {
		return defaultRef{}, false
	}
	return namedDefaultRef(repoPath, name)
}

func masterDefaultRef(repoPath string) (defaultRef, bool) {
	return namedDefaultRef(repoPath, "master")
}

func namedDefaultRef(repoPath, name string) (defaultRef, bool) {
	if ref, ok := verifiedDefaultRef(repoPath, "origin/"+name, name); ok {
		return ref, true
	}
	return verifiedDefaultRef(repoPath, "refs/heads/"+name, name)
}

func verifiedDefaultRef(repoPath, rev, label string) (defaultRef, bool) {
	if _, err := runLocalGit(repoPath, "rev-parse", "--verify", rev+"^{commit}"); err != nil {
		return defaultRef{}, false
	}
	return defaultRef{rev: rev, label: label}, true
}

func runLocalGit(path string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", path}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return string(out), fmt.Errorf("git %s failed: %s", strings.Join(args, " "), msg)
	}
	return string(out), nil
}

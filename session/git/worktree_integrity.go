package git

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// massRevertPathThreshold is deliberately narrow. A large unstaged edit is
// ordinary work; the dangerous #4092 shape is a populated index that differs
// from HEAD while the worktree still agrees with that index.
const massRevertPathThreshold = 20

// worktreeIntegrityTimeout bounds each read-only probe. These checks run from
// the daemon diagnostic loop and doctor, so a stalled checkout must not stall
// either caller indefinitely.
var worktreeIntegrityTimeout = 10 * time.Second

// WorktreeIntegrity is the read-only evidence collected from one checkout.
type WorktreeIntegrity struct {
	Branch                 string
	HeadSHA                string
	ReflogHeadSHA          string
	StagedPaths            int
	UnstagedPaths          int
	MassRevert             bool
	HeadMovedWithoutReflog bool
}

// InspectWorktreeIntegrity detects the two independent #4092 signals without
// changing the index, files, refs, or worktree registration.
func InspectWorktreeIntegrity(worktreePath string) (WorktreeIntegrity, error) {
	return InspectWorktreeIntegrityContext(context.Background(), worktreePath)
}

// InspectWorktreeIntegrityContext is InspectWorktreeIntegrity with caller
// cancellation, used so daemon shutdown does not wait for a diagnostic probe.
func InspectWorktreeIntegrityContext(ctx context.Context, worktreePath string) (WorktreeIntegrity, error) {
	if strings.TrimSpace(worktreePath) == "" {
		return WorktreeIntegrity{}, fmt.Errorf("cannot inspect worktree integrity: path is empty")
	}
	statusArgs := []string{"status", "--porcelain=v2", "--branch", "--untracked-files=no"}
	status, err := runIntegrityGit(ctx, worktreePath, statusArgs...)
	if err != nil {
		return WorktreeIntegrity{}, err
	}
	result, err := parseIntegrityStatus(status)
	if err != nil {
		return WorktreeIntegrity{}, fmt.Errorf("git status in %s could not be read: %w", worktreePath, err)
	}
	reflog, err := runIntegrityGit(ctx, worktreePath, "log", "-g", "-1", "--format=%H", "HEAD")
	if err != nil {
		return WorktreeIntegrity{}, err
	}
	result.ReflogHeadSHA = strings.TrimSpace(reflog)
	if result.ReflogHeadSHA == "" {
		return WorktreeIntegrity{}, fmt.Errorf("git HEAD reflog in %s is empty; worktree safety is unknown", worktreePath)
	}
	confirmedStatus, err := runIntegrityGit(ctx, worktreePath, statusArgs...)
	if err != nil {
		return WorktreeIntegrity{}, err
	}
	if confirmedStatus != status {
		return WorktreeIntegrity{}, fmt.Errorf("git status in %s changed during the worktree safety scan; worktree safety is unknown", worktreePath)
	}
	result.MassRevert = result.StagedPaths > massRevertPathThreshold && result.UnstagedPaths == 0
	result.HeadMovedWithoutReflog = result.HeadSHA != result.ReflogHeadSHA
	return result, nil
}

// RevalidateWorktreeIntegrityContext confirms that every Git observation used
// by a completed integrity result still agrees after its repository peers were
// inspected. A correlated clean result cannot be trusted when one checkout
// changed between its own probe window and the cohort-wide correlation.
func RevalidateWorktreeIntegrityContext(ctx context.Context, worktreePath string, observed WorktreeIntegrity) error {
	current, err := InspectWorktreeIntegrityContext(ctx, worktreePath)
	if err != nil {
		return fmt.Errorf("revalidate Git worktree observation: %w", err)
	}
	if current != observed {
		return fmt.Errorf("git worktree observation changed before branch correlation; worktree safety is unknown")
	}
	return nil
}

func parseIntegrityStatus(output string) (WorktreeIntegrity, error) {
	var result WorktreeIntegrity
	branchObserved := false
	for _, line := range strings.Split(output, "\n") {
		switch {
		case strings.HasPrefix(line, "# branch.oid "):
			result.HeadSHA = strings.TrimSpace(strings.TrimPrefix(line, "# branch.oid "))
		case strings.HasPrefix(line, "# branch.head "):
			branchObserved = true
			result.Branch = strings.TrimSpace(strings.TrimPrefix(line, "# branch.head "))
			if result.Branch == "(detached)" {
				result.Branch = ""
			} else if result.Branch == "" {
				return WorktreeIntegrity{}, fmt.Errorf("status contained an empty branch observation")
			}
		case strings.HasPrefix(line, "1 "), strings.HasPrefix(line, "2 "), strings.HasPrefix(line, "u "):
			if len(line) < 4 {
				return WorktreeIntegrity{}, fmt.Errorf("truncated tracked-path record %q", line)
			}
			if line[2] != '.' {
				result.StagedPaths++
			}
			if line[3] != '.' {
				result.UnstagedPaths++
			}
		}
	}
	if result.HeadSHA == "" || result.HeadSHA == "(initial)" {
		return WorktreeIntegrity{}, fmt.Errorf("status omitted a committed HEAD oid")
	}
	if !branchObserved {
		return WorktreeIntegrity{}, fmt.Errorf("status omitted branch metadata")
	}
	return result, nil
}

func runIntegrityGit(parent context.Context, worktreePath string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, worktreeIntegrityTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", worktreePath}, args...)...)
	cmd.Env = append(repoGoneGitCommandEnvironment(), "GIT_OPTIONAL_LOCKS=0", "GIT_PAGER=cat")
	isolateGitCommandTree(cmd)
	cmd.WaitDelay = gitWaitDelay
	output, err := cmd.Output()
	terminateGitCommandTree(cmd)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("git %s in %s timed out after %s: %w", strings.Join(args, " "), worktreePath, worktreeIntegrityTimeout, ctx.Err())
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return "", fmt.Errorf("git %s in %s canceled: %w", strings.Join(args, " "), worktreePath, ctx.Err())
		}
		// Git itself completed successfully and only a child held its output pipe;
		// the captured bytes are complete. Callers still validate every required
		// status/reflog field, so empty output never becomes a clean observation.
		if errors.Is(err, exec.ErrWaitDelay) {
			return string(output), nil
		}
		if detail := commandFailureDetail(err); detail != "" {
			return "", fmt.Errorf("git %s in %s failed: %s (%w)", strings.Join(args, " "), worktreePath, detail, err)
		}
		return "", fmt.Errorf("git %s in %s failed: %w", strings.Join(args, " "), worktreePath, err)
	}
	return string(output), nil
}

package git

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// LocalBranchExists reports whether `branch` exists as a local ref in the repo
// at `repoRoot`.
//
// The answer is DETERMINATE only when err is nil: (true, nil) means the ref
// verifiably exists, (false, nil) means it verifiably does not. A non-nil error
// means existence could not be established — UNKNOWN, not absent (#3243). A
// destructive caller that reads a failed probe as "absent" deletes the cleanup
// record that is its only durable pointer to the branch, so callers must treat
// the error as "leave the branch and its record alone".
//
// Determinate absence covers three observations:
//
//   - empty arguments name no branch to act on;
//   - the repo root does not exist — no repo, no local branch;
//   - the root exists but has no .git AND is not a bare metadata layout — the
//     repo was deleted out from under the record, taking its refs with it. A
//     BARE repository legitimately carries no .git child (its metadata — HEAD,
//     objects/, refs/ — lives directly at the root), so a missing .git alone is
//     not absence: the bare layout is recognized by bareMetadataDirectory (the
//     same structural check resolveRepoMetadataRoot uses on this path) and
//     falls through to the show-ref probe rather than fabricating absence for
//     a branch that verifiably exists (#5136: af sessions prune over a
//     bare-clone origin; a bare root is a first-class project identity — see
//     config.ResolveMainRepoRoot).
//
// Both filesystem observations are DIRECT, the same rule repoRegistersNothing
// applies for worktrees (#2110): "git errored, so there must be no repo" is
// exactly the inference this function exists to remove. Settling the de-git'd
// case here also keeps `git -C` from discovering an ENCLOSING repo upward of the
// recorded root and answering — or worse, letting the caller's `git branch
// -D` act — on a same-named branch AF never created.
//
// Everything else is probed with `git show-ref --verify --quiet`, whose exit
// surface git keeps MOSTLY distinct: exit 0 is "exists", a silent exit 1 is
// "no such ref", and operational failures (unreadable packed-refs, corrupt
// metadata, failed discovery) die loudly with another status. The one known
// fold is an unreadable LOOSE ref file, which some git versions report with
// the same silent exit 1 as a missing ref (git ≥2.45 grew `show-ref --exists`
// to separate them, but AF must hold on older gits too) — so a silent exit 1
// is confirmed by direct observation before it counts as absence: no loose
// ref file may exist at the branch's path under the repo's COMMON git dir,
// resolved via `git rev-parse --git-common-dir` because a gitfile root (a
// linked worktree, or --separate-git-dir) keeps its refs elsewhere than
// <root>/.git. A directory at the branch's path is fine (a stale hierarchy
// dir left by deleting a/b can never be a loose ref), and ENOENT/ENOTDIR is
// consistent with absence (no loose file; the reftable stub makes refs/heads
// itself a file); a present file, a failed stat, or a failed common-dir
// resolution returns the error.
func LocalBranchExists(repoRoot, branch string) (bool, error) {
	if repoRoot == "" || branch == "" {
		return false, nil
	}
	if _, err := os.Stat(repoRoot); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat repo %s: %w", repoRoot, err)
	}
	// .git is a directory in a main worktree and a file in a linked one; Stat
	// accepts either. A bare repository has NO .git child — its metadata lives
	// directly at the root (HEAD, objects/, refs/) — so a missing .git is
	// determinate absence only when the root is not a bare layout; otherwise
	// fall through to the show-ref probe (bareMetadataDirectory is the same
	// structural check resolveRepoMetadataRoot uses on this path).
	//
	// bareMetadataDirectory is STRUCTURAL: it proves the layout git clones
	// produce, not that git accepts this directory as a bare repo. A de-git'd
	// directory that happens to carry HEAD/config/objects/refs would satisfy
	// it, and `git -C repoRoot` could then walk upward to discover an
	// ENCLOSING repo and answer — or let the caller's `git branch -D` act — on
	// a same-named branch AF never created. Pin Git to repoRoot with GIT_DIR
	// on the fall-through so discovery cannot escape it; a directory git does
	// not accept as bare then errors (a failed probe, never absence) instead of
	// reaching a parent.
	gitPath := filepath.Join(repoRoot, ".git")
	bareProbe := false
	if _, err := os.Stat(gitPath); err != nil {
		if !os.IsNotExist(err) {
			return false, fmt.Errorf("stat %s: %w", gitPath, err)
		}
		if !bareMetadataDirectory(repoRoot) {
			return false, nil
		}
		// Bare origin: refs live at the root; fall through to show-ref.
		bareProbe = true
	}
	// Repository-selector variables (GIT_DIR, GIT_WORK_TREE, …) inherited from
	// a daemon launched inside a hook would redirect the probe at a foreign
	// repository — the same reason the bounded runner sanitizes its env
	// (worktree_remove_bounded.go). A foreign repo coincidentally holding the
	// branch would fabricate a recovery handle that does not exist here.
	repoEnv := append(repositoryPathEnvironment(os.Environ()), "GIT_TERMINAL_PROMPT=0")
	if bareProbe {
		// repositoryPathEnvironment stripped any inherited GIT_DIR; pin Git to
		// repoRoot so the bare fall-through probes exactly this directory and
		// cannot discover an enclosing repo.
		repoEnv = append(repoEnv, "GIT_DIR="+repoRoot)
	}
	check := exec.Command("git", "-C", repoRoot, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	check.Env = repoEnv
	var stderr bytes.Buffer
	check.Stderr = &stderr
	err := check.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && stderr.Len() == 0 {
		// The loose ref lives under the COMMON git dir, which is <root>/.git only
		// when that is a directory. When .git is a gitfile — a linked worktree, or
		// a main worktree made with --separate-git-dir — the refs live wherever it
		// (and, for a linked worktree, its commondir indirection) points, so an
		// Lstat under the gitfile would report ENOTDIR for every branch and
		// "confirm" any silent exit 1 as absence. Let git resolve the indirection;
		// a failed resolution is itself a failed probe, never absence.
		common := exec.Command("git", "-C", repoRoot, "rev-parse", "--git-common-dir")
		common.Env = repoEnv
		var commonOut, commonErr bytes.Buffer
		common.Stdout = &commonOut
		common.Stderr = &commonErr
		if err := common.Run(); err != nil {
			return false, fmt.Errorf("git rev-parse --git-common-dir in %s: %w: %s",
				repoRoot, err, strings.TrimSpace(commonErr.String()))
		}
		// Strip ONLY the terminating newline: a --separate-git-dir path may
		// legitimately end in whitespace, and TrimSpace would point the loose-ref
		// observation at a nonexistent trimmed path, whose ENOENT then fabricates
		// exactly the confirmed absence this arm exists to prevent.
		commonDir := strings.TrimSuffix(commonOut.String(), "\n")
		if !filepath.IsAbs(commonDir) {
			// Relative output is relative to the -C directory.
			commonDir = filepath.Join(repoRoot, commonDir)
		}
		loose := filepath.Join(commonDir, "refs", "heads", filepath.FromSlash(branch))
		fi, statErr := os.Lstat(loose)
		switch {
		case statErr == nil && fi.IsDir():
			return false, nil
		case statErr == nil:
			return false, fmt.Errorf("git reports no ref refs/heads/%s in %s, but a loose ref file is present at %s and could not be read as a ref", branch, repoRoot, loose)
		case os.IsNotExist(statErr) || errors.Is(statErr, syscall.ENOTDIR):
			return false, nil
		default:
			return false, fmt.Errorf("stat %s: %w", loose, statErr)
		}
	}
	return false, fmt.Errorf("git show-ref refs/heads/%s in %s: %w: %s",
		branch, repoRoot, err, strings.TrimSpace(stderr.String()))
}

// DeleteLocalBranch force-deletes the local branch `branch` in the repo at
// `repoRoot`, but only if it verifiably exists. It reports whether a branch was
// actually deleted.
//
// It is the SOLE branch-deletion path of the factory reset (`af reset`, #1736):
// reset removes worktree directories via RemoveWorktreeDir, which deletes no
// branches, and then deletes branches ONLY through here. The caller
// enumerates exactly the branches AF created for its own sessions — live and
// archived — gated on GitWorktreeData.BranchCreatedByUs, so the user's own
// branches (master/main/their feature branches, and any branch a session merely
// reused) are never touched, even for a session whose worktree was still
// registered with git.
//
// A DETERMINATELY missing branch is not an error (idempotent: a second
// `af reset` is a clean no-op), and neither is a missing or de-git'd repo path —
// there is simply nothing left to prune. But a failed existence probe is
// returned as an error, never read as absence (#3243): the caller holds the
// only durable record naming this branch, and must retain it whenever the
// branch could not be confirmed gone. `git branch -D` force-deletes regardless
// of merge state, which is intended: AF session branches may be unmerged work
// the reset is deliberately discarding.
func DeleteLocalBranch(repoRoot, branch string) (bool, error) {
	exists, err := LocalBranchExists(repoRoot, branch)
	if err != nil {
		return false, fmt.Errorf("cannot establish whether branch %s exists in %s: %w", branch, repoRoot, err)
	}
	if !exists {
		return false, nil
	}
	// Sanitize the delete's environment to match the probe above: an ambient
	// GIT_DIR/GIT_COMMON_DIR inherited from a hook would override `-C` and
	// redirect this destructive command at a foreign repository whose
	// same-named branch the probe never inspected — silently deleting a branch
	// AF never created while the intended branch survives and (true, nil) is
	// still reported (cf. worktree_remove_bounded.go). The probe's
	// sanitized-env verdict is only meaningful if the delete runs under the
	// same environment that produced it.
	del := exec.Command("git", "-C", repoRoot, "branch", "-D", branch)
	del.Env = append(repositoryPathEnvironment(os.Environ()), "GIT_TERMINAL_PROMPT=0")
	if out, err := del.CombinedOutput(); err != nil {
		return false, fmt.Errorf("git branch -D %s in %s: %w: %s", branch, repoRoot, err, string(out))
	}
	return true, nil
}

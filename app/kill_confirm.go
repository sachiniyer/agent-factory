package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// killGitTimeout bounds the local git metadata reads the kill confirmation runs.
// handleKill is synchronous on the Bubble Tea Update loop, so a wedged git (a
// hung network mount, a D-state process holding the worktree) would freeze the
// whole TUI without a deadline (#2030). The reads are local and offline (status,
// log, rev-parse, symbolic-ref), so this only trips on a genuine stall; it is
// generous enough that a slow-but-progressing read on a cold cache still
// completes. Mirrors session/git's localGitTimeout reasoning (#1917).
//
// A var (not a const) only so tests can shorten it; production never reassigns.
var killGitTimeout = 5 * time.Second

// killGitWaitDelay bounds how long cmd.Wait blocks after git exits or is killed on
// the deadline, before the inherited pipes are force-closed. A child that inherited
// the capture pipe would otherwise block Output() on pipe EOF past the deadline and
// defeat it (the #856/#1967 lesson). Mirrors gitWaitDelay.
const killGitWaitDelay = 2 * time.Second

// runKillGit runs `git -C dir args...` bounded by killGitTimeout, in its own
// process group so the deadline tears down git and any child it spawned together,
// with a WaitDelay so a straggler holding the capture pipe cannot outlast the
// bound. It mirrors session/git's runGitCommandContext (#856/#896/#1967),
// reproduced here because that runner is a private method on *GitWorktree in
// another package and these callers operate on a bare worktree path.
func runKillGit(dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), killGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	// Own process group so the deadline kills git AND any child together, rather
	// than exec.CommandContext's default of SIGKILLing only the git process.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Negative pid targets the whole group; a group already gone (ESRCH) maps to
		// os.ErrProcessDone, which Wait ignores rather than reporting as a failure.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	cmd.WaitDelay = killGitWaitDelay
	out, err := cmd.Output()
	if cmd.Process != nil {
		// Reap any child that outlived git on every exit path so a wedged read never
		// leaks a pipe-holding process; the group is led by the already-exited git,
		// so this is ESRCH (ignored) in the common case.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		// git itself exited (a non-zero exit surfaces as an *exec.ExitError, not
		// ErrWaitDelay); only a child held the pipe past killGitWaitDelay and was
		// just reaped. The output is complete, so this is not a failure (#676/#914).
		err = nil
	}
	return out, err
}

// This file holds the kill-confirmation copy and the data-loss detection behind
// it. handleKill (handle_actions.go) assembles these into the confirmation the
// user consents to. The governing rule (#2022): show the bare, safe-looking
// prompt ONLY with positive evidence there is no unmerged work to lose; unknown
// or unverifiable warns. Forgetting must be safe.

// rootKillConfirmKey is the confirm key the kill dialog demands for the reserved
// root agent (#1238). Deliberately NOT the ordinary 'y' so an inattentive D+y —
// the exact gesture that silently decapitated root's event pipeline on
// 2026-07-05 — cannot dispatch the kill; the key is surfaced in the rendered
// prompt ("Press k to confirm").
const rootKillConfirmKey = "k"

// unmergedKillConfirmKey is the confirm key the kill dialog demands when the
// session carries committed-but-unmerged-and-unpushed work (#2022). Like the
// reserved-root guard (#1238) it is deliberately NOT the ordinary 'y': killing
// force-deletes the branch with `git branch -D`, orphaning those commits for
// good, and the muscle-memory D+y must not dispatch that permanent loss. The
// user has to read the warning and press the named key surfaced in the prompt
// ("Press k to confirm"). It shares the root key's value on purpose — both mean
// "this kill is irreversible; press the named key".
const unmergedKillConfirmKey = "k"

// killConfirmMessage builds the kill-confirmation copy for a session. The
// reserved root agent (#1238) gets distinct, consequence-bearing copy instead of
// the generic "[!] Kill session 'root'?" that killing any throwaway worktree
// shows: killing root stops every scheduled/watch-task delivery to it (the
// inbound event pipeline) until it self-heals or the daemon is restarted. This
// mirrors the reserved-title guard the create path already applies
// (app/handle_input.go). #1237 made root self-heal ~2 min after a kill, so the
// copy names that recovery rather than the pre-#1237 "until the daemon restarts".
func killConfirmMessage(title, warning string, reserved bool) string {
	var message string
	if reserved {
		message = fmt.Sprintf(
			"[!] '%s' is the daemon-managed root agent, not a scratch session.\n"+
				"Killing it stops scheduled and watch-task delivery to '%s' until it\n"+
				"self-heals (~2 min) or you restart the daemon.\n\n"+
				"Kill the root agent anyway?", title, title)
	} else {
		message = fmt.Sprintf("[!] Kill session '%s'?", title)
	}
	if warning != "" {
		message += "\n\n" + warning
	}
	return message
}

// killConfirmationWarning returns the data-loss warning line for the kill
// confirmation dialog, or "" if the worktree at wt is verifiably clean. Kill
// tears the worktree down with `git worktree remove -f`, which bypasses git's
// own refusal to delete a dirty worktree, so this check is the only warning
// the user gets. If `git status` itself fails we cannot prove the worktree is
// clean — fail closed and warn that changes may be lost rather than silently
// skipping the warning (#815).
//
// --untracked-files=normal is load-bearing (#2101). Bare `git status --porcelain`
// honours status.showUntrackedFiles, and a session worktree shares .git/config
// with its main repo, so a user with that set to `no` (or inheriting it globally)
// got the bare, safe-looking "[!] Kill session 'x'?" prompt over a worktree full
// of untracked work — and D+y then fed it to `git worktree remove -f`. Whether
// the user gets a data-loss warning must not depend on their status preferences.
// `normal` rather than `all` because this is a boolean: `normal` collapses an
// untracked directory to a single `?? dir/` entry instead of walking every file
// under it, which matters on a path that runs synchronously on the Bubble Tea
// Update loop (#2030).
func killConfirmationWarning(wt string) string {
	out, err := runKillGit(wt, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		log.WarningLog.Printf("could not verify worktree status for %s before kill: %v", wt, err)
		return fmt.Sprintf("WARNING: Could not verify worktree status (%v); it may contain uncommitted changes that will be lost!", err)
	}
	if len(strings.TrimSpace(string(out))) > 0 {
		return "WARNING: This worktree has uncommitted changes that will be lost!"
	}
	return ""
}

// unmergedCommitWarning reports whether killing a session would permanently
// destroy committed work, and how loud the warning must be. Kill runs
// `git branch -D` (session/git/worktree_ops.go), which force-deletes the local
// branch regardless of merge state — so any commit on the branch that is not
// merged into its base AND not pushed to a remote is orphaned, recoverable only
// from the reflog until gc. That is the #2022 data-loss case; the bare
// "[!] Kill session 'x'?" prompt hid it entirely.
//
// It returns:
//   - (severe line, true) when the branch carries commits that exist ONLY
//     locally — unmerged and unpushed. This is unrecoverable, so the caller
//     escalates the confirmation to the distinct confirm key.
//   - (fail-closed line, false) when we cannot prove the branch is free of such
//     commits (base undeterminable, or a git command failed). Unknown is not
//     clean: we warn, mirroring the #815 fail-closed discipline for a dirty
//     worktree — but we do NOT escalate the key, because we have not established
//     that work is actually being lost.
//   - ("", false) when we have POSITIVE evidence there is nothing to lose: no
//     commits beyond base, or every such commit is already pushed to a remote or
//     carried by a merged PR (branch -D deletes only the LOCAL branch, so
//     pushed/merged commits survive the kill).
//
// The check is deliberately offline (no `git fetch`): a kill confirmation must
// stay as fast as the existing status check and is no place to touch the
// network. The cost is that a commit pushed since the last fetch, whose remote-
// tracking ref is stale, reads as local-only and over-warns. Over-warning is the
// conservative side — the loud path still kills on one keystroke — so we accept
// it rather than risk staying silent on genuine loss.
//
// branchName is the session's recorded branch — the exact ref Cleanup deletes —
// not whichever branch the agent has left checked out at HEAD. The fully
// qualified local ref must resolve before an empty warning can be returned;
// missing or unverifiable deletion targets fail closed (#2199).
func unmergedCommitWarning(worktreePath, branchName, recordedBaseSHA, prState string) (string, bool) {
	if strings.TrimSpace(worktreePath) == "" {
		return unmergedFailClosedLine(branchName, fmt.Errorf("no worktree path")), false
	}

	// Branch reachability and detached-HEAD reachability are independent. An
	// agent can detach from a clean recorded branch, create commits, and leave
	// those commits reachable only through the worktree's HEAD. Cleanup removes
	// that worktree, so a branch-only check would silently orphan them (#2210
	// review). Assess both and preserve every warning in the confirmation.
	branchLine, branchSevere := branchCommitWarning(worktreePath, branchName, recordedBaseSHA, prState)
	detachedLine, detachedSevere := detachedHeadCommitWarning(worktreePath)
	lines := make([]string, 0, 2)
	for _, line := range []string{branchLine, detachedLine} {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n\n"), branchSevere || detachedSevere
}

// branchCommitWarning is the recorded-branch half of
// unmergedCommitWarning. branchName is the GitWorktree's canonical branch —
// the exact ref Cleanup deletes — not Instance.Branch, which can be empty or
// stale on legacy/restored records (#2209 review).
func branchCommitWarning(worktreePath, branchName, recordedBaseSHA, prState string) (string, bool) {
	branchName = strings.TrimSpace(branchName)
	if branchName == "" {
		return unmergedFailClosedLine(branchName, fmt.Errorf("no session branch name")), false
	}
	branchRef := "refs/heads/" + branchName
	if _, err := runKillGit(worktreePath, "rev-parse", "--verify", "--quiet", branchRef+"^{commit}"); err != nil {
		return unmergedFailClosedLine(branchName, fmt.Errorf("session branch could not be resolved: %w", err)), false
	}
	base, baseLabel, ok := resolveKillBase(worktreePath, recordedBaseSHA)
	if !ok {
		return unmergedFailClosedLine(branchName, fmt.Errorf("base branch/commit could not be determined")), false
	}
	// Commits reachable from the recorded session branch but not from base: the
	// commits kill will orphan when Cleanup force-deletes that exact branch. An
	// agent may have checked out a different branch in the worktree, so HEAD is
	// deliberately not used here (#2199).
	uniqueOut, err := runKillGit(worktreePath, "log", "--oneline", base+".."+branchRef)
	if err != nil {
		return unmergedFailClosedLine(branchName, err), false
	}
	if countGitLines(uniqueOut) == 0 {
		return "", false // nothing beyond base — kill loses no committed work
	}
	// A merged PR means the work landed in the base's history (even a squash,
	// whose original commits branch -D would orphan, preserves the diff), so it
	// survives the kill.
	if strings.EqualFold(strings.TrimSpace(prState), "MERGED") {
		return "", false
	}
	// Of the session's commits, those NOT reachable from any remote-tracking ref
	// are the ones that exist only here. branch -D orphans exactly these.
	localOut, err := runKillGit(worktreePath, "log", "--oneline", base+".."+branchRef, "--not", "--remotes")
	if err != nil {
		return unmergedFailClosedLine(branchName, err), false
	}
	localOnly := countGitLines(localOut)
	if localOnly == 0 {
		return "", false // every commit is pushed somewhere — recoverable
	}
	return unmergedSevereLine(branchName, localOnly, baseLabel), true
}

// detachedHeadCommitWarning reports commits that would lose their final ref
// when the worktree is removed. `for-each-ref --contains=HEAD` asks the exact
// durability question across every ref namespace (branches, remotes, tags,
// stash, and custom refs) without guessing which namespaces a user relies on.
// Empty output while HEAD is detached proves its tip is unreferenced; at least
// that tip is then orphaned by cleanup even when the recorded branch is clean.
func detachedHeadCommitWarning(worktreePath string) (string, bool) {
	if _, err := runKillGit(worktreePath, "symbolic-ref", "--quiet", "HEAD"); err == nil {
		return "", false // attached HEAD: the branch check owns committed loss
	} else {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			return detachedHeadFailClosedLine(err), false
		}
	}
	if _, err := runKillGit(worktreePath, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err != nil {
		return detachedHeadFailClosedLine(fmt.Errorf("HEAD could not be resolved: %w", err)), false
	}
	refs, err := runKillGit(worktreePath, "for-each-ref", "--contains=HEAD", "--format=%(refname)")
	if err != nil {
		return detachedHeadFailClosedLine(err), false
	}
	if strings.TrimSpace(string(refs)) != "" {
		return "", false // some durable ref contains HEAD; removing the worktree preserves it
	}
	return "Detached HEAD points to one or more commits not reachable from any ref. " +
		"Killing removes the worktree and permanently orphans them · this cannot be undone.", true
}

func detachedHeadFailClosedLine(err error) string {
	return fmt.Sprintf("Could not verify whether detached HEAD contains local-only commits (%v); "+
		"any unreferenced commits would be permanently orphaned when the worktree is removed.", err)
}

// resolveKillBase resolves the commit the branch is measured against for the
// unmerged-work check, offline. It mirrors the base resolution the worktree code
// already uses (session/git/worktree_ops.go): the recorded base commit first,
// then origin's default branch. It deliberately does NOT fall back to HEAD:
// HEAD may be the recorded branch tip (which would fabricate a zero-commits
// result) or an unrelated branch the agent checked out. When neither resolves,
// ok is false and the caller fails closed. baseLabel names the base branch when
// known (for the copy), else "".
func resolveKillBase(worktreePath, recordedBaseSHA string) (base, baseLabel string, ok bool) {
	commitExists := func(rev string) bool {
		_, err := runKillGit(worktreePath, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
		return err == nil
	}
	if b := strings.TrimSpace(recordedBaseSHA); b != "" && commitExists(b) {
		return b, "", true
	}
	// origin/HEAD (symbolic ref to origin's default branch), then the usual names.
	if out, err := runKillGit(worktreePath, "symbolic-ref", "--short", "-q", "refs/remotes/origin/HEAD"); err == nil {
		if ref := strings.TrimSpace(string(out)); ref != "" && commitExists(ref) {
			return ref, strings.TrimPrefix(ref, "origin/"), true
		}
	}
	for _, name := range []string{"main", "master"} {
		if commitExists("origin/" + name) {
			return "origin/" + name, name, true
		}
	}
	return "", "", false
}

// unmergedSevereLine is the #2022 data-loss headline: it names the exact branch
// and count of commits that would be permanently deleted and that the loss is
// final. It is the critical (never-clipped, #1973) line of the confirmation.
func unmergedSevereLine(branchName string, n int, baseLabel string) string {
	commitWord, pronoun := "commits", "them"
	where := "that aren't merged or pushed anywhere"
	if n == 1 {
		commitWord, pronoun = "commit", "it"
		where = "that isn't merged or pushed anywhere"
	}
	if baseLabel != "" {
		where = fmt.Sprintf("not on %s and not pushed anywhere", baseLabel)
	}
	return fmt.Sprintf("Branch %q has %d %s %s. Killing permanently deletes %s · this cannot be undone.", branchName, n, commitWord, where, pronoun)
}

// unmergedFailClosedLine mirrors the #815 could-not-verify warning for the
// committed-work check: when we cannot prove the branch is free of local-only
// commits, we say so rather than showing the bare, safe-looking prompt.
func unmergedFailClosedLine(branchName string, err error) string {
	branchLabel := "the session branch"
	if branchName = strings.TrimSpace(branchName); branchName != "" {
		branchLabel = fmt.Sprintf("session branch %q", branchName)
	}
	return fmt.Sprintf("Could not verify whether %s has unmerged commits (%v); any that exist only here would be permanently deleted.", branchLabel, err)
}

// countGitLines counts non-empty lines in git command output.
func countGitLines(out []byte) int {
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// joinWarnings joins non-empty warning lines with blank-line spacing for the
// confirmation body, or returns "" when there are none.
func joinWarnings(lines []string) string {
	var kept []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			kept = append(kept, l)
		}
	}
	return strings.Join(kept, "\n\n")
}

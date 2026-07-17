package app

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/sachiniyer/agent-factory/log"
)

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
func killConfirmationWarning(wt string) string {
	out, err := exec.Command("git", "-C", wt, "status", "--porcelain").Output()
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
func unmergedCommitWarning(worktreePath, recordedBaseSHA, prState string) (string, bool) {
	if strings.TrimSpace(worktreePath) == "" {
		return unmergedFailClosedLine(fmt.Errorf("no worktree path")), false
	}
	base, baseLabel, ok := resolveKillBase(worktreePath, recordedBaseSHA)
	if !ok {
		return unmergedFailClosedLine(fmt.Errorf("base branch/commit could not be determined")), false
	}
	// Commits reachable from the branch tip (HEAD is the branch, checked out in
	// the worktree) but not from base: the session's own commits.
	uniqueOut, err := exec.Command("git", "-C", worktreePath, "log", "--oneline", base+"..HEAD").Output()
	if err != nil {
		return unmergedFailClosedLine(err), false
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
	localOut, err := exec.Command("git", "-C", worktreePath, "log", "--oneline", base+"..HEAD", "--not", "--remotes").Output()
	if err != nil {
		return unmergedFailClosedLine(err), false
	}
	localOnly := countGitLines(localOut)
	if localOnly == 0 {
		return "", false // every commit is pushed somewhere — recoverable
	}
	return unmergedSevereLine(localOnly, baseLabel), true
}

// resolveKillBase resolves the commit the branch is measured against for the
// unmerged-work check, offline. It mirrors the base resolution the worktree code
// already uses (session/git/worktree_ops.go): the recorded base commit first,
// then origin's default branch. It deliberately does NOT fall back to HEAD —
// HEAD is the branch tip itself, which would make every branch look zero commits
// ahead and fabricate a "nothing to lose" negative. When neither resolves, ok is
// false and the caller fails closed. baseLabel names the base branch when known
// (for the copy), else "".
func resolveKillBase(worktreePath, recordedBaseSHA string) (base, baseLabel string, ok bool) {
	commitExists := func(rev string) bool {
		_, err := exec.Command("git", "-C", worktreePath, "rev-parse", "--verify", "--quiet", rev+"^{commit}").Output()
		return err == nil
	}
	if b := strings.TrimSpace(recordedBaseSHA); b != "" && commitExists(b) {
		return b, "", true
	}
	// origin/HEAD (symbolic ref to origin's default branch), then the usual names.
	if out, err := exec.Command("git", "-C", worktreePath, "symbolic-ref", "--short", "-q", "refs/remotes/origin/HEAD").Output(); err == nil {
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

// unmergedSevereLine is the #2022 data-loss headline: it names the exact count
// of commits that would be permanently deleted and that the loss is final. It is
// the critical (never-clipped, #1973) line of the confirmation.
func unmergedSevereLine(n int, baseLabel string) string {
	commitWord, pronoun := "commits", "them"
	where := "that aren't merged or pushed anywhere"
	if n == 1 {
		commitWord, pronoun = "commit", "it"
		where = "that isn't merged or pushed anywhere"
	}
	if baseLabel != "" {
		where = fmt.Sprintf("not on %s and not pushed anywhere", baseLabel)
	}
	return fmt.Sprintf("This branch has %d %s %s. Killing permanently deletes %s · this cannot be undone.", n, commitWord, where, pronoun)
}

// unmergedFailClosedLine mirrors the #815 could-not-verify warning for the
// committed-work check: when we cannot prove the branch is free of local-only
// commits, we say so rather than showing the bare, safe-looking prompt.
func unmergedFailClosedLine(err error) string {
	return fmt.Sprintf("Could not verify whether this branch has unmerged commits (%v); any that exist only here would be permanently deleted.", err)
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

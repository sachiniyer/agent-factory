package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
)

// ----------------------------------------------------------------------------
// Regression tests for issue #2022: killing a session force-deletes its branch
// with `git branch -D`, permanently destroying committed-but-unmerged work,
// while the confirmation only ever warned about a DIRTY worktree. A clean
// worktree carrying an unmerged, unpushed commit — the normal state after an
// agent commits — was killed behind a bare "[!] Kill session 'x'?" with no
// data-loss warning at all.
//
// These drive the real confirmation path (handleKill -> the confirmation
// overlay -> its rendered text and confirm key), not the helper on a string, so
// they pin what the user actually sees and which key actually dispatches the
// kill. The data-loss case must fail against the pre-#2022 code.
// ----------------------------------------------------------------------------

// killGit runs a git subcommand in dir and fails the test on error.
func killGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	require.NoError(t, err, "git %s failed: %s", strings.Join(args, " "), out)
	return strings.TrimSpace(string(out))
}

// initBaseRepo creates a repo with a single base commit and returns its dir and
// the base commit SHA (the value a real session records as BaseCommitSHA).
func initBaseRepo(t *testing.T) (repoDir, baseSHA string) {
	t.Helper()
	repoDir = t.TempDir()
	killGit(t, repoDir, "init", "-q")
	killGit(t, repoDir, "config", "user.email", "test@example.com")
	killGit(t, repoDir, "config", "user.name", "Test")
	killGit(t, repoDir, "config", "commit.gpgsign", "false")
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "base.txt"), []byte("base\n"), 0o644))
	killGit(t, repoDir, "add", "-A")
	killGit(t, repoDir, "commit", "-q", "-m", "base commit")
	return repoDir, killGit(t, repoDir, "rev-parse", "HEAD")
}

// addWorktree creates a linked worktree on a fresh branch, checked out at base.
// A linked worktree shares the repo config, so user.* / commit.gpgsign carry
// over. The returned path is what a live session's GetWorktreePath yields.
func addWorktree(t *testing.T, repoDir, baseSHA, branch string) string {
	t.Helper()
	wt := filepath.Join(t.TempDir(), "wt")
	killGit(t, repoDir, "worktree", "add", "-q", "-b", branch, wt, baseSHA)
	return wt
}

// commitInWorktree adds one commit on the worktree's branch — the committed work
// that a kill would orphan when it is not merged or pushed.
func commitInWorktree(t *testing.T, wt string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(wt, "feature.txt"), []byte("work\n"), 0o644))
	killGit(t, wt, "add", "-A")
	killGit(t, wt, "commit", "-q", "-m", "agent: implement feature")
}

// startedWorktreeInstance builds a started, mock-backed instance (FakeBackend
// reports WorkspaceLocalWorktree) whose GetGitWorktree resolves to a real git
// worktree — the shape handleKill inspects for the #2022 check.
func startedWorktreeInstance(t *testing.T, title, repoDir, worktreePath, branch, baseSHA string) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: repoDir, Program: "test"})
	require.NoError(t, err)
	inst.Branch = branch
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Ready)
	gw, err := git.NewGitWorktreeFromStorage(repoDir, worktreePath, title, branch, baseSHA, false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
	return inst
}

// flatten collapses a rendered overlay to a single whitespace-normalized line so
// assertions survive lipgloss wrapping. It also drops the box-drawing border
// glyphs, which otherwise land between two words split across a wrap boundary.
func flatten(s string) string {
	repl := strings.NewReplacer("│", " ", "─", " ", "╭", " ", "╮", " ", "╰", " ", "╯", " ")
	return strings.Join(strings.Fields(repl.Replace(s)), " ")
}

// armKill selects the instance, presses kill, and returns the resulting home and
// its (non-nil) confirmation overlay.
func armKill(t *testing.T, inst *session.Instance) (*home, *home) {
	t.Helper()
	h := newTestHome(t)
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)
	model, _ := h.handleKill()
	hm := model.(*home)
	require.Equal(t, stateConfirm, hm.state, "kill must open the confirmation dialog")
	require.NotNil(t, hm.confirmationOverlay)
	return h, hm
}

// TestHandleKill_UnmergedUnpushedCommit_EscalatesAndNamesLoss is the core #2022
// guard and the data-loss case: a clean worktree whose branch carries one
// unmerged, unpushed commit (no PR) must NOT show the bare prompt. The
// confirmation must name the committed work with its count and the permanence,
// and demand the distinct confirm key so a reflexive 'y' cannot orphan the work.
func TestHandleKill_UnmergedUnpushedCommit_EscalatesAndNamesLoss(t *testing.T) {
	repoDir, baseSHA := initBaseRepo(t)
	wt := addWorktree(t, repoDir, baseSHA, "dev/gamma")
	commitInWorktree(t, wt)
	// Sanity: the worktree is clean (the pre-#2022 dirty check would say nothing).
	require.Empty(t, killGit(t, wt, "status", "--porcelain"), "worktree must be clean")

	inst := startedWorktreeInstance(t, "gamma", repoDir, wt, "dev/gamma", baseSHA)
	_, hm := armKill(t, inst)

	rendered := flatten(hm.confirmationOverlay.Render())
	assert.Contains(t, rendered, "1 commit", "must name the committed work with a count")
	assert.Contains(t, rendered, "permanently deletes it", "must name the permanence")
	assert.Contains(t, rendered, "cannot be undone")
	assert.NotContains(t, rendered, "uncommitted changes", "the loss is committed work, not a dirty worktree")

	require.Equal(t, unmergedKillConfirmKey, hm.confirmationOverlay.ConfirmKey,
		"a data-loss kill must demand the distinct confirm key, not the ordinary 'y'")
	require.NotEqual(t, "y", hm.confirmationOverlay.ConfirmKey)

	// A reflexive 'y' — the gesture that silently orphaned the commit — is ignored.
	model, cmd := hm.handleStateConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	hm = model.(*home)
	assert.Equal(t, stateConfirm, hm.state, "reflexive 'y' must not confirm a data-loss kill")
	assert.NotNil(t, hm.confirmationOverlay, "dialog must stay open after a rejected key")
	assert.Nil(t, cmd)
	assert.NotEqual(t, session.Deleting, inst.GetStatus(), "session must not be marked Deleting")

	// The named key confirms — kill stays POSSIBLE, only deliberate.
	model, cmd = hm.handleStateConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(unmergedKillConfirmKey)})
	hm = model.(*home)
	assert.Equal(t, stateDefault, hm.state, "the named key must confirm")
	assert.Nil(t, hm.confirmationOverlay, "dialog must close on confirm")
	require.NotNil(t, cmd, "confirm must forward the start-kill message")
	assert.Equal(t, session.Deleting, inst.GetStatus())
	startMsg, ok := cmd().(startKillMsg)
	require.True(t, ok, "confirm must emit startKillMsg")
	assert.Equal(t, "gamma", startMsg.target.title)
	assert.Equal(t, inst.ID, startMsg.target.id)
}

// TestHandleKill_AgentSwitchesBranch_WarnsForSessionBranchCommits is the #2199
// regression. An agent may check out another branch inside its worktree, but
// kill still force-deletes the branch recorded for the session. The confirmation
// must therefore inspect and name that recorded branch, not the worktree's
// current HEAD, or committed work on the deleted branch is silently orphaned.
func TestHandleKill_AgentSwitchesBranch_WarnsForSessionBranchCommits(t *testing.T) {
	repoDir, baseSHA := initBaseRepo(t)
	wt := addWorktree(t, repoDir, baseSHA, "dev/session-work")
	commitInWorktree(t, wt)

	// Simulate the agent switching away after committing on the session branch.
	// HEAD is now level with base, while the ref kill will delete remains one
	// local-only commit ahead.
	killGit(t, wt, "checkout", "-q", "-b", "agent-scratch", baseSHA)
	require.Empty(t, killGit(t, wt, "log", "--oneline", baseSHA+"..HEAD"),
		"detoured HEAD must look safe to the buggy confirmation")
	sessionLog := killGit(t, wt, "log", "--oneline", baseSHA+"..refs/heads/dev/session-work")
	require.NotEmpty(t, sessionLog, "the session branch must retain committed work")
	require.Len(t, strings.Split(sessionLog, "\n"), 1,
		"the session branch kill deletes must retain the unique commit")

	inst := startedWorktreeInstance(t, "switched", repoDir, wt, "dev/session-work", baseSHA)
	_, hm := armKill(t, inst)

	rendered := flatten(hm.confirmationOverlay.Render())
	assert.Contains(t, rendered, "dev/session-work",
		"confirmation must identify the branch that kill will delete")
	assert.NotContains(t, rendered, "agent-scratch",
		"confirmation must not describe the branch merely checked out at HEAD")
	assert.Contains(t, rendered, "1 commit",
		"confirmation must count commits on the branch that kill will delete")
	assert.Contains(t, rendered, "permanently deletes it")
	assert.Equal(t, unmergedKillConfirmKey, hm.confirmationOverlay.ConfirmKey,
		"local-only work on the deleted branch must require deliberate confirmation")
}

// TestHandleKill_UsesCanonicalWorktreeBranch is the #2209 review regression.
// Legacy/restored records can carry an empty or stale top-level Instance.Branch,
// while cleanup deletes GitWorktree.BranchName. The confirmation must inspect
// the latter or it can miss exactly the commits kill is about to orphan.
func TestHandleKill_UsesCanonicalWorktreeBranch(t *testing.T) {
	repoDir, baseSHA := initBaseRepo(t)
	wt := addWorktree(t, repoDir, baseSHA, "dev/canonical")
	commitInWorktree(t, wt)

	inst := startedWorktreeInstance(t, "legacy", repoDir, wt, "dev/canonical", baseSHA)
	inst.Branch = "stale-legacy-value"
	_, hm := armKill(t, inst)

	rendered := flatten(hm.confirmationOverlay.Render())
	assert.Contains(t, rendered, "dev/canonical",
		"warning must name the worktree branch cleanup will actually delete")
	assert.NotContains(t, rendered, "stale-legacy-value")
	assert.Contains(t, rendered, "1 commit")
	assert.Equal(t, unmergedKillConfirmKey, hm.confirmationOverlay.ConfirmKey)
}

// A merged PR is only evidence for the branch it was fetched from. Legacy
// records can retain PR state for Instance.Branch while cleanup deletes the
// GitWorktree branch; that stale state must not suppress a real loss warning.
func TestHandleKill_StalePRBranchCannotSuppressCanonicalLoss(t *testing.T) {
	repoDir, baseSHA := initBaseRepo(t)
	wt := addWorktree(t, repoDir, baseSHA, "dev/canonical-pr")
	commitInWorktree(t, wt)

	inst := startedWorktreeInstance(t, "stale-pr", repoDir, wt, "dev/canonical-pr", baseSHA)
	inst.Branch = "dev/legacy"
	inst.SetPRInfo(&git.PRInfo{Number: 7, State: "MERGED", Branch: "dev/legacy"})
	_, hm := armKill(t, inst)

	rendered := flatten(hm.confirmationOverlay.Render())
	assert.Contains(t, rendered, `Branch "dev/canonical-pr" has 1 commit`)
	assert.Contains(t, rendered, "permanently deletes it")
	assert.Equal(t, unmergedKillConfirmKey, hm.confirmationOverlay.ConfirmKey)
}

// Cleanup is a no-op for --here/legacy external worktrees. Their dirty files,
// private refs, branches, and detached commits all remain after killing the
// runtime, so destructive-worktree copy and escalation would be false.
func TestHandleKill_ExternalWorktreeSkipsWorktreeLossWarnings(t *testing.T) {
	repoDir, baseSHA := initBaseRepo(t)
	wt := addWorktree(t, repoDir, baseSHA, "dev/external")
	killGit(t, wt, "checkout", "-q", "--detach", baseSHA)
	commitInWorktree(t, wt)
	require.NoError(t, os.WriteFile(filepath.Join(wt, "dirty.txt"), []byte("kept\n"), 0o644))

	inst := startedWorktreeInstance(t, "external", repoDir, wt, "dev/external", baseSHA)
	gw, err := git.NewGitWorktreeFromStorage(repoDir, wt, "external", "dev/external", baseSHA, true, false)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
	_, hm := armKill(t, inst)

	rendered := flatten(hm.confirmationOverlay.Render())
	assert.Contains(t, rendered, "Kill session 'external'?")
	assert.NotContains(t, rendered, "will be lost")
	assert.NotContains(t, rendered, "Detached HEAD")
	assert.Equal(t, "y", hm.confirmationOverlay.ConfirmKey)
}

// TestHandleKill_DetachedLocalCommitEscalates is the #2210 review regression.
// The recorded branch is level with base, but HEAD carries a new commit that no
// ref contains. Removing the worktree drops the final reference to that commit,
// so the branch-only check used to show the ordinary safe-looking confirmation.
func TestHandleKill_DetachedLocalCommitEscalates(t *testing.T) {
	repoDir, baseSHA := initBaseRepo(t)
	wt := addWorktree(t, repoDir, baseSHA, "dev/detached")
	killGit(t, wt, "checkout", "-q", "--detach", baseSHA)
	commitInWorktree(t, wt)
	require.Empty(t, killGit(t, wt, "status", "--porcelain"), "detached worktree must be clean")
	require.Empty(t, killGit(t, wt, "for-each-ref", "--contains=HEAD", "--format=%(refname)"),
		"the detached tip must exist only through the worktree HEAD")

	inst := startedWorktreeInstance(t, "detached", repoDir, wt, "dev/detached", baseSHA)
	_, hm := armKill(t, inst)

	rendered := flatten(hm.confirmationOverlay.Render())
	assert.Contains(t, rendered, "Detached HEAD")
	assert.Contains(t, rendered, "not reachable from any ref")
	assert.Contains(t, rendered, "permanently orphans")
	assert.Contains(t, rendered, "cannot be undone")
	assert.Equal(t, unmergedKillConfirmKey, hm.confirmationOverlay.ConfirmKey,
		"an unreferenced detached commit must require deliberate confirmation")
}

// TestHandleKill_NoUniqueCommits_KeepsBareConfirmation guards against
// over-warning: a clean worktree whose branch is level with base (no committed
// work to lose) must kill behind the SAME bare confirmation and ordinary 'y' as
// before #2022.
func TestHandleKill_NoUniqueCommits_KeepsBareConfirmation(t *testing.T) {
	repoDir, baseSHA := initBaseRepo(t)
	wt := addWorktree(t, repoDir, baseSHA, "dev/empty") // no commit beyond base

	inst := startedWorktreeInstance(t, "empty", repoDir, wt, "dev/empty", baseSHA)
	_, hm := armKill(t, inst)

	rendered := flatten(hm.confirmationOverlay.Render())
	assert.Contains(t, rendered, "Kill session 'empty'?")
	assert.NotContains(t, rendered, "commit", "a level branch must not warn about commits")
	assert.NotContains(t, rendered, "Could not verify")
	assert.Equal(t, "y", hm.confirmationOverlay.ConfirmKey,
		"a session with nothing to lose must keep the ordinary 'y' confirm")
}

// TestHandleKill_PushedCommits_NotSevere: commits that also exist on a remote
// survive `git branch -D` (it deletes only the LOCAL branch), so a pushed branch
// must not fire the loud data-loss guard — firing it would train users to reflex
// past a warning that isn't real.
func TestHandleKill_PushedCommits_NotSevere(t *testing.T) {
	repoDir, baseSHA := initBaseRepo(t)
	wt := addWorktree(t, repoDir, baseSHA, "dev/pushed")
	commitInWorktree(t, wt)
	// Push the branch so a remote-tracking ref carries the commit.
	bare := filepath.Join(t.TempDir(), "origin.git")
	killGit(t, repoDir, "init", "-q", "--bare", bare)
	killGit(t, repoDir, "remote", "add", "origin", bare)
	killGit(t, wt, "push", "-q", "origin", "dev/pushed")

	inst := startedWorktreeInstance(t, "pushed", repoDir, wt, "dev/pushed", baseSHA)
	_, hm := armKill(t, inst)

	rendered := flatten(hm.confirmationOverlay.Render())
	assert.NotContains(t, rendered, "permanently deletes", "pushed commits survive the kill")
	assert.Equal(t, "y", hm.confirmationOverlay.ConfirmKey,
		"a pushed branch must keep the ordinary 'y' confirm")
}

// TestHandleKill_MergedPR_NotSevere: a merged PR means the work landed in base's
// history, so even unpushed branch commits are not a real loss and must not fire
// the loud guard.
func TestHandleKill_MergedPR_NotSevere(t *testing.T) {
	repoDir, baseSHA := initBaseRepo(t)
	wt := addWorktree(t, repoDir, baseSHA, "dev/merged")
	commitInWorktree(t, wt) // unpushed locally, but the PR is merged

	inst := startedWorktreeInstance(t, "merged", repoDir, wt, "dev/merged", baseSHA)
	inst.SetPRInfo(&git.PRInfo{Number: 7, State: "MERGED", Branch: "dev/merged"})
	_, hm := armKill(t, inst)

	rendered := flatten(hm.confirmationOverlay.Render())
	assert.NotContains(t, rendered, "permanently deletes", "merged work is not a loss")
	assert.Equal(t, "y", hm.confirmationOverlay.ConfirmKey)
}

// TestHandleKill_BaseUndeterminable_FailsClosed: when we cannot determine the
// base (no recorded base, no origin), we cannot prove the branch is free of
// local-only commits — so we WARN rather than show the bare prompt (mirroring
// the #815 fail-closed discipline). Unknown is not clean. We do not escalate the
// key: we have not established that work is actually being lost.
func TestHandleKill_BaseUndeterminable_FailsClosed(t *testing.T) {
	repoDir, baseSHA := initBaseRepo(t)
	wt := addWorktree(t, repoDir, baseSHA, "dev/nobody")
	commitInWorktree(t, wt)

	// Record no base and provide no origin, so base resolution has nothing to go on.
	inst := startedWorktreeInstance(t, "nobody", repoDir, wt, "dev/nobody", "")
	_, hm := armKill(t, inst)

	rendered := flatten(hm.confirmationOverlay.Render())
	assert.Contains(t, rendered, `Could not verify whether session branch "dev/nobody" has unmerged commits`)
	assert.Equal(t, "y", hm.confirmationOverlay.ConfirmKey,
		"unverifiable is a warning, not a proven loss — keep the ordinary confirm")
}

// TestUnmergedCommitWarning covers the predicate directly across the matrix. The
// rendered-path tests above prove the wiring; this pins the predicate itself.
func TestUnmergedCommitWarning(t *testing.T) {
	t.Run("unmerged unpushed commit is severe", func(t *testing.T) {
		repoDir, baseSHA := initBaseRepo(t)
		wt := addWorktree(t, repoDir, baseSHA, "dev/a")
		commitInWorktree(t, wt)
		line, severe := unmergedCommitWarning(wt, "dev/a", baseSHA, "", true)
		assert.True(t, severe)
		assert.Contains(t, line, "1 commit")
		assert.Contains(t, line, "cannot be undone")
	})

	t.Run("no commits beyond base is safe", func(t *testing.T) {
		repoDir, baseSHA := initBaseRepo(t)
		wt := addWorktree(t, repoDir, baseSHA, "dev/b")
		line, severe := unmergedCommitWarning(wt, "dev/b", baseSHA, "", true)
		assert.False(t, severe)
		assert.Empty(t, line)
	})

	t.Run("pushed commit is safe", func(t *testing.T) {
		repoDir, baseSHA := initBaseRepo(t)
		wt := addWorktree(t, repoDir, baseSHA, "dev/c")
		commitInWorktree(t, wt)
		bare := filepath.Join(t.TempDir(), "origin.git")
		killGit(t, repoDir, "init", "-q", "--bare", bare)
		killGit(t, repoDir, "remote", "add", "origin", bare)
		killGit(t, wt, "push", "-q", "origin", "dev/c")
		line, severe := unmergedCommitWarning(wt, "dev/c", baseSHA, "", true)
		assert.False(t, severe)
		assert.Empty(t, line)
	})

	t.Run("merged PR is safe despite local-only commit", func(t *testing.T) {
		repoDir, baseSHA := initBaseRepo(t)
		wt := addWorktree(t, repoDir, baseSHA, "dev/d")
		commitInWorktree(t, wt)
		line, severe := unmergedCommitWarning(wt, "dev/d", baseSHA, "MERGED", true)
		assert.False(t, severe)
		assert.Empty(t, line)
	})

	t.Run("multiple commits pluralize", func(t *testing.T) {
		repoDir, baseSHA := initBaseRepo(t)
		wt := addWorktree(t, repoDir, baseSHA, "dev/e")
		commitInWorktree(t, wt)
		require.NoError(t, os.WriteFile(filepath.Join(wt, "second.txt"), []byte("more\n"), 0o644))
		killGit(t, wt, "add", "-A")
		killGit(t, wt, "commit", "-q", "-m", "agent: more work")
		line, severe := unmergedCommitWarning(wt, "dev/e", baseSHA, "", true)
		assert.True(t, severe)
		assert.Contains(t, line, "2 commits")
		assert.Contains(t, line, "deletes them")
	})

	t.Run("undeterminable base fails closed", func(t *testing.T) {
		repoDir, baseSHA := initBaseRepo(t)
		wt := addWorktree(t, repoDir, baseSHA, "dev/f")
		commitInWorktree(t, wt)
		line, severe := unmergedCommitWarning(wt, "dev/f", "", "", true) // no recorded base, no origin
		assert.False(t, severe, "unverifiable must not claim a proven loss")
		assert.Contains(t, line, "Could not verify")
	})

	t.Run("missing session branch fails closed", func(t *testing.T) {
		repoDir, baseSHA := initBaseRepo(t)
		wt := addWorktree(t, repoDir, baseSHA, "dev/actual")
		line, severe := unmergedCommitWarning(wt, "dev/missing", baseSHA, "", true)
		assert.False(t, severe, "unverifiable must not claim a proven loss")
		assert.Contains(t, line, `session branch "dev/missing"`)
		assert.Contains(t, line, "Could not verify")
	})

	t.Run("empty session branch fails closed", func(t *testing.T) {
		repoDir, baseSHA := initBaseRepo(t)
		wt := addWorktree(t, repoDir, baseSHA, "dev/actual")
		line, severe := unmergedCommitWarning(wt, "", baseSHA, "", true)
		assert.False(t, severe, "unverifiable must not claim a proven loss")
		assert.Contains(t, line, "Could not verify whether the session branch")
	})

	t.Run("empty worktree path fails closed", func(t *testing.T) {
		line, severe := unmergedCommitWarning("", "dev/missing", "deadbeef", "", true)
		assert.False(t, severe)
		assert.Contains(t, line, "Could not verify")
	})

	t.Run("git error fails closed", func(t *testing.T) {
		// A non-repo directory makes the base rev-parse fail.
		line, severe := unmergedCommitWarning(t.TempDir(), "dev/missing", "deadbeef", "", true)
		assert.False(t, severe)
		assert.Contains(t, line, "Could not verify")
	})

	t.Run("detached unreferenced commit is severe", func(t *testing.T) {
		repoDir, baseSHA := initBaseRepo(t)
		wt := addWorktree(t, repoDir, baseSHA, "dev/detached-direct")
		killGit(t, wt, "checkout", "-q", "--detach", baseSHA)
		commitInWorktree(t, wt)
		line, severe := unmergedCommitWarning(wt, "dev/detached-direct", baseSHA, "", true)
		assert.True(t, severe)
		assert.Contains(t, line, "Detached HEAD")
		assert.Contains(t, line, "permanently orphans")
	})

	t.Run("detached referenced commit is safe", func(t *testing.T) {
		repoDir, baseSHA := initBaseRepo(t)
		wt := addWorktree(t, repoDir, baseSHA, "dev/detached-referenced")
		killGit(t, wt, "checkout", "-q", "--detach", baseSHA)
		line, severe := unmergedCommitWarning(wt, "dev/detached-referenced", baseSHA, "", true)
		assert.False(t, severe)
		assert.Empty(t, line, "a detached HEAD still contained by the session branch survives worktree removal")
	})

	t.Run("user-owned session branch survives cleanup", func(t *testing.T) {
		repoDir, baseSHA := initBaseRepo(t)
		wt := addWorktree(t, repoDir, baseSHA, "dev/user-owned")
		commitInWorktree(t, wt)
		line, severe := unmergedCommitWarning(wt, "dev/user-owned", baseSHA, "", false)
		assert.False(t, severe)
		assert.Empty(t, line, "cleanup preserves a branch AF did not create")
	})

	t.Run("per-worktree ref does not make detached tip durable", func(t *testing.T) {
		repoDir, baseSHA := initBaseRepo(t)
		wt := addWorktree(t, repoDir, baseSHA, "dev/private-ref")
		killGit(t, wt, "checkout", "-q", "--detach", baseSHA)
		commitInWorktree(t, wt)
		killGit(t, wt, "update-ref", "refs/worktree/keep", "HEAD")

		line, severe := detachedHeadCommitWarning(wt, "dev/private-ref", true)
		assert.True(t, severe)
		assert.Contains(t, line, "permanently orphans")
	})

	t.Run("deleted session branch does not make detached tip durable", func(t *testing.T) {
		repoDir, baseSHA := initBaseRepo(t)
		wt := addWorktree(t, repoDir, baseSHA, "dev/doomed-ref")
		commitInWorktree(t, wt)
		killGit(t, wt, "checkout", "-q", "--detach", "HEAD")

		line, severe := detachedHeadCommitWarning(wt, "dev/doomed-ref", true)
		assert.True(t, severe)
		assert.Contains(t, line, "permanently orphans")
	})

	t.Run("durable tag preserves detached tip", func(t *testing.T) {
		repoDir, baseSHA := initBaseRepo(t)
		wt := addWorktree(t, repoDir, baseSHA, "dev/tagged")
		killGit(t, wt, "checkout", "-q", "--detach", baseSHA)
		commitInWorktree(t, wt)
		killGit(t, wt, "tag", "keep-detached", "HEAD")

		line, severe := detachedHeadCommitWarning(wt, "dev/tagged", true)
		assert.False(t, severe)
		assert.Empty(t, line)
	})
}

package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// residueTestRepo sets up a real git repo with an initial commit and returns
// its root, ready for `git worktree add` to place real linked worktrees whose
// `.git` pointers BindAFRelocationResidue can authenticate.
func residueTestRepo(t *testing.T) string {
	t.Helper()
	repoRoot := initTempGitRepo(t)
	gitOut(t, repoRoot, "config", "user.email", "test@test.com")
	gitOut(t, repoRoot, "config", "user.name", "test")
	gitOut(t, repoRoot, "commit", "--allow-empty", "-m", "initial")
	return repoRoot
}

// simulateCrashWindowRename models the on-disk footprint the daemon leaves when
// it dies between the quarantine rename and the publish rename of a cross-
// device archive move: the complete worktree is renamed wholesale to a
// `.af-source-<32hex>` sibling (raw rename, so git's registration still names
// the now-absent recorded path), with no relocation record.
func simulateCrashWindowRename(t *testing.T, wtPath string) string {
	t.Helper()
	quarantine := filepath.Join(filepath.Dir(wtPath), ".af-source-"+strings.Repeat("a", 32))
	require.NoError(t, os.Rename(wtPath, quarantine))
	return quarantine
}

// TestMissingWorktreeRebuildRefusal_RefusesOverCrashWindowResidue is the
// restore-side regression for the #7804/#7807 crash window: the lost-restore
// rebuild must not silently re-check out the branch over a stranded
// `.af-source-<32hex>` quarantine. The source parent scan finds the residue,
// BindAFRelocationResidue proves it is this session's, and the rebuild is
// refused with the same ErrRelocateStateUnknown-shaped verdict and recovery
// guidance the archive endpoint gives.
func TestMissingWorktreeRebuildRefusal_RefusesOverCrashWindowResidue(t *testing.T) {
	repoRoot := residueTestRepo(t)
	const branch = "af/crash-window-own"
	wtParent := t.TempDir()
	wtPath := filepath.Join(wtParent, "wt-own")
	gitOut(t, repoRoot, "worktree", "add", "-b", branch, wtPath)
	require.NoError(t, os.WriteFile(filepath.Join(wtPath, "dirty.txt"), []byte("uncommitted"), 0o644))
	quarantine := simulateCrashWindowRename(t, wtPath)

	gw, err := git.NewGitWorktreeFromStorage(repoRoot, wtPath, "crash-window-own", branch, "", false, true)
	require.NoError(t, err)
	inst := &Instance{Title: "crash-window-own", Program: tmux.ProgramClaude, Path: repoRoot, gitWorktree: gw}

	err = missingWorktreeRebuildRefusal(inst, gw, true, "claude")
	require.Error(t, err, "a crash-window quarantine proven to be this session's must refuse the rebuild")
	assert.ErrorIs(t, err, git.ErrRelocateStateUnknown,
		"the refusal must be shaped as an unresolved relocation so callers can tell it from a committed deletion")
	assert.Contains(t, err.Error(), ".af-source-", "the refusal must name the stranded residue")
	assert.Contains(t, err.Error(), quarantine, "the refusal must name the specific quarantine path")
	assert.Contains(t, err.Error(), "mv "+quarantine+" "+wtPath,
		"the refusal must direct recovery from the proven-ours .af-source-* quarantine: move it back to the session path")

	// The stranded worktree survives untouched with its uncommitted file — the
	// exact loss the residue arbitration exists to prevent.
	dirty, rerr := os.ReadFile(filepath.Join(quarantine, "dirty.txt"))
	require.NoError(t, rerr, "the stranded worktree must survive the refused rebuild")
	assert.Equal(t, "uncommitted", string(dirty))
	// The recorded path stays absent: the rebuild never ran.
	_, statErr := os.Stat(wtPath)
	assert.True(t, os.IsNotExist(statErr), "the rebuild must not have recreated the recorded path")
}

// TestMissingWorktreeRebuildRefusal_ForeignResidueDoesNotBlockRebuild is the
// shared-parent guard: the source parent is shared by every session, so only
// residue proven to be THIS session's blocks. Another session's interrupted
// move, stranded beside a session whose own worktree was genuinely deleted,
// must not strand this session's rebuild forever.
func TestMissingWorktreeRebuildRefusal_ForeignResidueDoesNotBlockRebuild(t *testing.T) {
	repoRoot := residueTestRepo(t)
	const ownBranch = "af/foreign-own"
	const otherBranch = "af/foreign-other"
	gitOut(t, repoRoot, "branch", otherBranch)
	wtParent := t.TempDir()
	ownWt := filepath.Join(wtParent, "wt-foreign-own")
	otherWt := filepath.Join(wtParent, "wt-foreign-other")
	gitOut(t, repoRoot, "worktree", "add", "-b", ownBranch, ownWt)
	gitOut(t, repoRoot, "worktree", "add", otherWt, otherBranch)
	// This session's own worktree is genuinely deleted outside af.
	require.NoError(t, os.RemoveAll(ownWt))
	// A FOREIGN session's worktree is stranded under a residue name beside the
	// same shared parent — exactly the footprint another session's interrupted
	// cross-device move leaves.
	foreignResidue := filepath.Join(wtParent, ".af-source-"+strings.Repeat("f", 32))
	require.NoError(t, os.Rename(otherWt, foreignResidue))

	gw, err := git.NewGitWorktreeFromStorage(repoRoot, ownWt, "foreign-residue", ownBranch, "", false, true)
	require.NoError(t, err)
	inst := &Instance{Title: "foreign-residue", Program: tmux.ProgramClaude, Path: repoRoot, gitWorktree: gw}

	err = missingWorktreeRebuildRefusal(inst, gw, true, "claude")
	assert.NoError(t, err,
		"a foreign session's residue must not block this session's rebuild when its own branch survives")
	// The foreign residue is left untouched — it belongs to another session,
	// and this rebuild must never have been told to move it into this
	// session's path.
	assert.DirExists(t, foreignResidue, "the foreign session's residue must survive untouched")
}

// TestMissingWorktreeRebuildRefusal_UnverifiedResidueRefusesFailClosed pins
// the fail-closed shape for residue whose git binding cannot be read (a stale
// orphan a completed move left, or a directory a user created with af's private
// naming). The rebuild refuses rather than guess a deletion over a scan it
// could not classify, but the refusal must NOT claim the residue is this
// session's work and must NOT tell the operator to move it back into this
// session's path.
func TestMissingWorktreeRebuildRefusal_UnverifiedResidueRefusesFailClosed(t *testing.T) {
	repoRoot := residueTestRepo(t)
	const branch = "af/unverified"
	gitOut(t, repoRoot, "branch", branch)
	wtParent := t.TempDir()
	wtPath := filepath.Join(wtParent, "wt-unverified")
	// An orphan residue with NO linked-worktree .git pointer beside the
	// (absent) source parent — a residue whose git binding cannot be read.
	orphan := filepath.Join(wtParent, ".af-source-0123456789abcdef0123456789abcdee")
	require.NoError(t, os.Mkdir(orphan, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(orphan, "stale.txt"), []byte("orphan"), 0o644))

	gw, err := git.NewGitWorktreeFromStorage(repoRoot, wtPath, "unverified", branch, "", false, true)
	require.NoError(t, err)
	inst := &Instance{Title: "unverified", Program: tmux.ProgramClaude, Path: repoRoot, gitWorktree: gw}

	err = missingWorktreeRebuildRefusal(inst, gw, true, "claude")
	require.Error(t, err, "an unverified residue must refuse fail-closed rather than guess a deletion")
	assert.ErrorIs(t, err, git.ErrRelocateStateUnknown)
	assert.Contains(t, err.Error(), orphan, "the refusal must name the unverified residue")
	assert.NotContains(t, err.Error(), "move it back to",
		"the refusal for an unverified residue must never instruct the operator to move it back into this session's path")
	assert.NotContains(t, err.Error(), "move it back to "+wtPath,
		"the refusal must not name this session's recorded path as a move-back target")
	assert.Contains(t, err.Error(), "could not verify",
		"the refusal must say the binding could not be verified, not claim the residue is this session's work")
	assert.Contains(t, err.Error(), "move it out of "+filepath.Dir(orphan),
		"the refusal must tell the operator how to get unblocked: move the residue out of its parent, then retry")
	assert.Contains(t, err.Error(), "#7819",
		"the refusal must cite the follow-up tracking the ownership-model redesign")
	assert.Contains(t, err.Error(), "do not move it into "+wtPath,
		"the refusal must keep the warning against moving unverified residue into this session's path")
}

// TestRecover_RefusesRebuildOverCrashWindowResidue is the end-to-end
// regression: the automatic lost-restore rebuild runs through the production
// LocalBackend.Recover → respawnWithConversation path. With the crash-window
// footprint (worktree renamed to `.af-source-<32hex>`, no relocation record,
// Lost row), Recover must refuse to rebuild, leave the row Lost, keep the
// stranded quarantine's uncommitted work intact, and never re-create the
// recorded path — preserving the rescue window the archive-side arbitration
// needs to surface the residue on a later retry.
func TestRecover_RefusesRebuildOverCrashWindowResidue(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	repoRoot := residueTestRepo(t)
	const branch = "af/lost-crash-window"
	wtParent := t.TempDir()
	worktreePath := filepath.Join(wtParent, "wt-lost-crash")
	gitOut(t, repoRoot, "worktree", "add", "-b", branch, worktreePath)
	require.NoError(t, os.WriteFile(filepath.Join(worktreePath, "dirty.txt"), []byte("uncommitted"), 0o644))
	quarantine := simulateCrashWindowRename(t, worktreePath)

	const agentName = "af_recover_crash_window"
	shellName := agentName + tmuxTabSeparator + shellTabName
	const conversationID = "019f386f-7206-7fc2-803b-f7045e07a242"
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	writeCodexRolloutFile(t, codexHome, "rollout-2026-07-06T10-17-35-"+conversationID+".jsonl")
	branchCreatedByUs := true
	data := InstanceData{
		Title:    "lost-crash-window",
		Path:     repoRoot,
		Branch:   branch,
		Program:  tmux.ProgramCodex,
		Status:   Lost,
		TmuxName: agentName,
		Tabs: []TabData{
			{
				Name:     agentTabName,
				Kind:     TabKindAgent,
				TmuxName: agentName,
				Conversation: &AgentConversationData{
					Agent:       tmux.ProgramCodex,
					ID:          conversationID,
					CaptureKind: ConversationCaptureCodexRollout,
				},
			},
			{Name: shellTabName, Kind: TabKindShell, TmuxName: shellName},
		},
		Worktree: GitWorktreeData{
			RepoPath:          repoRoot,
			WorktreePath:      worktreePath,
			SessionName:       "lost-crash-window",
			BranchName:        branch,
			BranchCreatedByUs: &branchCreatedByUs,
		},
	}

	var newSessions int
	var spawns []string
	exec := deadPaneAwareExec(map[string]bool{},
		recordingExec(map[string]bool{}, &newSessions, &spawns))
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })

	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	require.Equal(t, Lost, restored.GetStatus())
	restored.SetRebuildTeardownNotify(func(*Instance) error { return nil })
	restored.SetRebuildTeardownRuntimesStop(func(*Instance) error { return nil })

	err = restored.Recover()
	require.Error(t, err, "Recover must refuse to rebuild over the crash-window quarantine")
	assert.ErrorIs(t, err, git.ErrRelocateStateUnknown,
		"the refusal must be shaped as an unresolved relocation so the lost-restore loop classifies it consistently with the archive side")
	assert.Contains(t, err.Error(), ".af-source-", "the refusal must name the stranded residue")
	assert.Contains(t, err.Error(), quarantine)
	assert.Equal(t, Lost, restored.GetStatus(),
		"refusing the rebuild must leave the row Lost so the rescue window stays open")
	assert.Zero(t, newSessions, "no agent may be spawned over the stranded quarantine")

	// The stranded quarantine survives untouched with its uncommitted file.
	dirty, rerr := os.ReadFile(filepath.Join(quarantine, "dirty.txt"))
	require.NoError(t, rerr, "the stranded worktree must survive the refused rebuild")
	assert.Equal(t, "uncommitted", string(dirty))
	// The recorded path stays absent: the rebuild never recreated it, so a
	// later archive retry still reaches the archive-side bare-ENOENT route.
	_, statErr := os.Stat(worktreePath)
	assert.True(t, os.IsNotExist(statErr),
		"the recorded path must stay absent so the archive-side residue arbitration still runs on retry")
}

// crashWindowRefusalRecord builds the crash-window rebuild refusal for
// inst/gw and stores it exactly the way the daemon does — wrapped in the
// respawn path's WorktreeUnavailableError + stat-ENOENT frame — returning the
// durable, user-facing record text LostRestoreFailure keeps (#7847).
func crashWindowRefusalRecord(t *testing.T, inst *Instance, gw *git.GitWorktree, wtPath string) string {
	t.Helper()
	refusal := missingWorktreeRebuildRefusal(inst, gw, true, "claude")
	require.Error(t, refusal, "the crash-window footprint must refuse the rebuild")
	assert.ErrorIs(t, refusal, git.ErrRelocateStateUnknown,
		"the refusal must keep the ErrRelocateStateUnknown verdict for the errors.Is classifiers")
	_, statErr := os.Stat(wtPath)
	require.Error(t, statErr, "the recorded path is absent in the crash-window footprint")
	stored := &WorktreeUnavailableError{Title: inst.Title, WorktreePath: wtPath,
		Err: fmt.Errorf("%w (%w)", statErr, refusal)}
	require.True(t, inst.SetLostRestoreFailure(4, stored))
	snap := inst.LostRestoreFailureSnapshot()
	require.NotNil(t, snap)
	return snap.Error
}

// TestLostRestoreFailure_ResidueRefusalFitsCap_ProvenOurs pins #7847: the
// durable record — capped at lostRestoreFailureMaxRunes runes by
// sanitizeLostRestoreError — must keep the residue path and the move-it-back
// remedy for a realistic path length, so the user-facing row tells the
// operator what to do, not just that the restore refused. On master the
// ~780-rune refusal was cut before it ever named either.
func TestLostRestoreFailure_ResidueRefusalFitsCap_ProvenOurs(t *testing.T) {
	repoRoot := residueTestRepo(t)
	const branch = "af/residue-fits-own"
	// A long, realistic leaf — the shape #7847 measured
	// (/home/user/Desktop/myrepo-fix-login-bug-with-a-long-name) — so the
	// record cannot borrow short-path slack the old message needed.
	wtParent := t.TempDir()
	wtPath := filepath.Join(wtParent, "myrepo-fix-login-bug-with-a-long-name")
	gitOut(t, repoRoot, "worktree", "add", "-b", branch, wtPath)
	quarantine := simulateCrashWindowRename(t, wtPath)

	gw, err := git.NewGitWorktreeFromStorage(repoRoot, wtPath, "residue-fits-own", branch, "", false, true)
	require.NoError(t, err)
	inst := &Instance{Title: "residue-fits-own", Program: tmux.ProgramClaude, Path: repoRoot, gitWorktree: gw}

	record := crashWindowRefusalRecord(t, inst, gw, wtPath)
	assert.LessOrEqual(t, len([]rune(record)), lostRestoreFailureMaxRunes,
		"the stored record stays inside the durable cap by construction")
	assert.Contains(t, record, "mv "+quarantine+" "+wtPath,
		"the stored record must lead with the remedy: mv the proven .af-source-* quarantine back to the session path")
	assert.Contains(t, record, "move this session's stranded worktree back",
		"the stored record must keep the remedy purpose, not just the fact of the refusal")
}

// TestLostRestoreFailure_ResidueRefusalFitsCap_Unverified is the unverified
// half of #7847: the stored record must keep the residue path and the
// move-it-out remedy at a realistic path length — and must never tell the
// operator to move unverified residue into this session's path.
func TestLostRestoreFailure_ResidueRefusalFitsCap_Unverified(t *testing.T) {
	repoRoot := residueTestRepo(t)
	const branch = "af/residue-fits-unverified"
	gitOut(t, repoRoot, "branch", branch)
	wtParent := t.TempDir()
	wtPath := filepath.Join(wtParent, "myrepo-fix-login-bug-with-a-long-name")
	// An orphan residue with NO linked-worktree .git pointer beside the
	// (absent) source — a residue whose git binding cannot be read.
	orphan := filepath.Join(wtParent, ".af-source-0123456789abcdef0123456789abcdee")
	require.NoError(t, os.Mkdir(orphan, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(orphan, "stale.txt"), []byte("orphan"), 0o644))

	gw, err := git.NewGitWorktreeFromStorage(repoRoot, wtPath, "residue-fits-unverified", branch, "", false, true)
	require.NoError(t, err)
	inst := &Instance{Title: "residue-fits-unverified", Program: tmux.ProgramClaude, Path: repoRoot, gitWorktree: gw}

	record := crashWindowRefusalRecord(t, inst, gw, wtPath)
	assert.LessOrEqual(t, len([]rune(record)), lostRestoreFailureMaxRunes)
	assert.Contains(t, record, orphan,
		"the stored record must name the unverified residue")
	assert.Contains(t, record, "move it out of "+wtParent,
		"the stored record must keep the remedy: move unverified residue out of its folder once checked, then retry")
	assert.NotContains(t, record, "move it back",
		"the stored record must never direct unverified residue into this session's path")
}

package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// residueTestRepo sets up a real git repo with an initial commit and returns
// its root — the kernel spelling, canonicalized through EvalSymlinks — ready
// for `git worktree add` to place real linked worktrees whose `.git`
// pointers BindAFRelocationResidue can authenticate.
func residueTestRepo(t *testing.T) string {
	t.Helper()
	repoRoot := initTempGitRepo(t)
	gitOut(t, repoRoot, "config", "user.email", "test@test.com")
	gitOut(t, repoRoot, "config", "user.name", "test")
	gitOut(t, repoRoot, "commit", "--allow-empty", "-m", "initial")
	resolved, err := filepath.EvalSymlinks(repoRoot)
	require.NoError(t, err)
	return resolved
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
	assert.Contains(t, err.Error(), filepath.Base(quarantine),
		"the refusal must name the specific quarantine")
	assert.Contains(t, err.Error(), wtParent,
		"the refusal must name the residue's parent so its basename resolves to the full path")
	assert.Contains(t, err.Error(),
		shellsuggest.Command("cd", wtParent)+" && "+
			shellsuggest.Command("mv", "--", filepath.Base(quarantine), filepath.Base(wtPath)),
		"the refusal must direct recovery from the proven-ours .af-source-* quarantine: move it back onto the session path")

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
//
// The parent is the 200-rune padded temp dir: the depth that dropped the
// warning and the #7819 citation out of the stored record on macOS CI (#7855),
// so Linux fails first if a mandatory clause ever becomes droppable again.
func TestMissingWorktreeRebuildRefusal_UnverifiedResidueRefusesFailClosed(t *testing.T) {
	repoRoot := residueTestRepo(t)
	const branch = "af/unverified"
	gitOut(t, repoRoot, "branch", branch)
	wtParent := residueParentAtCapDepth(t)
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
	assert.Contains(t, err.Error(), filepath.Base(orphan),
		"the refusal must name the unverified residue")
	assert.Contains(t, err.Error(), "in "+filepath.Dir(orphan),
		"the refusal must name the residue's folder so its basename resolves to the full path")
	assert.NotContains(t, err.Error(), "move it back to",
		"the refusal for an unverified residue must never instruct the operator to move it back into this session's path")
	assert.NotContains(t, err.Error(), "move it back to "+wtPath,
		"the refusal must not name this session's recorded path as a move-back target")
	assert.Contains(t, err.Error(), "could not verify",
		"the refusal must say the binding could not be verified, not claim the residue is this session's work")
	assert.Contains(t, err.Error(), "move it out of that folder",
		"the refusal must tell the operator how to get unblocked: move the residue out of its parent, then retry")
	assert.Contains(t, err.Error(), "#7819",
		"the refusal must cite the follow-up tracking the ownership-model redesign")
	// The session path's leaf resolves against the parent the record already
	// named — the same warning at a fraction of the runes, which is what
	// keeps it inside the cap at this depth (#7855).
	assert.Contains(t, err.Error(), "do not move it into "+filepath.Base(wtPath)+" unless proven",
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
	assert.Contains(t, err.Error(), filepath.Base(quarantine),
		"the refusal must name the specific quarantine")
	assert.Contains(t, err.Error(), wtParent,
		"the refusal must name the residue's parent so its basename resolves to the full path")
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

// residueTestParentRunes is the fixed length the residue-cap tests pad the
// worktree's parent directory to — about the depth macOS's /var/folders/…
// tempdirs reached in #7855, where the stored record's remedy was clipped.
// Padding rather than relying on t.TempDir() makes Linux exercise the same
// budget macOS does.
const residueTestParentRunes = 200

// residueParentAtCapDepth returns a fresh parent directory exactly
// residueTestParentRunes runes long, built under a canonicalized t.TempDir()
// — deterministic across platforms, so the stored-record budget is
// exercised at the depth that clipped the remedy on macOS (#7855) no matter
// which OS runs the test. The base is resolved through EvalSymlinks so the
// path the record stores is the kernel spelling: on darwin the lexical
// /var/... spelling is shorter than the /private/var/... the walk resolves
// to, and the depth premise must hold on the spelling the verdict sees.
func residueParentAtCapDepth(t *testing.T) string {
	t.Helper()
	base := canonicalTempDir(t)
	pad := residueTestParentRunes - len([]rune(base)) - 1
	require.GreaterOrEqual(t, pad, 1,
		"t.TempDir() must leave room for at least one padding rune")
	parent := filepath.Join(base, strings.Repeat("p", pad))
	require.NoError(t, os.Mkdir(parent, 0o755))
	return parent
}

// crashWindowRefusalRecord builds the crash-window rebuild refusal for
// inst/gw and stores it exactly the way the daemon does — wrapped in the
// respawn path's WorktreeUnavailableError, with the refusal LEADING the
// "%w (%w)" pair and the stat ENOENT trailing (#7855) — returning the
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
		Err: fmt.Errorf("%w (%w)", refusal, statErr)}
	require.True(t, inst.SetLostRestoreFailure(4, stored))
	snap := inst.LostRestoreFailureSnapshot()
	require.NotNil(t, snap)
	return snap.Error
}

// TestLostRestoreFailure_ResidueRefusalFitsCap_ProvenOurs pins #7847/#7855:
// the durable record — capped at lostRestoreFailureMaxRunes runes by
// sanitizeLostRestoreError — must keep the residue path and the move-it-back
// remedy at a realistic path DEPTH (a 200-rune parent, the size macOS tempdirs
// reach), so the user-facing row tells the operator what to do, not just that
// the restore refused. On master before #7855 the ~790-rune stored string was
// cut inside the mv command's second operand.
func TestLostRestoreFailure_ResidueRefusalFitsCap_ProvenOurs(t *testing.T) {
	repoRoot := residueTestRepo(t)
	const branch = "af/residue-fits-own"
	// A long, realistic leaf — the shape #7847 measured
	// (/home/user/Desktop/myrepo-fix-login-bug-with-a-long-name) — under a
	// parent padded to the depth #7855 measured on macOS, so the record
	// cannot borrow short-path slack the old message needed.
	wtParent := residueParentAtCapDepth(t)
	wtPath := filepath.Join(wtParent, "myrepo-fix-login-bug-with-a-long-name")
	gitOut(t, repoRoot, "worktree", "add", "-b", branch, wtPath)
	quarantine := simulateCrashWindowRename(t, wtPath)

	gw, err := git.NewGitWorktreeFromStorage(repoRoot, wtPath, "residue-fits-own", branch, "", false, true)
	require.NoError(t, err)
	inst := &Instance{Title: "residue-fits-own", Program: tmux.ProgramClaude, Path: repoRoot, gitWorktree: gw}

	record := crashWindowRefusalRecord(t, inst, gw, wtPath)
	assert.LessOrEqual(t, len([]rune(record)), lostRestoreFailureMaxRunes,
		"the stored record stays inside the durable cap by construction")
	wantCmd := shellsuggest.Command("cd", wtParent) + " && " +
		shellsuggest.Command("mv", "--", filepath.Base(quarantine), filepath.Base(wtPath))
	assert.Contains(t, record, wantCmd,
		"the stored record must lead with the remedy: mv the proven .af-source-* quarantine back onto the session path")
	assert.Contains(t, record, "to move this session's stranded worktree back",
		"the stored record must keep the remedy purpose, not just the fact of the refusal")
	assert.Contains(t, record, "(stat ",
		"the stat detail trails the refusal where the cap can clip it — the remedy leads the pair")
}

// TestLostRestoreFailure_ResidueRefusalFitsCap_Unverified is the unverified
// half of #7847/#7855: the stored record must keep the residue path (basename
// + the folder resolving it) and the move-it-out remedy at a realistic path
// depth — and must never tell the operator to move unverified residue into
// this session's path.
func TestLostRestoreFailure_ResidueRefusalFitsCap_Unverified(t *testing.T) {
	repoRoot := residueTestRepo(t)
	const branch = "af/residue-fits-unverified"
	gitOut(t, repoRoot, "branch", branch)
	wtParent := residueParentAtCapDepth(t)
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
	assert.Contains(t, record, filepath.Base(orphan),
		"the stored record must name the unverified residue")
	assert.Contains(t, record, "in "+wtParent,
		"the stored record must name the residue's folder so its basename resolves to the full path")
	assert.Contains(t, record, "move it out of that folder",
		"the stored record must keep the remedy: move unverified residue out of its folder once checked, then retry")
	assert.Contains(t, record, "once checked, then retry")
	assert.Contains(t, record, "do not move it into "+filepath.Base(wtPath)+" unless proven",
		"the stored record must keep the warning — the session path's leaf resolving against the named parent (#7855)")
	assert.Contains(t, record, "#7819",
		"the stored record must keep the citation at this depth")
	assert.NotContains(t, record, "move it back",
		"the stored record must never direct unverified residue into this session's path")
}

// TestLostRestoreFailure_ResidueRefusalFitsCap_MultiUnverified pins the
// counted-list half of #7847/#7855: when several unverified residue paths
// cannot all fit under the cap, the stored record names the first ones whole
// and counts the rest with a pointer to the daemon log — and that ", and N
// more" suffix is itself budgeted, so it cannot push the remedy past the cap.
// The 200-rune parent is deliberate: it forces real budget pressure on every
// platform, so only basenames + the counted suffix can survive.
func TestLostRestoreFailure_ResidueRefusalFitsCap_MultiUnverified(t *testing.T) {
	repoRoot := residueTestRepo(t)
	const branch = "af/residue-fits-multi"
	gitOut(t, repoRoot, "branch", branch)
	wtParent := residueParentAtCapDepth(t)
	wtPath := filepath.Join(wtParent, "myrepo-fix-login-bug")
	// Six unverified orphans (no .git pointer): sorted readdir order names
	// them in the record, so the "0"-hex one is always the first named.
	var orphans []string
	for _, h := range []string{"0", "1", "2", "b", "c", "d"} {
		orphan := filepath.Join(wtParent, ".af-source-"+strings.Repeat(h, 32))
		require.NoError(t, os.Mkdir(orphan, 0o755))
		orphans = append(orphans, orphan)
	}

	gw, err := git.NewGitWorktreeFromStorage(repoRoot, wtPath, "residue-fits-multi", branch, "", false, true)
	require.NoError(t, err)
	inst := &Instance{Title: "residue-fits-multi", Program: tmux.ProgramClaude, Path: repoRoot, gitWorktree: gw}

	record := crashWindowRefusalRecord(t, inst, gw, wtPath)
	assert.LessOrEqual(t, len([]rune(record)), lostRestoreFailureMaxRunes,
		"the stored record stays inside the durable cap by construction")
	assert.Contains(t, record, filepath.Base(orphans[0]),
		"the first residue is always named whole — the remedy acts on it")
	assert.Contains(t, record, "more (full list in the daemon log)",
		"the residue paths that do not fit are counted with a pointer to the daemon log, never silently truncated")
	assert.Contains(t, record, "in "+wtParent,
		"the stored record must name the residue's folder so the basenames resolve to full paths")
	assert.Contains(t, record, "move it out of that folder",
		"the stored record must keep the remedy after the counted residue list")
	assert.Contains(t, record, "do not move it into "+filepath.Base(wtPath)+" unless proven",
		"the stored record must keep the warning — the session path's leaf resolving against the named parent (#7855)")
	assert.Contains(t, record, "#7819",
		"the stored record must keep the citation at this depth")
	assert.NotContains(t, record, "move it back",
		"the stored record must never direct unverified residue into this session's path")
}

// TestLostRestoreFailure_ResidueRefusalFitsCap_QuotedMv pins the remedy
// command's quoting (#7847, #7855): the stored record leads with a literal
// `cd <parent> && mv <q> <s>` the operator is meant to paste, so a worktree
// path containing spaces or shell metacharacters must still arrive as exactly
// one operand per piece.
func TestLostRestoreFailure_ResidueRefusalFitsCap_QuotedMv(t *testing.T) {
	repoRoot := residueTestRepo(t)
	const branch = "af/residue-fits-space"
	// A parent with a space — the shape that splits an unquoted `cd a` or
	// `mv a b` into extra operands and makes the suggested recovery fail on
	// paste.
	wtParent := filepath.Join(testguard.SocketTempDir(t), "My Projects")
	require.NoError(t, os.MkdirAll(wtParent, 0o755))
	wtPath := filepath.Join(wtParent, "myrepo-fix-login-bug")
	gitOut(t, repoRoot, "worktree", "add", "-b", branch, wtPath)
	quarantine := simulateCrashWindowRename(t, wtPath)

	gw, err := git.NewGitWorktreeFromStorage(repoRoot, wtPath, "residue-fits-space", branch, "", false, true)
	require.NoError(t, err)
	inst := &Instance{Title: "residue-fits-space", Program: tmux.ProgramClaude, Path: repoRoot, gitWorktree: gw}

	record := crashWindowRefusalRecord(t, inst, gw, wtPath)
	assert.LessOrEqual(t, len([]rune(record)), lostRestoreFailureMaxRunes)
	wantCmd := shellsuggest.Command("cd", wtParent) + " && " +
		shellsuggest.Command("mv", "--", filepath.Base(quarantine), filepath.Base(wtPath))
	assert.Contains(t, record, wantCmd,
		"the stored record must carry the remedy with each path as exactly one operand")
	assert.NotContains(t, record, "cd "+wtParent,
		"an unquoted cd would split a spaced parent into extra operands when pasted")
}

// TestLostRestoreFailure_ResidueRefusal_AllForms is the property guard of
// #7855's second round: at the 200-rune padded parent AND a long session
// title — the wrapper alone spends a third of the cap — the stored record
// keeps every MANDATORY part of EVERY refusal form, because the record
// abbreviates its variable-length fields through fixed ladders (full path →
// leaf against the named parent → "the session path"; named residue →
// counted residue → the daemon log) rather than dropping a clause. Each
// subtest asserts exactly the parts its form must keep.
func TestLostRestoreFailure_ResidueRefusal_AllForms(t *testing.T) {
	// Deliberately long: realistic titles reach this size, and the recover
	// wrapper spends the title before the record is even written.
	const title = "fix-login-bug-and-modernize-the-auth-flow-across-the-whole-app"
	const wtLeaf = "myrepo-fix-login-bug-with-a-long-name"

	assertFitsCap := func(t *testing.T, record string) {
		t.Helper()
		assert.LessOrEqual(t, len([]rune(record)), lostRestoreFailureMaxRunes,
			"the stored record stays inside the durable cap by construction")
	}

	// stageOursCopy plants a `.af-copy-<hex>` sibling proven this session's:
	// it carries the worktree's own .git pointer — the same shape the
	// archive-side staged copy has — so the residue binding reads ours.
	stageOursCopy := func(t *testing.T, wtPath, hexStr string) string {
		t.Helper()
		copyDir := filepath.Join(filepath.Dir(wtPath), ".af-copy-"+hexStr)
		require.NoError(t, os.Mkdir(copyDir, 0o755))
		pointer, err := os.ReadFile(filepath.Join(wtPath, ".git"))
		require.NoError(t, err, "the worktree's .git pointer file is what proves the copy ours")
		require.NoError(t, os.WriteFile(filepath.Join(copyDir, ".git"), pointer, 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(copyDir, "staged.txt"), []byte("copy"), 0o644))
		return copyDir
	}

	t.Run("proven-ours quarantine", func(t *testing.T) {
		repoRoot := residueTestRepo(t)
		wtParent := residueParentAtCapDepth(t)
		wtPath := filepath.Join(wtParent, wtLeaf)
		gitOut(t, repoRoot, "worktree", "add", "-b", "af/forms-own", wtPath)
		quarantine := simulateCrashWindowRename(t, wtPath)

		gw, err := git.NewGitWorktreeFromStorage(repoRoot, wtPath, title, "af/forms-own", "", false, true)
		require.NoError(t, err)
		inst := &Instance{Title: title, Program: tmux.ProgramClaude, Path: repoRoot, gitWorktree: gw}

		record := crashWindowRefusalRecord(t, inst, gw, wtPath)
		assertFitsCap(t, record)
		wantCmd := shellsuggest.Command("cd", wtParent) + " && " +
			shellsuggest.Command("mv", "--", filepath.Base(quarantine), filepath.Base(wtPath))
		assert.Contains(t, record, wantCmd,
			"the proven-ours form must keep the literal remedy command")
		assert.Contains(t, record, "to move this session's stranded worktree back, then retry",
			"the proven-ours form must keep the remedy purpose and the retry")
	})

	t.Run("copy-only", func(t *testing.T) {
		repoRoot := residueTestRepo(t)
		wtParent := residueParentAtCapDepth(t)
		wtPath := filepath.Join(wtParent, wtLeaf)
		gitOut(t, repoRoot, "worktree", "add", "-b", "af/forms-copy", wtPath)
		copyDir := stageOursCopy(t, wtPath, strings.Repeat("b", 32))
		require.NoError(t, os.RemoveAll(wtPath))

		gw, err := git.NewGitWorktreeFromStorage(repoRoot, wtPath, title, "af/forms-copy", "", false, true)
		require.NoError(t, err)
		inst := &Instance{Title: title, Program: tmux.ProgramClaude, Path: repoRoot, gitWorktree: gw}

		record := crashWindowRefusalRecord(t, inst, gw, wtPath)
		assertFitsCap(t, record)
		assert.Contains(t, record, filepath.Base(copyDir),
			"the copy-only form must name the staged copy")
		assert.Contains(t, record, "in "+wtParent,
			"the copy-only form must name the residue's folder")
		assert.Contains(t, record, "proven ours",
			"the copy-only form must say the binding read this session's")
		assert.Contains(t, record, "possibly incomplete",
			"the copy-only form must keep the reason the copy is not the recovery source")
		assert.Contains(t, record, "do not move it into the session path unless verified complete",
			"the copy-only form must keep the warning — abbreviated to the session-path reference at this depth")
		assert.Contains(t, record, "then retry",
			"the copy-only form must keep the retry")
		assert.NotContains(t, record, "stranded worktree back",
			"the copy-only form must never name the copy as the move-back source")
	})

	t.Run("unverified", func(t *testing.T) {
		repoRoot := residueTestRepo(t)
		gitOut(t, repoRoot, "branch", "af/forms-unverified")
		wtParent := residueParentAtCapDepth(t)
		wtPath := filepath.Join(wtParent, wtLeaf)
		orphan := filepath.Join(wtParent, ".af-source-"+strings.Repeat("0", 32))
		require.NoError(t, os.Mkdir(orphan, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(orphan, "stale.txt"), []byte("orphan"), 0o644))

		gw, err := git.NewGitWorktreeFromStorage(repoRoot, wtPath, title, "af/forms-unverified", "", false, true)
		require.NoError(t, err)
		inst := &Instance{Title: title, Program: tmux.ProgramClaude, Path: repoRoot, gitWorktree: gw}

		record := crashWindowRefusalRecord(t, inst, gw, wtPath)
		assertFitsCap(t, record)
		assert.Contains(t, record, filepath.Base(orphan),
			"the unverified form must name the residue")
		assert.Contains(t, record, "in "+wtParent,
			"the unverified form must name the residue's folder")
		assert.Contains(t, record, "could not verify",
			"the unverified form must keep the verdict")
		assert.Contains(t, record, "move it out of that folder once checked, then retry",
			"the unverified form must keep the remedy")
		assert.Contains(t, record, "do not move it into the session path unless proven",
			"the unverified form must keep the warning — abbreviated to the session-path reference at this depth")
		assert.Contains(t, record, "#7819",
			"the unverified form must keep the citation")
	})

	t.Run("unverified-multiple", func(t *testing.T) {
		repoRoot := residueTestRepo(t)
		gitOut(t, repoRoot, "branch", "af/forms-multi")
		wtParent := residueParentAtCapDepth(t)
		wtPath := filepath.Join(wtParent, wtLeaf)
		var orphans []string
		for _, h := range []string{"0", "1", "2"} {
			orphan := filepath.Join(wtParent, ".af-source-"+strings.Repeat(h, 32))
			require.NoError(t, os.Mkdir(orphan, 0o755))
			orphans = append(orphans, orphan)
		}

		gw, err := git.NewGitWorktreeFromStorage(repoRoot, wtPath, title, "af/forms-multi", "", false, true)
		require.NoError(t, err)
		inst := &Instance{Title: title, Program: tmux.ProgramClaude, Path: repoRoot, gitWorktree: gw}

		record := crashWindowRefusalRecord(t, inst, gw, wtPath)
		assertFitsCap(t, record)
		assert.Contains(t, record, filepath.Base(orphans[0]),
			"the first residue is named whole")
		assert.Contains(t, record, "and 2 more (full list in the daemon log)",
			"the rest are counted with a pointer to the daemon log")
		// The parent is the field that gives at this depth: its head and
		// tail survive the elision so the residue still resolves.
		assert.Contains(t, record, "in "+wtParent[:30],
			"the parent's head survives its middle elision")
		assert.Contains(t, record, "…",
			"the elided parent carries the same mark the cap leaves")
		assert.Contains(t, record, "move it out of that folder once checked, then retry",
			"the unverified form must keep the remedy")
		assert.Contains(t, record, "do not move it into "+filepath.Base(wtPath)+" unless proven",
			"the unverified form must keep the warning — the session path's leaf resolving against the named parent")
		assert.Contains(t, record, "#7819",
			"the unverified form must keep the citation")
	})

	t.Run("mixed residue", func(t *testing.T) {
		repoRoot := residueTestRepo(t)
		wtParent := residueParentAtCapDepth(t)
		wtPath := filepath.Join(wtParent, wtLeaf)
		gitOut(t, repoRoot, "worktree", "add", "-b", "af/forms-mixed", wtPath)
		stageOursCopy(t, wtPath, strings.Repeat("b", 32))
		orphan := filepath.Join(wtParent, ".af-source-"+strings.Repeat("5", 32))
		require.NoError(t, os.Mkdir(orphan, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(orphan, "stale.txt"), []byte("orphan"), 0o644))
		quarantine := simulateCrashWindowRename(t, wtPath)

		gw, err := git.NewGitWorktreeFromStorage(repoRoot, wtPath, title, "af/forms-mixed", "", false, true)
		require.NoError(t, err)
		inst := &Instance{Title: title, Program: tmux.ProgramClaude, Path: repoRoot, gitWorktree: gw}

		record := crashWindowRefusalRecord(t, inst, gw, wtPath)
		assertFitsCap(t, record)
		assert.Contains(t, record, "mv -- "+filepath.Base(quarantine),
			"the mixed form must keep the quarantine the remedy acts on — the parentless mv floor at this squeeze")
		assert.Contains(t, record, "move this session's stranded worktree back",
			"the mixed form must keep the remedy purpose")
		assert.Contains(t, record, "then retry",
			"the mixed form must keep the retry")
		assert.Contains(t, record, "unverified residue "+filepath.Base(orphan),
			"the mixed form must name the unverified residue while the clause fits")
		assert.Contains(t, record, "do not move it into the session path unless proven",
			"the mixed form must keep the unverified warning")
		assert.Contains(t, record, "#7819",
			"the mixed form must keep the citation")
		assert.Contains(t, record, "staged copy",
			"the mixed form must record the staged copy's presence")
		assert.Contains(t, record, "recover from the quarantine, never the copy",
			"the mixed form must keep the warning against recovering from the copy")
	})
}

// TestMissingWorktreeRebuildRefusal_DanglingBindingSaysFixRemove pins #7900
// R3: an Unknown verdict that came from a component that is present but
// unresolvable — here the recorded source path spelled through a dangling
// symlink, ErrUnresolvedComponent — is unproven but NOT retryable. The link
// itself is readable and no retry makes it resolve, so the refusal names
// the fix-or-remove remedy — "<component> is a dangling link: fix or remove
// it, or move the residue out" — never the retry wording. The verdict stays
// Unknown and the refusal still refuses fail-closed.
func TestMissingWorktreeRebuildRefusal_DanglingBindingSaysFixRemove(t *testing.T) {
	repoRoot := residueTestRepo(t)
	const branch = "af/dangling-bind"
	wtParent := canonicalTempDir(t)
	realWt := filepath.Join(wtParent, "wt-real")
	gitOut(t, repoRoot, "worktree", "add", "-b", branch, realWt)
	quarantine := filepath.Join(wtParent, ".af-source-"+strings.Repeat("a", 32))
	require.NoError(t, os.Rename(realWt, quarantine))
	// The recorded source path is spelled through a dangling symlink inside
	// the real parent: the anchor comparison can only fall back past it
	// lexically, which is a guess — the component is not proven absent, so
	// the bind's error is unproven rather than an answered comparison.
	wtPath := filepath.Join(wtParent, "link")
	require.NoError(t, os.Symlink(filepath.Join(wtParent, "nonexistent"), wtPath))

	// The fixture must produce the intended verdict on linux and darwin
	// alike — assert the bind the refusal will see before asserting on its
	// wording, so a platform that spells the fixture differently fails here
	// rather than downstream of a clause it never produced.
	binding, bindErr := git.BindAFRelocationResidue(quarantine, wtPath, repoRoot)
	assert.Equal(t, git.ResidueUnknown, binding,
		"premise: a dangling-spelled anchor binds Unknown on both platforms")
	require.True(t, git.ResidueBindingUnproven(bindErr),
		"premise: the dangling component is unproven on both platforms: %v", bindErr)
	require.True(t, git.ResidueBindingDangling(bindErr),
		"premise: the unproven class is dangling-link on both platforms: %v", bindErr)
	// The clause names whatever component the bind's error chain carried —
	// computed off the real bind so the assertion tracks the failed
	// component rather than guessing which path the PathError kept. The
	// refusal's caller names the residue itself when the chain keeps none.
	component := git.ResidueUnprovenComponent(bindErr)
	if component == "" {
		component = quarantine
	}

	gw, err := git.NewGitWorktreeFromStorage(repoRoot, wtPath, "dangling-bind", branch, "", false, true)
	require.NoError(t, err)
	inst := &Instance{Title: "dangling-bind", Program: tmux.ProgramClaude, Path: repoRoot, gitWorktree: gw}

	err = missingWorktreeRebuildRefusal(inst, gw, true, "claude")
	require.Error(t, err, "an unverified residue must refuse fail-closed")
	assert.ErrorIs(t, err, git.ErrRelocateStateUnknown,
		"the verdict stays Unknown — unproven never becomes ours or foreign")
	assert.Contains(t, err.Error(), filepath.Base(component)+" is a dangling link: fix or remove it, or move the residue out",
		"the refusal names the dangling component — its basename resolving against the parent the record already names — and the fix-or-remove remedy")
	assert.NotContains(t, err.Error(), "retry once",
		"a dangling link is itself readable — the retry promise is for a read a retry can complete, never for a link whose target is gone")
	assert.Contains(t, err.Error(), filepath.Base(quarantine),
		"the refusal still names the unverified residue")
	assert.NotContains(t, err.Error(), "move it back to "+wtPath,
		"the refusal for an unverified residue must never direct it into this session's path")
}

// TestMissingWorktreeRebuildRefusal_UnprovenBindingSaysTransient pins #7900:
// an Unknown verdict that came from a component af could not read — here
// the recorded source path mode-000, so the binding's path comparison rests
// on a lookup that did not happen — is transient. The refusal must say so
// and name the component: "retry once <component> is readable", not only
// "may belong to another session… move it out". The verdict stays Unknown —
// the residue is still unverified, never promoted — and the refusal still
// refuses fail-closed.
func TestMissingWorktreeRebuildRefusal_UnprovenBindingSaysTransient(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod-based unreadability is meaningless as root")
	}
	repoRoot := residueTestRepo(t)
	const branch = "af/unproven-bind"
	wtParent := canonicalTempDir(t)
	realWt := filepath.Join(wtParent, "wt-real")
	gitOut(t, repoRoot, "worktree", "add", "-b", branch, realWt)
	quarantine := filepath.Join(wtParent, ".af-source-"+strings.Repeat("a", 32))
	require.NoError(t, os.Rename(realWt, quarantine))
	// The recorded source path is a real directory af cannot open: the
	// anchor comparison's resolution fails EACCES — a lookup that did not
	// happen, the retryable class of unproven.
	wtPath := filepath.Join(wtParent, "wt-mode000")
	require.NoError(t, os.Mkdir(wtPath, 0o700))
	require.NoError(t, os.Chmod(wtPath, 0o000))
	t.Cleanup(func() { _ = os.Chmod(wtPath, 0o700) })

	gw, err := git.NewGitWorktreeFromStorage(repoRoot, wtPath, "unproven-bind", branch, "", false, true)
	require.NoError(t, err)
	inst := &Instance{Title: "unproven-bind", Program: tmux.ProgramClaude, Path: repoRoot, gitWorktree: gw}

	// The component the clause names is whatever path the binding's error
	// chain carries — read it off the real bind so the assertion tracks the
	// failed component rather than guessing which path the PathError kept.
	// The verdict is pinned too: the fixture must produce the intended
	// unproven class on linux and darwin alike.
	binding, bindErr := git.BindAFRelocationResidue(quarantine, wtPath, repoRoot)
	assert.Equal(t, git.ResidueUnknown, binding,
		"premise: an unreadable anchor binds Unknown on both platforms")
	require.True(t, git.ResidueBindingUnproven(bindErr),
		"premise: the chmod-000 anchor makes the bind unproven, not an answered comparison: %v", bindErr)
	require.False(t, git.ResidueBindingDangling(bindErr),
		"premise: the retryable class is NOT the dangling-link class: %v", bindErr)
	component := git.ResidueUnprovenComponent(bindErr)
	require.NotEmpty(t, component, "the unproven bind names a component")

	err = missingWorktreeRebuildRefusal(inst, gw, true, "claude")
	require.Error(t, err, "an unverified residue must refuse fail-closed")
	assert.ErrorIs(t, err, git.ErrRelocateStateUnknown,
		"the verdict stays Unknown — unproven never becomes ours or foreign")
	assert.Contains(t, err.Error(), "retry once "+filepath.Base(component)+" is readable",
		"the refusal must name the unreadable component — its basename resolving against the parent the record already names — and say the retry may clear once it is readable")
	assert.Contains(t, err.Error(), "could not read the binding",
		"the transient clause says WHY the binding failed")
	assert.Contains(t, err.Error(), filepath.Base(quarantine),
		"the refusal still names the unverified residue")
	assert.NotContains(t, err.Error(), "move it back to "+wtPath,
		"the refusal for an unverified residue must never direct it into this session's path")
}

// TestLostRestoreFailure_ResidueRefusalFitsCap_Unproven pins the unproven
// clause at the depth that clipped the remedy on macOS (#7855): at a
// 200-rune parent the unproven clause rides the cap ladder like every other
// mandatory clause — naming the component's basename while that fits and
// falling to its floor below — so the hint is never silently dropped the
// way optional context is. The fixture spells the anchor through a dangling
// link, so the mandatory clause is the dangling variant (#7900 R3): the
// fix-or-remove hint, never the retry promise. Either form keeps the
// dangling-link wording and the remedy.
func TestLostRestoreFailure_ResidueRefusalFitsCap_Unproven(t *testing.T) {
	repoRoot := residueTestRepo(t)
	const branch = "af/unproven-cap"
	wtParent := residueParentAtCapDepth(t)
	realWt := filepath.Join(wtParent, "wt-real")
	gitOut(t, repoRoot, "worktree", "add", "-b", branch, realWt)
	quarantine := filepath.Join(wtParent, ".af-source-"+strings.Repeat("a", 32))
	require.NoError(t, os.Rename(realWt, quarantine))
	wtPath := filepath.Join(wtParent, "link")
	require.NoError(t, os.Symlink(filepath.Join(wtParent, "nonexistent"), wtPath))

	// Pin the verdict the fixture produces on linux and darwin alike, so a
	// platform spelling difference fails at the premise rather than
	// downstream of a clause it never produced.
	binding, bindErr := git.BindAFRelocationResidue(quarantine, wtPath, repoRoot)
	assert.Equal(t, git.ResidueUnknown, binding,
		"premise: a dangling-spelled anchor binds Unknown on both platforms")
	require.True(t, git.ResidueBindingDangling(bindErr),
		"premise: the unproven class is dangling-link on both platforms: %v", bindErr)

	gw, err := git.NewGitWorktreeFromStorage(repoRoot, wtPath, "unproven-cap", branch, "", false, true)
	require.NoError(t, err)
	inst := &Instance{Title: "unproven-cap", Program: tmux.ProgramClaude, Path: repoRoot, gitWorktree: gw}

	record := crashWindowRefusalRecord(t, inst, gw, wtPath)
	assert.LessOrEqual(t, len([]rune(record)), lostRestoreFailureMaxRunes,
		"the stored record stays inside the durable cap by construction")
	assert.Contains(t, record, "dangling link",
		"the unproven clause survives the cap at whichever rung fits — a mandatory clause is never dropped")
	assert.Contains(t, record, "move the residue out",
		"the dangling clause keeps its fix-or-remove remedy at whichever rung fits")
	assert.NotContains(t, record, "retry once",
		"a dangling link never gets the retry promise — the link is readable; its target is gone")
	assert.Contains(t, record, filepath.Base(quarantine),
		"the unverified residue is still named")
}

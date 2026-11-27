package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// arbitrationInstance builds the Instance the rebuild-refusal tests run
// against: a stored-shape GitWorktree bound to an instance id the durable
// arbitration keys on, inside an isolated AF home.
func arbitrationInstance(t *testing.T, repoRoot, wtPath, title, branch, id string) (*Instance, *git.GitWorktree) {
	t.Helper()
	gw, err := git.NewGitWorktreeFromStorage(repoRoot, wtPath, title, branch, "", false, true)
	require.NoError(t, err)
	return &Instance{ID: id, Title: title, Program: tmux.ProgramClaude, Path: repoRoot, gitWorktree: gw}, gw
}

// restoreMoveInFlightRecord installs a move_in_flight record naming the given
// residue the way a crashed archive would have left it durably: quarantine
// beside the recorded source, staging beside the publish destination.
func restoreMoveInFlightRecord(t *testing.T, gw *git.GitWorktree, dest, quarantine, staging string) {
	t.Helper()
	require.NoError(t, gw.RestoreRelocationRecovery(git.RelocationRecovery{
		State:             git.RelocationRecoveryMoveInFlight,
		AlternatePath:     dest,
		IdentityKnown:     true,
		Device:            42,
		Inode:             4242,
		FileType:          4,
		QuarantinePath:    quarantine,
		StagingPath:       staging,
		DestDevice:        42,
		DestInode:         999,
		DestFileType:      4,
		DestIdentityKnown: true,
	}))
}

// TestMissingWorktreeRebuildRefusal_RecordNamesResidue_PrunedLeaf is #7819 R1:
// once af's own durable move record names the `.af-source-*` quarantine, a
// PRUNED git leaf can no longer degrade the verdict to Unknown. On master the
// only proof was the leaf — `git worktree prune` (which af itself runs on
// every archive, kill and setup) erased it, so the same footprint refused
// fail-closed with "could not verify". Now the record answers ours outright
// and the refusal keeps the move-it-back recovery guidance.
func TestMissingWorktreeRebuildRefusal_RecordNamesResidue_PrunedLeaf(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := residueTestRepo(t)
	const branch = "af/record-pruned"
	wtParent := t.TempDir()
	wtPath := filepath.Join(wtParent, "wt-record-pruned")
	gitOut(t, repoRoot, "worktree", "add", "-b", branch, wtPath)
	require.NoError(t, os.WriteFile(filepath.Join(wtPath, "dirty.txt"), []byte("uncommitted"), 0o644))
	quarantine := simulateCrashWindowRename(t, wtPath)
	// af prunes the registration leaf on every archive, kill and setup — the
	// exact reason the leaf can never prove ownership (#7819).
	gitOut(t, repoRoot, "worktree", "prune")

	inst, gw := arbitrationInstance(t, repoRoot, wtPath, "record-pruned", branch, "self-record-pruned")
	staging := filepath.Join(wtParent, ".af-copy-"+strings.Repeat("b", 32))
	restoreMoveInFlightRecord(t, gw, filepath.Join(wtParent, "wt-dest"), quarantine, staging)

	err := missingWorktreeRebuildRefusal(inst, gw, true, "claude")
	require.Error(t, err, "residue this session's own durable record names must refuse the rebuild")
	assert.ErrorIs(t, err, git.ErrRelocateStateUnknown)
	assert.Contains(t, err.Error(), filepath.Base(quarantine))
	assert.Contains(t, err.Error(),
		shellsuggest.Command("mv", "--", filepath.Base(quarantine), filepath.Base(wtPath)),
		"a record-named quarantine is proven ours: the refusal must direct recovery from it, exactly like a leaf-proven one")
	assert.NotContains(t, err.Error(), "could not verify",
		"the durable record answers ownership without the leaf — the unverified wording is gone")
}

// TestMissingWorktreeRebuildRefusal_RecordNamesResidue_RecycledLeaf is #7819 R2
// and the #7835 R4 direction: git recycles leaf names, so a new worktree
// registered under this leaf's old name flips the same residue's verdict to
// foreign on master — this session's own stranded worktree then fails to
// block its own rebuild. The durable record outranks the recycled leaf: the
// residue still refuses as ours.
func TestMissingWorktreeRebuildRefusal_RecordNamesResidue_RecycledLeaf(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := residueTestRepo(t)
	const branch = "af/record-recycled"
	const otherBranch = "af/recycled-usurper"
	gitOut(t, repoRoot, "branch", otherBranch)
	wtParent := t.TempDir()
	wtPath := filepath.Join(wtParent, "wt-recycled")
	gitOut(t, repoRoot, "worktree", "add", "-b", branch, wtPath)
	quarantine := simulateCrashWindowRename(t, wtPath)
	gitOut(t, repoRoot, "worktree", "prune")
	// Recycle the leaf: a new worktree under the SAME basename re-creates the
	// .git/worktrees/wt-recycled registration pointing at the new path, so the
	// quarantine's own .git pointer now resolves to a backpointer that names
	// someone else's path — foreign under leaf inference.
	usurperParent := t.TempDir()
	gitOut(t, repoRoot, "worktree", "add", filepath.Join(usurperParent, "wt-recycled"), otherBranch)

	inst, gw := arbitrationInstance(t, repoRoot, wtPath, "record-recycled", branch, "self-record-recycled")
	staging := filepath.Join(wtParent, ".af-copy-"+strings.Repeat("c", 32))
	restoreMoveInFlightRecord(t, gw, filepath.Join(wtParent, "wt-dest"), quarantine, staging)

	err := missingWorktreeRebuildRefusal(inst, gw, true, "claude")
	require.Error(t, err,
		"a recycled leaf must not flip record-named residue to foreign — the record still proves it ours")
	assert.ErrorIs(t, err, git.ErrRelocateStateUnknown)
	assert.Contains(t, err.Error(), filepath.Base(quarantine))
	assert.Contains(t, err.Error(),
		shellsuggest.Command("mv", "--", filepath.Base(quarantine), filepath.Base(wtPath)),
		"the refusal treats record-named residue as this session's own stranded work — it directs recovery from it")
}

// TestMissingWorktreeRebuildRefusal_OwnRetainedTreeIsNotResidue is #7819 R3:
// this session's own persisted ArchiveReport.RetainedTrees is kept work
// already accounted for, so a retained tree with residue-shaped naming is
// adopted forward, never refused as crash residue. On master the same tree
// reads Unknown (it has no linked-worktree .git binding to prove) and refuses.
func TestMissingWorktreeRebuildRefusal_OwnRetainedTreeIsNotResidue(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := residueTestRepo(t)
	const branch = "af/retained-own"
	gitOut(t, repoRoot, "branch", branch)
	wtParent := t.TempDir()
	wtPath := filepath.Join(wtParent, "wt-retained-own")
	// The worktree path is absent but a retained tree sits beside it under a
	// residue-shaped name — the shape a skipped-file retention can carry.
	retained := filepath.Join(wtParent, ".af-copy-"+strings.Repeat("d", 32))
	require.NoError(t, os.MkdirAll(filepath.Join(retained, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(retained, "sub", "kept.txt"), []byte("kept"), 0o644))

	inst, gw := arbitrationInstance(t, repoRoot, wtPath, "retained-own", branch, "self-retained-own")
	gw.RestoreArchiveReport(git.ArchiveReport{
		RetainedTrees: []git.ArchiveRetainedTree{{Path: retained}},
	})

	err := missingWorktreeRebuildRefusal(inst, gw, true, "claude")
	assert.NoError(t, err,
		"a tree this session's own retained-tree report names is carried forward, never refused as crash residue")
	assert.DirExists(t, retained, "the retained tree is adopted forward untouched")
}

// TestMissingWorktreeRebuildRefusal_ForeignRecordDoesNotBlock is #7835 R3:
// one session's crash residue must not refuse every OTHER session's rebuild.
// The residue is an orphan — nothing a leaf could prove — but another
// session's durable record names it, which makes it foreign: never blocking,
// never adopted, never moved by this session.
func TestMissingWorktreeRebuildRefusal_ForeignRecordDoesNotBlock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	repoRoot := residueTestRepo(t)
	const branch = "af/foreign-record-own"
	gitOut(t, repoRoot, "branch", branch)
	wtParent := t.TempDir()
	wtPath := filepath.Join(wtParent, "wt-foreign-record")
	orphan := filepath.Join(wtParent, ".af-source-"+strings.Repeat("e", 32))
	require.NoError(t, os.Mkdir(orphan, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(orphan, "stranded.txt"), []byte("someone else's"), 0o644))

	// The OTHER session's durable record names the residue — stored in the
	// projected form a same-release row takes on disk (claim_stale carrying
	// move_in_flight in its additive lifecycle).
	foreignRows, err := json.Marshal([]InstanceData{{
		ID:    "other-inst",
		Title: "other-session",
		Path:  repoRoot,
		Worktree: GitWorktreeData{
			RepoPath:     repoRoot,
			WorktreePath: filepath.Join(wtParent, "wt-other"),
			RelocationRecovery: &GitWorktreeRelocationRecoveryData{
				State:            git.RelocationRecoveryClaimStale,
				CleanupLifecycle: git.RelocationRecoveryMoveInFlight,
				AlternatePath:    filepath.Join(wtParent, "wt-other-dest"),
				IdentityKnown:    true,
				QuarantinePath:   orphan,
				StagingPath:      filepath.Join(wtParent, ".af-copy-"+strings.Repeat("f", 32)),
			},
		},
	}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("otherrepo", foreignRows))

	inst, gw := arbitrationInstance(t, repoRoot, wtPath, "foreign-record", branch, "self-foreign-record")
	err = missingWorktreeRebuildRefusal(inst, gw, true, "claude")
	assert.NoError(t, err,
		"residue another session's durable record names is foreign — it must not block this session's rebuild")
	assert.DirExists(t, orphan, "the foreign session's residue is never touched")
}

// TestMissingWorktreeRebuildRefusal_UnreadableStoreNamesItself pins the
// maintainer-narrowed fail-closed (#7819 review): an unreadable session store
// only hardens a refusal when a residue candidate actually exists that the
// store could have claimed — and the refusal names that store so the operator
// knows what to repair. With no residue at all the same broken store never
// fires.
func TestMissingWorktreeRebuildRefusal_UnreadableStoreNamesItself(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	repoRoot := residueTestRepo(t)
	const branch = "af/unreadable-store"
	gitOut(t, repoRoot, "branch", branch)
	wtParent := t.TempDir()
	wtPath := filepath.Join(wtParent, "wt-unreadable")
	orphan := filepath.Join(wtParent, ".af-source-"+strings.Repeat("0", 32))
	require.NoError(t, os.Mkdir(orphan, 0o755))

	badRepo := "corruptrepo"
	require.NoError(t, config.SaveRepoInstances(badRepo, []byte("{not valid json")))

	inst, gw := arbitrationInstance(t, repoRoot, wtPath, "unreadable-store", branch, "self-unreadable")
	err := missingWorktreeRebuildRefusal(inst, gw, true, "claude")
	require.Error(t, err,
		"unverifiable residue still refuses fail-closed when the foreign-record enumeration cannot run")
	assert.ErrorIs(t, err, git.ErrRelocateStateUnknown)
	assert.Contains(t, err.Error(), "for repo "+badRepo,
		"the refusal names the unreadable session store's repo so the operator knows what to repair")
	assert.Contains(t, err.Error(), "could not rule this out",
		"the refusal says the foreign-record enumeration could not clear the candidate")

	// And with NO residue candidate the same unreadable store never fires:
	// the bare-ENOENT rebuild proceeds.
	require.NoError(t, os.RemoveAll(orphan))
	err = missingWorktreeRebuildRefusal(inst, gw, true, "claude")
	assert.NoError(t, err,
		"an unreadable store with no residue candidate present must not block the rebuild")
}

// TestRebuildAbsentArchivedWorktree_RefusesOverRecordNamedQuarantine pins the
// archived-row restore route's arbitration (#7819): RebuildAbsentArchivedWorktree
// re-creates the recorded path from the kept branch, and on master it did so
// with no residue consultation at all — a `.af-source-*` quarantine this
// session's durable record names was rebuilt over, stranding the complete
// worktree. Now the same record that names the residue refuses the rebuild
// with recovery directed at the quarantine.
func TestRebuildAbsentArchivedWorktree_RefusesOverRecordNamedQuarantine(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := residueTestRepo(t)
	const branch = "af/rebuild-record-quarantine"
	wtParent := t.TempDir()
	wtPath := filepath.Join(wtParent, "wt-rebuild-record")
	gitOut(t, repoRoot, "worktree", "add", "-b", branch, wtPath)
	require.NoError(t, os.WriteFile(filepath.Join(wtPath, "dirty.txt"), []byte("uncommitted"), 0o644))
	quarantine := simulateCrashWindowRename(t, wtPath)

	inst, gw := arbitrationInstance(t, repoRoot, wtPath, "rebuild-record", branch, "self-rebuild-record")
	staging := filepath.Join(wtParent, ".af-copy-"+strings.Repeat("9", 32))
	restoreMoveInFlightRecord(t, gw, filepath.Join(wtParent, "wt-dest"), quarantine, staging)

	dest := filepath.Join(t.TempDir(), "rebuilt")
	err := inst.RebuildAbsentArchivedWorktree(dest)
	require.Error(t, err,
		"rebuilding the recorded path over a quarantine this session's record names must refuse")
	assert.ErrorIs(t, err, git.ErrRelocateStateUnknown)
	assert.Contains(t, err.Error(), filepath.Base(quarantine),
		"the refusal names the stranded quarantine")
	assert.DirExists(t, quarantine, "the stranded worktree survives the refused rebuild")
	dirty, rerr := os.ReadFile(filepath.Join(quarantine, "dirty.txt"))
	require.NoError(t, rerr)
	assert.Equal(t, "uncommitted", string(dirty))
	assert.NoDirExists(t, dest, "the refused route never re-creates the destination")
}

// TestRebuildAbsentArchivedWorktree_RefusesOverUnverifiedResidue pins the
// record-free direction on the same route: an `.af-source-*` candidate no
// durable record names keeps master's leaf inference, and an unreadable
// binding still refuses fail-closed — named as unverified rather than this
// session's stranded work.
func TestRebuildAbsentArchivedWorktree_RefusesOverUnverifiedResidue(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := residueTestRepo(t)
	const branch = "af/rebuild-unverified"
	gitOut(t, repoRoot, "branch", branch)
	wtParent := t.TempDir()
	wtPath := filepath.Join(wtParent, "wt-rebuild-unverified")
	orphan := filepath.Join(wtParent, ".af-source-"+strings.Repeat("7", 32))
	require.NoError(t, os.Mkdir(orphan, 0o755))

	inst, _ := arbitrationInstance(t, repoRoot, wtPath, "rebuild-unverified", branch, "self-rebuild-unverified")
	dest := filepath.Join(t.TempDir(), "rebuilt")
	err := inst.RebuildAbsentArchivedWorktree(dest)
	require.Error(t, err,
		"rebuilding over unverifiable residue must refuse fail-closed")
	assert.ErrorIs(t, err, git.ErrRelocateStateUnknown)
	assert.Contains(t, err.Error(), filepath.Base(orphan))
	assert.Contains(t, err.Error(), "could not verify",
		"record-free residue whose binding cannot be read stays unverified — never claimed as this session's")
}

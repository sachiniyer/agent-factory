package git

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the durable move_in_flight record of #7819: a cross-device
// move names its `.af-source-*` quarantine and `.af-copy-*` staging paths in a
// record that is persisted BEFORE any of those paths exist, carries the staged
// destination identity ahead of the quarantine rename, and retires only when
// the move finishes or a retry's claim resolution consumes it. None of this
// exists on master, where the same crash points leave residue no record names.

// crashRecordFile is where the child harness writes the checkpoint-visible
// recovery record — the stand-in for the daemon's fsynced session-row write.
type crashRecordFile struct {
	RelocationRecovery
	WorktreePath string `json:"worktree_path"`
}

// TestMoveInFlightRecord_NamesResidueBeforeItExists drives a real
// cross-device ArchiveWorktree and asserts, inside the installed durable
// checkpoint, that the record names BOTH residue paths while neither exists
// yet (the V1 write) and that the staged destination identity joins the
// record before the quarantine rename (the V2 write).
func TestMoveInFlightRecord_NamesResidueBeforeItExists(t *testing.T) {
	gw, _, wtPath := archiveTestWorktree(t)
	secondFS := findOrMakeSecondFilesystem(t)
	dest := filepath.Join(secondFS, "archived-mif")

	type write struct {
		recovery           RelocationRecovery
		residueAtWriteTime []string
	}
	var writes []write
	restore := gw.SetRelocationRecordCheckpoint(func() error {
		_, recovery, ok := gw.RelocationSnapshot()
		require.True(t, ok, "the durable checkpoint must see the in-flight record")
		require.Equal(t, RelocationRecoveryMoveInFlight, recovery.State)
		residue, err := ListAFRelocationResidue(filepath.Dir(wtPath), secondFS)
		require.NoError(t, err)
		writes = append(writes, write{recovery: recovery, residueAtWriteTime: residue})
		// At the V2 write the staged copy exists and the quarantine does not
		// yet: the record is durable ahead of the commit boundary it precedes.
		if recovery.DestIdentityKnown {
			staged, serr := os.Lstat(recovery.StagingPath)
			require.NoError(t, serr, "at the V2 write the staged copy must exist")
			require.True(t, staged.IsDir())
			assert.NoDirExists(t, recovery.QuarantinePath)
		}
		return nil
	})
	defer restore()

	require.NoError(t, gw.ArchiveWorktree(dest))
	require.Len(t, writes, 2,
		"a cross-device move must publish the in-flight record before the copy and again before the quarantine rename")

	// V1: both residue paths are named BEFORE either exists. This is the whole
	// durability property — a crash anywhere later leaves named residue.
	assert.Equal(t, RelocationRecoveryMoveInFlight, writes[0].recovery.State)
	assert.True(t, privateMoveResidueName(filepath.Base(writes[0].recovery.QuarantinePath)),
		"the V1 record must name the .af-source-* quarantine before it exists")
	assert.True(t, privateMoveResidueName(filepath.Base(writes[0].recovery.StagingPath)),
		"the V1 record must name the .af-copy-* staging before it exists")
	assert.Equal(t, filepath.Dir(wtPath), filepath.Dir(writes[0].recovery.QuarantinePath))
	assert.Equal(t, filepath.Dir(dest), filepath.Dir(writes[0].recovery.StagingPath))
	assert.Empty(t, writes[0].residueAtWriteTime,
		"the V1 write must land before ANY residue exists; nothing may be named retroactively")
	assert.Equal(t, dest, writes[0].recovery.AlternatePath)
	assert.False(t, writes[0].recovery.DestIdentityKnown)

	// V2: the staged destination's identity is durable before the quarantine
	// rename — the only proof a post-publish crash can resolve to dest, since
	// a copy's inode differs from the source's.
	assert.True(t, writes[1].recovery.DestIdentityKnown)
	assert.Equal(t, writes[0].recovery.QuarantinePath, writes[1].recovery.QuarantinePath)
	assert.Equal(t, writes[0].recovery.StagingPath, writes[1].recovery.StagingPath)
	// The V2 write still lands before the quarantine rename: the staged copy
	// is the only residue present at that point.
	assert.ElementsMatch(t, []string{writes[1].recovery.StagingPath}, writes[1].residueAtWriteTime)

	// Success retires the record entirely.
	_, _, unresolved := gw.RelocationSnapshot()
	assert.False(t, unresolved, "a completed move retires its in-flight record")
	assert.NoDirExists(t, writes[1].recovery.QuarantinePath)
	assert.NoDirExists(t, writes[1].recovery.StagingPath)
	assert.DirExists(t, dest)
}

// TestMoveInFlightRecord_PersistFailureAbortsBeforeResidue pins the abort: a
// failed durable write at V1 fails the move before a single byte is staged,
// because residue without its record is exactly the gap this state closes.
func TestMoveInFlightRecord_PersistFailureAbortsBeforeResidue(t *testing.T) {
	gw, _, wtPath := archiveTestWorktree(t)
	secondFS := findOrMakeSecondFilesystem(t)
	dest := filepath.Join(secondFS, "archived-mif")

	restore := gw.SetRelocationRecordCheckpoint(func() error {
		return os.ErrPermission
	})
	defer restore()

	err := gw.ArchiveWorktree(dest)
	require.Error(t, err, "the move must fail when its record cannot be persisted")
	assert.Contains(t, err.Error(), "could not durably record the in-flight move")

	// Nothing moved and nothing was stranded: src is intact, dest never
	// existed, and no residue name appears beside either endpoint.
	assert.DirExists(t, wtPath)
	assert.NoDirExists(t, dest)
	residue, rerr := ListAFRelocationResidue(filepath.Dir(wtPath), secondFS)
	require.NoError(t, rerr)
	assert.Empty(t, residue)

	// The in-memory record still names the move so the next archive resolves
	// it rather than retrying blind — and it resolves back to the live source.
	_, recovery, unresolved := gw.RelocationSnapshot()
	require.True(t, unresolved)
	assert.Equal(t, RelocationRecoveryMoveInFlight, recovery.State)
	claim, claimErr := gw.ClaimRelocationSource()
	require.NoError(t, claimErr)
	assert.Equal(t, wtPath, claim.Path)
}

// moveInFlightRecordFor builds the durable record an interrupted cross-device
// move would have persisted: src's identity plus both generated residue names.
func moveInFlightRecordFor(t *testing.T, src, dest string) RelocationRecovery {
	t.Helper()
	identity, err := boundedRelocationPathIdentity(src)
	require.NoError(t, err)
	return RelocationRecovery{
		State:             RelocationRecoveryMoveInFlight,
		AlternatePath:     dest,
		IdentityKnown:     true,
		Device:            identity.device,
		Inode:             identity.inode,
		FileType:          identity.fileType,
		QuarantinePath:    filepath.Join(filepath.Dir(src), ".af-source-"+strings.Repeat("a", 32)),
		StagingPath:       filepath.Join(filepath.Dir(dest), ".af-copy-"+strings.Repeat("b", 32)),
		DestIdentityKnown: false,
	}
}

// TestMoveInFlightRecord_ResolutionQuarantineRefuses pins the refuse-with-exit
// rule: a surviving `.af-source-*` named by this session's own record is the
// only complete copy of the session's work, so resolution keeps the record
// and refuses, naming the quarantine and the operator exit.
func TestMoveInFlightRecord_ResolutionQuarantineRefuses(t *testing.T) {
	gw, _, wtPath := archiveTestWorktree(t)
	dest := filepath.Join(t.TempDir(), "archived-mif")
	recovery := moveInFlightRecordFor(t, wtPath, dest)
	require.NoError(t, gw.RestoreRelocationRecovery(recovery))
	require.NoError(t, os.Rename(wtPath, recovery.QuarantinePath))

	_, claimErr := gw.ClaimRelocationSource()
	require.Error(t, claimErr)
	assert.True(t, errors.Is(claimErr, ErrRelocateStateUnknown),
		"the refusal keeps the record so the operator can fix and retry: %v", claimErr)
	assert.Contains(t, claimErr.Error(), recovery.QuarantinePath)
	assert.Contains(t, claimErr.Error(), "move it back to "+wtPath,
		"the refusal must carry the actionable exit, not just a state name")

	// The operator exit: move the quarantine back to the recorded path and
	// retry — resolution then claims the live source.
	require.NoError(t, os.Rename(recovery.QuarantinePath, wtPath))
	claim, err := gw.ClaimRelocationSource()
	require.NoError(t, err)
	assert.Equal(t, wtPath, claim.Path)
	_, snap, unresolved := gw.RelocationSnapshot()
	assert.True(t, unresolved,
		"an activated claim projects as unresolved until it is settled or preserved")
	assert.Equal(t, RelocationRecoveryClaimStale, snap.State)
}

// TestMoveInFlightRecord_ResolutionSourceMatch covers the crash-before-rename
// window: the quarantine rename never ran, src still carries the recorded
// identity, and resolution simply claims it again.
func TestMoveInFlightRecord_ResolutionSourceMatch(t *testing.T) {
	gw, _, wtPath := archiveTestWorktree(t)
	dest := filepath.Join(t.TempDir(), "archived-mif")
	require.NoError(t, gw.RestoreRelocationRecovery(moveInFlightRecordFor(t, wtPath, dest)))

	claim, err := gw.ClaimRelocationSource()
	require.NoError(t, err)
	assert.Equal(t, wtPath, claim.Path)
}

// TestMoveInFlightRecord_ResolutionDestMatch covers the post-publish crash:
// src and quarantine are gone (the copy completed and the staging published
// at dest), and the record's dest identity — captured before the quarantine
// rename — resolves the move forward to the destination.
func TestMoveInFlightRecord_ResolutionDestMatch(t *testing.T) {
	gw, _, wtPath := archiveTestWorktree(t)
	dest := filepath.Join(t.TempDir(), "archived-mif")
	recovery := moveInFlightRecordFor(t, wtPath, dest)

	// Model the post-publish footprint: the staged copy published at dest and
	// the source is gone. The dest identity in the record is what resolves it.
	require.NoError(t, os.Rename(wtPath, dest))
	destIdentity, err := boundedRelocationPathIdentity(dest)
	require.NoError(t, err)
	recovery.DestIdentityKnown = true
	recovery.DestDevice = destIdentity.device
	recovery.DestInode = destIdentity.inode
	recovery.DestFileType = destIdentity.fileType
	require.NoError(t, gw.RestoreRelocationRecovery(recovery))

	claim, err := gw.ClaimRelocationSource()
	require.NoError(t, err, "the dest identity must resolve the post-publish crash forward")
	assert.Equal(t, dest, claim.Path)
	path, _, unresolved := gw.RelocationSnapshot()
	assert.Equal(t, dest, path, "the recorded worktree path relocates with the resolution")
	assert.True(t, unresolved,
		"the activated claim projects as unresolved until the move settles it")
}

// TestMoveInFlightRecord_ResolutionUnresolvableRefuses covers the case where
// neither endpoint answers the record: src gone, dest absent or mismatched,
// quarantine gone. The record stays and the claim refuses rather than
// guessing.
func TestMoveInFlightRecord_ResolutionUnresolvableRefuses(t *testing.T) {
	gw, _, wtPath := archiveTestWorktree(t)
	dest := filepath.Join(t.TempDir(), "archived-mif")
	require.NoError(t, gw.RestoreRelocationRecovery(moveInFlightRecordFor(t, wtPath, dest)))
	require.NoError(t, os.RemoveAll(wtPath))

	_, claimErr := gw.ClaimRelocationSource()
	require.Error(t, claimErr)
	assert.True(t, errors.Is(claimErr, ErrRelocateStateUnknown))
	_, _, unresolved := gw.RelocationSnapshot()
	assert.True(t, unresolved, "an unresolvable in-flight record is kept, never cleared")
}

// TestMoveInFlightRecord_DoesNotFenceCleanup pins the maintainer's kill
// carve-out: kill deletes the worktree path and the owned branch, never the
// `.af-source-*`/`.af-copy-*` siblings the record names, so an in-flight
// record must not add a cleanup refusal master does not have.
func TestMoveInFlightRecord_DoesNotFenceCleanup(t *testing.T) {
	gw, _, wtPath := archiveTestWorktree(t)
	dest := filepath.Join(t.TempDir(), "archived-mif")
	require.NoError(t, gw.RestoreRelocationRecovery(moveInFlightRecordFor(t, wtPath, dest)))
	require.NoError(t, os.RemoveAll(wtPath))

	state, err := gw.cleanup(false)
	assert.Equal(t, CleanupSettled, state,
		"cleanup must proceed on a move_in_flight record exactly as on master's record-free row (err=%v)", err)
	if err != nil {
		assert.NotContains(t, err.Error(), "refusing to inspect or clean up worktree recovery state")
	}
}

// TestMoveInFlightRecord_PresenceStaysUnknown keeps the conservative fence for
// routes that DO arbitrate: while the record is live the recorded path's
// presence is Unknown, so rebuild/adopt consumers cannot treat the missing
// path as deleted-outside-af.
func TestMoveInFlightRecord_PresenceStaysUnknown(t *testing.T) {
	gw, _, wtPath := archiveTestWorktree(t)
	dest := filepath.Join(t.TempDir(), "archived-mif")
	require.NoError(t, gw.RestoreRelocationRecovery(moveInFlightRecordFor(t, wtPath, dest)))
	require.NoError(t, os.RemoveAll(wtPath))

	assert.Equal(t, WorktreePresenceUnknown, gw.ProbeWorktreePresence(),
		"the in-flight record owns the path question until resolution retires it")
}

// TestMoveInFlightRecord_CrashPoints is the end-to-end crash matrix: the
// child process drives the REAL ArchiveWorktree across a real second
// filesystem, armed with the durable-record checkpoint writing the visible
// record to disk, and os.Exit(9)s at each modeled crash point. The parent
// asserts the on-disk footprint at every point is either no residue, or
// residue a persisted record already names.
func TestMoveInFlightRecord_CrashPoints(t *testing.T) {
	if os.Getenv("AF_CRASH_CHILD_MIF") == "1" {
		runChildMoveInFlightCrash(t)
		return
	}
	points := []struct {
		name             string
		wantRecord       bool
		wantDestIdentity bool
		wantQuarantine   bool
		wantStaging      bool
		wantSrc          bool
		wantDest         bool
		wantClaimOutcome string
	}{
		// Crash inside the very first durable write: nothing on disk, src
		// intact, no residue anywhere.
		{"pre-record", false, false, false, false, true, false, "claims-src"},
		// Crash during the copy: the V1 record names staging + quarantine; a
		// partial staging tree exists; src is still the worktree.
		{"mid-copy", true, false, false, true, true, false, "claims-src"},
		// Crash after the copy, before the quarantine rename: the V2 record
		// carries the staged destination identity; src intact.
		{"pre-rename", true, true, false, true, true, false, "claims-src"},
		// The bug's window: quarantine + staging both stranded. The record
		// names both and resolution refuses with the operator exit.
		{"pre-publish", true, true, true, true, false, false, "refuses-quarantine"},
		// Crash after the publish rename, before retirement: dest holds the
		// worktree, the quarantine still waits for cleanup.
		{"post-publish", true, true, true, false, false, true, "refuses-quarantine"},
	}
	for _, point := range points {
		t.Run(point.name, func(t *testing.T) {
			secondFS := findOrMakeSecondFilesystem(t)
			sandboxHome(t)
			repoRoot := createGitRepo(t)
			runGitInPlaceTest(t, repoRoot, "commit", "--allow-empty", "-m", "init")
			src := filepath.Join(filepath.Dir(repoRoot), "repo-arch-src")
			runGitInPlaceTest(t, repoRoot, "worktree", "add", "-b", "arch/branch", src)
			require.NoError(t, os.WriteFile(filepath.Join(src, "dirty.txt"), []byte("uncommitted work"), 0644))
			dest := filepath.Join(secondFS, "archived-mif")
			recordFile := filepath.Join(t.TempDir(), "record.json")

			cmd := exec.Command(os.Args[0], "-test.run=^TestMoveInFlightRecord_CrashPoints$", "-test.timeout=60s")
			cmd.Dir = filepath.Dir(src)
			cmd.Env = append(os.Environ(),
				"AF_CRASH_CHILD_MIF=1",
				"AF_CRASH_POINT="+point.name,
				"AF_CRASH_SRC="+src,
				"AF_CRASH_DEST="+dest,
				"AF_CRASH_REPO="+repoRoot,
				"AF_CRASH_RECORD="+recordFile,
			)
			var childOut strings.Builder
			cmd.Stdout = &childOut
			cmd.Stderr = &childOut
			err := cmd.Run()
			exitErr, ok := err.(*exec.ExitError)
			require.True(t, ok, "the child must self-terminate via os.Exit(9) to model the crash; got %v\noutput: %s", err, childOut.String())
			assert.Equal(t, 9, exitErr.ExitCode(), "child output: %s", childOut.String())

			// The on-disk footprint: every residue either absent, or named by
			// the record that was persisted before it could exist.
			var record crashRecordFile
			data, rerr := os.ReadFile(recordFile)
			if point.wantRecord {
				require.NoError(t, rerr, "a record must be on disk once V1 has landed")
				require.NoError(t, json.Unmarshal(data, &record))
				assert.Equal(t, RelocationRecoveryMoveInFlight, record.State)
				assert.Equal(t, dest, record.AlternatePath)
				assert.Equal(t, point.wantDestIdentity, record.DestIdentityKnown,
					"dest identity must be durable exactly once the copy has landed")
				assert.True(t, privateMoveResidueName(filepath.Base(record.QuarantinePath)))
				assert.True(t, privateMoveResidueName(filepath.Base(record.StagingPath)))
			} else {
				assert.True(t, errors.Is(rerr, os.ErrNotExist),
					"before the record lands there must be no record and no residue")
			}

			_, srcErr := os.Lstat(src)
			assert.Equal(t, point.wantSrc, srcErr == nil, "src presence at this crash point")
			_, destErr := os.Lstat(dest)
			assert.Equal(t, point.wantDest, destErr == nil, "dest presence at this crash point")

			sourceResidue, sErr := ListAFRelocationResidue(filepath.Dir(src))
			require.NoError(t, sErr)
			destResidue, dErr := ListAFRelocationResidue(filepath.Dir(dest))
			require.NoError(t, dErr)
			var quarantine, staging string
			for _, p := range sourceResidue {
				if strings.HasPrefix(filepath.Base(p), ".af-source-") {
					quarantine = p
				}
			}
			for _, p := range destResidue {
				if strings.HasPrefix(filepath.Base(p), ".af-copy-") {
					staging = p
				}
			}
			if point.wantQuarantine {
				require.NotEmpty(t, quarantine, "the quarantine residue must exist at this point")
				assert.Equal(t, record.QuarantinePath, quarantine,
					"residue exists and the persisted record already names it — the #7819 property")
				dirty, derr := os.ReadFile(filepath.Join(quarantine, "dirty.txt"))
				require.NoError(t, derr, "the quarantined worktree keeps the uncommitted work")
				assert.Equal(t, "uncommitted work", string(dirty))
			} else {
				assert.Empty(t, quarantine, "no quarantine residue at this crash point")
			}
			if point.wantStaging {
				require.NotEmpty(t, staging)
				assert.Equal(t, record.StagingPath, staging)
			} else {
				assert.Empty(t, staging)
			}
			// And the stronger invariant: any residue present is a path the
			// record names.
			for _, p := range append(sourceResidue, destResidue...) {
				assert.True(t, p == record.QuarantinePath || p == record.StagingPath,
					"residue %s exists with no persisted record naming it", p)
			}

			// Claim resolution over the persisted record — or record-free
			// behavior for the pre-record crash.
			reloaded, loadErr := NewGitWorktreeFromStorage(repoRoot, src, "arch", "arch/branch", "", false, true)
			require.NoError(t, loadErr)
			if !point.wantRecord {
				claim, claimErr := reloaded.ClaimRelocationSource()
				require.NoError(t, claimErr)
				assert.Equal(t, src, claim.Path)
				return
			}
			require.NoError(t, reloaded.RestoreRelocationRecovery(record.RelocationRecovery))
			claim, claimErr := reloaded.ClaimRelocationSource()
			switch point.wantClaimOutcome {
			case "claims-src":
				require.NoError(t, claimErr)
				assert.Equal(t, src, claim.Path)
			case "refuses-quarantine":
				require.Error(t, claimErr)
				assert.True(t, errors.Is(claimErr, ErrRelocateStateUnknown))
				assert.Contains(t, claimErr.Error(), record.QuarantinePath)
			}
		})
	}
}

// runChildMoveInFlightCrash is the child harness: it arms the durable-record
// checkpoint with a file write (the stand-in for the daemon's fsynced row
// persist), installs the crash seam named by AF_CRASH_POINT, and drives the
// real ArchiveWorktree across a real second filesystem.
func runChildMoveInFlightCrash(t *testing.T) {
	src := os.Getenv("AF_CRASH_SRC")
	dest := os.Getenv("AF_CRASH_DEST")
	repo := os.Getenv("AF_CRASH_REPO")
	recordFile := os.Getenv("AF_CRASH_RECORD")
	point := os.Getenv("AF_CRASH_POINT")
	if src == "" || dest == "" || repo == "" || recordFile == "" || point == "" {
		t.Fatal("AF_CRASH_* environment is incomplete")
	}
	gw, err := NewGitWorktreeFromStorage(repo, src, "arch", "arch/branch", "", false, true)
	if err != nil {
		t.Fatalf("reload worktree: %v", err)
	}
	writeRecord := func() error {
		_, recovery, ok := gw.RelocationSnapshot()
		if !ok {
			t.Fatal("the durable checkpoint fired with no in-flight record")
		}
		data, err := json.Marshal(crashRecordFile{RelocationRecovery: recovery, WorktreePath: src})
		if err != nil {
			return err
		}
		return os.WriteFile(recordFile, data, 0o644)
	}
	firstWrite := true
	gw.SetRelocationRecordCheckpoint(func() error {
		if point == "pre-record" && firstWrite {
			os.Exit(9)
		}
		firstWrite = false
		return writeRecord()
	})
	switch point {
	case "pre-record":
	case "mid-copy":
		copyTreeAfterDestCreate = func(string) error { os.Exit(9); return nil }
	case "pre-rename":
		moveDirBeforeSourceCommit = func(string) error { os.Exit(9); return nil }
	case "pre-publish":
		moveDirBeforeDestCommit = func(string) error { os.Exit(9); return nil }
	case "post-publish":
		moveDirAfterDestCommit = func(string) error { os.Exit(9); return nil }
	default:
		t.Fatalf("unknown crash point %q", point)
	}
	_ = gw.ArchiveWorktree(dest)
	t.Fatalf("the %s crash seam did not fire; the move completed unexpectedly", point)
}

package session

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/git"
)

// TestMoveInFlightRecord_CrashPoints is the end-to-end crash matrix for the
// #7819 durable record. A child process drives the REAL ArchiveWorktree —
// forced onto the cross-device path by an EXDEV answer at the first rename,
// so every write stays inside t.TempDir() — armed with the durable checkpoint
// persisting the session row through the REAL ToInstanceData/ForStorage/
// config.UpdateRepoInstances sequence persistInstanceData runs, and os.Exit(9)s
// at each modeled crash point. The parent then reads the stored row back
// through the REAL raw-row path and resolves it through the REAL
// FromInstanceData path: at every crash point there is either no residue, or
// residue a persisted record already names.
func TestMoveInFlightRecord_CrashPoints(t *testing.T) {
	if os.Getenv("AF_CRASH_CHILD_MIF") == "1" {
		moveInFlightCrashChild(t)
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
		// Crash inside the very first durable write: the row on disk is the
		// pre-move row — no record, no residue, src intact.
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
			home := t.TempDir()
			// Parent and child share one AF home: the parent's seed +
			// verification and the child's checkpoint persist all read and
			// write the same instances.json.
			t.Setenv("AGENT_FACTORY_HOME", home)
			repoRoot := residueTestRepo(t)
			wtParent := t.TempDir()
			src := filepath.Join(wtParent, "wt-crash")
			branch := "af/crash-" + strings.ReplaceAll(point.name, "-", "")
			gitOut(t, repoRoot, "worktree", "add", "-b", branch, src)
			require.NoError(t, os.WriteFile(filepath.Join(src, "dirty.txt"), []byte("uncommitted work"), 0o644))
			destParent := t.TempDir()
			dest := filepath.Join(destParent, "archived-mif")
			title := "crash-" + point.name
			instID := "id-" + point.name
			repoID := "repo-" + point.name

			// Seed the session row exactly the way the daemon's storage leaves
			// it before the archive begins — the checkpoint's targeted upsert
			// matches on title + stable id, so the row must exist first.
			seed, _ := arbitrationInstance(t, repoRoot, src, title, branch, instID)
			seed.liveness = LiveArchived
			persistRowForTest(t, repoID, seed.ToInstanceData())

			cmd := exec.Command(os.Args[0], "-test.run=^TestMoveInFlightRecord_CrashPoints$", "-test.timeout=60s")
			cmd.Dir = filepath.Dir(src)
			cmd.Env = append(os.Environ(),
				"AF_CRASH_CHILD_MIF=1",
				// TestMain's SandboxHome repoints AGENT_FACTORY_HOME in the
				// child process; the child restores this shared home in its
				// own body so its checkpoint persists into the store the
				// parent verifies.
				"AF_CRASH_HOME="+home,
				"AF_CRASH_POINT="+point.name,
				"AF_CRASH_SRC="+src,
				"AF_CRASH_DEST="+dest,
				"AF_CRASH_REPO="+repoRoot,
				"AF_CRASH_BRANCH="+branch,
				"AF_CRASH_REPO_ID="+repoID,
				"AF_CRASH_TITLE="+title,
				"AF_CRASH_ID="+instID,
			)
			var childOut strings.Builder
			cmd.Stdout = &childOut
			cmd.Stderr = &childOut
			err := cmd.Run()
			exitErr, ok := err.(*exec.ExitError)
			require.True(t, ok, "the child must self-terminate via os.Exit(9) to model the crash; got %v\noutput: %s", err, childOut.String())
			assert.Equal(t, 9, exitErr.ExitCode(), "child output: %s", childOut.String())

			// The on-disk footprint: every residue either absent, or named by
			// the durable row the child's real persist left behind.
			_, srcErr := os.Lstat(src)
			assert.Equal(t, point.wantSrc, srcErr == nil, "src presence at this crash point")
			_, destErr := os.Lstat(dest)
			assert.Equal(t, point.wantDest, destErr == nil, "dest presence at this crash point")

			sourceResidue, sErr := git.ListAFRelocationResidue(filepath.Dir(src))
			require.NoError(t, sErr)
			destResidue, dErr := git.ListAFRelocationResidue(filepath.Dir(dest))
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
			assert.Equal(t, point.wantQuarantine, quarantine != "", "quarantine residue presence at this point")
			assert.Equal(t, point.wantStaging, staging != "", "staging residue presence at this point")

			// The record must be read back the way a daemon restart reads it:
			// raw row -> rollback-fence restore -> Originals restore.
			row := loadStoredRowForTest(t, repoID, title)
			if !point.wantRecord {
				assert.Nil(t, row.Worktree.RelocationRecovery,
					"a crash inside the first durable write leaves the pre-move row — no record, no residue")
				assert.Empty(t, sourceResidue)
				assert.Empty(t, destResidue)
			} else {
				revealed := row.RestoreArchiveRollbackFence()
				restored, rerr := revealed.RestoreRelocationRecoveryOriginals()
				require.NoError(t, rerr, "the persisted row must restore, not error out")
				rec := restored.Worktree.RelocationRecovery
				require.NotNil(t, rec, "a record must be on disk once V1 has landed")
				assert.Equal(t, git.RelocationRecoveryMoveInFlight, rec.State)
				assert.Equal(t, dest, rec.AlternatePath)
				assert.Equal(t, point.wantDestIdentity, rec.DestIdentityKnown,
					"dest identity must be durable exactly once the copy has landed")

				// The stronger invariant: any residue present is a path the
				// persisted record names — the #7819 property.
				for _, p := range append(sourceResidue, destResidue...) {
					assert.True(t, p == rec.QuarantinePath || p == rec.StagingPath,
						"residue %s exists with no persisted record naming it", p)
				}
				if point.wantQuarantine {
					assert.Equal(t, rec.QuarantinePath, quarantine,
						"the persisted record already names the stranded quarantine")
					dirty, derr := os.ReadFile(filepath.Join(quarantine, "dirty.txt"))
					require.NoError(t, derr, "the quarantined worktree keeps the uncommitted work")
					assert.Equal(t, "uncommitted work", string(dirty))
				}
				if point.wantStaging {
					assert.Equal(t, rec.StagingPath, staging)
				}

				// Claim resolution through the REAL FromInstanceData reload —
				// not a hand-restored record — is what the daemon restart runs.
				inst2, err := FromInstanceData(row)
				require.NoError(t, err)
				claim, claimErr := inst2.gitWorktree.ClaimRelocationSource()
				switch point.wantClaimOutcome {
				case "claims-src":
					require.NoError(t, claimErr)
					assert.Equal(t, src, claim.Path)
				case "refuses-quarantine":
					require.Error(t, claimErr)
					assert.True(t, errors.Is(claimErr, git.ErrRelocateStateUnknown))
					assert.Contains(t, claimErr.Error(), rec.QuarantinePath)
					// The operator exit the refusal names: put the stranded
					// quarantine back when the move never published, drop it
					// when the destination already holds the worktree — then
					// re-claim.
					if point.name == "post-publish" {
						require.NoError(t, os.RemoveAll(quarantine))
					} else {
						require.NoError(t, os.Rename(quarantine, src))
					}
					claim, claimErr = inst2.gitWorktree.ClaimRelocationSource()
					require.NoError(t, claimErr, "after the operator exit the claim must resolve")
				}

				// N1 (#7819 round two): a move_in_flight record's residue names
				// retire only once that residue is gone. Resolution removes its
				// own record-named .af-copy-* staging (identity-checked), so the
				// old staging is gone after the claim — it must never be left on
				// disk named by nothing, where it would degrade to leaf
				// inference that refuses every later archive and restore once
				// the leaf is pruned.
				if staging != "" {
					assert.NoDirExists(t, staging,
						"resolution must remove the record-named staging it retires")
				}
				require.NoError(t, inst2.gitWorktree.SettleRelocationClaim(claim))
				if claim.Path == src {
					require.NoError(t, inst2.gitWorktree.ArchiveWorktree(dest),
						"the retry after resolution succeeds")
				}
				postResidue, pErr := git.ListAFRelocationResidue(filepath.Dir(src), filepath.Dir(dest))
				require.NoError(t, pErr)
				assert.Empty(t, postResidue,
					"a resolved-and-retried move leaves no residue — none may be left unnamed")
			}
		})
	}
}

// moveInFlightCrashChild is the child harness: it rebuilds the instance,
// installs the durable-record checkpoint as the REAL row persist (the same
// ToInstanceData -> ForStorage -> UpdateRepoInstances upsert the daemon's
// persistInstanceData runs), forces the cross-device path via the EXDEV seam
// so no write leaves the test's temp dirs, installs the crash seam named by
// AF_CRASH_POINT, and drives the real ArchiveWorktree.
func moveInFlightCrashChild(t *testing.T) {
	src := os.Getenv("AF_CRASH_SRC")
	dest := os.Getenv("AF_CRASH_DEST")
	repo := os.Getenv("AF_CRASH_REPO")
	branch := os.Getenv("AF_CRASH_BRANCH")
	repoID := os.Getenv("AF_CRASH_REPO_ID")
	title := os.Getenv("AF_CRASH_TITLE")
	instID := os.Getenv("AF_CRASH_ID")
	point := os.Getenv("AF_CRASH_POINT")
	if src == "" || dest == "" || repo == "" || repoID == "" || title == "" || instID == "" || point == "" {
		t.Fatal("AF_CRASH_* environment is incomplete")
	}
	home := os.Getenv("AF_CRASH_HOME")
	if home == "" {
		t.Fatal("AF_CRASH_HOME is unset")
	}
	// TestMain's SandboxHome pointed this child process at its own fresh AF
	// home; repoint back to the shared test home so the checkpoint's row
	// persist lands in the store the parent seeds and verifies. The directory
	// is the parent's t.TempDir(), so every write still stays inside the
	// test's temp tree.
	require.NoError(t, os.Setenv("AGENT_FACTORY_HOME", home))
	inst, gw := arbitrationInstance(t, repo, src, title, branch, instID)
	inst.liveness = LiveArchived
	firstWrite := true
	inst.SetRelocationRecordCheckpoint(func() error {
		if point == "pre-record" && firstWrite {
			os.Exit(9)
		}
		firstWrite = false
		persistRowForTest(t, repoID, inst.ToInstanceData())
		return nil
	})
	seams := git.MoveDirCrashSeams{
		FastMove: func(*git.GitWorktree, string, string) error {
			return errors.New("invalid cross-device link")
		},
		FirstRename: func(string, string) error { return syscall.EXDEV },
	}
	crash := func(string) error { os.Exit(9); return nil }
	switch point {
	case "pre-record":
	case "mid-copy":
		seams.AfterDestCreate = crash
	case "pre-rename":
		seams.BeforeSourceCommit = crash
	case "pre-publish":
		seams.BeforeDestCommit = crash
	case "post-publish":
		seams.AfterDestCommit = crash
	default:
		t.Fatalf("unknown crash point %q", point)
	}
	git.SetMoveDirCrashSeamsForTest(seams)
	_ = gw.ArchiveWorktree(dest)
	t.Fatalf("the %s crash seam did not fire; the move completed unexpectedly", point)
}

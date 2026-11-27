package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/log"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A relocation probe that reaches its deadline latches the worktree as stalled
// even though it has not yet created two pathname candidates. The Lost loop must
// honor that latch before os.Stat or rebuild touches the same filesystem: those
// unbounded calls can wedge the daemon's sole status/self-healing goroutine.
//
// Since #5295 the refusal is not the first answer: Recover first tries the
// designed consumer — the bounded relocation-claim machinery — to resolve the
// record itself. The refusal below therefore comes one step later, when the
// claim's own bounded identity probe cannot answer either (injected here as
// the deadline a still-stalled filesystem would produce). The wedge property
// the latch exists for is unchanged: nothing unbounded ever touches the
// filesystem.
func TestRecover_RefusesWorktreeAfterRelocationProbeTimeout(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_recover_relocation_stall"
	shellName := agentName + tmuxTabSeparator + shellTabName
	var newSessions int
	exec := countingExec(map[string]bool{}, &newSessions)
	restored := lostInstanceForRecover(t, agentName, shellName, exec)

	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nexec sleep 300\n"), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(sessiongit.SetLocalGitTimeoutForTest(100 * time.Millisecond))

	worktree, err := restored.GetGitWorktree()
	require.NoError(t, err)
	moveErr := worktree.MoveWorktree(filepath.Join(t.TempDir(), "archive"))
	require.ErrorIs(t, moveErr, context.DeadlineExceeded,
		"the setup must trip the bounded pre-move probe")
	recovery, hasRecovery := worktree.GetRelocationRecovery()
	require.True(t, hasRecovery,
		"every latched relocation stall must materialize recovery state; absent cannot mean safe")
	assert.Equal(t, sessiongit.RelocationRecoveryStalled, recovery.State)
	storedRecovery := restored.ToInstanceData().Worktree.RelocationRecovery
	require.NotNil(t, storedRecovery, "the stall latch must survive the daemon's durable snapshot")
	assert.Equal(t, sessiongit.RelocationRecoveryStalled, storedRecovery.State)

	// The filesystem is still not answering: the claim's bounded identity
	// probe of the recorded path hits the same deadline the pre-move probe
	// did, so resolution fails closed and the refusal stands — carrying the
	// state-unknown sentinel the retry loop classifies as environmental.
	primary, _, _ := worktree.RelocationSnapshot()
	t.Cleanup(sessiongit.SetRelocationIdentityErrorForTest(primary, context.DeadlineExceeded))

	err = restored.Recover()
	require.Error(t, err, "Lost recovery must refuse a worktree whose filesystem still cannot answer")
	assert.Contains(t, err.Error(), "unresolved worktree recovery state")
	assert.ErrorIs(t, err, sessiongit.ErrRelocateStateUnknown)
	assert.Equal(t, 0, newSessions, "the stalled worktree must be refused before any agent spawn")
}

// TestRecover_ResolvesClaimStaleWorktreeRelocation is the #5295 mechanism:
// an interrupted relocation (an ENOSPC-cut archive) leaves a durable
// claim_stale record naming which directory IS the worktree. Recover must
// resolve it through the bounded claim machinery and respawn — the session
// heals itself without an unrelated archive consuming the record first.
func TestRecover_ResolvesClaimStaleWorktreeRelocation(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_recover_claim_stale"
	shellName := agentName + tmuxTabSeparator + shellTabName
	var newSessions int
	exec := countingExec(map[string]bool{}, &newSessions)
	restored := lostInstanceForRecover(t, agentName, shellName, exec)

	worktree, err := restored.GetGitWorktree()
	require.NoError(t, err)
	worktreePath, _, _ := worktree.RelocationSnapshot()

	// The record an ENOSPC-interrupted archive leaves: identity known, no
	// second candidate — exactly what RestoreRelocationRecovery reads back
	// after a daemon restart.
	var st syscall.Stat_t
	require.NoError(t, syscall.Stat(worktreePath, &st))
	require.NoError(t, worktree.RestoreRelocationRecovery(sessiongit.RelocationRecovery{
		State:         sessiongit.RelocationRecoveryClaimStale,
		IdentityKnown: true,
		Device:        uint64(st.Dev),
		Inode:         uint64(st.Ino),
		FileType:      uint32(st.Mode & syscall.S_IFMT),
	}))

	require.NoError(t, restored.Recover())

	assert.Equal(t, Running, restored.GetStatus())
	assert.Greater(t, newSessions, 0, "Recover must respawn once the record resolves")
	_, hasRecovery := worktree.GetRelocationRecovery()
	assert.False(t, hasRecovery, "a resolved record must be consumed, not left to refuse the next operation")
}

// TestRecover_ResolvesMoveInFlightWorktreeRelocation is the #7819 round-two
// counterpart of the claim_stale resolution above: a Lost row holding a
// crash-left move_in_flight with its source intact owes no repair — the
// record is only written on the cross-device path, where no `git worktree
// move` ever ran — so Recover resolves it through the same claim an archive
// retry takes: the primary claims the recorded path, the staged copy the
// record names is retired, and the respawn proceeds.
func TestRecover_ResolvesMoveInFlightWorktreeRelocation(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_recover_move_in_flight"
	shellName := agentName + tmuxTabSeparator + shellTabName
	var newSessions int
	exec := countingExec(map[string]bool{}, &newSessions)
	restored := lostInstanceForRecover(t, agentName, shellName, exec)

	worktree, err := restored.GetGitWorktree()
	require.NoError(t, err)
	worktreePath, _, _ := worktree.RelocationSnapshot()

	// The record a mid-copy crash leaves: the source still holds the
	// worktree, the staged copy it names sits beside the never-published
	// destination, the quarantine rename never ran so its named path is
	// absent, and no destination identity landed before the crash.
	destParent := t.TempDir()
	staging := filepath.Join(destParent, ".af-copy-0123456789abcdef0123456789abcdef")
	require.NoError(t, os.MkdirAll(staging, 0o755))
	var st syscall.Stat_t
	require.NoError(t, syscall.Stat(worktreePath, &st))
	require.NoError(t, worktree.RestoreRelocationRecovery(sessiongit.RelocationRecovery{
		State:         sessiongit.RelocationRecoveryMoveInFlight,
		AlternatePath: filepath.Join(destParent, "archive-dest"),
		IdentityKnown: true,
		Device:        uint64(st.Dev),
		Inode:         uint64(st.Ino),
		FileType:      uint32(st.Mode & syscall.S_IFMT),
		QuarantinePath: filepath.Join(
			filepath.Dir(worktreePath), ".af-source-0123456789abcdef0123456789abcdef"),
		StagingPath: staging,
	}))

	require.NoError(t, restored.Recover())

	assert.Equal(t, Running, restored.GetStatus())
	assert.Greater(t, newSessions, 0, "Recover must respawn once the move_in_flight record resolves")
	assert.NoDirExists(t, staging,
		"the claim's resolution must retire the staging the record named")
	_, hasRecovery := worktree.GetRelocationRecovery()
	assert.False(t, hasRecovery, "a resolved record must be consumed, not left to refuse the next operation")
}

// TestRecover_SettlePersistsBeforeSpawn is the #5295-review regression: the
// respawn's settle is in-memory only, so the daemon installs a persist
// callback that MUST fire before the spawn — otherwise a daemon exit in the
// settle→boundary window reloads the stale record and strands the recovery.
// A notify failure must abort the spawn the same way.
func TestRecover_SettlePersistsBeforeSpawn(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_recover_settle_notify"
	shellName := agentName + tmuxTabSeparator + shellTabName
	var newSessions int
	exec := countingExec(map[string]bool{}, &newSessions)
	restored := lostInstanceForRecover(t, agentName, shellName, exec)

	worktree, err := restored.GetGitWorktree()
	require.NoError(t, err)
	worktreePath, _, _ := worktree.RelocationSnapshot()

	var st syscall.Stat_t
	require.NoError(t, syscall.Stat(worktreePath, &st))
	require.NoError(t, worktree.RestoreRelocationRecovery(sessiongit.RelocationRecovery{
		State:         sessiongit.RelocationRecoveryClaimStale,
		IdentityKnown: true,
		Device:        uint64(st.Dev),
		Inode:         uint64(st.Ino),
		FileType:      uint32(st.Mode & syscall.S_IFMT),
	}))

	var notified int
	var spawnedAtNotify = -1
	restored.SetRelocationSettleNotify(func(*Instance) error {
		notified++
		spawnedAtNotify = newSessions
		return nil
	})
	require.NoError(t, restored.Recover())
	assert.Equal(t, 1, notified, "the settle persist must fire exactly once")
	assert.Equal(t, 0, spawnedAtNotify, "the notify must run BEFORE the spawn proceeds")

	// The same edge must fail closed: a notify error aborts the attempt
	// before any spawn.
	var newSessions2 int
	exec2 := countingExec(map[string]bool{}, &newSessions2)
	restored2 := lostInstanceForRecover(t, agentName+"2", shellName+"2", exec2)
	worktree2, err := restored2.GetGitWorktree()
	require.NoError(t, err)
	wp2, _, _ := worktree2.RelocationSnapshot()
	var st2 syscall.Stat_t
	require.NoError(t, syscall.Stat(wp2, &st2))
	require.NoError(t, worktree2.RestoreRelocationRecovery(sessiongit.RelocationRecovery{
		State:         sessiongit.RelocationRecoveryClaimStale,
		IdentityKnown: true,
		Device:        uint64(st2.Dev),
		Inode:         uint64(st2.Ino),
		FileType:      uint32(st2.Mode & syscall.S_IFMT),
	}))
	restored2.SetRelocationSettleNotify(func(*Instance) error {
		return fmt.Errorf("disk full")
	})
	err = restored2.Recover()
	require.Error(t, err, "a persist failure must fail the attempt, not spawn ahead of the durable clear")
	assert.Contains(t, err.Error(), "not yet durable")
	assert.Equal(t, 0, newSessions2, "no spawn may proceed on top of a non-durable settle")

	// #5295 review: the failed persist leaves the durable record in place, so
	// the in-memory fence MUST be re-armed — the next retry re-enters the
	// settle→persist checkpoint instead of skipping it and spawning on top of
	// the stale durable record.
	_, hasRecovery := worktree2.GetRelocationRecovery()
	assert.True(t, hasRecovery, "a failed settle-persist must re-arm the record so the next retry re-attempts the durable clear")

	var notified2 int
	restored2.SetRelocationSettleNotify(func(*Instance) error {
		notified2++
		return nil
	})
	require.NoError(t, restored2.Recover(), "a retry after the re-armed fence must settle and persist again")
	assert.Equal(t, 1, notified2, "the retry must re-enter the settle→persist checkpoint")
	assert.Equal(t, 2, newSessions2, "the retry's spawn proceeds only behind a durable clear")
	_, hasRecovery = worktree2.GetRelocationRecovery()
	assert.False(t, hasRecovery, "the settled record is consumed once durable")
}

// TestRecover_UnresolvableClaimKeepsRecordAndRefuses is the fail-closed half:
// when neither recorded path still holds the claimed identity — the worktree
// was replaced or deleted behind the record — Recover must refuse and KEEP
// the durable evidence rather than consuming it, so a later archive, kill,
// or identity-qualified retry still sees what was left.
func TestRecover_UnresolvableClaimKeepsRecordAndRefuses(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_recover_claim_gone"
	shellName := agentName + tmuxTabSeparator + shellTabName
	var newSessions int
	exec := countingExec(map[string]bool{}, &newSessions)
	restored := lostInstanceForRecover(t, agentName, shellName, exec)

	worktree, err := restored.GetGitWorktree()
	require.NoError(t, err)

	// An identity no directory answers: a stale claim whose captured
	// (device, inode) belongs to nothing at either recorded path.
	require.NoError(t, worktree.RestoreRelocationRecovery(sessiongit.RelocationRecovery{
		State:         sessiongit.RelocationRecoveryClaimStale,
		IdentityKnown: true,
		Device:        ^uint64(0),
		Inode:         ^uint64(0),
		FileType:      uint32(syscall.S_IFDIR),
	}))

	err = restored.Recover()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unresolved worktree recovery state")
	assert.ErrorIs(t, err, sessiongit.ErrRelocateStateUnknown,
		"a bare identity-mismatch verdict is definitive: it carries no errno, so the restore loop keeps it on the structural budget")
	assert.Equal(t, 0, newSessions, "no spawn may happen while the directory identity is unestablished")
	_, hasRecovery := worktree.GetRelocationRecovery()
	assert.True(t, hasRecovery, "the durable record must survive a failed resolution for the next operation")
}

// TestRecover_RefusesRepairOwedRelocationRecord pins the repair boundary:
// a two-candidate record (move_unknown — a timed-out `git worktree move`)
// can owe `git worktree repair` and submodule gitdir repair, work that
// belongs to the relocation-capable consumers. Recover must refuse WITHOUT
// touching the claim machinery: spawning into an un-repaired worktree, or
// settling the record that would drive the missing repair, both fail.
func TestRecover_RefusesRepairOwedRelocationRecord(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_recover_move_unknown"
	shellName := agentName + tmuxTabSeparator + shellTabName
	var newSessions int
	exec := countingExec(map[string]bool{}, &newSessions)
	restored := lostInstanceForRecover(t, agentName, shellName, exec)

	worktree, err := restored.GetGitWorktree()
	require.NoError(t, err)
	worktreePath, _, _ := worktree.RelocationSnapshot()

	// The record a timed-out move leaves: two identity-qualified candidates.
	var st syscall.Stat_t
	require.NoError(t, syscall.Stat(worktreePath, &st))
	require.NoError(t, worktree.RestoreRelocationRecovery(sessiongit.RelocationRecovery{
		State:         sessiongit.RelocationRecoveryMoveUnknown,
		AlternatePath: filepath.Join(t.TempDir(), "moved-candidate"),
		IdentityKnown: true,
		Device:        uint64(st.Dev),
		Inode:         uint64(st.Ino),
		FileType:      uint32(st.Mode & syscall.S_IFMT),
	}))

	err = restored.Recover()
	require.Error(t, err, "a repair-owed record must refuse until a relocation-capable consumer resolves it")
	assert.Contains(t, err.Error(), "unresolved worktree recovery state")
	assert.Equal(t, 0, newSessions, "no spawn may happen while a move's repair obligation is unestablished")
	recovery, hasRecovery := worktree.GetRelocationRecovery()
	require.True(t, hasRecovery, "the move record must stay durable for the repair-capable consumer")
	assert.Equal(t, sessiongit.RelocationRecoveryMoveUnknown, recovery.State)
}

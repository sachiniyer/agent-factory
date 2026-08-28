package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInstanceData_PreservesArchiveReportJSON proves an incomplete archive's
// report survives the same JSON round trip as the Archived session record. The
// newline is load-bearing: paths must remain JSON-delimited rather than becoming
// ambiguous lines in an in-tree report (#3066).
func TestInstanceData_PreservesArchiveReportJSON(t *testing.T) {
	const reportJSON = `{"archive_report":{"retained_trees":[{"path":"/worktrees/.af-source-0123456789abcdef0123456789abcdef","identity_known":true,"device":1,"inode":2,"file_type":16384,"skipped":[{"path":"private/locked\ncredential","reason":"permission_denied"}]}]}}`
	var data InstanceData
	require.NoError(t, json.Unmarshal([]byte(reportJSON), &data))

	roundTrip, err := json.Marshal(data)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(roundTrip, &decoded))

	report, ok := decoded["archive_report"].(map[string]any)
	require.True(t, ok, "the durable session record discarded archive_report: %s", roundTrip)
	trees, ok := report["retained_trees"].([]any)
	require.True(t, ok)
	require.Len(t, trees, 1)
	tree := trees[0].(map[string]any)
	skipped, ok := tree["skipped"].([]any)
	require.True(t, ok)
	require.Len(t, skipped, 1)
	entry := skipped[0].(map[string]any)
	assert.Equal(t, "private/locked\ncredential", entry["path"])
	assert.Equal(t, "permission_denied", entry["reason"])
}

// TestFromInstanceData_ArchiveReportSurvivesReload exercises the actual session
// reconstruction path used when restore happens in a later daemon process.
func TestFromInstanceData_ArchiveReportSurvivesReload(t *testing.T) {
	data := deadInstanceData(t, Archived, "af_report_agent", "af_report_shell")
	data.ArchiveReport = &git.ArchiveReport{
		RetainedTrees: []git.ArchiveRetainedTree{{
			Path: "/worktrees/.af-source-0123456789abcdef0123456789abcdef", IdentityKnown: true,
			Device: 1, Inode: 2, FileType: 0o040000,
			Skipped: []git.ArchiveSkippedEntry{{
				Path: "private/locked\ncredential", Reason: git.ArchiveSkipPermissionDenied,
			}},
		}},
	}

	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	report := restored.GetArchiveReport()
	require.Equal(t, data.ArchiveReport.Clone(), report)
	live := restored.ToInstanceData()
	require.Nil(t, live.ArchiveReport, "the unbounded report must not ride live snapshots")
	require.Contains(t, live.ArchiveWarning, `"private/locked\ncredential"`,
		"live snapshots still need the bounded user-facing warning")
	stored := live.ForStorage()
	require.NotNil(t, stored.ArchiveReport)
	require.Equal(t, data.ArchiveReport.RetainedTrees, stored.ArchiveReport.RetainedTrees,
		"the complete report must remain durable after load and the next storage projection")
}

func TestFromInstanceData_PreservesSnapshotArchiveWarning(t *testing.T) {
	data := deadInstanceData(t, Archived, "af_snapshot_warning_agent", "af_snapshot_warning_shell")
	data.ArchiveWarning = "restore completed with an incomplete archive: complete original tree retained at /retained/source"

	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	assert.Equal(t, data.ArchiveWarning, restored.ToInstanceData().ArchiveWarning,
		"a thin client rebuilding a live snapshot must not discard its only archive-loss notice")
}

func TestReconcileArchiveWarningMirrorsAndClearsProjection(t *testing.T) {
	inst := &Instance{}
	require.True(t, inst.ReconcileArchiveWarning("retained at /retained/source; incomplete archive"))
	require.Equal(t, "retained at /retained/source; incomplete archive", inst.ArchiveWarning())
	require.False(t, inst.ReconcileArchiveWarning(inst.ArchiveWarning()))
	require.True(t, inst.ReconcileArchiveWarning(""))
	require.Empty(t, inst.ArchiveWarning())
}

func TestLostArchiveWarningDoesNotClaimRestoreCompleted(t *testing.T) {
	data := deadInstanceData(t, Lost, "af_lost_report_agent", "af_lost_report_shell")
	data.Liveness = LiveLost
	data.ArchiveReport = &git.ArchiveReport{RetainedTrees: []git.ArchiveRetainedTree{{
		Path: "/worktrees/.af-source-0123456789abcdef0123456789abcdef",
		Skipped: []git.ArchiveSkippedEntry{{
			Path: "private/credential", Reason: git.ArchiveSkipPermissionDenied,
		}},
	}}}

	diskProjection := data.ForClientRead().ArchiveWarning
	require.Contains(t, diskProjection, "incomplete archive")
	require.NotContains(t, diskProjection, "completed",
		"a Lost disk row has not established that restore ever finished")

	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	liveProjection := restored.ToInstanceData().ArchiveWarning
	require.Contains(t, liveProjection, "incomplete archive")
	require.NotContains(t, liveProjection, "completed",
		"a Lost daemon row may still be waiting for registration repair or recovery")
}

func TestToInstanceDataArchiveReportProjectionAllocationsAreBounded(t *testing.T) {
	const skippedCount = 512
	skipped := make([]git.ArchiveSkippedEntry, skippedCount)
	for index := range skipped {
		rawPath := []byte(fmt.Sprintf("private/credential-%04d-", index))
		rawPath = append(rawPath, 0xff)
		skipped[index] = git.ArchiveSkippedEntry{
			Path: string(rawPath), PathBytes: rawPath, Reason: git.ArchiveSkipPermissionDenied,
		}
	}
	data := deadInstanceData(t, Archived, "af_bounded_report_agent", "af_bounded_report_shell")
	data.ArchiveReport = &git.ArchiveReport{RetainedTrees: []git.ArchiveRetainedTree{{
		Path: "/worktrees/.af-source-0123456789abcdef0123456789abcdef", Skipped: skipped,
	}}}
	restored, err := FromInstanceData(data)
	require.NoError(t, err)

	var projection InstanceData
	allocations := testing.AllocsPerRun(3, func() {
		projection = restored.ToInstanceData()
	})
	require.NotEmpty(t, projection.ArchiveWarning)
	require.Less(t, allocations, float64(128),
		"a live snapshot must use bounded summary state instead of cloning every lossless report path")
}

func TestArchiveReportStorageProjectionFencesOlderReaders(t *testing.T) {
	branchCreatedByUs := true
	data := deadInstanceData(t, Archived, "af_report_fence_agent", "af_report_fence_shell")
	data.Worktree.ExternalWorktree = false
	data.Worktree.BranchCreatedByUs = &branchCreatedByUs
	data.ArchiveReport = &git.ArchiveReport{RetainedTrees: []git.ArchiveRetainedTree{{
		Path: "/worktrees/.af-source-0123456789abcdef0123456789abcdef", IdentityKnown: true,
		Device: 1, Inode: 2, FileType: 0o040000,
		Skipped: []git.ArchiveSkippedEntry{{
			Path: "private/credential", Reason: git.ArchiveSkipPermissionDenied,
		}},
	}}}

	loaded, err := FromInstanceData(data)
	require.NoError(t, err)
	stored := loaded.ToInstanceData().ForStorage()
	require.NotNil(t, stored.ArchiveReport)
	require.NotNil(t, stored.ArchiveReport.RollbackFence)
	assert.True(t, stored.StartupStateUnknown, "an older reader must load the row inert")
	assert.True(t, stored.Worktree.ExternalWorktree, "an older reader must treat the incomplete copy as user-owned")
	require.NotNil(t, stored.Worktree.BranchCreatedByUs)
	assert.False(t, *stored.Worktree.BranchCreatedByUs, "an older reader must lack branch deletion authority")

	payload, err := json.Marshal(stored)
	require.NoError(t, err)
	var legacyWire v10228InstanceData
	require.NoError(t, json.Unmarshal(payload, &legacyWire),
		"the supported rollback binary used encoding/json without DisallowUnknownFields")
	legacyPayload, err := json.Marshal(legacyWire)
	require.NoError(t, err)
	var legacyView InstanceData
	require.NoError(t, json.Unmarshal(legacyPayload, &legacyView))
	require.Nil(t, legacyView.ArchiveReport, "the old-reader fixture must actually omit the new report")
	legacy, err := FromInstanceData(legacyView)
	require.NoError(t, err)
	require.ErrorContains(t, legacy.ValidateRuntimeAction(RuntimeActionRestoreArchived), "unknown startup state",
		"the old-reader projection must refuse to restore the incomplete published copy")
	assert.True(t, legacy.IsExternalWorktree(), "old-reader cleanup must preserve the incomplete worktree")

	current, err := FromInstanceData(stored)
	require.NoError(t, err)
	assert.False(t, current.StartupStateUnknown(), "the current reader must remove the compatibility fence")
	assert.False(t, current.IsExternalWorktree(), "the current reader must recover the real ownership")
	require.NotNil(t, current.gitWorktree)
	assert.True(t, current.gitWorktree.BranchCreatedByUs(), "the current reader must recover branch ownership")
	assert.Equal(t, data.ArchiveReport.RetainedTrees, current.GetArchiveReport().RetainedTrees)
}

// v10235InstanceData is the last release's archive-report-blind wire shape.
// Unlike v1.0.228, v1.0.235 understands relocation recovery, so the rollback
// projection can use that existing admission fence to keep an explicit kill
// from deleting the row which owns the otherwise-unknown report.
type v10235InstanceData struct {
	ID                  string          `json:"id,omitempty"`
	Title               string          `json:"title"`
	Path                string          `json:"path"`
	Branch              string          `json:"branch"`
	Status              Status          `json:"status"`
	Liveness            Liveness        `json:"liveness,omitempty"`
	Program             string          `json:"program"`
	BackendType         string          `json:"backend_type,omitempty"`
	StartupStateUnknown bool            `json:"startup_state_unknown,omitempty"`
	Worktree            GitWorktreeData `json:"worktree"`
	Tabs                []TabData       `json:"tabs,omitempty"`
}

func TestArchiveReportStorageProjectionMakesV10235KillRefuse(t *testing.T) {
	root := t.TempDir()
	currentPath := filepath.Join(root, "published-archive")
	retainedPath := filepath.Join(root, "stale-retained-source")
	require.NoError(t, os.Mkdir(currentPath, 0o755))
	require.NoError(t, os.Mkdir(retainedPath, 0o755))
	retainedInfo, err := os.Stat(retainedPath)
	require.NoError(t, err)
	retainedStat, ok := retainedInfo.Sys().(*syscall.Stat_t)
	require.True(t, ok)

	branchCreatedByUs := true
	data := deadInstanceData(t, Archived, "af_report_kill_fence_agent", "af_report_kill_fence_shell")
	data.Worktree.WorktreePath = currentPath
	data.Worktree.ExternalWorktree = false
	data.Worktree.BranchCreatedByUs = &branchCreatedByUs
	data.ArchiveReport = &git.ArchiveReport{RetainedTrees: []git.ArchiveRetainedTree{{
		Path: retainedPath, IdentityKnown: true,
		Device: uint64(retainedStat.Dev), Inode: uint64(retainedStat.Ino),
		FileType: uint32(retainedStat.Mode & syscall.S_IFMT),
		Skipped: []git.ArchiveSkippedEntry{{
			Path: "private/credential", Reason: git.ArchiveSkipPermissionDenied,
		}},
	}}}

	loaded, err := FromInstanceData(data)
	require.NoError(t, err)
	stored := loaded.ToInstanceData().ForStorage()

	payload, err := json.Marshal(stored)
	require.NoError(t, err)
	var legacyWire v10235InstanceData
	require.NoError(t, json.Unmarshal(payload, &legacyWire))
	legacyPayload, err := json.Marshal(legacyWire)
	require.NoError(t, err)
	var legacyView InstanceData
	require.NoError(t, json.Unmarshal(legacyPayload, &legacyView))
	require.Nil(t, legacyView.ArchiveReport, "the rollback fixture must actually omit the new report")
	require.NotNil(t, legacyView.Worktree.RelocationRecovery,
		"the previous reader needs an admission fence it understands")

	legacy, err := FromInstanceData(legacyView)
	require.NoError(t, err)
	err = legacy.Kill()
	require.ErrorContains(t, err, "worktree recovery state",
		"v1.0.235 explicit kill must retain the row instead of no-oping external cleanup and deleting it")
	claim, err := legacy.gitWorktree.ClaimRelocationSource()
	require.ErrorIs(t, err, git.ErrRelocateStateUnknown,
		"the compatibility kill fence must not let v1.0.235 restore from an older retained snapshot")
	assert.Empty(t, claim.Path)

	current, err := FromInstanceData(stored)
	require.NoError(t, err)
	require.NotNil(t, current.gitWorktree)
	require.False(t, current.gitWorktree.HasUnresolvedRelocation(),
		"the current reader must remove the rollback-only kill fence")
}

// TestFromInstanceData_ArchivedLoadsInert: an Archived record (#1028) loads
// WITHOUT calling Start — no tmux is spawned or reconnected, the instance stays
// started=false, and its gitWorktree is bound to the persisted (archived) path.
// This is the invariant that makes the status poll, the Lost-restore loop, and
// EnsureRootAgents all pass an archived session by; it is also #970-consistent
// (a load never un-archives). Unlike the Lost path, the worktree directory need
// not even exist on disk — inertness means nothing touches it at load.
func TestFromInstanceData_ArchivedLoadsInert(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_archived_agent"
	shellName := agentName + tmuxTabSeparator + shellTabName

	// Inject a spawn-counting exec so we can prove no tmux session is created on
	// load. Because the Archived branch skips Start entirely, no Restore/spawn
	// ever runs — the counter must stay at zero.
	var newSessions int
	exec := countingExec(map[string]bool{}, &newSessions)
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })

	data := deadInstanceData(t, Archived, agentName, shellName)
	data.Prompt = "run the nightly report"
	// Deliberately DO NOT create data.Worktree.WorktreePath: an inert load must
	// not depend on the worktree existing (the Lost path would MkdirAll it).

	restored, err := FromInstanceData(data)
	require.NoError(t, err)

	assert.Equal(t, Archived, restored.GetStatus(), "status round-trips as Archived")
	assert.False(t, restored.Started(), "an archived session loads inert: Start is skipped, started=false")
	assert.False(t, restored.TabAlive(0), "no tmux session is spawned or reconnected on load")
	assert.Equal(t, 0, newSessions, "loading an archived session must never spawn tmux")
	assert.Equal(t, data.Prompt, restored.Prompt, "persisted prompts must restore onto the instance")
	assert.Equal(t, data.Worktree.WorktreePath, restored.GetWorktreePath(),
		"gitWorktree is bound to the persisted archived path so restore knows where the worktree lives")

	// Round-trip: re-serializing preserves Archived + the archived worktree path.
	out := restored.ToInstanceData()
	assert.Equal(t, Archived, out.Status)
	assert.Equal(t, data.Worktree.WorktreePath, out.Worktree.WorktreePath)
}

// TestArchivedInstance_NotRecoverable: Recover refuses an Archived session even
// if something forced it started — Recover is the Lost off-ramp only. This locks
// the boundary between the two states at the backend level (belt-and-suspenders
// alongside the daemon restore loop's ==Lost gate).
func TestArchivedInstance_NotRecoverable(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	inst := &Instance{Title: "arch", liveness: LiveArchived, backend: &LocalBackend{}}
	err := inst.Recover()
	require.Error(t, err, "an archived session is not Lost, so Recover must reject it")
}

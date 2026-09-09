package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func doctorIntegrityGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s: %s", cmd.String(), string(out))
}

func TestDoctorReportsWorktreeIntegrityDangerWithoutFix(t *testing.T) {
	report := &Report{}
	checkWorktreeIntegrityRows(report, []session.SessionWorktreeInspection{{
		Title:   "idle-lane",
		Warning: "DANGER: 2063 staged paths and zero unstaged paths; HEAD moved without a worktree-local reflog entry",
	}})

	rows := make([]CheckResult, 0)
	for _, row := range report.Checks {
		if row.Name == "worktree-integrity" {
			rows = append(rows, row)
		}
	}
	require.Len(t, rows, 1)
	assert.Equal(t, StatusFail, rows[0].Status)
	assert.True(t, rows[0].Problem)
	assert.Contains(t, rows[0].Detail, "idle-lane")
	assert.Contains(t, rows[0].Remediation, "does not reset or clean")
	assert.Empty(t, report.Findings, "the read-only check must never carry a --fix action")
}

func TestDoctorPassesCleanWorktreeIntegrityScan(t *testing.T) {
	report := &Report{}
	checkWorktreeIntegrityRows(report, []session.SessionWorktreeInspection{{Title: "editing-lane"}})
	require.Len(t, report.Checks, 1)
	assert.Equal(t, StatusPass, report.Checks[0].Status)
}

func TestDoctorPassesOrdinaryLargeStagedChange(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, exec.Command("git", "init", "-q", repo).Run())
	for i := 0; i < 25; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(repo, fmt.Sprintf("file-%02d.txt", i)), []byte("base\n"), 0o644))
	}
	doctorIntegrityGit(t, repo, "add", "-A")
	doctorIntegrityGit(t, repo, "commit", "-q", "-m", "base")
	for i := 0; i < 25; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(repo, fmt.Sprintf("file-%02d.txt", i)), []byte("intentional edit\n"), 0o644))
	}
	doctorIntegrityGit(t, repo, "add", "-A")

	inspections := session.InspectSessionWorktrees([]session.InstanceData{{
		ID: "editor-id", Title: "editor", Liveness: session.LiveReady, BackendType: "local",
		Worktree: session.GitWorktreeData{RepoPath: repo, WorktreePath: repo},
	}})
	require.Len(t, inspections, 1)
	assert.True(t, inspections[0].Evidence.MassRevert, "precondition: the staged-count symptom is present")
	assert.False(t, inspections[0].Evidence.HeadMovedWithoutReflog, "precondition: local HEAD and reflog agree")
	assert.Empty(t, inspections[0].Warning, "an ordinary git add -A must not produce a warning")

	report := &Report{}
	checkWorktreeIntegrityRows(report, inspections)
	require.Len(t, report.Checks, 1)
	assert.Equal(t, StatusPass, report.Checks[0].Status, "an ordinary git add -A must not fail doctor")
}

func TestDoctorReportsMissingHeadReflogAsIncomplete(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, exec.Command("git", "init", "-q", repo).Run())
	doctorIntegrityGit(t, repo, "config", "core.logAllRefUpdates", "false")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "file.txt"), []byte("base\n"), 0o644))
	doctorIntegrityGit(t, repo, "add", "--all")
	doctorIntegrityGit(t, repo, "commit", "-q", "-m", "base")

	inspections := session.InspectSessionWorktrees([]session.InstanceData{{
		ID: "no-reflog", Title: "no-reflog", Liveness: session.LiveReady, BackendType: "local",
		Worktree: session.GitWorktreeData{RepoPath: repo, WorktreePath: repo},
	}})
	require.Len(t, inspections, 1)
	require.Error(t, inspections[0].Err)

	report := &Report{}
	checkWorktreeIntegrityRows(report, inspections)
	require.Len(t, report.Checks, 1)
	assert.Equal(t, StatusWarn, report.Checks[0].Status)
	assert.Contains(t, report.Checks[0].Detail, "HEAD reflog")
	assert.Equal(t, []string{"worktree-integrity"}, report.Incomplete)
}

func TestDoctorReportsIncompleteBranchCorrelation(t *testing.T) {
	report := &Report{}
	checkWorktreeIntegrityRows(report, []session.SessionWorktreeInspection{{
		Title:          "readable",
		CorrelationErr: errors.New("another lane was unreadable"),
	}})

	require.Len(t, report.Checks, 1)
	assert.Equal(t, StatusWarn, report.Checks[0].Status)
	assert.Contains(t, report.Checks[0].Detail, "another lane was unreadable")
	assert.Equal(t, []string{"worktree-integrity"}, report.Incomplete)
}

func TestDoctorDoesNotRequireWorktreeInventoryWhenDaemonIsStopped(t *testing.T) {
	opts := testOptions(t, false)
	called := false
	opts.worktreeInventory = func() ([]session.InstanceData, error) {
		called = true
		return nil, errors.New("dial daemon: socket is absent")
	}

	report, err := Run(opts)
	require.NoError(t, err)
	assert.False(t, called, "a stopped daemon must use persisted state instead of requiring its unavailable snapshot")
	assert.NotContains(t, report.Incomplete, "worktree-integrity")
	for _, check := range report.Checks {
		if check.Name == "worktree-integrity" {
			assert.Equal(t, StatusPass, check.Status)
			assert.Contains(t, check.Detail, "0 live local worktree")
			return
		}
	}
	t.Fatal("doctor omitted the worktree-integrity check")
}

func TestDoctorInspectsPersistedLiveLanesWhenDaemonIsStopped(t *testing.T) {
	opts := testOptions(t, false)
	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, exec.Command("git", "init", "-q", repo).Run())
	require.NoError(t, os.WriteFile(filepath.Join(repo, "file.txt"), []byte("base\n"), 0o644))
	doctorIntegrityGit(t, repo, "add", "--all")
	doctorIntegrityGit(t, repo, "commit", "-q", "-m", "base")
	holder := filepath.Join(filepath.Dir(repo), "holder")
	sibling := filepath.Join(filepath.Dir(repo), "sibling")
	doctorIntegrityGit(t, repo, "worktree", "add", "-q", "-b", "shared", holder, "HEAD")
	doctorIntegrityGit(t, repo, "worktree", "add", "-q", "-b", "takeover", sibling, "HEAD")
	doctorIntegrityGit(t, sibling, "checkout", "-q", "--ignore-other-worktrees", "-B", "shared", "shared")

	rows := []session.InstanceData{
		{ID: "holder-id", Title: "holder", Liveness: session.LiveReady, BackendType: "local", Worktree: session.GitWorktreeData{RepoPath: repo, WorktreePath: holder}},
		{ID: "sibling-id", Title: "sibling", Liveness: session.LiveReady, BackendType: "local", Worktree: session.GitWorktreeData{RepoPath: repo, WorktreePath: sibling}},
	}
	raw, err := json.Marshal(rows)
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("persisted-repo", raw))
	opts.worktreeInventory = func() ([]session.InstanceData, error) {
		return nil, errors.New("dial daemon: socket is absent")
	}

	report, err := Run(opts)
	require.NoError(t, err)
	for _, check := range report.Checks {
		if check.Name == "worktree-integrity" {
			assert.Equal(t, StatusFail, check.Status, "a stopped daemon must not hide live lanes retained in persisted state")
			assert.Contains(t, check.Detail, "holder")
			assert.Contains(t, check.Detail, "sibling")
			return
		}
	}
	t.Fatal("doctor omitted the worktree-integrity check")
}

func TestMergeWorktreeInventoriesRetainsPersistedLaneSkippedByDaemon(t *testing.T) {
	persisted := []session.InstanceData{
		{ID: "holder", Title: "holder", Liveness: session.LiveReady, BackendType: "local", Worktree: session.GitWorktreeData{RepoPath: "/repo", WorktreePath: "/repo/holder"}},
		{ID: "skipped", Title: "skipped", Liveness: session.LiveReady, BackendType: "local", Worktree: session.GitWorktreeData{RepoPath: "/repo", WorktreePath: "/repo/skipped"}},
	}
	live := []session.InstanceData{
		{ID: "holder", Title: "holder-newer", Liveness: session.LiveReady, BackendType: "local", Worktree: session.GitWorktreeData{RepoPath: "/repo", WorktreePath: "/repo/holder"}},
	}

	merged := mergeWorktreeInventories(live, persisted)
	require.Len(t, merged, 2, "a daemon materialization skip must not erase a persisted worktree owner")
	assert.Equal(t, "holder-newer", merged[0].Title, "the daemon projection wins for a row it did materialize")
	assert.Equal(t, "skipped", merged[1].Title)
}

package session

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func worktreeScanGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s: %s", cmd.String(), string(out))
}

func TestInspectSessionWorktreesNamesTheOtherLiveLane(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, exec.Command("git", "init", "-q", repo).Run())
	require.NoError(t, os.WriteFile(filepath.Join(repo, "file.txt"), []byte("base\n"), 0o644))
	worktreeScanGit(t, repo, "add", "--all")
	worktreeScanGit(t, repo, "commit", "-q", "-m", "base")
	holder := filepath.Join(filepath.Dir(repo), "holder")
	sibling := filepath.Join(filepath.Dir(repo), "sibling")
	worktreeScanGit(t, repo, "worktree", "add", "-q", "-b", "shared", holder, "HEAD")
	worktreeScanGit(t, repo, "worktree", "add", "-q", "-b", "takeover", sibling, "HEAD")
	worktreeScanGit(t, sibling, "checkout", "-q", "--ignore-other-worktrees", "-B", "shared", "shared")

	rows := []InstanceData{
		{ID: "holder-id", Title: "holder", Liveness: LiveReady, BackendType: "local", Worktree: GitWorktreeData{RepoPath: repo, WorktreePath: holder}},
		{ID: "takeover-id", Title: "takeover", Liveness: LiveReady, BackendType: "local", Worktree: GitWorktreeData{RepoPath: repo, WorktreePath: sibling}},
	}
	got := InspectSessionWorktrees(rows)
	require.Len(t, got, 2)
	assert.Contains(t, got[0].Warning, `live lane(s) "takeover"`)
	assert.Contains(t, got[1].Warning, `live lane(s) "holder"`)

	rows[1].Liveness = LiveArchived
	got = InspectSessionWorktrees(rows)
	require.Len(t, got, 1, "an archived worktree retains its ref but is not a live collision")
	assert.Empty(t, got[0].Warning)
}

func TestInspectSessionWorktreesIgnoresOrdinaryFullyStagedChange(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, exec.Command("git", "init", "-q", repo).Run())
	for i := 0; i < 25; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(repo, fmt.Sprintf("file-%02d.txt", i)), []byte("base\n"), 0o644))
	}
	worktreeScanGit(t, repo, "add", "--all")
	worktreeScanGit(t, repo, "commit", "-q", "-m", "base")
	for i := 0; i < 25; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(repo, fmt.Sprintf("file-%02d.txt", i)), []byte("intentional edit\n"), 0o644))
	}
	worktreeScanGit(t, repo, "add", "--all")

	got := InspectSessionWorktrees([]InstanceData{{
		ID: "editor-id", Title: "editor", Liveness: LiveReady, BackendType: "local",
		Worktree: GitWorktreeData{RepoPath: repo, WorktreePath: repo},
	}})
	require.Len(t, got, 1)
	assert.True(t, got[0].Evidence.MassRevert, "the raw index shape is present")
	assert.False(t, got[0].Evidence.HeadMovedWithoutReflog, "HEAD still agrees with this worktree's reflog")
	assert.Empty(t, got[0].Warning, "an ordinary fully staged commit must never be surfaced as a takeover danger")
}

func TestInspectSessionWorktreesDistinguishesBranchNamedDetached(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, exec.Command("git", "init", "-q", repo).Run())
	require.NoError(t, os.WriteFile(filepath.Join(repo, "file.txt"), []byte("base\n"), 0o644))
	worktreeScanGit(t, repo, "add", "--all")
	worktreeScanGit(t, repo, "commit", "-q", "-m", "base")
	worktreeScanGit(t, repo, "branch", "-m", "(detached)")

	got := InspectSessionWorktrees([]InstanceData{{
		ID: "legal-detached-name", Title: "legal-detached-name", Liveness: LiveReady, BackendType: "local",
		Worktree: GitWorktreeData{RepoPath: repo, WorktreePath: repo},
	}})
	require.Len(t, got, 1)
	require.NoError(t, got[0].Err)
	require.NoError(t, got[0].CorrelationErr,
		"the repository-wide structural branch record must disambiguate the legal name from detached HEAD")
	assert.Equal(t, "(detached)", got[0].Evidence.Branch)
	assert.Empty(t, got[0].Warning)
}

func TestInspectSessionWorktreesReportsMissingLocalPath(t *testing.T) {
	got := InspectSessionWorktrees([]InstanceData{{
		ID: "missing", Title: "missing", Liveness: LiveReady, BackendType: "local",
		Worktree: GitWorktreeData{RepoPath: t.TempDir()},
	}})
	require.Len(t, got, 1, "a live local lane with no inspectable path is unknown, not inapplicable")
	require.Error(t, got[0].Err)
	assert.Contains(t, got[0].Err.Error(), "path")
}

func TestInspectSessionWorktreesReportsMissingRepositoryIdentity(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, exec.Command("git", "init", "-q", repo).Run())
	require.NoError(t, os.WriteFile(filepath.Join(repo, "file.txt"), []byte("base\n"), 0o644))
	worktreeScanGit(t, repo, "add", "--all")
	worktreeScanGit(t, repo, "commit", "-q", "-m", "base")

	got := InspectSessionWorktrees([]InstanceData{
		{ID: "readable", Title: "readable", Liveness: LiveReady, BackendType: "local", Worktree: GitWorktreeData{RepoPath: repo, WorktreePath: repo}},
		{ID: "missing-repo", Title: "missing-repo", Liveness: LiveReady, BackendType: "local", Worktree: GitWorktreeData{WorktreePath: t.TempDir()}},
	})
	require.Len(t, got, 2, "a missing repository identity must not join an invented clean group")
	require.NoError(t, got[0].Err)
	require.Error(t, got[0].CorrelationErr, "an ungroupable live lane makes every absent duplicate correlation unknown")
	require.Error(t, got[1].Err)
	assert.Contains(t, got[1].Err.Error(), "repository")
}

func TestInspectSessionWorktreesMarksPartialRepositoryScanUnknown(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, exec.Command("git", "init", "-q", repo).Run())
	require.NoError(t, os.WriteFile(filepath.Join(repo, "file.txt"), []byte("base\n"), 0o644))
	worktreeScanGit(t, repo, "add", "--all")
	worktreeScanGit(t, repo, "commit", "-q", "-m", "base")

	got := InspectSessionWorktrees([]InstanceData{
		{ID: "readable", Title: "readable", Liveness: LiveReady, BackendType: "local", Worktree: GitWorktreeData{RepoPath: repo, WorktreePath: repo}},
		{ID: "unreadable", Title: "unreadable", Liveness: LiveReady, BackendType: "local", Worktree: GitWorktreeData{RepoPath: repo, WorktreePath: filepath.Join(repo, "missing")}},
	})
	require.Len(t, got, 2)
	require.NoError(t, got[0].Err, "the checkout itself was readable")
	require.Error(t, got[0].CorrelationErr, "the repository-wide duplicate observation is incomplete")
	assert.Contains(t, got[0].CorrelationErr.Error(), "correlate")
	require.Error(t, got[1].Err)
}

func TestInspectSessionWorktreesKeepsDistinctLanesSharingOneWorktree(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, exec.Command("git", "init", "-q", repo).Run())
	require.NoError(t, os.WriteFile(filepath.Join(repo, "file.txt"), []byte("base\n"), 0o644))
	worktreeScanGit(t, repo, "add", "--all")
	worktreeScanGit(t, repo, "commit", "-q", "-m", "base")

	got := InspectSessionWorktrees([]InstanceData{
		{ID: "first-id", Title: "first", Liveness: LiveReady, BackendType: "local", Worktree: GitWorktreeData{RepoPath: repo, WorktreePath: repo}},
		{ID: "second-id", Title: "second", Liveness: LiveReady, BackendType: "local", Worktree: GitWorktreeData{RepoPath: repo, WorktreePath: repo}},
	})
	require.Len(t, got, 2)
	assert.Contains(t, got[0].Warning, `"second"`)
	assert.Contains(t, got[1].Warning, `"first"`)
}

func TestInspectSessionWorktreesReportsDetachedLanesSharingOneWorktree(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, exec.Command("git", "init", "-q", repo).Run())
	require.NoError(t, os.WriteFile(filepath.Join(repo, "file.txt"), []byte("base\n"), 0o644))
	worktreeScanGit(t, repo, "add", "--all")
	worktreeScanGit(t, repo, "commit", "-q", "-m", "base")
	worktreeScanGit(t, repo, "checkout", "-q", "--detach")

	got := InspectSessionWorktrees([]InstanceData{
		{ID: "first-id", Title: "first", Liveness: LiveReady, BackendType: "local", Worktree: GitWorktreeData{RepoPath: repo, WorktreePath: repo}},
		{ID: "second-id", Title: "second", Liveness: LiveReady, BackendType: "local", Worktree: GitWorktreeData{RepoPath: repo, WorktreePath: repo}},
	})
	require.Len(t, got, 2)
	assert.Empty(t, got[0].Evidence.Branch, "precondition: HEAD is detached, not a branch named (detached)")
	assert.Contains(t, got[0].Warning, `"second"`)
	assert.Contains(t, got[1].Warning, `"first"`)
}

func TestInspectSessionWorktreesRejectsBranchObservationThatChangedAfterPeerScan(t *testing.T) {
	binDir := t.TempDir()
	repo := t.TempDir()
	holder := filepath.Join(repo, "holder")
	peer := filepath.Join(repo, "peer")
	require.NoError(t, os.MkdirAll(holder, 0o755))
	require.NoError(t, os.MkdirAll(peer, 0o755))
	peerFirstStatus := filepath.Join(t.TempDir(), "peer-first-status")
	peerSecondStatus := filepath.Join(t.TempDir(), "peer-second-status")
	fakeGit := filepath.Join(binDir, "git")
	oid := "1111111111111111111111111111111111111111"
	script := fmt.Sprintf(`#!/bin/sh
	if [ "$3" = "worktree" ]; then
		printf 'worktree %%s\0HEAD %%s\0branch refs/heads/shared\0\0worktree %%s\0HEAD %%s\0branch refs/heads/shared\0\0' %q %s %q %s
		exit 0
	fi
if [ "$3" = "log" ]; then
	printf '%%s\n' %s
	exit 0
fi
if [ "$3" != "status" ]; then
	exit 2
fi
branch=shared
if [ "$2" = %q ]; then
	if [ ! -e %q ]; then
		: > %q
		branch=other
	elif [ ! -e %q ]; then
		: > %q
		branch=other
	fi
fi
printf '%%s\n' '# branch.oid %s' "# branch.head $branch"
`, holder, oid, peer, oid, oid, peer, peerFirstStatus, peerFirstStatus, peerSecondStatus, peerSecondStatus, oid)
	require.NoError(t, os.WriteFile(fakeGit, []byte(script), 0o700))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	got := InspectSessionWorktrees([]InstanceData{
		{ID: "holder", Title: "holder", Liveness: LiveReady, BackendType: "local", Worktree: GitWorktreeData{RepoPath: repo, WorktreePath: holder}},
		{ID: "peer", Title: "peer", Liveness: LiveReady, BackendType: "local", Worktree: GitWorktreeData{RepoPath: repo, WorktreePath: peer}},
	})
	require.Len(t, got, 2)
	require.Error(t, got[0].CorrelationErr,
		"a clean correlation must be rejected when a peer's Git observation changed after its local probe")
	require.Error(t, got[1].CorrelationErr,
		"the common repository snapshot invalidates every result in the correlated cohort")
}

func TestInspectSessionWorktreesBoundsCohortCorrelationFailures(t *testing.T) {
	binDir := t.TempDir()
	repo := t.TempDir()
	oid := strings.Repeat("1", 40)
	fakeGit := filepath.Join(binDir, "git")
	script := fmt.Sprintf(`#!/bin/sh
if [ "$3" = "worktree" ]; then
	printf 'worktree %%s\0HEAD %s\0branch refs/heads/main\0\0' "$2"
	exit 0
fi
if [ "$3" = "log" ]; then
	printf '%s\n'
	exit 0
fi
if [ "$3" = "status" ]; then
	printf '# branch.oid %s\n# branch.head main\n'
	exit 0
fi
exit 2
`, oid, oid, oid)
	require.NoError(t, os.WriteFile(fakeGit, []byte(script), 0o700))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	const extraFailures = 4
	rows := make([]InstanceData, 0, maxWorktreeCohortFailureDetails+extraFailures)
	for index := 0; index < cap(rows); index++ {
		path := filepath.Join(repo, fmt.Sprintf("missing-worktree-%02d", index))
		require.NoError(t, os.Mkdir(path, 0o755))
		rows = append(rows, InstanceData{
			ID: fmt.Sprintf("lane-%02d", index), Title: fmt.Sprintf("lane-%02d", index),
			Liveness: LiveReady, BackendType: "local",
			Worktree: GitWorktreeData{RepoPath: repo, WorktreePath: path},
		})
	}

	got := InspectSessionWorktrees(rows)
	require.Len(t, got, len(rows))
	require.Error(t, got[0].CorrelationErr)
	detail := got[0].CorrelationErr.Error()
	for index := 0; index < maxWorktreeCohortFailureDetails; index++ {
		assert.Contains(t, detail, rows[index].Worktree.WorktreePath)
	}
	assert.NotContains(t, detail, rows[len(rows)-1].Worktree.WorktreePath,
		"one lane's warning must not embed every repository failure")
	assert.Contains(t, detail, fmt.Sprintf("and %d more", extraFailures))
}

func TestInspectSessionWorktreesSkipsOnlyPositivelyInapplicableRows(t *testing.T) {
	got := InspectSessionWorktrees([]InstanceData{
		{ID: "archived", Title: "archived", Liveness: LiveArchived, BackendType: "local"},
		{ID: "remote", Title: "remote", Liveness: LiveReady, BackendType: "remote"},
		{ID: "creating", Title: "creating", Liveness: LiveReady, InFlightOp: OpCreating},
		{ID: "legacy-loading", Title: "legacy-loading", Status: Loading},
	})
	assert.Empty(t, got, "archived, remote, and not-yet-created worktrees are positively inapplicable")
	assert.True(t, NeedsWorktreeIntegrityInspection(InstanceData{
		ID: "replacing", Title: "replacing", Status: Loading, Liveness: LiveReady, InFlightOp: OpReplacing,
	}), "Loading is only create-inapplicable without a non-create operation axis")
}

func TestWorktreeWarningIsProjectionOnly(t *testing.T) {
	data := InstanceData{ID: "warning", Title: "lane", BackendType: "remote", Liveness: LiveReady, WorktreeWarning: "DANGER"}
	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	assert.Equal(t, "DANGER", restored.WorktreeWarning())
	assert.Empty(t, restored.ToInstanceData().ForStorage().WorktreeWarning)
	assert.Empty(t, data.ForClientRead().WorktreeWarning)
}

func TestIncompleteWorktreeScanReplacesStaleWarningWithNewDanger(t *testing.T) {
	inst, err := NewInstance(InstanceOptions{Title: "holder", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	require.True(t, inst.ReconcileWorktreeInspection("DANGER: old branch held by old-lane", nil))

	require.True(t, inst.ReconcileWorktreeInspection("DANGER: new branch held by new-lane", assert.AnError))
	warning := inst.WorktreeWarning()
	assert.Contains(t, warning, "new branch held by new-lane", "new positive evidence must replace a stale confirmed warning")
	assert.NotContains(t, warning, "old branch held by old-lane")
	assert.Contains(t, warning, "could not be verified", "the correlation gap must remain visible")
}

func TestWorktreeInspectionCohortRejectsRestoredLane(t *testing.T) {
	inst, err := NewInstance(InstanceOptions{Title: "holder", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	require.True(t, inst.ReconcileWorktreeInspection("DANGER: confirmed duplicate branch", nil))
	inst.SetStatusForTest(Archived)
	archivedSnapshot := inst.ToInstanceData()
	inst.SetStatusForTest(Ready)

	changed, applied := ReconcileWorktreeInspectionCohortIfCurrent([]WorktreeInspectionReconciliation{
		{Instance: inst, Snapshot: archivedSnapshot},
	}, nil)
	assert.False(t, applied, "a restored lane no longer matches its archived scan snapshot")
	assert.Empty(t, changed)
	assert.Contains(t, inst.WorktreeWarning(), "confirmed duplicate branch")
}

func TestWorktreeInspectionCohortRejectsRestoredPeerBeforeAnyClear(t *testing.T) {
	holder, err := NewInstance(InstanceOptions{Title: "holder", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	peer, err := NewInstance(InstanceOptions{Title: "peer", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	require.True(t, holder.ReconcileWorktreeInspection("DANGER: confirmed duplicate branch", nil))
	holderSnapshot := holder.ToInstanceData()
	peer.SetStatusForTest(Archived)
	peerSnapshot := peer.ToInstanceData()
	peer.SetStatusForTest(Ready)

	changed, applied := ReconcileWorktreeInspectionCohortIfCurrent([]WorktreeInspectionReconciliation{
		{Instance: holder, Snapshot: holderSnapshot},
		{Instance: peer, Snapshot: peerSnapshot},
	}, nil)

	assert.False(t, applied, "one changed peer invalidates the whole correlated observation")
	assert.Empty(t, changed)
	assert.Contains(t, holder.WorktreeWarning(), "confirmed duplicate branch",
		"a later peer mismatch must not follow an earlier warning clear")
}

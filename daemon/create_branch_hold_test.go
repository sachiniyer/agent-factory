package daemon

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// holdBranchInArchivedWorktree reproduces what archiving leaves behind (#2013):
// a worktree on `branch`, MOVED into the AF home's archive layout and still
// registered with git, so the branch stays checked out under a path no session
// record points at. It returns the archived worktree path.
func holdBranchInArchivedWorktree(t *testing.T, repoPath, branch, archiveName string) string {
	t.Helper()
	live := filepath.Join(t.TempDir(), "live")
	out, err := exec.Command("git", "-C", repoPath, "worktree", "add", "-b", branch, live).CombinedOutput()
	require.NoError(t, err, string(out))

	archived := filepath.Join(t.TempDir(), archiveName+" (archived)")
	out, err = exec.Command("git", "-C", repoPath, "worktree", "move", live, archived).CombinedOutput()
	require.NoError(t, err, string(out))
	return archived
}

// TestNextAvailableTitle_SkipsBranchHeldByArchivedWorktree is the #2091
// regression lock at the production call site: the derived-title walk that every
// session-creating scheduled task goes through (taskrun passes TitleBase, which
// routes here).
//
// The rot: a recurring task creates "sweep", archives it, and the archived
// worktree keeps branch <prefix>sweep checked out. Nothing in the title walk
// consulted git, so the next run happily handed back a title whose branch a
// registered worktree already held — and `git worktree add` then failed hard,
// every run, forever.
//
// The seeded worktrees deliberately have NO session records: that is the field
// shape (#2091 shows archived worktrees whose rows had been renamed out from
// under them), and it is the case only git can answer.
func TestNextAvailableTitle_SkipsBranchHeldByArchivedWorktree(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)

	holdBranchInArchivedWorktree(t, repoPath, manager.branchForTitle("sweep"), "sweep")
	holdBranchInArchivedWorktree(t, repoPath, manager.branchForTitle("sweep-2"), "sweep-2")

	manager.mu.Lock()
	title, err := manager.nextAvailableTitleLocked(repoID, repoPath, "sweep", "claude", runtimeNamespaceLocalTmux, nil)
	manager.mu.Unlock()
	require.NoError(t, err)

	assert.Equal(t, "sweep-3", title,
		"the walk must skip every suffix an archived worktree still holds")

	// Not merely unheld on paper — the worktree the create exists to build
	// actually gets built under the resolved name.
	dest := filepath.Join(t.TempDir(), "run")
	out, addErr := exec.Command("git", "-C", repoPath, "worktree", "add", "-b",
		manager.branchForTitle(title), dest).CombinedOutput()
	require.NoError(t, addErr, "resolved title %q must be usable: %s", title, string(out))
}

// TestNextAvailableTitle_UncontestedNameKeepsBareForm is the no-regression side:
// consulting git must not push an uncontested name off its bare form.
func TestNextAvailableTitle_UncontestedNameKeepsBareForm(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)

	manager.mu.Lock()
	title, err := manager.nextAvailableTitleLocked(repoID, repoPath, "sweep", "claude", runtimeNamespaceLocalTmux, nil)
	manager.mu.Unlock()

	require.NoError(t, err)
	assert.Equal(t, "sweep", title)
}

// TestNextAvailableTitle_LongTitleWalkConverges is the #2528 P3-b regression at
// the daemon call site. A long base title whose derived branch is already held
// used to make the walk NON-CONVERGENT: branch truncation collapsed every
// suffixed rung ("base-2", "base-3", …) to the SAME branch as the held base, so
// the walk skipped all 10,000 rungs under m.mu and failed with "could not find an
// available title". Bounding the base so the suffix survives lets it resolve at a
// low rung.
func TestNextAvailableTitle_LongTitleWalkConverges(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)

	base := strings.Repeat("a", 300)
	// Hold the base's derived branch exactly as an archived session would, so the
	// bare base (rung 1) is taken and the walk must find a distinct suffix.
	holdBranchInArchivedWorktree(t, repoPath, manager.branchForTitle(base), "held")

	manager.mu.Lock()
	title, err := manager.nextAvailableTitleLocked(repoID, repoPath, base, "claude", runtimeNamespaceLocalTmux, nil)
	manager.mu.Unlock()
	require.NoError(t, err, "the walk must converge, not exhaust 10,000 rungs")

	require.NotEqual(t, base, title, "the held bare base must be skipped")
	assert.NotEqual(t, manager.branchForTitle(base), manager.branchForTitle(title),
		"the resolved title must derive a DISTINCT branch, not one truncation re-collides")

	// The resolved branch is actually creatable — not merely unheld on paper.
	dest := filepath.Join(t.TempDir(), "run")
	out, addErr := exec.Command("git", "-C", repoPath, "worktree", "add", "-b",
		manager.branchForTitle(title), dest).CombinedOutput()
	require.NoError(t, addErr, "resolved title %q must be usable: %s", title, string(out))
}

// TestNextAvailableTitle_ExistingBranchIsNotAHold keeps the new check as narrow
// as the failure it fixes. AF reuses an existing branch when one matches the
// derived name (setupFromExistingBranch), so a branch that merely EXISTS must
// not cost the session its bare title — only a branch some worktree has
// checked out is unusable.
func TestNextAvailableTitle_ExistingBranchIsNotAHold(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)

	out, err := exec.Command("git", "-C", repoPath, "branch", manager.branchForTitle("sweep")).CombinedOutput()
	require.NoError(t, err, string(out))

	manager.mu.Lock()
	title, err := manager.nextAvailableTitleLocked(repoID, repoPath, "sweep", "claude", runtimeNamespaceLocalTmux, nil)
	manager.mu.Unlock()

	require.NoError(t, err)
	assert.Equal(t, "sweep", title)
}

// linesMentioning returns the captured log lines that name subject, so an
// assertion about ONE create's output is not disturbed by whatever else a
// Manager's background goroutines log into the same capture.
func linesMentioning(captured, subject string) []string {
	var out []string
	for _, line := range strings.Split(captured, "\n") {
		if strings.Contains(line, subject) {
			out = append(out, line)
		}
	}
	return out
}

// TestNextAvailableTitle_HeldSuffixWalkLogsOneLine is the #3838 regression. The
// walk logged one INFO line per rung it skipped, so a recurring task whose
// archived predecessors hold 110+ consecutive suffixes re-logged its entire own
// history on every hourly create — 89% of the daemon log on 2026-09-03, and a
// size-capped log whose diagnostic retention window shrinks as the archive
// grows. The per-rung line carries nothing after the first occurrence; the
// count, the two ends of the run, and the suffix chosen do.
//
// The assertion is a COUNT, which is what makes it anti-vacuous: a capture that
// was never installed on this Manager reads zero lines and fails "exactly one"
// as loudly as the per-rung walk fails it with five. It reads the per-Manager
// INFO seam (#3797) rather than the process-global sink, so no other Manager
// alive in the test binary can satisfy it.
func TestNextAvailableTitle_HeldSuffixWalkLogsOneLine(t *testing.T) {
	manager, logs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)

	const held = 5
	first := holdBranchInArchivedWorktree(t, repoPath, manager.branchForTitle("sweep"), "sweep")
	last := first
	for i := 2; i <= held; i++ {
		rung := fmt.Sprintf("sweep-%d", i)
		last = holdBranchInArchivedWorktree(t, repoPath, manager.branchForTitle(rung), rung)
	}

	// Only the walk's own output is under test; the constructor logs on its own.
	logs.info.Reset()

	manager.mu.Lock()
	title, err := manager.nextAvailableTitleLocked(repoID, repoPath, "sweep", "claude", runtimeNamespaceLocalTmux, nil)
	manager.mu.Unlock()
	require.NoError(t, err)
	require.Equal(t, "sweep-6", title)

	lines := linesMentioning(logs.info.String(), "sweep")
	require.Len(t, lines, 1,
		"the walk must log ONE line per create, not one per skipped rung; captured:\n%s", logs.info.String())
	assert.Contains(t, lines[0], "skipped 5 suffixes",
		"the one line must carry the count the per-rung lines used to spell out")
	assert.Contains(t, lines[0], first, "…and the first holder path")
	assert.Contains(t, lines[0], last, "…and the last holder path")
	assert.Contains(t, lines[0], `using "sweep-6"`, "…and the suffix it settled on")
}

// TestNextAvailableTitle_CeilingFailureNamesHeldSuffixHolders is the other half
// of #3838: the per-rung detail the routine create stopped logging has to
// survive where it is actually needed. A walk that exhausts its rungs found
// nothing, and "which worktrees are in the way" is the only actionable thing
// left to say — so the holders go in the error, not in 10,000 log lines nobody
// reads on the 9,999 creates that succeed.
//
// Every rung is held through the branchesHeldByWorktrees seam rather than by
// seeding 10,000 real worktrees: the ceiling is the production constant, and
// this is the only way to reach it in a test.
func TestNextAvailableTitle_CeilingFailureNamesHeldSuffixHolders(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)

	const rungs = 10000
	held := make(map[string]string, rungs)
	holderFor := func(rung string) string { return filepath.Join("/archived", rung+" (archived)") }
	rungTitle := func(i int) string {
		if i == 1 {
			return "sweep"
		}
		return fmt.Sprintf("sweep-%d", i)
	}
	for i := 1; i <= rungs; i++ {
		held[manager.branchForTitle(rungTitle(i))] = holderFor(rungTitle(i))
	}
	prev := branchesHeldByWorktrees
	branchesHeldByWorktrees = func(string) (map[string]string, error) { return held, nil }
	t.Cleanup(func() { branchesHeldByWorktrees = prev })

	manager.mu.Lock()
	_, err := manager.nextAvailableTitleLocked(repoID, repoPath, "sweep", "claude", runtimeNamespaceLocalTmux, nil)
	manager.mu.Unlock()
	require.Error(t, err)

	assert.Contains(t, err.Error(), `could not find an available title for "sweep"`,
		"the message callers already match on must survive the added detail")
	assert.Contains(t, err.Error(), fmt.Sprintf("%d of the suffixes tried", rungs))
	assert.Contains(t, err.Error(), fmt.Sprintf("%q at %s", rungTitle(1), holderFor(rungTitle(1))),
		"the first holder must be named")
	assert.Contains(t, err.Error(), fmt.Sprintf("%q at %s", rungTitle(rungs), holderFor(rungTitle(rungs))),
		"…and so must the last")

	// The list is capped, and says so. Uncapped, this one error is several
	// hundred kilobytes — and a create that fails here fails on every run, which
	// would reinstate the log volume this issue removes an order of magnitude
	// larger.
	assert.Contains(t, err.Error(), fmt.Sprintf("… %d more …", rungs-heldRungListCap),
		"an elided list must name what it dropped")
	assert.Less(t, len(err.Error()), 64*1024,
		"the failure detail must stay bounded, not scale with the 10,000-rung ceiling")
}

// TestHeldSuffixDetail_ListsEveryHolderBelowTheCap is the uncapped side: a real
// archive depth is far below the cap, and there the list is complete.
func TestHeldSuffixDetail_ListsEveryHolderBelowTheCap(t *testing.T) {
	var skipped []heldRung
	for i := 1; i <= 3; i++ {
		skipped = append(skipped, heldRung{
			title:  fmt.Sprintf("sweep-%d", i),
			holder: fmt.Sprintf("/archived/sweep-%d (archived)", i),
		})
	}

	detail := heldRungDetail(skipped)
	for _, rung := range skipped {
		assert.Contains(t, detail, fmt.Sprintf("%q at %s", rung.title, rung.holder))
	}
	assert.NotContains(t, detail, "more …", "a list under the cap is complete, not elided")

	assert.Empty(t, heldRungDetail(nil), "no holds, no clause")
}

package daemon

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// registerMissingWorktreeRow registers an instance whose recorded worktree path
// never existed, so the next probe pass flags it missing — the #5547 fixture.
// Unlike registerGoneWorktree it creates no real worktree:
// NewGitWorktreeFromStorage is a pure projection of the record and the probe is
// a bounded lstat, so plain ENOENT flags the row without a `git worktree add`
// per fixture (which matters at the ~threshold counts these tests need).
func registerMissingWorktreeRow(t *testing.T, m *Manager, repoID, repoPath, title string, status session.Status) *session.Instance {
	t.Helper()
	wtPath := filepath.Join(repoPath, "missing-wt-"+title)
	gw, err := sessiongit.NewGitWorktreeFromStorage(repoPath, wtPath, title, "af/"+title, "", false, true)
	require.NoError(t, err)
	inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: repoPath, Program: "claude"})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetGitWorktreeForTest(gw)
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(status)
	m.mu.Lock()
	m.instances[daemonInstanceKey(repoID, title)] = inst
	m.mu.Unlock()
	return inst
}

// missingWorktreePass registers n rows of one settled status — Ready for a
// live row, Archived for an archived one — each flaggable by the next pass, and
// seeds their disk records in ONE SaveInstances call so the flag write lands
// (seedDiskInstance overwrites the file per call, so a multi-row fixture
// cannot seed a row at a time).
func missingWorktreePass(t *testing.T, m *Manager, repoID, repoPath, prefix string, n int, status session.Status) []worktreeProbeTarget {
	t.Helper()
	targets := make([]worktreeProbeTarget, 0, n)
	rows := make([]session.InstanceData, 0, n)
	for i := 0; i < n; i++ {
		title := fmt.Sprintf("%s-%d", prefix, i)
		inst := registerMissingWorktreeRow(t, m, repoID, repoPath, title, status)
		targets = append(targets, worktreeProbeTarget{repoID: repoID, instance: inst})
		rows = append(rows, session.InstanceData{Title: title, Path: repoPath, Status: status})
	}
	raw, err := json.Marshal(rows)
	require.NoError(t, err)
	require.NoError(t, config.LoadState().SaveInstances(repoID, raw))
	return targets
}

// A live row whose tracked worktree vanishes is the anomaly the per-session
// WARNING exists for. One row, one pass: the line must survive #5547's summary
// path byte-for-byte.
func TestRefreshWorktreesMissing_SingleLiveMissKeepsPerRowWarning(t *testing.T) {
	manager, logs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	targets := missingWorktreePass(t, manager, repoID, repoPath, "lone", 1, session.Ready)

	manager.refreshWorktreesMissing(targets)

	want := fmt.Sprintf("session \"lone-0\": tracked worktree %s is gone (deleted outside af); flagging it — archive it to resolve (its branch is kept); if a directory reappears at that path, inspect it before acting, since af cannot verify it is the session's",
		targets[0].instance.GetWorktreePath())
	warnings := logs.warnings.String()
	assert.Equal(t, 1, strings.Count(warnings, "is gone (deleted outside af)"),
		"a single live miss must produce exactly one warning line")
	assert.Contains(t, warnings, want, "the live-row warning must keep its pre-#5547 text")
}

// An archived row whose tracked worktree is gone is a normal state on a home
// that reclaims archives (on_archive_command, manual cleanup) — not an anomaly
// worth a WARNING. #5547: the transition logs at INFO while the flag still
// sets and persists exactly as before.
func TestRefreshWorktreesMissing_ArchivedMissesLogInfoNotWarning(t *testing.T) {
	manager, logs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	const n = 3
	targets := missingWorktreePass(t, manager, repoID, repoPath, "arch", n, session.Archived)

	manager.refreshWorktreesMissing(targets)

	assert.NotContains(t, logs.warnings.String(), "is gone (deleted outside af)",
		"archived misses must not warn")
	assert.Equal(t, n, strings.Count(logs.info.String(), "is gone (deleted outside af)"),
		"each archived miss still reports its row at INFO")
	for _, target := range targets {
		missing, _ := target.instance.WorktreeMissing()
		assert.True(t, missing, "the flag itself is unaffected by the log-level change")
	}
}

// A mass flagging — a whole worktree root deleted out from under a live fleet —
// must not bury the log a line per row: past worktreeMissingSummaryThreshold
// the pass emits ONE summary WARNING and moves the per-row detail to INFO
// (#5547).
func TestRefreshWorktreesMissing_OverThresholdLiveEmitsOneSummaryWarning(t *testing.T) {
	manager, logs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	const n = worktreeMissingSummaryThreshold + 1
	targets := missingWorktreePass(t, manager, repoID, repoPath, "live", n, session.Ready)

	manager.refreshWorktreesMissing(targets)

	warnings := logs.warnings.String()
	assert.Equal(t, 1, strings.Count(warnings, "sessions whose tracked worktree is missing"),
		"the pass must emit exactly one summary warning")
	assert.Contains(t, warnings,
		fmt.Sprintf("flagged %d sessions whose tracked worktree is missing (0 archived); 'af sessions list' shows them", n))
	assert.NotContains(t, warnings, "is gone (deleted outside af)",
		"per-row detail must leave the WARNING level")
	assert.Equal(t, n, strings.Count(logs.info.String(), "is gone (deleted outside af)"),
		"every flagged row still reports at INFO")
}

// The reported #5547 shape: thousands of archived rows converge to
// worktree-missing on a home that reclaims archive directories. Over the
// threshold the pass is still ONE summary WARNING — every flagged row here is
// archived, so the summary's parenthetical accounts for all of them — with the
// per-row detail at INFO.
func TestRefreshWorktreesMissing_OverThresholdAllArchivedSummarizes(t *testing.T) {
	manager, logs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	const n = worktreeMissingSummaryThreshold + 1
	targets := missingWorktreePass(t, manager, repoID, repoPath, "arch", n, session.Archived)

	manager.refreshWorktreesMissing(targets)

	warnings := logs.warnings.String()
	assert.Equal(t, 1, strings.Count(warnings, "sessions whose tracked worktree is missing"),
		"the backlog sweep must produce exactly one warning line")
	assert.Contains(t, warnings,
		fmt.Sprintf("flagged %d sessions whose tracked worktree is missing (%d archived); 'af sessions list' shows them", n, n))
	assert.Equal(t, n, strings.Count(logs.info.String(), "is gone (deleted outside af)"),
		"every flagged archived row still reports at INFO")
}

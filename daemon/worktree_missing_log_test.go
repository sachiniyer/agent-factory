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

// missingWorktreeGroup is one homogeneously-statused batch of fixture rows:
// prefix-0 … prefix-(n-1) at the given settled status (Ready for live,
// Archived for archived).
type missingWorktreeGroup struct {
	prefix string
	n      int
	status session.Status
}

// missingWorktreeFixture registers every group's rows — each flaggable by the
// next pass — and seeds ALL their disk records in ONE SaveInstances call so
// every flag write lands (SaveInstances overwrites the file, so a mixed
// fixture cannot seed per group). Returns the pass's probe targets.
func missingWorktreeFixture(t *testing.T, m *Manager, repoID, repoPath string, groups ...missingWorktreeGroup) []worktreeProbeTarget {
	t.Helper()
	var targets []worktreeProbeTarget
	var rows []session.InstanceData
	for _, g := range groups {
		for i := 0; i < g.n; i++ {
			title := fmt.Sprintf("%s-%d", g.prefix, i)
			inst := registerMissingWorktreeRow(t, m, repoID, repoPath, title, g.status)
			targets = append(targets, worktreeProbeTarget{repoID: repoID, instance: inst})
			rows = append(rows, session.InstanceData{Title: title, Path: repoPath, Status: g.status})
		}
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
	targets := missingWorktreeFixture(t, manager, repoID, repoPath,
		missingWorktreeGroup{"lone", 1, session.Ready})

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
	targets := missingWorktreeFixture(t, manager, repoID, repoPath,
		missingWorktreeGroup{"arch", n, session.Archived})

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

// A mass flagging that is genuinely anomalous — a whole worktree root deleted
// out from under a live fleet — earns exactly one summary WARNING for the
// pass, with the per-row detail at INFO (#5547).
func TestRefreshWorktreesMissing_OverThresholdLiveEmitsOneSummaryWarning(t *testing.T) {
	manager, logs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	const n = worktreeMissingSummaryThreshold + 1
	targets := missingWorktreeFixture(t, manager, repoID, repoPath,
		missingWorktreeGroup{"live", n, session.Ready})

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
// worktree-missing on a home that reclaims archive directories. However large
// the backlog, it is intentional there — the summary WARNING is keyed on the
// LIVE count, so an archive-only pass produces ZERO warnings and the per-row
// detail collapses to one summary INFO line.
func TestRefreshWorktreesMissing_OverThresholdAllArchivedEmitsNoWarning(t *testing.T) {
	manager, logs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	const n = worktreeMissingSummaryThreshold + 1
	targets := missingWorktreeFixture(t, manager, repoID, repoPath,
		missingWorktreeGroup{"arch", n, session.Archived})

	manager.refreshWorktreesMissing(targets)

	assert.NotContains(t, logs.warnings.String(), "tracked worktree",
		"an archive-only backlog must never warn, however large")
	infos := logs.info.String()
	assert.Equal(t, 1, strings.Count(infos, "sessions whose tracked worktree is missing"),
		"the backlog collapses to one summary INFO line")
	assert.Contains(t, infos,
		fmt.Sprintf("flagged %d archived sessions whose tracked worktree is missing; 'af sessions list' shows them", n))
	assert.NotContains(t, infos, "is gone (deleted outside af)",
		"per-row detail gives way to the summary at this scale")
	for _, target := range targets {
		missing, _ := target.instance.WorktreeMissing()
		assert.True(t, missing)
	}
}

// A pass flagging an over-threshold LIVE set warns once even when archived
// rows are flagged beside it: the summary WARNING counts all the rows and its
// parenthetical keeps the archived share visible (#5547).
func TestRefreshWorktreesMissing_MixedOverThresholdEmitsOneSummaryWarning(t *testing.T) {
	manager, logs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	const live = worktreeMissingSummaryThreshold + 1
	const archived = 3
	targets := missingWorktreeFixture(t, manager, repoID, repoPath,
		missingWorktreeGroup{"live", live, session.Ready},
		missingWorktreeGroup{"arch", archived, session.Archived})

	manager.refreshWorktreesMissing(targets)

	warnings := logs.warnings.String()
	assert.Equal(t, 1, strings.Count(warnings, "sessions whose tracked worktree is missing"),
		"a mixed over-threshold pass still emits exactly one warning")
	assert.Contains(t, warnings,
		fmt.Sprintf("flagged %d sessions whose tracked worktree is missing (%d archived); 'af sessions list' shows them", live+archived, archived))
	assert.NotContains(t, warnings, "is gone (deleted outside af)")
	assert.Equal(t, live+archived, strings.Count(logs.info.String(), "is gone (deleted outside af)"),
		"live and archived rows alike report their detail at INFO")
}

// Live misses below the threshold keep their own WARNINGs even when the same
// pass sweeps an over-threshold archived backlog: the backlog summarizes at
// INFO while the anomalies stay loud (#5547).
func TestRefreshWorktreesMissing_SmallLiveSetStillWarnsBesideArchivedBacklog(t *testing.T) {
	manager, logs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	const live = 2
	const archived = worktreeMissingSummaryThreshold + 1
	targets := missingWorktreeFixture(t, manager, repoID, repoPath,
		missingWorktreeGroup{"live", live, session.Ready},
		missingWorktreeGroup{"arch", archived, session.Archived})

	manager.refreshWorktreesMissing(targets)

	warnings := logs.warnings.String()
	assert.Equal(t, live, strings.Count(warnings, "is gone (deleted outside af)"),
		"each live miss keeps its own warning")
	assert.NotContains(t, warnings, "sessions whose tracked worktree is missing",
		"no summary WARNING without an over-threshold live count")
	assert.Contains(t, logs.info.String(),
		fmt.Sprintf("flagged %d archived sessions whose tracked worktree is missing; 'af sessions list' shows them", archived))
}

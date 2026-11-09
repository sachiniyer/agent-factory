package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/overlay"
)

// -- Helper function tests --

func TestEnsureSkippedRowsSortedDeterministically(t *testing.T) {
	// A skipped set with multiple ID-only repos appends synthesized rows in a
	// deterministic, sorted order after the already-sorted project list. The
	// background snapshot runs ensureSkippedRows on every poll; iterating the
	// map directly would reorder unchanged skipped repos between polls, making
	// rows jump and churning repaints (#4742 review).
	projects := []overlay.Project{
		{RepoID: "alpha", Name: "alpha", Root: "/repos/alpha"},
	}
	skipped := map[string]bool{"delta-skipped": true, "beta-skipped": true, "gamma-skipped": true}
	got := ensureSkippedRows(projects, skipped)
	require.Len(t, got, 4, "the three skipped IDs each synthesize a row after alpha")
	assert.Equal(t, "alpha", got[0].RepoID, "the existing sorted project stays first")
	assert.Equal(t, "beta-skipped", got[1].RepoID)
	assert.Equal(t, "delta-skipped", got[2].RepoID)
	assert.Equal(t, "gamma-skipped", got[3].RepoID)

	// A repeated call with the same set produces the same order (no map-iteration churn).
	got2 := ensureSkippedRows(projects, skipped)
	assert.Equal(t, got, got2, "synthesized rows are stable across calls")

	// An empty/nil set is a no-op; an already-present RepoID is not duplicated.
	assert.Equal(t, projects, ensureSkippedRows(projects, nil))
	dup := ensureSkippedRows(projects, map[string]bool{"alpha": true})
	assert.Len(t, dup, 1, "a skipped ID already represented is not synthesized again")
}

func TestSkippedRepoForRepoID(t *testing.T) {
	skipped := []daemon.SkippedRepo{
		{RepoID: "alpha", Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON},
		{RepoID: "beta", Reason: daemon.SkippedRepoReasonUnreadableInstancesJSON},
	}
	t.Run("found", func(t *testing.T) {
		s := skippedRepoForRepoID(skipped, "alpha")
		require.NotNil(t, s)
		assert.Equal(t, "alpha", s.RepoID)
		assert.Equal(t, daemon.SkippedRepoReasonCorruptedInstancesJSON, s.Reason)
	})
	t.Run("not found returns nil", func(t *testing.T) {
		assert.Nil(t, skippedRepoForRepoID(skipped, "gamma"))
	})
	t.Run("empty set returns nil", func(t *testing.T) {
		assert.Nil(t, skippedRepoForRepoID(nil, "alpha"))
	})
}

func TestSkippedRepoReasonLabel(t *testing.T) {
	for _, tc := range []struct {
		reason, want string
	}{
		{daemon.SkippedRepoReasonCorruptedInstancesJSON, "its instances.json is corrupted"},
		{daemon.SkippedRepoReasonUnreadableInstancesJSON, "the daemon could not read its instances.json"},
		{daemon.SkippedRepoReasonNewerSchemaInstancesJSON, "its instances.json was written by a newer af"},
		{daemon.SkippedRepoReasonRowsFailedToLoad, "some sessions could not be loaded (worktree or tmux gone)"},
		{"", "its instances.json is corrupted"},
		{"unknown-reason", "its instances.json is corrupted"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			assert.Equal(t, tc.want, skippedRepoReasonLabel(tc.reason))
		})
	}
}

// TestSkippedRepoRepairHintsMirrorAPI asserts the TUI's repair hints are verbatim
// copies of api/skipped_repos.go's constants, so the TUI, the CLI and the web all
// send the operator to the SAME remedy. Drift is the bug (#4742 parity).
func TestSkippedRepoRepairHintsMirrorAPI(t *testing.T) {
	// corruptedRepoRepairHint mirrors api/api.go's corruptedRepoRepairHint.
	assert.Equal(t, "Stop the af daemon, copy the corrupt file(s) aside, fix the JSON or restore it from a backup, then start the daemon; af keeps no backup of these files.", corruptedRepoRepairHint)
	// unreadableRepoRepairHint mirrors api/skipped_repos.go's unreadableRepoRepairHint.
	assert.Equal(t, "Make the file(s) readable by the user the af daemon runs as (check ownership, permissions and the disk), then restart the daemon so it re-reads them; do not delete them, they hold the hidden sessions.", unreadableRepoRepairHint)
	// newerSchemaRepoRepairHint mirrors api/skipped_repos.go's newerSchemaRepoRepairHint.
	assert.Equal(t, "Upgrade af to a version that understands the newer state file, then restart the daemon; do not edit or delete these files, they hold sessions this binary cannot read.", newerSchemaRepoRepairHint)
	// rowsFailedRepoRepairHint mirrors api/skipped_repos.go's rowsFailedRepoRepairHint.
	assert.Equal(t, "Restore or delete the sessions the daemon could not load (their worktree or tmux is gone), then the daemon re-reads the complete list on the next poll; e.g. `af sessions restore <title>` or `af sessions kill <title>`; the instances.json itself is not the problem.", rowsFailedRepoRepairHint)
}

func TestSkippedRepoRepairHintForReason(t *testing.T) {
	assert.Equal(t, corruptedRepoRepairHint, skippedRepoRepairHintForReason(daemon.SkippedRepoReasonCorruptedInstancesJSON))
	assert.Equal(t, unreadableRepoRepairHint, skippedRepoRepairHintForReason(daemon.SkippedRepoReasonUnreadableInstancesJSON))
	assert.Equal(t, newerSchemaRepoRepairHint, skippedRepoRepairHintForReason(daemon.SkippedRepoReasonNewerSchemaInstancesJSON))
	assert.Equal(t, rowsFailedRepoRepairHint, skippedRepoRepairHintForReason(daemon.SkippedRepoReasonRowsFailedToLoad))
	// Unknown/empty → corrupted.
	assert.Equal(t, corruptedRepoRepairHint, skippedRepoRepairHintForReason(""))
	assert.Equal(t, corruptedRepoRepairHint, skippedRepoRepairHintForReason("unknown"))
}

func TestSkippedRepoStandingNotice(t *testing.T) {
	t.Run("nil returns empty", func(t *testing.T) {
		assert.Equal(t, "", skippedRepoStandingNotice(nil, "E"))
	})
	t.Run("corrupted reason", func(t *testing.T) {
		s := &daemon.SkippedRepo{RepoID: "repo-1", Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON}
		notice := skippedRepoStandingNotice(s, "E")
		assert.Contains(t, notice, "Sessions in this project are hidden")
		assert.Contains(t, notice, "its instances.json is corrupted")
		assert.Contains(t, notice, "Press E for repair steps")
	})
	t.Run("rows-failed reason", func(t *testing.T) {
		s := &daemon.SkippedRepo{RepoID: "repo-2", Reason: daemon.SkippedRepoReasonRowsFailedToLoad, FailedRows: 3}
		notice := skippedRepoStandingNotice(s, "E")
		assert.Contains(t, notice, "some sessions could not be loaded")
	})
	t.Run("empty details key omits the instruction", func(t *testing.T) {
		s := &daemon.SkippedRepo{RepoID: "repo-3", Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON}
		notice := skippedRepoStandingNotice(s, "")
		assert.Contains(t, notice, "Sessions in this project are hidden")
		assert.NotContains(t, notice, "Press", "no key instruction when details cannot be opened")
	})
}

func TestSkippedRepoNotice(t *testing.T) {
	t.Run("nil returns nil", func(t *testing.T) {
		assert.Nil(t, skippedRepoNotice(nil))
	})
	t.Run("corrupted reason includes repo id, state file, and hint", func(t *testing.T) {
		s := &daemon.SkippedRepo{RepoID: "repo-1", Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON}
		err := skippedRepoNotice(s)
		require.NotNil(t, err)
		msg := err.Error()
		assert.Contains(t, msg, "repo-1")
		assert.Contains(t, msg, "its instances.json is corrupted")
		assert.Contains(t, msg, corruptedRepoRepairHint)
		// The notice names the actual instances.json path (resolved the same
		// way the CLI resolves it) instead of only the opaque RepoID, so the
		// repair details are actionable for a user who does not already know
		// the $AGENT_FACTORY_HOME storage layout (#4742 review).
		assert.Contains(t, msg, "State file:")
		assert.Contains(t, msg, "instances.json")
	})
	t.Run("rows-failed reason includes hint", func(t *testing.T) {
		s := &daemon.SkippedRepo{RepoID: "repo-2", Reason: daemon.SkippedRepoReasonRowsFailedToLoad, FailedRows: 3}
		err := skippedRepoNotice(s)
		require.NotNil(t, err)
		assert.Contains(t, err.Error(), rowsFailedRepoRepairHint)
		// FailedRows is the count the daemon reports for rows-failed-to-load;
		// the notice includes it so a change from one hidden session to many
		// is visible, mirroring the CLI and web (#4742 review).
		assert.Contains(t, err.Error(), "3 session(s) hidden")
	})
	t.Run("rows-failed with zero FailedRows omits the count", func(t *testing.T) {
		s := &daemon.SkippedRepo{RepoID: "repo-3", Reason: daemon.SkippedRepoReasonRowsFailedToLoad}
		err := skippedRepoNotice(s)
		require.NotNil(t, err)
		assert.NotContains(t, err.Error(), "session(s) hidden")
		assert.Contains(t, err.Error(), rowsFailedRepoRepairHint)
	})
}

func TestSkippedRepoNoticeChanged(t *testing.T) {
	t.Run("both nil", func(t *testing.T) {
		assert.False(t, skippedRepoNoticeChanged(nil, nil))
	})
	t.Run("nil to non-nil", func(t *testing.T) {
		assert.True(t, skippedRepoNoticeChanged(nil, &daemon.SkippedRepo{RepoID: "a"}))
	})
	t.Run("non-nil to nil", func(t *testing.T) {
		assert.True(t, skippedRepoNoticeChanged(&daemon.SkippedRepo{RepoID: "a"}, nil))
	})
	t.Run("same entry", func(t *testing.T) {
		prev := &daemon.SkippedRepo{RepoID: "a", Reason: "corrupted", FailedRows: 0}
		cur := &daemon.SkippedRepo{RepoID: "a", Reason: "corrupted", FailedRows: 0}
		assert.False(t, skippedRepoNoticeChanged(prev, cur))
	})
	t.Run("reason change", func(t *testing.T) {
		prev := &daemon.SkippedRepo{RepoID: "a", Reason: "corrupted"}
		cur := &daemon.SkippedRepo{RepoID: "a", Reason: "unreadable"}
		assert.True(t, skippedRepoNoticeChanged(prev, cur))
	})
	t.Run("failed rows change", func(t *testing.T) {
		prev := &daemon.SkippedRepo{RepoID: "a", Reason: "rows-failed-to-load", FailedRows: 1}
		cur := &daemon.SkippedRepo{RepoID: "a", Reason: "rows-failed-to-load", FailedRows: 2}
		assert.True(t, skippedRepoNoticeChanged(prev, cur))
	})
	t.Run("repo id change re-fires", func(t *testing.T) {
		prev := &daemon.SkippedRepo{RepoID: "a", Reason: "corrupted", FailedRows: 0}
		cur := &daemon.SkippedRepo{RepoID: "b", Reason: "corrupted", FailedRows: 0}
		assert.True(t, skippedRepoNoticeChanged(prev, cur), "switching between two skipped projects must re-fire")
	})
}

// -- Cold start path tests --

// TestColdStartFromSnapshot_SkippedRepoRaisesNotice proves the cold-start path
// surfaces a skipped repo (#4742): a repo the daemon dropped at startup
// cold-starts with a standing notice, not a clean empty sidebar.
func TestColdStartFromSnapshot_SkippedRepoRaisesNotice(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot, h.repoID = "/project", "project"
	resizeHome(h, 200, 36)

	h.snapshotFetcher = func(repoID string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{
			Instances:    nil,
			SkippedRepos: []daemon.SkippedRepo{{RepoID: "project", Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON}},
		}, nil
	}

	require.NoError(t, h.coldStartFromSnapshot())
	require.False(t, h.snapshotUnavailable, "a skipped repo is not a daemon-unavailable state")
	require.Equal(t, 0, h.store.NumInstances(), "the skipped repo's sessions are not in the store")

	// The standing notice must be visible on the bar.
	h.errBox.SetSize(200, 1)
	rendered := ansi.Strip(h.errBox.String())
	assert.Contains(t, rendered, "Sessions in this project are hidden")
	assert.Contains(t, rendered, "corrupted")

	// The pending transient notice carries the full repair hint for `E details`.
	require.NotEmpty(t, h.pendingSkippedRepoNotice)
	assert.Contains(t, h.pendingSkippedRepoNotice, corruptedRepoRepairHint)
}

// TestColdStartFromSnapshot_NonSkippedRepoNoNotice proves the cold-start path
// does NOT raise a notice when the repo is not skipped — the healthy steady state.
func TestColdStartFromSnapshot_NonSkippedRepoNoNotice(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot, h.repoID = "/project", "project"
	resizeHome(h, 200, 36)

	h.snapshotFetcher = func(repoID string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{
			Instances: []session.InstanceData{{Title: "alpha"}},
		}, nil
	}
	t.Cleanup(SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		return newSnapshotTestInstance(t, d.Title), nil
	}))

	require.NoError(t, h.coldStartFromSnapshot())
	require.False(t, h.snapshotUnavailable)
	require.NotNil(t, findSidebarInstance(h, "alpha"))

	// No standing notice, no pending notice.
	h.errBox.SetSize(200, 1)
	assert.NotContains(t, ansi.Strip(h.errBox.String()), "hidden")
	assert.Empty(t, h.pendingSkippedRepoNotice)
}

// TestColdStartFromSnapshot_SkippedRepoNotInScope proves the cold-start path
// does NOT raise a notice when the skip set names a DIFFERENT repo — the skip is
// scoped to the request's RepoID, so a skip for another repo is irrelevant.
func TestColdStartFromSnapshot_SkippedRepoNotInScope(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot, h.repoID = "/project", "project"
	resizeHome(h, 200, 36)

	h.snapshotFetcher = func(repoID string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{
			Instances:    []session.InstanceData{{Title: "alpha"}},
			SkippedRepos: []daemon.SkippedRepo{{RepoID: "other-repo", Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON}},
		}, nil
	}
	t.Cleanup(SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		return newSnapshotTestInstance(t, d.Title), nil
	}))

	require.NoError(t, h.coldStartFromSnapshot())
	require.False(t, h.snapshotUnavailable)
	require.NotNil(t, findSidebarInstance(h, "alpha"))

	// No standing notice for a skip that names a different repo.
	h.errBox.SetSize(200, 1)
	assert.NotContains(t, ansi.Strip(h.errBox.String()), "hidden")
	assert.Empty(t, h.pendingSkippedRepoNotice)
}

// -- Poll path (handleSnapshot) tests --

// TestHandleSnapshot_SkippedRepoRaisesNotice proves the poll path surfaces a
// skipped repo (#4742): a successful snapshot with a non-empty skip set naming
// the current repo raises a standing notice instead of presenting the partial
// list as complete.
func TestHandleSnapshot_SkippedRepoRaisesNotice(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot, h.repoID = "/project", "project"
	resizeHome(h, 200, 36)

	// First poll: healthy — no skip, no notice.
	h.handleSnapshot(snapshotFetchedMsg{
		repoID: "project",
		data:   []session.InstanceData{{Title: "alpha"}},
	})
	t.Cleanup(SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		return newSnapshotTestInstance(t, d.Title), nil
	}))
	h.errBox.SetSize(200, 1)
	assert.NotContains(t, ansi.Strip(h.errBox.String()), "hidden")

	// Second poll: the daemon skipped this repo. The notice must appear.
	changed := h.handleSnapshot(snapshotFetchedMsg{
		repoID: "project",
		skippedRepos: []daemon.SkippedRepo{
			{RepoID: "project", Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON},
		},
	})
	assert.True(t, changed, "a skip-state transition must repaint")
	rendered := ansi.Strip(h.errBox.String())
	assert.Contains(t, rendered, "Sessions in this project are hidden")
	assert.Contains(t, rendered, "corrupted")

	// The pending transient notice carries the full repair hint.
	assert.NotEmpty(t, h.pendingSkippedRepoNotice)
	assert.Contains(t, h.pendingSkippedRepoNotice, corruptedRepoRepairHint)
}

// TestHandleSnapshot_SkippedRepoClearedRetractsNotice proves the poll path
// retracts the standing notice when the repo is no longer skipped (the daemon
// re-read the file on a polling refresh, #603).
func TestHandleSnapshot_SkippedRepoClearedRetractsNotice(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot, h.repoID = "/project", "project"
	resizeHome(h, 200, 36)

	// First poll: repo is skipped.
	h.applySkippedRepo(&daemon.SkippedRepo{RepoID: "project", Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON})
	h.errBox.SetSize(200, 1)
	require.Contains(t, ansi.Strip(h.errBox.String()), "hidden")

	// Second poll: repo is no longer skipped (the daemon re-read the file).
	changed := h.handleSnapshot(snapshotFetchedMsg{
		repoID:       "project",
		data:         []session.InstanceData{{Title: "alpha"}},
		skippedRepos: nil,
	})
	assert.True(t, changed, "clearing the skip state must repaint")
	assert.NotContains(t, ansi.Strip(h.errBox.String()), "hidden")
	assert.Nil(t, h.skippedRepo, "the skip entry must be cleared")
}

// TestHandleSnapshot_SkippedRepoSameEntryDoesNotRefire proves a same-entry poll
// does not re-fire the transient notice — the 750ms cadence never churns the bar.
func TestHandleSnapshot_SkippedRepoSameEntryDoesNotRefire(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot, h.repoID = "/project", "project"
	resizeHome(h, 200, 36)

	skip := &daemon.SkippedRepo{RepoID: "project", Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON}

	// First poll: skip appears.
	h.applySkippedRepo(skip)
	require.NotEmpty(t, h.pendingSkippedRepoNotice)

	// Drain the pending notice.
	cmd := h.consumeSkippedRepoNotice()
	require.NotNil(t, cmd)
	assert.Empty(t, h.pendingSkippedRepoNotice, "the pending notice must be drained")

	// Second poll: same skip entry — must not re-fire.
	changed := h.applySkippedRepo(skip)
	assert.False(t, changed, "a same-entry poll must not report a change")
	assert.Empty(t, h.pendingSkippedRepoNotice, "a same-entry poll must not queue a new notice")
}

// TestHandleSnapshot_SkippedRepoReasonChangeRefires proves a reason change
// re-fires the transient notice with the new reason's repair hint.
func TestHandleSnapshot_SkippedRepoReasonChangeRefires(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot, h.repoID = "/project", "project"
	resizeHome(h, 200, 36)

	// First poll: corrupted.
	h.applySkippedRepo(&daemon.SkippedRepo{RepoID: "project", Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON})
	require.Contains(t, h.pendingSkippedRepoNotice, corruptedRepoRepairHint)

	// Drain.
	h.consumeSkippedRepoNotice()

	// Second poll: reason changed to unreadable.
	changed := h.applySkippedRepo(&daemon.SkippedRepo{RepoID: "project", Reason: daemon.SkippedRepoReasonUnreadableInstancesJSON})
	assert.True(t, changed, "a reason change must report a change")
	assert.NotEmpty(t, h.pendingSkippedRepoNotice, "a reason change must queue a new notice")
	assert.Contains(t, h.pendingSkippedRepoNotice, unreadableRepoRepairHint)

	// The standing notice reflects the new reason. Expire the transient so the
	// standing (persistent) notice is what the bar renders.
	h.errBox.Expire()
	h.errBox.SetSize(200, 1)
	assert.Contains(t, ansi.Strip(h.errBox.String()), "could not read")
}

// TestHandleSnapshot_SkippedRepoFailedFetchRetainsNotice proves a transient RPC
// error does NOT clear the standing notice — the last-known skip stays up rather
// than clearing a real skip on a hiccup, matching applyDeliveryAlarms.
func TestHandleSnapshot_SkippedRepoFailedFetchRetainsNotice(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot, h.repoID = "/project", "project"
	resizeHome(h, 200, 36)

	// First: repo is skipped.
	h.applySkippedRepo(&daemon.SkippedRepo{RepoID: "project", Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON})
	h.errBox.SetSize(200, 1)
	require.Contains(t, ansi.Strip(h.errBox.String()), "hidden")

	// Second: fetch fails — the notice must stay.
	h.handleSnapshot(snapshotFetchedMsg{
		repoID: "project",
		err:    errors.New("connection refused"),
	})
	assert.Contains(t, ansi.Strip(h.errBox.String()), "hidden", "a failed fetch must retain the skip notice")
	assert.NotNil(t, h.skippedRepo, "the skip entry must not be cleared on a failed fetch")
}

// TestHandleSnapshot_SkippedRepoNotInScopeNoNotice proves a skip for a different
// repo does not raise a notice for the current repo.
func TestHandleSnapshot_SkippedRepoNotInScopeNoNotice(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot, h.repoID = "/project", "project"
	resizeHome(h, 200, 36)

	changed := h.handleSnapshot(snapshotFetchedMsg{
		repoID: "project",
		data:   []session.InstanceData{{Title: "alpha"}},
		skippedRepos: []daemon.SkippedRepo{
			{RepoID: "other-repo", Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON},
		},
	})
	t.Cleanup(SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		return newSnapshotTestInstance(t, d.Title), nil
	}))
	assert.False(t, changed, "a skip for a different repo must not change the current repo's state")
	assert.Nil(t, h.skippedRepo)
	h.errBox.SetSize(200, 1)
	assert.NotContains(t, ansi.Strip(h.errBox.String()), "hidden")
}

// -- consumeSkippedRepoNotice tests --

// TestConsumeSkippedRepoNotice_DrainsPending proves the method drains the
// pending notice and returns a tea.Cmd that paints the full repair hint on the
// bar for one cycle, while the skip-owned skippedRepoRetained slot carries it
// for `E details` independently of the shared retained slot (#4742 review).
func TestConsumeSkippedRepoNotice_DrainsPending(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot, h.repoID = "/project", "project"
	resizeHome(h, 200, 36)

	h.applySkippedRepo(&daemon.SkippedRepo{RepoID: "project", Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON})
	require.NotEmpty(t, h.pendingSkippedRepoNotice)

	cmd := h.consumeSkippedRepoNotice()
	require.NotNil(t, cmd)
	assert.Empty(t, h.pendingSkippedRepoNotice, "the pending notice must be drained")

	// The skip-owned retained slot carries the full repair hint for `E details`,
	// independent of the shared most-recent-notice slot any later transient
	// overwrites.
	assert.NotEmpty(t, h.skippedRepoRetained, "the skip condition owns its repair hint")
	assert.Contains(t, h.skippedRepoRetained, corruptedRepoRepairHint)

	// The transient copy paints on the bar for one cycle (a hint, not a notice:
	// it must not land in the shared retained slot).
	assert.Contains(t, h.errBox.FullError(), corruptedRepoRepairHint,
		"the transient copy must paint the full hint on the bar")
	text, failure := h.errBox.RetainedNotice()
	assert.Empty(t, text, "the skip's hint must NOT land in the shared retained slot")
	assert.False(t, failure, "a skip notice is guidance, not a failure")
}

// TestConsumeSkippedRepoNotice_NoPendingReturnsNil proves the method returns nil
// when no notice is pending, so the caller appends it unconditionally.
func TestConsumeSkippedRepoNotice_NoPendingReturnsNil(t *testing.T) {
	h := newTestHome(t)
	assert.Nil(t, h.consumeSkippedRepoNotice())
}

// -- Project picker tests --

// TestBuildProjectListWithSnapshotStatus_SkippedReposCarriesSkipSet proves the
// project picker carries the daemon's startup-skip set (#4742) so the
// destructive D path can gate on the HIGHLIGHTED project being skipped. A
// skipped repo's zero count is incomplete, but the incompleteness is scoped to
// the affected project: a fetch that answered must NOT set the global
// snapshotDegraded flag (which would disable D for every healthy project in a
// multi-project workspace); the skip set travels as the fourth return instead.
func TestBuildProjectListWithSnapshotStatus_SkippedReposCarriesSkipSet(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot, h.repoID = "/project", "project"
	resizeHome(h, 200, 36)

	t.Cleanup(SetAllReposSnapshotFetcherForTest(func(context.Context) ([]session.InstanceData, []daemon.SkippedRepo, error) {
		return nil, []daemon.SkippedRepo{
			{RepoID: "project", Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON},
		}, nil
	}))

	_, _, snapshotDegraded, skipped := h.buildProjectListWithSnapshotStatus()
	assert.False(t, snapshotDegraded, "a fetch that answered must not set the global degraded flag")
	require.Len(t, skipped, 1, "the skip set must travel so the picker can gate D on the highlighted project")
	assert.Equal(t, "project", skipped[0].RepoID)
}

// TestBuildProjectListWithSnapshotStatus_NoSkipNotDegraded proves the project
// picker is NOT degraded when the skip set is empty — the healthy steady state.
func TestBuildProjectListWithSnapshotStatus_NoSkipNotDegraded(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot, h.repoID = "/project", "project"
	resizeHome(h, 200, 36)

	t.Cleanup(SetAllReposSnapshotFetcherForTest(func(context.Context) ([]session.InstanceData, []daemon.SkippedRepo, error) {
		return nil, nil, nil
	}))

	_, _, snapshotDegraded, skipped := h.buildProjectListWithSnapshotStatus()
	assert.False(t, snapshotDegraded)
	assert.Empty(t, skipped)
}

// -- allReposSnapshotFetcher return signature test --

// TestAllReposSnapshotFetcher_ReturnsSkippedRepos proves the fetcher returns the
// skip set alongside the instances, so the project picker and the Projects
// section can mark a skipped repo's zero count as incomplete (#4742).
func TestAllReposSnapshotFetcher_ReturnsSkippedRepos(t *testing.T) {
	t.Cleanup(SetAllReposSnapshotFetcherForTest(func(context.Context) ([]session.InstanceData, []daemon.SkippedRepo, error) {
		return []session.InstanceData{{Title: "alpha"}},
			[]daemon.SkippedRepo{{RepoID: "skipped", Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON}},
			nil
	}))

	data, skipped, err := allReposSnapshotFetcher(context.Background())
	require.NoError(t, err)
	require.Len(t, data, 1)
	require.Len(t, skipped, 1)
	assert.Equal(t, "skipped", skipped[0].RepoID)
	assert.Equal(t, daemon.SkippedRepoReasonCorruptedInstancesJSON, skipped[0].Reason)
}

// -- fetchSnapshotCmd carries skippedRepos test --

// TestFetchSnapshotCmd_CarriesSkippedRepos proves the poll path's message carries
// the skip set from the repo-scoped fetch, so handleSnapshot can raise the notice.
func TestFetchSnapshotCmd_CarriesSkippedRepos(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot, h.repoID = "/project", "project"
	resizeHome(h, 200, 36)

	h.snapshotFetcher = func(repoID string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{
			Instances:    nil,
			SkippedRepos: []daemon.SkippedRepo{{RepoID: "project", Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON}},
		}, nil
	}

	msg := h.fetchSnapshotCmd()().(snapshotFetchedMsg)
	require.Len(t, msg.skippedRepos, 1, "the repo-scoped skip set must travel in the message")
	assert.Equal(t, "project", msg.skippedRepos[0].RepoID)
	assert.Equal(t, daemon.SkippedRepoReasonCorruptedInstancesJSON, msg.skippedRepos[0].Reason)
}

// -- fetchColdStartSnapshot returns skip set test --

// TestFetchColdStartSnapshot_ReturnsSkippedRepos proves the cold-start fetch
// returns the skip set alongside the instances, so coldStartFromSnapshot can
// raise the notice (#4742).
func TestFetchColdStartSnapshot_ReturnsSkippedRepos(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot, h.repoID = "/project", "project"

	h.snapshotFetcher = func(repoID string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{
			Instances:    []session.InstanceData{{Title: "alpha"}},
			SkippedRepos: []daemon.SkippedRepo{{RepoID: "project", Reason: daemon.SkippedRepoReasonUnreadableInstancesJSON}},
		}, nil
	}

	data, skipped, err := h.fetchColdStartSnapshot("project")
	require.NoError(t, err)
	require.Len(t, data, 1)
	require.Len(t, skipped, 1)
	assert.Equal(t, "project", skipped[0].RepoID)
	assert.Equal(t, daemon.SkippedRepoReasonUnreadableInstancesJSON, skipped[0].Reason)
}

// -- End-to-end: Update drains the skip notice test --

// TestUpdate_SnapshotFetchedMsg_DrainsSkippedRepoNotice proves the snapshotFetchedMsg
// handler drains the pending skip notice as a tea.Cmd, so `E details` can open the
// full repair hint. This is the end-to-end path from the poll to the operator-facing
// notice.
func TestUpdate_SnapshotFetchedMsg_DrainsSkippedRepoNotice(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot, h.repoID = "/project", "project"
	resizeHome(h, 200, 36)

	h.snapshotFetcher = func(repoID string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{
			SkippedRepos: []daemon.SkippedRepo{{RepoID: "project", Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON}},
		}, nil
	}

	// Drive one Update cycle with a snapshotFetchedMsg. The handler must:
	// 1. Call handleSnapshot, which calls applySkippedRepo.
	// 2. Drain the pending skip notice as a tea.Cmd.
	model, cmd := h.Update(snapshotFetchedMsg{repoID: "project", skippedRepos: []daemon.SkippedRepo{
		{RepoID: "project", Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON},
	}})
	require.Equal(t, h, model)

	// The cmd must be non-nil (it includes the tick + the skip notice drain).
	require.NotNil(t, cmd, "Update must return a cmd that drains the skip notice")

	// The pending notice must be drained (the cmd was built, but not yet executed).
	// Execute the cmd to paint the transient hint on the bar.
	cmd()

	// The skip condition owns its repair hint (skippedRepoRetained), independent
	// of the shared retained slot, so `E details` opens it for as long as the
	// skip is true — never an unrelated notice that overwrote the shared slot.
	assert.Contains(t, h.skippedRepoRetained, corruptedRepoRepairHint,
		"the skip condition must own its repair hint for E details")
	// The shared retained slot is NOT the skip hint: a later transient can
	// overwrite it without displacing the skip's details.
	text, _ := h.errBox.RetainedNotice()
	assert.NotContains(t, text, corruptedRepoRepairHint,
		"the skip's hint must not ride in the shared retained slot")
}

// -- All reasons covered test --

// TestApplySkippedRepo_AllReasonsCovered proves every reason code the daemon sends
// produces a standing notice with the matching reason label and repair hint, so no
// skip is ever silently dropped regardless of the failure mode.
func TestApplySkippedRepo_AllReasonsCovered(t *testing.T) {
	for _, tc := range []struct {
		name       string
		reason     string
		wantLabel  string
		wantHint   string
		failedRows int
	}{
		{
			name:      "corrupted",
			reason:    daemon.SkippedRepoReasonCorruptedInstancesJSON,
			wantLabel: "its instances.json is corrupted",
			wantHint:  corruptedRepoRepairHint,
		},
		{
			name:      "unreadable",
			reason:    daemon.SkippedRepoReasonUnreadableInstancesJSON,
			wantLabel: "the daemon could not read its instances.json",
			wantHint:  unreadableRepoRepairHint,
		},
		{
			name:      "newer schema",
			reason:    daemon.SkippedRepoReasonNewerSchemaInstancesJSON,
			wantLabel: "its instances.json was written by a newer af",
			wantHint:  newerSchemaRepoRepairHint,
		},
		{
			name:       "rows failed",
			reason:     daemon.SkippedRepoReasonRowsFailedToLoad,
			wantLabel:  "some sessions could not be loaded (worktree or tmux gone)",
			wantHint:   rowsFailedRepoRepairHint,
			failedRows: 3,
		},
		{
			name:      "unknown reason defaults to corrupted",
			reason:    "some-new-unknown-reason",
			wantLabel: "its instances.json is corrupted",
			wantHint:  corruptedRepoRepairHint,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHome(t)
			h.repoRoot, h.repoID = "/project", "project"
			resizeHome(h, 200, 36)

			changed := h.applySkippedRepo(&daemon.SkippedRepo{
				RepoID:     "project",
				Reason:     tc.reason,
				FailedRows: tc.failedRows,
			})
			assert.True(t, changed, "a nil-to-non-nil transition must report a change")

			// Standing notice has the reason label.
			h.errBox.SetSize(200, 1)
			rendered := ansi.Strip(h.errBox.String())
			assert.Contains(t, rendered, tc.wantLabel)

			// Pending notice has the repair hint.
			assert.Contains(t, h.pendingSkippedRepoNotice, tc.wantHint)
		})
	}
}

// -- Switch project applies skip notice test --

// TestSwitchProject_AppliesSkippedRepoNotice proves switching to a project the
// daemon skipped at startup raises the standing notice, not a clean empty sidebar.
func TestSwitchProject_AppliesSkippedRepoNotice(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot, h.repoID = "/initial", "initial"
	resizeHome(h, 200, 36)

	// The initial project has a session so the switch is real.
	inst := newSnapshotTestInstance(t, "initial-session")
	h.store.AddInstance(inst)

	// The incoming project is skipped by the daemon.
	h.snapshotFetcher = func(repoID string) (daemon.SnapshotResponse, error) {
		if repoID == "skipped-project" {
			return daemon.SnapshotResponse{
				SkippedRepos: []daemon.SkippedRepo{{RepoID: "skipped-project", Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON}},
			}, nil
		}
		return daemon.SnapshotResponse{}, nil
	}

	// Drive the switch via switchProject's fetch path. We can't call switchProject
	// directly (it needs a config.Project), so we test the fetch + apply path it uses.
	data, skipped, err := h.fetchColdStartSnapshot("skipped-project")
	require.NoError(t, err)
	require.Empty(t, data)
	require.Len(t, skipped, 1)

	// Apply the skip, as switchProject does after materializeSnapshot.
	h.applySkippedRepo(skippedRepoForRepoID(skipped, "skipped-project"))
	h.errBox.SetSize(200, 1)
	rendered := ansi.Strip(h.errBox.String())
	assert.Contains(t, rendered, "hidden")
	assert.Contains(t, rendered, "corrupted")
}

// TestApplySkippedRepo_StandingComposesWithWebListener proves the skip and
// web-listener standing notices are tracked in separate state and composed
// before updating the bar (#4742 review): a 15-second listener poll cannot
// overwrite the skip's hidden-sessions warning, and clearing the skip cannot
// erase a still-valid listener warning.
func TestApplySkippedRepo_StandingComposesWithWebListener(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot, h.repoID = "/project", "project"
	resizeHome(h, 200, 36)

	// The skip is up first.
	h.applySkippedRepo(&daemon.SkippedRepo{RepoID: "project", Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON})
	h.errBox.Expire() // drop the transient hint so the standing line renders
	h.errBox.SetSize(200, 1)
	require.Contains(t, ansi.Strip(h.errBox.String()), "hidden")

	// A later listener poll reports an unbound web listener: the skip's notice
	// must stay (the listener poll must not overwrite it).
	h.handleWebListenerNotice(webListenerNoticeMsg{notice: "web listener unbound"})
	h.errBox.SetSize(200, 1)
	rendered := ansi.Strip(h.errBox.String())
	assert.Contains(t, rendered, "hidden", "the skip notice must survive a listener poll")
	assert.Contains(t, rendered, "web listener unbound", "the listener notice must compose in")

	// Clearing the skip re-applies the (still-tracked) listener warning rather
	// than erasing it.
	h.applySkippedRepo(nil)
	h.errBox.SetSize(200, 1)
	rendered = ansi.Strip(h.errBox.String())
	assert.NotContains(t, rendered, "hidden", "the skip retraction must remove the skip notice")
	assert.Contains(t, rendered, "web listener unbound", "clearing the skip must restore the listener warning")

	// The listener binding clears the last condition.
	h.handleWebListenerNotice(webListenerNoticeMsg{notice: ""})
	h.errBox.SetSize(200, 1)
	rendered = ansi.Strip(h.errBox.String())
	assert.NotContains(t, rendered, "hidden", "no conditions must render no condition text")
	assert.NotContains(t, rendered, "web listener", "no conditions must render no condition text")
}

// TestApplySkippedRepo_DetailsSurviveTransientOverwrite proves the skip's full
// repair hint is bound to the skip condition (skippedRepoRetained), not the
// shared most-recent-notice slot any later transient overwrites (#4742
// review): while the standing skip warning is what the bar shows, `E details`
// opens the skip's hint; while a later transient is on the bar, `E details`
// opens THAT notice so the currently displayed failure's clipped content stays
// reachable instead of being shadowed by the skip's repair text. The skip's
// hint retracts with the skip.
func TestApplySkippedRepo_DetailsSurviveTransientOverwrite(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot, h.repoID = "/project", "project"
	resizeHome(h, 200, 36)

	h.applySkippedRepo(&daemon.SkippedRepo{RepoID: "project", Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON})
	require.Contains(t, h.skippedRepoRetained, corruptedRepoRepairHint)

	// Drain the skip's transient hint so the standing line is what the bar
	// shows: no transient notice is up, the skip is active.
	h.consumeSkippedRepoNotice()
	h.errBox.Expire()
	h.errBox.SetSize(200, 1)

	// With the standing skip warning visible, `E details` opens the skip's
	// hint (skippedRepoRetained), not the shared retained slot.
	assert.Empty(t, h.errBox.FullError(), "precondition: no transient is on the bar")
	model, _ := h.showErrorDetails()
	h = model.(*home)
	require.Equal(t, stateHelp, h.state, "E details must open the skip's hint when the standing warning is visible")
	require.NotNil(t, h.textOverlay)
	assert.Contains(t, h.textOverlay.Render(), "af keeps no backup of these files",
		"the standing-warning path opens the skip's condition-owned hint")
	h.state = stateDefault
	h.textOverlay = nil

	// An unrelated transient notice fires and lands in the shared retained
	// slot. While it is on the bar, `E details` opens THAT notice so the
	// displayed failure's clipped content stays reachable; the skip's hint
	// still lives in skippedRepoRetained for when the transient fades.
	h.setTransientNotice(errors.New("unrelated transient notice"))
	shared, _ := h.errBox.RetainedNotice()
	assert.Contains(t, shared, "unrelated transient notice", "precondition: the shared slot holds the unrelated notice")
	assert.NotEmpty(t, h.errBox.FullError(), "precondition: the unrelated transient is on the bar")
	assert.Contains(t, h.skippedRepoRetained, corruptedRepoRepairHint,
		"the skip condition still owns its repair hint after an unrelated transient")
	model, _ = h.showErrorDetails()
	h = model.(*home)
	require.Equal(t, stateHelp, h.state, "E details opens the active notice while one is on the bar")
	require.NotNil(t, h.textOverlay)
	assert.Contains(t, h.textOverlay.Render(), "unrelated transient notice",
		"a transient on the bar must keep its own details reachable through E")
	assert.NotContains(t, h.textOverlay.Render(), "af keeps no backup of these files",
		"the skip's repair text must not shadow the active notice while it is displayed")
	h.state = stateDefault
	h.textOverlay = nil

	// Once the transient fades the standing skip warning reappears, and
	// `E details` opens the skip's hint again.
	h.errBox.Expire()
	assert.Empty(t, h.errBox.FullError(), "precondition: the transient has faded")
	model, _ = h.showErrorDetails()
	h = model.(*home)
	require.Equal(t, stateHelp, h.state)
	require.NotNil(t, h.textOverlay)
	assert.Contains(t, h.textOverlay.Render(), "af keeps no backup of these files",
		"once the transient fades the skip's hint is reachable again")

	// When the skip retracts, its details retract with it.
	h.applySkippedRepo(nil)
	assert.Empty(t, h.skippedRepoRetained, "the skip's details must retract with the condition")
}

// TestRefreshSidebarProjectsFromSnapshot_MarksSkippedRepoHidden proves the
// background poll threads the cross-repo skip set (#4742) into the always-
// visible Projects section: a skipped repo's zero count is incomplete, not
// "no sessions", so its row is marked "· hidden" instead of presented as a
// clean empty project — the same incompleteness channel the picker carries.
func TestRefreshSidebarProjectsFromSnapshot_MarksSkippedRepoHidden(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot = "/tmp/af-projects-skip/acme"
	h.repoID = config.RepoIDFromRoot(h.repoRoot)
	resizeHome(h, 120, 45)

	// The active-root pre-seed surfaces the project row even with no live
	// sessions; the daemon skipped it, so its zero count is hidden.
	skipped := []daemon.SkippedRepo{{RepoID: h.repoID, Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON}}
	changed := h.refreshSidebarProjectsFromSnapshot(nil, skipped, nil)
	require.True(t, changed, "a skipped repo's row must reach the section")
	require.NotEmpty(t, h.projects.Projects(), "the active-root pre-seed must surface the skipped project's row")
	row := h.projects.Projects()[0]
	assert.True(t, row.Hidden, "a skipped repo's row must be marked hidden")
	h.projects.SetRect(layout.Rect{X: 0, Y: 0, W: 60, H: 8})
	assert.Contains(t, ansi.Strip(h.projects.String()), "· hidden",
		"the section must render the hidden marker for a skipped repo")

	// A healthy re-poll (no skip) retracts the marker.
	h.refreshSidebarProjectsFromSnapshot(nil, nil, nil)
	require.NotEmpty(t, h.projects.Projects())
	assert.False(t, h.projects.Projects()[0].Hidden,
		"clearing the skip must retract the hidden marker")
	h.projects.SetRect(layout.Rect{X: 0, Y: 0, W: 60, H: 8})
	assert.NotContains(t, ansi.Strip(h.projects.String()), "· hidden",
		"the section must not render the hidden marker once the repo is no longer skipped")
}

// TestApplySkippedRepo_ClearRetractsPaintedTransient pins the #4742 review
// finding on the retraction path: when the skip's full repair-hint transient
// has already been consumed onto the bar (consumeSkippedRepoNotice paints it
// for three seconds before the standing notice takes over), clearing the skip
// within that window must drop the painted transient too — otherwise a switch
// to a healthy project (or a repaired repo) leaves the old "sessions are
// hidden" hint visibly painted until its timer expires. The retraction must
// not touch an unrelated notice the bar happens to be showing.
func TestApplySkippedRepo_ClearRetractsPaintedTransient(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot, h.repoID = "/project", "project"
	resizeHome(h, 200, 36)

	// Fire the skip and drain its full repair-hint transient onto the bar —
	// the bar now shows the skip's hint (consumeSkippedRepoNotice uses
	// SetHint, so the retained slot is untouched).
	h.applySkippedRepo(&daemon.SkippedRepo{RepoID: "project", Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON})
	require.NotEmpty(t, h.pendingSkippedRepoNotice)
	cmd := h.consumeSkippedRepoNotice()
	require.NotNil(t, cmd)
	h.errBox.SetSize(200, 1)
	require.Contains(t, h.errBox.FullError(), corruptedRepoRepairHint,
		"precondition: the skip's repair-hint transient is painted on the bar")
	require.NotEmpty(t, h.errBox.FullError(), "precondition: a transient is on the bar")

	// Within the three-second window the repo is repaired (or the user
	// switches to a healthy project): clearing the skip must retract the
	// painted transient so the bar stops advertising a condition that is
	// no longer true.
	h.applySkippedRepo(nil)
	h.errBox.SetSize(200, 1)
	assert.Empty(t, h.errBox.FullError(), "clearing the skip must drop its painted repair-hint transient")
	assert.NotContains(t, ansi.Strip(h.errBox.String()), "hidden",
		"the bar must not render the retracted skip condition")

	// An unrelated notice on the bar survives a skip retraction: the skip's
	// own transient was never painted onto the bar, so clearing the skip has
	// nothing matching to retract and must leave the unrelated notice alone.
	h.applySkippedRepo(&daemon.SkippedRepo{RepoID: "project", Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON})
	require.NotEmpty(t, h.skippedRepoRetained, "precondition: the skip owns a retained hint")
	h.setTransientNotice(errors.New("unrelated transient notice"))
	h.errBox.SetSize(200, 1)
	require.Contains(t, ansi.Strip(h.errBox.String()), "unrelated transient notice")
	h.applySkippedRepo(nil)
	h.errBox.SetSize(200, 1)
	assert.Contains(t, ansi.Strip(h.errBox.String()), "unrelated transient notice",
		"an unrelated notice the bar is showing must survive a skip retraction")
}

// TestApplySkippedRepo_SkippedToSkippedRetractsOutgoingTransient pins the #4742
// review finding on the skipped-to-skipped switch: when the user switches
// directly from skipped project A to skipped project B while A's repair-hint
// transient is still within its three-second painted window, the incoming
// skip must retract A's outgoing transient before installing B's — otherwise
// the bar keeps showing A's project-specific guidance until B's pending
// notice is consumed (or the timer expires), and `E details` cannot resolve in
// that interval because showErrorDetails compares A's painted text against B's
// newly retained text. The retraction must not touch an unrelated notice.
func TestApplySkippedRepo_SkippedToSkippedRetractsOutgoingTransient(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot, h.repoID = "/project", "project"
	resizeHome(h, 200, 36)

	// Fire skip A and drain its full repair-hint transient onto the bar.
	h.applySkippedRepo(&daemon.SkippedRepo{RepoID: "project", Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON})
	require.NotEmpty(t, h.pendingSkippedRepoNotice)
	cmd := h.consumeSkippedRepoNotice()
	require.NotNil(t, cmd)
	h.errBox.SetSize(200, 1)
	require.Contains(t, h.errBox.FullError(), corruptedRepoRepairHint,
		"precondition: skip A's repair-hint transient is painted on the bar")
	require.Contains(t, h.skippedRepoRetained, corruptedRepoRepairHint,
		"precondition: skip A owns its retained hint")

	// Switch directly to skipped project B (a different reason so the entry
	// changed). A's painted transient must retract so the bar stops showing
	// A's guidance, and B's pending notice / retained hint take over.
	h.applySkippedRepo(&daemon.SkippedRepo{RepoID: "project", Reason: daemon.SkippedRepoReasonUnreadableInstancesJSON})
	h.errBox.SetSize(200, 1)
	assert.Empty(t, h.errBox.FullError(),
		"a skipped-to-skipped switch must retract the outgoing skip's painted transient")
	assert.NotContains(t, ansi.Strip(h.errBox.String()), corruptedRepoRepairHint,
		"the bar must not keep rendering the outgoing skip's repair hint")
	assert.Contains(t, h.skippedRepoRetained, unreadableRepoRepairHint,
		"the incoming skip owns its own retained hint")
	assert.Equal(t, h.skippedRepoRetained, h.pendingSkippedRepoNotice,
		"the incoming skip's transient is queued for the bar")

	// An unrelated notice on the bar survives a skipped-to-skipped switch:
	// retraction only drops a transient matching the outgoing skip's hint, so
	// an unrelated notice the bar is showing stays untouched.
	h.applySkippedRepo(&daemon.SkippedRepo{RepoID: "project", Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON})
	h.errBox.Expire()
	h.setTransientNotice(errors.New("unrelated transient notice"))
	h.errBox.SetSize(200, 1)
	require.Contains(t, ansi.Strip(h.errBox.String()), "unrelated transient notice")
	h.applySkippedRepo(&daemon.SkippedRepo{RepoID: "project", Reason: daemon.SkippedRepoReasonUnreadableInstancesJSON})
	h.errBox.SetSize(200, 1)
	assert.Contains(t, ansi.Strip(h.errBox.String()), "unrelated transient notice",
		"an unrelated notice the bar is showing must survive a skipped-to-skipped switch")
}

// TestUpdateOpenPickerSkipState_DegradedRegistryPreservesRegistryRows pins the
// #4742 review finding on the open-picker refresh: when a successful background
// snapshot coincides with a degraded registry read, buildProjectListFrom omits
// registry-only projects, but the picker must not lose them — Ctrl+P would
// temporarily drop those projects and their switch/rebind actions until another
// poll succeeds. The picker must merge its previous registry-backed rows (the
// same preserving merge the Projects section takes) instead of replacing the
// rows wholesale.
func TestUpdateOpenPickerSkipState_DegradedRegistryPreservesRegistryRows(t *testing.T) {
	h := newTestHome(t)
	h.appConfig.RootAgents = map[string]config.RootAgentConfig{}

	// Two registered git repos. alpha is the active project and has a live
	// session; beta is registered but sessionless — the row only the registry
	// vouches for, so a degraded registry read drops it from the fresh list.
	alphaRoot := initTestGitRepo(t)
	betaRoot := initTestGitRepo(t)
	_, err := config.RegisterProject(betaRoot)
	require.NoError(t, err)
	h.repoRoot = alphaRoot
	h.repoID = config.RepoIDFromRoot(alphaRoot)

	alphaSnapshot := []session.InstanceData{
		{Worktree: session.GitWorktreeData{RepoPath: alphaRoot}},
	}

	// Healthy tick: the registry reads, so both alpha (live session + active
	// root) and beta (registry-only) appear, both carrying a RegistryID.
	healthy, degraded := h.buildProjectListFrom(alphaSnapshot)
	require.False(t, degraded, "precondition: the healthy registry read is not degraded")
	byRoot := map[string]overlay.Project{}
	for _, p := range healthy {
		byRoot[p.Root] = p
	}
	require.Contains(t, byRoot, betaRoot, "precondition: the registered sessionless project appears on a healthy read")
	require.NotEmpty(t, byRoot[betaRoot].RegistryID, "precondition: beta's row is registry-backed")

	// Open the picker on the healthy rows (the state a Ctrl+P opened before the
	// registry degraded).
	h.state = stateSwitchProject
	h.projectPickerOverlay = overlay.NewProjectPickerOverlay(healthy, h.repoRoot)
	require.Len(t, h.projectPickerOverlay.Projects(), len(healthy), "precondition: the picker holds the healthy rows")

	// Degrade the registry: a stray file makes the strict read fail, so
	// buildProjectListFrom drops every registered-but-sessionless project.
	dir, err := config.ProjectRegistryDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stray"), []byte("not a record"), 0o644))
	_, err = config.ListProjects()
	require.Error(t, err, "fixture: the strict registry read must fail")

	// Background poll lands a successful snapshot for alpha while the registry
	// is degraded. Without the preserving merge, the picker's rows are replaced
	// wholesale and beta vanishes; with it, beta's registry-backed row survives.
	h.updateOpenPickerSkipState(alphaSnapshot, nil, nil, 0)
	pickerRows := h.projectPickerOverlay.Projects()
	pickerByRoot := map[string]overlay.Project{}
	for _, p := range pickerRows {
		pickerByRoot[p.Root] = p
	}
	require.Contains(t, pickerByRoot, betaRoot,
		"a degraded registry read must not drop a registered sessionless project from an open picker")
	assert.NotEmpty(t, pickerByRoot[betaRoot].RegistryID,
		"the preserved row keeps its registry identity through the degraded merge")
}

// TestUpdateOpenPickerSkipState_FailedFetchRetainsSkipMarkers pins the #4742
// review finding on the open-picker refresh's error path: when Ctrl+P is
// already showing a skipped repo and the next background all-repos request
// fails, the picker must NOT clear its known skip IDs and hints — the failed
// request arrives with a nil skip set yet cannot establish that the
// persistent skip was repaired, so clearing them would render the row
// ordinary while the sidebar still marks it hidden. The picker must keep the
// cached IDs/hints (mirroring applySidebarProjects, which reuses its retained
// sidebarProjectsSkipped on the same error path) while SnapshotDegraded gates
// the D path on the global fetch-failure refusal.
func TestUpdateOpenPickerSkipState_FailedFetchRetainsSkipMarkers(t *testing.T) {
	h := newTestHome(t)
	h.appConfig.RootAgents = map[string]config.RootAgentConfig{}

	alphaRoot := initTestGitRepo(t)
	h.repoRoot = alphaRoot
	h.repoID = config.RepoIDFromRoot(alphaRoot)

	healthy := []session.InstanceData{
		{Worktree: session.GitWorktreeData{RepoPath: alphaRoot}},
	}
	projects, degraded := h.buildProjectListFrom(healthy)
	require.False(t, degraded, "precondition: the registry read is not degraded")

	// Open the picker with alpha already marked skipped (the state a Ctrl+P
	// opened while the daemon was skipping alpha).
	h.state = stateSwitchProject
	h.projectPickerOverlay = overlay.NewProjectPickerOverlay(projects, h.repoRoot)
	h.projectPickerOverlay.SetSkippedRepoIDs([]string{h.repoID})
	h.projectPickerOverlay.SetSkippedRepoHints(map[string]string{h.repoID: "repair alpha"})
	require.True(t, h.projectPickerOverlay.IsRepoSkipped(h.repoID), "precondition: alpha is marked skipped")
	require.Equal(t, "repair alpha", h.projectPickerOverlay.SkippedRepoHint(h.repoID), "precondition: alpha carries its repair hint")

	// Background poll fails. The failed request cannot prove the skip was
	// repaired, so the picker must retain the last-known skip markers instead
	// of clearing them; only SnapshotDegraded flips to gate D globally.
	h.updateOpenPickerSkipState(healthy, nil, errors.New("rpc boom"), 0)
	assert.True(t, h.projectPickerOverlay.SnapshotDegraded(),
		"a failed background fetch must set SnapshotDegraded so the D path falls back to the fetch-failure refusal")
	assert.True(t, h.projectPickerOverlay.IsRepoSkipped(h.repoID),
		"a failed background fetch must not clear a previously skipped repo's marker")
	assert.Equal(t, "repair alpha", h.projectPickerOverlay.SkippedRepoHint(h.repoID),
		"a failed background fetch must not clear a previously skipped repo's repair hint")
}

// TestUpdateOpenPickerSkipState_PreservesSessionOnlySkippedRows pins the #4742
// review finding on the open-picker refresh: when a project represented ONLY
// by its live snapshot sessions becomes skipped while Ctrl+P stays open, the
// daemon drops its instance, so buildProjectListFrom cannot reconstruct a row
// and a wholesale RefreshRows would remove it before the skip marker is
// installed — the skipped project would vanish from the picker instead of
// being shown as hidden. The picker must graft its last-known row for each
// skipped ID back in (mirroring preserveSkippedProjectRows for the Projects
// section) so the row stays, marked hidden, with its repair hint.
func TestUpdateOpenPickerSkipState_PreservesSessionOnlySkippedRows(t *testing.T) {
	h := newTestHome(t)
	h.appConfig.RootAgents = map[string]config.RootAgentConfig{}

	// alpha is the active project (registered, with a live session). gamma is
	// a separate git repo whose ONLY presence is a live snapshot session — not
	// registered, not a root agent, not the active root — so the row a healthy
	// snapshot produces is session-derived and disappears once the daemon skips
	// gamma and drops its instance.
	alphaRoot := initTestGitRepo(t)
	gammaRoot := initTestGitRepo(t)
	_, err := config.RegisterProject(alphaRoot)
	require.NoError(t, err)
	h.repoRoot = alphaRoot
	h.repoID = config.RepoIDFromRoot(alphaRoot)
	gammaID := config.RepoIDFromRoot(gammaRoot)

	healthy := []session.InstanceData{
		{Worktree: session.GitWorktreeData{RepoPath: alphaRoot}},
		{Worktree: session.GitWorktreeData{RepoPath: gammaRoot}},
	}
	projects, degraded := h.buildProjectListFrom(healthy)
	require.False(t, degraded, "precondition: the registry read is not degraded")
	byID := map[string]overlay.Project{}
	for _, p := range projects {
		byID[p.RepoID] = p
	}
	require.Contains(t, byID, gammaID, "precondition: the session-only gamma appears on a healthy snapshot")
	require.Empty(t, byID[gammaID].RegistryID, "precondition: gamma's row is session-only, not registry-backed")

	// Open the picker on the healthy rows (the state a Ctrl+P opened before the
	// daemon skipped gamma).
	h.state = stateSwitchProject
	h.projectPickerOverlay = overlay.NewProjectPickerOverlay(projects, h.repoRoot)
	require.Contains(t, rowByID(t, h.projectPickerOverlay.Projects()), gammaID,
		"precondition: the picker holds gamma's session-only row")

	// The daemon now skips gamma and drops its instance: the fresh snapshot
	// carries only alpha's session, and gamma appears in the skip set. Without
	// the preserving graft, the picker's rows are replaced wholesale and gamma
	// vanishes; with it, gamma's last-known row survives, marked hidden.
	h.updateOpenPickerSkipState(
		[]session.InstanceData{{Worktree: session.GitWorktreeData{RepoPath: alphaRoot}}},
		[]daemon.SkippedRepo{{RepoID: gammaID, Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON}},
		nil, 0)

	pickerByRoot := map[string]overlay.Project{}
	for _, p := range h.projectPickerOverlay.Projects() {
		pickerByRoot[p.Root] = p
	}
	require.Contains(t, pickerByRoot, gammaRoot,
		"a session-only repo the daemon skipped must stay in an open picker instead of vanishing")
	assert.True(t, h.projectPickerOverlay.IsRepoSkipped(gammaID),
		"the preserved session-only row is marked skipped so it renders hidden and D is gated")
	require.NotEmpty(t, h.projectPickerOverlay.SkippedRepoHint(gammaID),
		"the preserved row carries the daemon's repair hint for the skip reason")
}

func rowByID(t *testing.T, rows []overlay.Project) map[string]overlay.Project {
	t.Helper()
	out := make(map[string]overlay.Project, len(rows))
	for _, p := range rows {
		out[p.RepoID] = p
	}
	return out
}

// TestShowProjectPickerOverlay_PreservesSessionOnlySkippedRows pins the #4742
// review finding on the fresh-picker open path: when a project represented ONLY
// by its live snapshot sessions is skipped, applySidebarProjects keeps its
// last-known row in m.sidebarProjects (marked hidden), but the fresh picker
// built solely from buildProjectListWithSnapshotStatus cannot reconstruct it
// (its instance is gone), so closing and reopening Ctrl+P would drop the row
// even while the Projects section still shows it as hidden. The picker must
// graft the retained sidebar row for each skipped ID before creating the
// overlay (mirrors preserveSkippedPickerRows for the background refresh and
// preserveSkippedProjectRows for the sidebar).
func TestShowProjectPickerOverlay_PreservesSessionOnlySkippedRows(t *testing.T) {
	h := newTestHome(t)
	h.appConfig.RootAgents = map[string]config.RootAgentConfig{}

	alphaRoot := initTestGitRepo(t)
	gammaRoot := initTestGitRepo(t)
	_, err := config.RegisterProject(alphaRoot)
	require.NoError(t, err)
	h.repoRoot = alphaRoot
	h.repoID = config.RepoIDFromRoot(alphaRoot)
	gammaID := config.RepoIDFromRoot(gammaRoot)

	// Seed m.sidebarProjects with gamma's session-only row (the state a
	// healthy poll left before the daemon skipped gamma), so the fresh
	// picker has a last-known row to graft.
	h.setSidebarProjects([]overlay.Project{
		{RepoID: h.repoID, Name: filepath.Base(alphaRoot), Root: alphaRoot, RegistryID: "prj-alpha"},
		{RepoID: gammaID, Name: filepath.Base(gammaRoot), Root: gammaRoot, SessionCount: 2},
	}, false, nil, nil)
	require.Contains(t, rowByID(t, h.sidebarProjects), gammaID, "precondition: the sidebar retains gamma's session-only row")

	// The daemon now skips gamma and drops its instance: the fresh cross-repo
	// snapshot carries only alpha's session, and gamma appears in the skip
	// set. Without the graft, the picker opens without gamma; with it,
	// gamma's last-known row survives, marked hidden.
	t.Cleanup(SetAllReposSnapshotFetcherForTest(func(context.Context) ([]session.InstanceData, []daemon.SkippedRepo, error) {
		return []session.InstanceData{{Worktree: session.GitWorktreeData{RepoPath: alphaRoot}}},
			[]daemon.SkippedRepo{{RepoID: gammaID, Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON}},
			nil
	}))

	h.showProjectPickerOverlay()
	require.NotNil(t, h.projectPickerOverlay)
	pickerByRoot := map[string]overlay.Project{}
	for _, p := range h.projectPickerOverlay.Projects() {
		pickerByRoot[p.Root] = p
	}
	require.Contains(t, pickerByRoot, gammaRoot,
		"a session-only repo the daemon skipped must survive a fresh Ctrl+P open instead of vanishing")
	assert.True(t, h.projectPickerOverlay.IsRepoSkipped(gammaID),
		"the preserved session-only row is marked skipped so it renders hidden and D is gated")
	require.NotEmpty(t, h.projectPickerOverlay.SkippedRepoHint(gammaID),
		"the preserved row carries the daemon's repair hint for the skip reason")
}

// TestHandleProjectsFocus_RefusesDeleteOnActiveScopedSkip pins the #4742 review
// finding (P1) on the Projects-section D path: the repo-scoped and cross-repo
// snapshots are independent requests, so the scoped cold-start/poll can set
// m.skippedRepo while the following cross-repo refresh fails — especially at
// cold start, where there is no retained cross-repo skip set to mark the row
// Hidden. The Hidden gate alone would pass and D would open a confirmation
// from absent or stale counts even though the daemon still archives or tears
// down the hidden sessions. D must also refuse when the highlighted row's
// RepoID matches the active scoped skip.
func TestHandleProjectsFocus_RefusesDeleteOnActiveScopedSkip(t *testing.T) {
	h := newTestHome(t)
	repoID := config.RepoIDFromRoot(deleteProjectTestRoot)
	h.repoRoot = deleteProjectTestRoot
	h.repoID = repoID

	// A healthy cross-repo fetch (no skip set) so the active row is NOT marked
	// Hidden — the gap: the scoped skip is set below, but the cross-repo skip
	// set the Hidden flag is built from never arrived. A decoy session makes
	// the Projects section hold 2 rows so the grid keeps it visible and
	// focusable (a single project reserves no rail rows).
	decoyRoot := "/tmp/af-delete-project-decoy/zzz-decoy"
	decoySession := session.InstanceData{
		Title: "decoy",
		Worktree: session.GitWorktreeData{
			RepoPath: decoyRoot, WorktreePath: decoyRoot, SessionName: "decoy",
		},
	}
	t.Cleanup(SetAllReposSnapshotFetcherForTest(func(context.Context) ([]session.InstanceData, []daemon.SkippedRepo, error) {
		return []session.InstanceData{deleteProjectSession("alpha", false), decoySession}, nil, nil
	}))
	resizeHome(h, 120, 45)
	h.refreshSidebarProjects()
	h.relayout()
	require.Len(t, h.projects.Projects(), 2, "precondition: active + decoy so the Projects section is visible")
	activeRow := h.projects.Projects()[0]
	require.True(t, activeRow.Active)
	require.False(t, activeRow.Hidden, "precondition: the cross-repo skip set never arrived, so the row is NOT hidden")
	require.Equal(t, repoID, activeRow.RepoID)

	// The repo-scoped snapshot, independently, reports this repo skipped —
	// the cold-start state where m.skippedRepo is set before any cross-repo
	// skip set exists.
	h.applySkippedRepo(&daemon.SkippedRepo{RepoID: repoID, Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON})
	require.NotNil(t, h.skippedRepo, "precondition: the active scoped skip is set")
	require.False(t, h.projects.Projects()[0].Hidden,
		"precondition: the scoped skip does NOT mark the cross-repo row Hidden")

	// Focus the Projects section and press D on the active (skipped) row.
	h.focusRegion(layout.RegionProjects)
	require.Equal(t, layout.RegionProjects, h.ring.Active())
	model, _, consumed := h.handleProjectsFocus(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})
	require.True(t, consumed, "`D` must be consumed by the Projects-section handler")
	h = model.(*home)
	require.NotEqual(t, stateConfirm, h.state,
		"D on an actively skipped project must NOT open the delete confirmation")
	require.Nil(t, h.confirmationOverlay,
		"no consent dialog may be opened against an actively skipped project's incomplete count")
	notice := h.errBox.FullError()
	require.NotEmpty(t, notice, "the refuse must surface a user-visible notice")
	assert.Contains(t, notice, "cannot delete project")
	assert.Contains(t, notice, "sessions are hidden")
	assert.NotContains(t, notice, "no live sessions",
		"the refusal must never claim a no-op delete")
}

// TestApplyStandingNotice_RecomputesDetailsKeyOnStateChange pins the #4742
// review finding on the standing notice: the details-key suffix ("Press E for
// repair steps.") was frozen into ErrBox.standing only when the skip/listener
// condition changed, not when m.state changed — so a skip composed in the
// default state kept advertising E inside an overlay where that key is
// unavailable, and a skip detected while an overlay was open omitted the
// repair key and still omitted it after the overlay closed. View() recomposes
// the standing notice every frame (the same place it recomputes the
// clipped-notice hint), so the suffix always matches the input state.
func TestApplyStandingNotice_RecomputesDetailsKeyOnStateChange(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot, h.repoID = "/project", "project"
	resizeHome(h, 200, 36)

	// Default state: the standing notice advertises the E key.
	h.applySkippedRepo(&daemon.SkippedRepo{RepoID: "project", Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON})
	h.errBox.Expire()
	h.errBox.SetSize(200, 1)
	require.Contains(t, ansi.Strip(h.errBox.String()), "Press", "precondition: the standing notice advertises the details key in the default state")

	// Entering an overlay where E does not open the details (stateSearch owns
	// the keyboard): recomposing the standing notice must drop the suffix so
	// the bar stops advertising a dead key. View() does this recomposition;
	// call applyStandingNotice the same way View does.
	h.state = stateSearch
	h.applyStandingNotice()
	h.errBox.SetSize(200, 1)
	rendered := ansi.Strip(h.errBox.String())
	assert.Contains(t, rendered, "hidden", "the skip condition still names itself")
	assert.NotContains(t, rendered, "Press",
		"the standing notice must not advertise a details key that does not open in this state")

	// Returning to the default state re-adds the suffix.
	h.state = stateDefault
	h.applyStandingNotice()
	h.errBox.SetSize(200, 1)
	assert.Contains(t, ansi.Strip(h.errBox.String()), "Press",
		"the standing notice re-advertises the details key once the default state returns")
}

// TestConsumeSkippedRepoNotice_DoesNotOverwriteNewerTransient pins the #4742
// review finding on the drain path: a cold start or project switch can queue
// pendingSkippedRepoNotice before the snapshot handler drains it, and another
// operation may raise a transient in that interval. An unconditional SetHint
// would paint the older skip guidance over the newer message, and
// showErrorDetails would then route E to the skip's hint while the overwritten
// failure stays marooned in the shared retained slot for as long as the repo
// stays skipped. The drain must leave a different transient on the bar alone;
// the skip's standing notice and condition-owned retained slot still carry the
// repair guidance.
func TestConsumeSkippedRepoNotice_DoesNotOverwriteNewerTransient(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot, h.repoID = "/project", "project"
	resizeHome(h, 200, 36)

	// Queue the skip's repair-hint notice (the state a cold start / switch
	// leaves before the snapshot handler drains it).
	h.applySkippedRepo(&daemon.SkippedRepo{RepoID: "project", Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON})
	require.NotEmpty(t, h.pendingSkippedRepoNotice)
	require.Contains(t, h.skippedRepoRetained, corruptedRepoRepairHint)

	// A different operation raises a newer transient on the bar in the
	// interval before the drain.
	h.setTransientFailure(errors.New("newer transient failure"))
	require.Equal(t, "newer transient failure", h.errBox.FullError(), "precondition: a newer transient is on the bar")

	// The drain must not overwrite the newer transient with the skip hint.
	cmd := h.consumeSkippedRepoNotice()
	assert.Nil(t, cmd, "the drain must not schedule a clear timer when it did not paint the skip hint")
	assert.Empty(t, h.pendingSkippedRepoNotice, "the pending notice is drained regardless")
	assert.Equal(t, "newer transient failure", h.errBox.FullError(),
		"the newer transient on the bar must survive the skip-notice drain")

	// The skip's standing notice and condition-owned retained slot still
	// carry the repair guidance, so E details and the standing line remain
	// correct after the drain skipped.
	assert.NotEmpty(t, h.skippedRepoRetained, "the skip condition still owns its repair hint")
	h.errBox.Expire()
	h.errBox.SetSize(200, 1)
	assert.Contains(t, ansi.Strip(h.errBox.String()), "hidden", "the standing skip notice still names the condition")

	// Control: with no newer transient on the bar, the drain paints the skip
	// hint as before.
	h.pendingSkippedRepoNotice = h.skippedRepoRetained
	h.errBox.Expire() // drop any transient so the bar is clear
	cmd = h.consumeSkippedRepoNotice()
	require.NotNil(t, cmd, "with no newer transient the drain paints the skip hint")
	assert.Contains(t, h.errBox.FullError(), corruptedRepoRepairHint,
		"the skip hint paints on the bar when nothing newer is up")
}

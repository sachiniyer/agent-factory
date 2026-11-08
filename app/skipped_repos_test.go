package app

import (
	"context"
	"errors"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"
)

// -- Helper function tests --

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
	t.Run("corrupted reason includes repo id and hint", func(t *testing.T) {
		s := &daemon.SkippedRepo{RepoID: "repo-1", Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON}
		err := skippedRepoNotice(s)
		require.NotNil(t, err)
		msg := err.Error()
		assert.Contains(t, msg, "repo-1")
		assert.Contains(t, msg, "its instances.json is corrupted")
		assert.Contains(t, msg, corruptedRepoRepairHint)
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

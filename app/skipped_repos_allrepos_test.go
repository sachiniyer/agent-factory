package app

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/overlay"
)

// #6986, the all-repos half: in project mode the scoped reply's skipped set is
// scoped to the active repo, so the witness for ANOTHER skipped repo must ride
// the unscoped all-repos read — the same response the Projects section's rows
// come from. These tests exercise that carrier end to end: the skip surfaces
// while the active repo is healthy, the merge keeps unscoped entries a scoped
// poll cannot see, and every synchronous all-repos read (section refresh,
// rebind conflict) carries the set with its rows.

// allReposReplyStub swaps the all-repos fetcher for one returning the given
// rows+skipped pair — rows and skip state from ONE reply, as the seam requires.
func allReposReplyStub(t *testing.T, skipped []daemon.SkippedRepo, err error) {
	t.Helper()
	t.Cleanup(SetAllReposSkippedFetcherForTest(
		func(context.Context) ([]session.InstanceData, []daemon.SkippedRepo, error) {
			return nil, skipped, err
		}))
}

// TestSkippedRepoAllReposCarrier: the notice for a skipped repo the TUI is not
// scoped to rides the all-repos half of the poll — this is the "Projects row
// reads 0 sessions" half of #6986 in project mode.
func TestSkippedRepoAllReposCarrier(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(220, 1)
	h.snapshotFetcher = func(string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{}, nil // the active repo is healthy
	}
	allReposReplyStub(t, []daemon.SkippedRepo{{
		RepoID: "corrupted-elsewhere", Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON,
	}}, nil)

	_, _ = h.Update(h.fetchSnapshotCmd()())
	bar := requireSkippedRepoNotice(t, h)
	assert.Contains(t, bar, "corrupted-elsewhere")
}

// TestSkippedRepoScopedMergePreservesUnscopedSet: a scoped reply reports only
// its own repo's entries, so it FOLDS into the retained set — the unscoped
// witness for a skipped repo elsewhere survives a poll that only re-read the
// active repo (here: the all-repos fetch then fails, so the scoped merge is
// the only fresh completeness input).
func TestSkippedRepoScopedMergePreservesUnscopedSet(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(400, 1)

	// Seed the retained set from an all-repos reply, then restore the default
	// (failing) stub so the next poll's all-repos half retains.
	allReposReplyStub(t, []daemon.SkippedRepo{{
		RepoID: "corrupted-elsewhere", Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON,
	}}, nil)
	h.snapshotFetcher = func(string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{}, nil
	}
	_, _ = h.Update(h.fetchSnapshotCmd()())
	requireSkippedRepoNotice(t, h)

	// The scoped reply now reports the ACTIVE repo skipped while the
	// all-repos read is down: the union must stand — both entries named.
	allReposReplyStub(t, nil, errTestFetcherUnstubbed)
	pollSnapshotReply(t, h, daemon.SnapshotResponse{
		SkippedRepos: []daemon.SkippedRepo{{
			RepoID: h.repoID, Reason: daemon.SkippedRepoReasonUnreadableInstancesJSON,
		}},
	}, nil)

	bar := requireSkippedRepoNotice(t, h)
	assert.Contains(t, bar, "corrupted-elsewhere",
		"the unscoped entry survives a later scoped merge")
	assert.Contains(t, bar, h.repoID,
		"the scoped entry lands from its own reply")
}

// TestSkippedRepoAllReposClear: the all-repos read is the completeness
// authority — a clean reply retracts the notice even when an earlier reply
// reported a skip.
func TestSkippedRepoAllReposClear(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(400, 1)
	h.snapshotFetcher = func(string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{}, nil
	}
	allReposReplyStub(t, []daemon.SkippedRepo{{
		RepoID: "corrupted-elsewhere", Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON,
	}}, nil)
	_, _ = h.Update(h.fetchSnapshotCmd()())
	requireSkippedRepoNotice(t, h)

	allReposReplyStub(t, nil, nil)
	_, _ = h.Update(h.fetchSnapshotCmd()())
	h.errBox.Expire() // the earlier transient lingers for its fade; the standing must not
	assert.NotContains(t, h.errBox.String(), "hidden",
		"a complete all-repos reply retracts the notice")
}

// TestSkippedRepoSectionRefreshAppliesSet: the synchronous Projects-section
// rebuild (launch, project switch, add/delete/rebind completion) reads the
// same all-repos reply — its skipped set applies too.
func TestSkippedRepoSectionRefreshAppliesSet(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(220, 1)
	allReposReplyStub(t, []daemon.SkippedRepo{{
		RepoID: "corrupted-elsewhere", Reason: daemon.SkippedRepoReasonNewerSchemaInstancesJSON,
	}}, nil)

	h.refreshSidebarProjects()
	bar := requireSkippedRepoNotice(t, h)
	assert.Contains(t, bar, "corrupted-elsewhere")

	// The standing line's tail can truncate at the bar's width; the full
	// reason+remedy lands in the retained notice, which the poll drains.
	_, _ = h.Update(h.fetchSnapshotCmd()())
	full, _ := h.errBox.RetainedNotice()
	assert.Contains(t, full, "newer state file",
		"the notice carries the remedy, not only the repo name")
}

// TestSkippedRepoRebindConflictAppliesSet: the rebind-conflict off-loop read
// is one more all-repos snapshot — its skipped set applies with its rows.
func TestSkippedRepoRebindConflictAppliesSet(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(220, 1)
	_, _ = h.handleRebindConflictSnapshot(rebindConflictSnapshotMsg{
		skipped: []daemon.SkippedRepo{{
			RepoID: "corrupted-elsewhere", Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON,
		}},
	})
	bar := requireSkippedRepoNotice(t, h)
	assert.Contains(t, bar, "corrupted-elsewhere")
}

// TestSkippedRepoLabelPrefersProjectRoot: when the Projects list knows the
// skipped repo, the notice names its root — a repoID hash alone is not an
// actionable name (the web resolves the same way through the registry).
func TestSkippedRepoLabelPrefersProjectRoot(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(220, 1)
	h.sidebarProjects = []overlay.Project{
		{RepoID: "corrupted-elsewhere", Root: "/repos/acme", Name: "acme"},
	}
	allReposReplyStub(t, []daemon.SkippedRepo{{
		RepoID: "corrupted-elsewhere", Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON,
	}}, nil)
	h.snapshotFetcher = func(string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{}, nil
	}
	_, _ = h.Update(h.fetchSnapshotCmd()())
	bar := requireSkippedRepoNotice(t, h)
	assert.Contains(t, bar, "/repos/acme")
}

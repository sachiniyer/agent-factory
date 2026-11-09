package app

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/ui"
)

// #6986: every TUI snapshot read dropped SnapshotResponse.SkippedRepos — the
// repos the daemon could not load because their instances.json is corrupted or
// unreadable (#4742 added the field, #5413 closed the same gap on the web) — so
// the sidebar and the always-visible Projects section presented a truncated
// list as complete: a corrupted repo's sessions simply vanished, and its
// Projects row read "0 sessions".
//
// These tests drive the REAL read path — h.snapshotFetcher is the per-home seam
// the production poll calls, and fetchSnapshotCmd + Update carry the reply to
// the handler — so each assertion fails on a transport that drops SkippedRepos
// rather than on fields the fix itself introduces. The property under test is
// deliberately the conservative one: while the LATEST snapshot the TUI holds
// reports any skipped repo, the bar carries a standing "this list is
// incomplete" notice naming the repos and their repair, and the one destructive
// verb whose safety depends on a complete view — delete-project — refuses.

// pollSkippedRepoSnapshot drives one full snapshot poll whose daemon reply
// reports the given skipped repos: the off-loop fetch builds the message the
// ticker would carry, Update applies it on the event loop.
func pollSnapshotReply(t *testing.T, h *home, resp daemon.SnapshotResponse, err error) {
	t.Helper()
	h.snapshotFetcher = func(string) (daemon.SnapshotResponse, error) { return resp, err }
	msg, ok := h.fetchSnapshotCmd()().(snapshotFetchedMsg)
	require.True(t, ok, "fetchSnapshotCmd must return a snapshotFetchedMsg")
	_, _ = h.Update(msg)
}

// skippedRepoBar asserts the standing notice on the bar. The skipped set
// survives a transient notice's fade — String() paints the standing line
// whenever no transient is up — so it is read through String(), not FullError().
func requireSkippedRepoNotice(t *testing.T, h *home) string {
	t.Helper()
	bar := h.errBox.String()
	require.Contains(t, bar, "hidden", "a skipped repo must surface as an incomplete-list notice")
	return bar
}

// TestSkippedRepoPollShowsStandingNotice pins the core property: a snapshot
// whose SkippedRepos names the ACTIVE repo must leave an incomplete-list
// notice on the bar, naming the repo and its remedy — never a clean-looking
// empty sidebar.
func TestSkippedRepoPollShowsStandingNotice(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(220, 1)

	pollSnapshotReply(t, h, daemon.SnapshotResponse{
		SkippedRepos: []daemon.SkippedRepo{{
			RepoID: h.repoID,
			Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON,
		}},
	}, nil)

	bar := requireSkippedRepoNotice(t, h)
	assert.Contains(t, bar, "skipped")
	assert.Contains(t, bar, h.repoID, "the notice must name which repo is skipped")
}

// TestSkippedRepoRegistryMode: an unscoped (registry-mode) snapshot reports
// the WHOLE skipped set, so a skipped repo the TUI is not even scoped to must
// still produce the notice — this is the "Projects row reads 0 sessions" half
// of #6986: without it, a healthy active project hides another repo's loss.
func TestSkippedRepoRegistryMode(t *testing.T) {
	h := newTestHome(t)
	h.repoID = "" // registry mode: the scoped fetch is the all-repos read
	h.errBox.SetSize(220, 1)

	pollSnapshotReply(t, h, daemon.SnapshotResponse{
		SkippedRepos: []daemon.SkippedRepo{{
			RepoID: "corrupted-other-repo",
			Reason: daemon.SkippedRepoReasonUnreadableInstancesJSON,
		}},
	}, nil)

	bar := requireSkippedRepoNotice(t, h)
	assert.Contains(t, bar, "corrupted-other-repo")
}

// TestSkippedRepoNoticeCarriesReasonAndRemedy: the notice must carry the same
// per-reason repair wording the CLI and web print (api/skipped_repos.go's four
// hints), so the operator gets a remedy, not just a warning. Unknown reasons
// fall back to the corrupted remedy, matching splitSkippedRepos.
func TestSkippedRepoNoticeCarriesReasonAndRemedy(t *testing.T) {
	cases := []struct {
		reason string
		hint   string
	}{
		{daemon.SkippedRepoReasonCorruptedInstancesJSON, "Stop the af daemon, copy the corrupt file(s) aside"},
		{daemon.SkippedRepoReasonUnreadableInstancesJSON, "Make the file(s) readable by the user the af daemon runs as"},
		{daemon.SkippedRepoReasonNewerSchemaInstancesJSON, "Upgrade af to a version that understands the newer state file"},
		{daemon.SkippedRepoReasonRowsFailedToLoad, "Restore or delete the sessions the daemon could not load"},
		{"some-future-reason", "Stop the af daemon, copy the corrupt file(s) aside"},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			h := newTestHome(t)
			h.errBox.SetSize(220, 1)
			pollSnapshotReply(t, h, daemon.SnapshotResponse{
				SkippedRepos: []daemon.SkippedRepo{{RepoID: h.repoID, Reason: tc.reason}},
			}, nil)
			full, _ := h.errBox.RetainedNotice()
			require.NotEmpty(t, full, "the skip must be reachable as a retained notice, not only a truncated bar line")
			assert.Contains(t, full, tc.hint)
		})
	}
}

// TestSkippedRepoRowsFailedNamesCount: a rows-failed skip reports how many
// sessions are hidden (SkippedRepo.FailedRows), the number the CLI's
// "%d session(s) ... could not be loaded" line carries — the notice must name
// it so the operator knows the size of the hole.
func TestSkippedRepoRowsFailedNamesCount(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(220, 1)
	pollSnapshotReply(t, h, daemon.SnapshotResponse{
		SkippedRepos: []daemon.SkippedRepo{{
			RepoID:     h.repoID,
			Reason:     daemon.SkippedRepoReasonRowsFailedToLoad,
			FailedRows: 3,
		}},
	}, nil)
	full, _ := h.errBox.RetainedNotice()
	assert.Contains(t, full, "3 session(s) hidden")
}

// TestSkippedRepoClearsOnHealthySnapshot: the notice retracts once a later
// snapshot reports a complete read — the skipped set belongs to the LATEST
// snapshot, not to the repo forever.
func TestSkippedRepoClearsOnHealthySnapshot(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(220, 1)
	pollSnapshotReply(t, h, daemon.SnapshotResponse{
		SkippedRepos: []daemon.SkippedRepo{{
			RepoID: h.repoID, Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON,
		}},
	}, nil)
	requireSkippedRepoNotice(t, h)

	pollSnapshotReply(t, h, daemon.SnapshotResponse{}, nil)
	assert.NotContains(t, h.errBox.String(), "hidden",
		"a healthy snapshot must retract the incomplete-list notice")
}

// TestSkippedRepoRetainedAcrossFailedFetch: a failed poll must NOT clear the
// last-known skipped set — rows and skip state are retained together (a hiccup
// is not a healthy read), so the guard stays armed until a real snapshot
// reports the repos loadable again.
func TestSkippedRepoRetainedAcrossFailedFetch(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(220, 1)
	pollSnapshotReply(t, h, daemon.SnapshotResponse{
		SkippedRepos: []daemon.SkippedRepo{{
			RepoID: h.repoID, Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON,
		}},
	}, nil)
	requireSkippedRepoNotice(t, h)

	pollSnapshotReply(t, h, daemon.SnapshotResponse{}, errors.New("connection refused"))
	requireSkippedRepoNotice(t, h)
}

// TestSkippedRepoCoexistsWithWebListenerNotice: two independent standing
// conditions — a skipped repo and the web listener's "not bound" notice — must
// both stay visible. Each must be tracked on its own; one slot written by two
// owners is the race that lets one erase the other (#6997 review finding).
func TestSkippedRepoCoexistsWithWebListenerNotice(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(400, 1)

	// Skip first, then the listener notice — the web read must not erase the
	// skipped set's warning.
	pollSnapshotReply(t, h, daemon.SnapshotResponse{
		SkippedRepos: []daemon.SkippedRepo{{
			RepoID: h.repoID, Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON,
		}},
	}, nil)
	_, _ = h.handleWebListenerNotice(webListenerNoticeMsg{
		notice: "web UI not served: 127.0.0.1:8443 is not bound",
	})
	bar := requireSkippedRepoNotice(t, h)
	assert.Contains(t, bar, "web UI not served",
		"the web-listener standing notice must coexist with the skipped-repo one")

	// And the reverse: the listener standing down must not erase the skip.
	_, _ = h.handleWebListenerNotice(webListenerNoticeMsg{notice: ""})
	bar = requireSkippedRepoNotice(t, h)
	assert.NotContains(t, bar, "web UI not served")
}

// TestDeleteProjectRefusedWhileRepoSkipped: delete-project archives — and for
// in-place sessions tears down — every session the daemon holds for a repo, so
// its safety depends on the counts the confirmation shows being the COMPLETE
// list. While the latest snapshot reports a skipped repo the confirm must never
// open: the action refuses with the incompleteness as the reason.
func TestDeleteProjectRefusedWhileRepoSkipped(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(220, 1)
	pollSnapshotReply(t, h, daemon.SnapshotResponse{
		SkippedRepos: []daemon.SkippedRepo{{
			RepoID: h.repoID, Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON,
		}},
	}, nil)

	_, _ = h.handleDeleteProject(ui.SidebarProject{
		RepoID: h.repoID, Name: "acme", Root: "/tmp/acme", SessionCount: 1,
	})
	require.Nil(t, h.confirmationOverlay,
		"the delete confirmation must never open while the list is incomplete")
	full, _ := h.errBox.RetainedNotice()
	assert.Contains(t, full, "skipped",
		"the refusal must name the incomplete list as its reason")
}

// TestDeleteProjectRefusedForUnskippedRepo: the guard is global on the latest
// skipped set — a partial view makes EVERY count unreliable (a repo's 0-count
// row may be hiding everything), so delete-project refuses for any target
// while any repo is skipped, naming the skipped set, not a fetch failure.
func TestDeleteProjectRefusedForUnskippedRepo(t *testing.T) {
	h := newTestHome(t)
	h.repoID = "" // registry mode: the reply below is the global set
	h.errBox.SetSize(220, 1)
	pollSnapshotReply(t, h, daemon.SnapshotResponse{
		SkippedRepos: []daemon.SkippedRepo{{
			RepoID: "corrupted-repo", Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON,
		}},
	}, nil)

	_, _ = h.handleDeleteProject(ui.SidebarProject{
		RepoID: "healthy-repo", Name: "healthy", Root: "/tmp/healthy",
	})
	require.Nil(t, h.confirmationOverlay)
	full, _ := h.errBox.RetainedNotice()
	assert.Contains(t, full, "corrupted-repo")
}

// TestDeleteProjectMsgRefusedWhileRepoSkipped: the dispatch path — the confirm
// already open or a queued startDeleteProjectMsg — must re-check the same
// guard, so a skip that appears between confirm-open and confirm-answer still
// refuses the run.
func TestDeleteProjectMsgRefusedWhileRepoSkipped(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(220, 1)
	pollSnapshotReply(t, h, daemon.SnapshotResponse{
		SkippedRepos: []daemon.SkippedRepo{{
			RepoID: h.repoID, Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON,
		}},
	}, nil)

	_, _ = h.Update(startDeleteProjectMsg{root: "/tmp/acme", repoID: h.repoID, name: "acme"})
	full, _ := h.errBox.RetainedNotice()
	assert.Contains(t, full, "skipped",
		"a queued delete must refuse while the latest snapshot reports a skipped repo")
}

// TestSkippedRepoNoticeOncePerSet: a poll that reports the same skipped set as
// the last must not re-fire the transient — the standing line carries the
// condition; the transient announces a CHANGE once.
func TestSkippedRepoNoticeOncePerSet(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(220, 1)
	skip := daemon.SnapshotResponse{
		SkippedRepos: []daemon.SkippedRepo{{
			RepoID: h.repoID, Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON,
		}},
	}
	pollSnapshotReply(t, h, skip, nil)
	full, _ := h.errBox.RetainedNotice()
	require.NotEmpty(t, full, "the first report announces the skip")

	h.errBox.Expire() // the transient fades; the standing line remains
	pollSnapshotReply(t, h, skip, nil)
	assert.Empty(t, h.errBox.FullError(),
		"an identical skipped set must not re-announce every 750ms")
	requireSkippedRepoNotice(t, h)

	// A DIFFERENT set is a new announcement.
	pollSnapshotReply(t, h, daemon.SnapshotResponse{
		SkippedRepos: []daemon.SkippedRepo{
			{RepoID: h.repoID, Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON},
			{RepoID: "second-broken-repo", Reason: daemon.SkippedRepoReasonUnreadableInstancesJSON},
		},
	}, nil)
	assert.NotEmpty(t, h.errBox.FullError(), "a changed skipped set announces again")
}

// TestSkippedRepoColdStart: a launch (or the synchronous re-reads that mirror
// it) that finds a skipped repo must carry the notice from the start — the
// first painted frame is already an incomplete list, not a clean one that the
// first poll has to correct.
func TestSkippedRepoColdStart(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(220, 1)
	h.snapshotFetcher = func(string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{
			SkippedRepos: []daemon.SkippedRepo{{
				RepoID: h.repoID, Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON,
			}},
		}, nil
	}
	require.NoError(t, h.coldStartFromSnapshot())
	requireSkippedRepoNotice(t, h)
}

// TestSkippedRepoSwitchProject: switching INTO a project whose snapshot
// reports a skip must carry the skipped set with the rows — the notice follows
// the incoming project's state, it does not stay bound to the outgoing repo.
func TestSkippedRepoSwitchProject(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(220, 1)
	repoBRoot := initTestGitRepo(t)
	repoB := &config.RepoContext{Root: repoBRoot, ID: config.RepoIDFromRoot(repoBRoot)}
	h.snapshotFetcher = func(repoID string) (daemon.SnapshotResponse, error) {
		resp := daemon.SnapshotResponse{}
		if repoID == repoB.ID {
			resp.SkippedRepos = []daemon.SkippedRepo{{
				RepoID: repoB.ID, Reason: daemon.SkippedRepoReasonUnreadableInstancesJSON,
			}}
		}
		return resp, nil
	}

	_, _ = h.switchProject(repoB)
	bar := requireSkippedRepoNotice(t, h)
	assert.Contains(t, bar, repoB.ID,
		"the notice must name the skipped repo the switch just entered")
}

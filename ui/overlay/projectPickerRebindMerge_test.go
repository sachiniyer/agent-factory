package overlay

import "testing"

// TestMergePreservingSessionRowsKeepsTheSessionWorkspaceOverAMissingRoot pins
// the failed-snapshot merge to the healthy build's root ranking (#4888 review):
// a registration whose recorded root is missing is outranked by a live session
// workspace, so the healthy row showed that workspace as Root and kept the
// missing path only in RegistryRoot. A conflict refresh whose session snapshot
// failed rebuilds the row from the registry alone, falling back to the missing
// path — the merge must keep the navigable workspace (and the name derived from
// it) while still taking every registry field from the fresh read.
func TestMergePreservingSessionRowsKeepsTheSessionWorkspaceOverAMissingRoot(t *testing.T) {
	old := []Project{{
		Name: "feature", Root: "/work/feature", RepoID: "repo-alpha",
		RegistryID: "prj_aaa", RegistryRoot: "/repos/alpha", RegistryCheckoutID: "chk_old",
		MissingPath: true, SessionCount: 2, InPlaceCount: 1,
	}}
	fresh := []Project{{
		Name: "alpha", Root: "/repos/alpha", RepoID: "repo-alpha",
		RegistryID: "prj_aaa", RegistryRoot: "/repos/alpha", RegistryCheckoutID: "chk_new",
		MissingPath: true,
	}}
	merged := MergePreservingSessionRows(old, fresh, false)
	if len(merged) != 1 {
		t.Fatalf("one row, got %+v", merged)
	}
	row := merged[0]
	if row.Root != "/work/feature" || row.Name != "feature" {
		t.Fatalf("the live session workspace must stay the navigable Root over the missing registry root, got Root=%q Name=%q", row.Root, row.Name)
	}
	if row.RegistryRoot != "/repos/alpha" || row.RegistryCheckoutID != "chk_new" || !row.MissingPath {
		t.Fatalf("registry fields come from the fresh read, got %+v", row)
	}
	if row.SessionCount != 2 || row.InPlaceCount != 1 {
		t.Fatalf("last-known counts carry over, got %+v", row)
	}

	t.Run("a fresh Root a higher-priority source supplies is kept", func(t *testing.T) {
		active := []Project{{
			Name: "active", Root: "/work/active", RepoID: "repo-alpha",
			RegistryID: "prj_aaa", RegistryRoot: "/repos/alpha", MissingPath: true,
		}}
		got := MergePreservingSessionRows(old, active, false)
		if len(got) != 1 || got[0].Root != "/work/active" {
			t.Fatalf("the active workspace outranks the preserved session workspace, got %+v", got)
		}
	})

	t.Run("a proven registry root still wins", func(t *testing.T) {
		proven := []Project{{
			Name: "alpha", Root: "/repos/alpha", RepoID: "repo-alpha",
			RegistryID: "prj_aaa", RegistryRoot: "/repos/alpha",
		}}
		got := MergePreservingSessionRows(old, proven, false)
		if len(got) != 1 || got[0].Root != "/repos/alpha" {
			t.Fatalf("a registry root that exists outranks a session workspace, got %+v", got)
		}
	})
}

// TestMergePreservingSessionRowsDegradedRegistryKeepsFreshLiveCountsForRegistryIDCarryingRows
// pins the remote-mode shape that #5833 introduced over #4888's merge: when the
// session snapshot LANDED but the registry read degraded, the caller zeroes the
// preserved rows' counts (registryBackedRows) on the premise that the fresh
// session row's own counts are live truth. In LOCAL mode the fresh row carries
// no RegistryID (a failed config.ListProjects yields no registration identity),
// so it takes the RegistryID-less graft whose guard already respects the live
// counts. In REMOTE mode buildRemoteProjectList stamps a RegistryID on the
// fresh row from the stale cached registry even while degraded, so the fresh
// row takes the RegistryID-match branch instead — and an unconditional count
// copy would graft the just-zeroed counts over the fresh row's live ones,
// making the sidebar read (0) for every registered project for the life of the
// outage. The branch must copy the preserved row's counts only when the fresh
// row itself reports none, mirroring the RegistryID-less graft's guard; a repo
// the healthy snapshot counts keeps those counts, and a repo it does not count
// still clears (the snapshot is the count authority).
func TestMergePreservingSessionRowsDegradedRegistryKeepsFreshLiveCountsForRegistryIDCarryingRows(t *testing.T) {
	// Two registered projects as a prior healthy tick left them: alpha has live
	// sessions, beta has none. registryBackedOnly mirrors registryBackedRows'
	// caller contract for the snapshot-landed / registry-degraded merge: both
	// rows are stripped to zero because the fresh snapshot is the count authority.
	old := []Project{
		{Name: "alpha", Root: "/srv/box/alpha", RepoID: "aaaaaaaaaaaa",
			RegistryID: "prj_aaa", RegistryRoot: "/srv/box/alpha", RegistryCheckoutID: "chk_a",
			SessionCount: 2, InPlaceCount: 1},
		{Name: "beta", Root: "/srv/box/beta", RepoID: "bbbbbbbbbbbb",
			RegistryID: "prj_bbb", RegistryRoot: "/srv/box/beta", RegistryCheckoutID: "chk_b",
			SessionCount: 0},
	}
	zeroed := registryBackedOnly(old)

	// Remote-mode fresh rows: the snapshot landed and tallied live counts, AND
	// each carries a RegistryID stamped from the stale cached registry while
	// degraded (buildRemoteProjectList reads m.remote.projects for identity).
	fresh := []Project{
		{Name: "alpha", Root: "/srv/box/alpha", RepoID: "aaaaaaaaaaaa",
			RegistryID: "prj_aaa", RegistryRoot: "/srv/box/alpha", RegistryCheckoutID: "chk_a",
			SessionCount: 2, InPlaceCount: 1},
		{Name: "beta", Root: "/srv/box/beta", RepoID: "bbbbbbbbbbbb",
			RegistryID: "prj_bbb", RegistryRoot: "/srv/box/beta", RegistryCheckoutID: "chk_b",
			SessionCount: 0},
	}
	merged := MergePreservingSessionRows(zeroed, fresh, true)
	if len(merged) != 2 {
		t.Fatalf("two registered projects survive the degraded merge, got %+v", merged)
	}
	rowByRepoID := map[string]Project{}
	for _, r := range merged {
		rowByRepoID[r.RepoID] = r
	}

	// alpha's live counts win: the fresh row carried them, and the zeroed
	// preserved row must NOT clobber them. The registry identity still grafts.
	alpha, ok := rowByRepoID["aaaaaaaaaaaa"]
	if !ok {
		t.Fatalf("alpha survives, got %+v", merged)
	}
	if alpha.SessionCount != 2 || alpha.InPlaceCount != 1 {
		t.Fatalf("the fresh row's live counts must win, got %+v", alpha)
	}
	if alpha.RegistryID != "prj_aaa" || alpha.RegistryRoot != "/srv/box/alpha" || alpha.RegistryCheckoutID != "chk_a" {
		t.Fatalf("the registration identity grafts across, got %+v", alpha)
	}

	// beta was uncovered by the snapshot: the landed snapshot is the count
	// authority, so its tally clears — the fix must not resurrect last-known
	// counts for a repo the fresh snapshot proved has none.
	beta, ok := rowByRepoID["bbbbbbbbbbbb"]
	if !ok {
		t.Fatalf("beta survives a degraded read (absence unproven), got %+v", merged)
	}
	if beta.SessionCount != 0 || beta.InPlaceCount != 0 {
		t.Fatalf("an uncovered repo's tally clears — the snapshot is the count authority, got %+v", beta)
	}
	if beta.RegistryID != "prj_bbb" {
		t.Fatalf("the uncovered registry row keeps its identity behind the degraded read, got %+v", beta)
	}
}

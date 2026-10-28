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

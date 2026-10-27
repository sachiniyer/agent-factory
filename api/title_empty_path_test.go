package api

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// A session row whose top-level Path is "" can reach the daemon snapshot from
// out-of-band hand-edits of instances.json or legacy/foreign rows (the normal
// create flow sets Path unconditionally for every backend). These tests pin the
// contract that an unscoped `sessions get <title>` (no --repo) still refuses to
// guess between two DISTINCT PROJECTS when both holders carry an empty Path —
// the case the snapshot path's Path-keyed ambiguity check missed because
// session.DedupeSorted drops empty strings, and the disk widening backstop
// missed because it threw away the repoID map key and re-keyed on Path.

// saveOneRepoInstances is the single-row seeding helper these tests share.
func saveOneRepoInstances(t *testing.T, repoID string, rows ...session.InstanceData) {
	t.Helper()
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("marshal %s: %v", repoID, err)
	}
	if err := config.SaveRepoInstances(repoID, raw); err != nil {
		t.Fatalf("save %s: %v", repoID, err)
	}
}

// TestDiskRepoPathsForTitle_CountsDistinctReposByRepoIDNotPath is the unit core
// of the fix: two repos each holding a same-titled empty-Path row must report
// two distinct repoIDs. Before the fix the helper re-keyed on Path, so
// DedupeSorted dropped both empty values and collapsed the count to zero.
func TestDiskRepoPathsForTitle_CountsDistinctReposByRepoIDNotPath(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	saveOneRepoInstances(t, "repo-alpha", session.InstanceData{Title: "foo", Path: "", BackendType: "docker"})
	saveOneRepoInstances(t, "repo-beta", session.InstanceData{Title: "foo", Path: "", BackendType: "docker"})

	repoIDs, repoPaths, gaps, err := diskRepoPathsForTitle("foo", nil)
	if err != nil {
		t.Fatalf("diskRepoPathsForTitle: %v", err)
	}
	if len(gaps) != 0 {
		t.Fatalf("expected no unreadable gaps, got %v", gaps)
	}
	if len(repoIDs) != 2 {
		t.Fatalf("expected two distinct repoIDs, got %v (collapse on empty Path not fixed)", repoIDs)
	}
	for _, want := range []string{"repo-alpha", "repo-beta"} {
		if !contains(repoIDs, want) {
			t.Errorf("repoIDs must include %q, got %v", want, repoIDs)
		}
	}
	// Both holders have an empty Path, so the human-readable path set is empty;
	// AmbiguousTitleError is expected to fall back to its defensive wording
	// (see TestGetSessionByTitle_AmbiguousFromSnapshotEmptyPath).
	if len(repoPaths) != 0 {
		t.Errorf("expected no human-readable paths when every Path is empty, got %v", repoPaths)
	}
}

// TestGetSessionByTitle_AmbiguousFromSnapshotEmptyPath is the end-to-end guard the
// bug report asked for: an unscoped `sessions get foo` must refuse a title held by
// two distinct projects even when both snapshot rows carry Path "". The daemon
// loaded both rows from disk (per-repo title uniqueness means two same-title
// snapshot rows are two repos), so the disk-widening backstop — now keyed on the
// repoID map key — sees both and reports ErrAmbiguousTitle instead of silently
// returning one of them.
func TestGetSessionByTitle_AmbiguousFromSnapshotEmptyPath(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	// The daemon snapshot would serve both rows; seed the underlying disk so the
	// local widening backstop reads the same state the daemon loaded.
	saveOneRepoInstances(t, "repo-alpha", session.InstanceData{Title: "foo", Path: "", BackendType: "docker"})
	saveOneRepoInstances(t, "repo-beta", session.InstanceData{Title: "foo", Path: "", BackendType: "docker"})
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return []session.InstanceData{
			{Title: "foo", Path: "", BackendType: "docker"},
			{Title: "foo", Path: "", BackendType: "docker"},
		}, nil
	})

	got, _, err := getSessionByTitle("foo")
	if err == nil {
		t.Fatalf("expected ErrAmbiguousTitle for a title held by two distinct projects, got nil (silently returned %v)", got)
	}
	if !errors.Is(err, session.ErrAmbiguousTitle) {
		t.Fatalf("expected ErrAmbiguousTitle, got: %v", err)
	}
	// With every holder's Path empty, AmbiguousTitleError lands on its defensive
	// wording (no repo could be named) — the same branch the robust disk twin
	// reaches for this shape.
	if !strings.Contains(err.Error(), "multiple projects") {
		t.Errorf("error must report the title exists in multiple projects, got: %v", err)
	}
	if !strings.Contains(err.Error(), "--repo") {
		t.Errorf("error must tell the user how to disambiguate, got: %v", err)
	}
}

// TestGetSessionByTitle_SnapshotUnionsDiskRowsEmptyPath is the empty-Path twin of
// TestGetSessionByTitle_SnapshotUnionsDiskRows: a lone snapshot match is not
// proof of uniqueness, and when the unseen second repo's row also has Path ""
// the widening must still flag the cross-project ambiguity rather than resolve
// to the one restored row.
func TestGetSessionByTitle_SnapshotUnionsDiskRowsEmptyPath(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	saveOneRepoInstances(t, "repo-alpha", session.InstanceData{Title: "foo", Path: "", BackendType: "docker"})
	saveOneRepoInstances(t, "repo-beta", session.InstanceData{Title: "foo", Path: "", BackendType: "docker"})
	// The daemon only restored repo-alpha's session (refresh skipped repo-beta's
	// because its worktree/tmux is gone), so the snapshot serves a lone match.
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return []session.InstanceData{{Title: "foo", Path: "", BackendType: "docker"}}, nil
	})

	_, _, err := getSessionByTitle("foo")
	if err == nil {
		t.Fatalf("a lone empty-Path snapshot match must not resolve while another repo holds the title on disk")
	}
	if !errors.Is(err, session.ErrAmbiguousTitle) {
		t.Fatalf("expected ErrAmbiguousTitle, got: %v", err)
	}
}

// TestGetSessionByTitle_UniqueEmptyPathMatchResolves guards against over-refusing:
// a globally unique title whose single match carries Path "" must still resolve
// via the bare-title convenience. The widening finds exactly one repo, so no
// ambiguity is reported.
func TestGetSessionByTitle_UniqueEmptyPathMatchResolves(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	saveOneRepoInstances(t, "repo-alpha", session.InstanceData{Title: "foo", Path: "", BackendType: "docker"})
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return []session.InstanceData{{Title: "foo", Path: "", BackendType: "docker"}}, nil
	})

	got, notice, err := getSessionByTitle("foo")
	if err != nil {
		t.Fatalf("a globally unique empty-Path title must still resolve, got: %v", err)
	}
	if got.Title != "foo" {
		t.Errorf("resolved wrong session: %q", got.Title)
	}
	if notice != "" {
		t.Errorf("a clean unique resolution must carry no widening notice, got: %q", notice)
	}
}

// TestGetSessionByTitle_RemoteEmptyPathKnownLimitation documents the known
// limitation the bug report names: against a REMOTE daemon the widening is
// skipped (the local instances.json has nothing to do with the remote's
// sessions), so the snapshot-only Path-keyed check is the only guard. The
// snapshot carries no repoID, so two empty-Path rows it serves cannot be told
// apart on this side of the wire. Closing that needs a daemon-side resolve
// (see the comment in getSessionByTitle); until then this pins the CURRENT
// behavior so a future daemon-side fix is a visible behavior change, not a
// silent one.
func TestGetSessionByTitle_RemoteEmptyPathKnownLimitation(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	remoteTarget(t)
	// Even if the local disk happens to hold the same shape, a remote lookup
	// must never consult it (TestGetSessionByTitle_RemoteIgnoresLocalDisk).
	saveOneRepoInstances(t, "repo-alpha", session.InstanceData{Title: "foo", Path: "", BackendType: "docker"})
	saveOneRepoInstances(t, "repo-beta", session.InstanceData{Title: "foo", Path: "", BackendType: "docker"})
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return []session.InstanceData{
			{Title: "foo", Path: "", BackendType: "docker"},
			{Title: "foo", Path: "", BackendType: "docker"},
		}, nil
	})

	got, _, err := getSessionByTitle("foo")
	if err != nil {
		t.Fatalf("remote path: expected the snapshot-only known limitation to resolve (not error), got: %v", err)
	}
	if got == nil || got.Title != "foo" {
		t.Fatalf("remote path: expected the snapshot to return one of the matches, got %v", got)
	}
}

// TestGetSessionByTitle_PendingSnapshotHolderNotOnDiskCollidesWithDisk guards
// the regression the repoID-only count introduced: an in-flight pendingCreates
// row is exposed by daemon.SnapshotWithSkipped but is not written to
// instances.json until creation completes, so it is invisible to the disk
// widening. When a persisted row with the same title exists in ANOTHER repo, the
// snapshot serves a lone non-empty-Path match and the disk widening finds one
// repo — a count of one would silently return the pending row. The snapshot
// holder must be retained in the project count (via its non-empty Path) so the
// collision is still flagged.
func TestGetSessionByTitle_PendingSnapshotHolderNotOnDiskCollidesWithDisk(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	// The persisted twin in repo-beta (its worktree/tmux is gone, so refresh
	// skipped it and it never reached the snapshot, but it is on disk).
	saveOneRepoInstances(t, "repo-beta", session.InstanceData{Title: "foo", Path: "/repos/beta", BackendType: "docker"})
	// The snapshot serves a lone pendingCreates row NOT yet on disk (a Path the
	// disk widening will not find among the persisted rows).
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return []session.InstanceData{{Title: "foo", Path: "/repos/pending", BackendType: "docker"}}, nil
	})

	_, _, err := getSessionByTitle("foo")
	if err == nil {
		t.Fatalf("a lone pending snapshot match must not resolve while another repo holds the title on disk")
	}
	if !errors.Is(err, session.ErrAmbiguousTitle) {
		t.Fatalf("expected ErrAmbiguousTitle, got: %v", err)
	}
	for _, want := range []string{"/repos/pending", "/repos/beta"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name both holders (%q and %q), got: %v", "/repos/pending", "/repos/beta", err)
		}
	}
}

// TestGetSessionByTitle_SnapshotHolderOnDiskDoesNotOverCount guards the other
// side of the pending fix: when the snapshot's lone match IS the same project as
// a disk row (the normal, fully-persisted case), folding the holder in by Path
// must not double-count it. The holder's Path matches a disk row's Path, so it
// is not added as a synthetic id and a unique title still resolves.
func TestGetSessionByTitle_SnapshotHolderOnDiskDoesNotOverCount(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	saveOneRepoInstances(t, "repo-alpha", session.InstanceData{Title: "foo", Path: "/repos/alpha", BackendType: "docker"})
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return []session.InstanceData{{Title: "foo", Path: "/repos/alpha", BackendType: "docker"}}, nil
	})

	got, notice, err := getSessionByTitle("foo")
	if err != nil {
		t.Fatalf("a unique title whose snapshot match is on disk must resolve, got: %v", err)
	}
	if got.Title != "foo" {
		t.Errorf("resolved wrong session: %q", got.Title)
	}
	if notice != "" {
		t.Errorf("a clean unique resolution must carry no widening notice, got: %q", notice)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

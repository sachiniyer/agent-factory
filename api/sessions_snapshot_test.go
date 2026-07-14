package api

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// remoteTarget points the CLI read path at a remote daemon for the duration of a
// test (so apiclient.IsRemoteTarget() reports true) and restores the prior value
// on cleanup. Without a daemonURL the read path is a LOCAL unix-socket target.
func remoteTarget(t *testing.T) {
	t.Helper()
	prev := apiclient.FlagDaemonURL
	apiclient.FlagDaemonURL = "wss://remote.example.com"
	t.Cleanup(func() { apiclient.FlagDaemonURL = prev })
}

// stubSnapshot swaps snapshotViaDaemon for the duration of a test so the
// list/get/whoami read paths can be exercised against a canned daemon snapshot
// (or a forced ErrDaemonUnavailable) without a live daemon (#1029 PR 2). The
// returned pointer captures every request the code under test issued so a test
// can assert --repo scoping is threaded through.
func stubSnapshot(t *testing.T, fn func(daemon.SnapshotRequest) ([]session.InstanceData, error)) *[]daemon.SnapshotRequest {
	t.Helper()
	var reqs []daemon.SnapshotRequest
	prev := snapshotViaDaemon
	snapshotViaDaemon = func(req daemon.SnapshotRequest) ([]session.InstanceData, error) {
		reqs = append(reqs, req)
		return fn(req)
	}
	t.Cleanup(func() { snapshotViaDaemon = prev })
	return &reqs
}

// daemonUnavailable is the stub for "no daemon reachable" — every call falls
// back to disk.
func daemonUnavailable(daemon.SnapshotRequest) ([]session.InstanceData, error) {
	return nil, daemon.ErrDaemonUnavailable
}

func titlesOf(data []session.InstanceData) []string {
	out := make([]string, 0, len(data))
	for _, d := range data {
		out = append(out, d.Title)
	}
	return out
}

// TestListSessions_DaemonUp verifies the list read path returns the daemon's
// live snapshot verbatim (scoping the request by repoID) and never consults
// disk when a daemon is reachable.
func TestListSessions_DaemonUp(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	// Disk holds DIFFERENT data so a result matching the snapshot proves the
	// live path won and disk was not read.
	diskJSON, err := json.Marshal([]session.InstanceData{{Title: "stale-disk"}})
	if err != nil {
		t.Fatalf("marshal disk: %v", err)
	}
	if err := config.SaveRepoInstances("repo-x", diskJSON); err != nil {
		t.Fatalf("save disk: %v", err)
	}

	live := []session.InstanceData{{Title: "live-a"}, {Title: "live-b"}}
	reqs := stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return live, nil
	})

	got, err := listSessions("repo-x")
	if err != nil {
		t.Fatalf("listSessions: %v", err)
	}
	if want := []string{"live-a", "live-b"}; !equalStrs(titlesOf(got), want) {
		t.Fatalf("listSessions returned %v, want live snapshot %v", titlesOf(got), want)
	}
	if len(*reqs) != 1 || (*reqs)[0].RepoID != "repo-x" {
		t.Fatalf("expected one snapshot request scoped to repo-x, got %+v", *reqs)
	}
}

// TestListSessions_DiskFallbackNoDaemon verifies that with no daemon reachable
// the list path falls back to disk, aggregates across repos, and sorts the
// result by the daemon's (repoID, title) key so both sources agree.
func TestListSessions_DiskFallbackNoDaemon(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	stubSnapshot(t, daemonUnavailable)

	// Two repos, titles deliberately out of order within and across repos.
	aJSON, err := json.Marshal([]session.InstanceData{{Title: "a2"}, {Title: "a1"}})
	if err != nil {
		t.Fatalf("marshal a: %v", err)
	}
	bJSON, err := json.Marshal([]session.InstanceData{{Title: "b1"}})
	if err != nil {
		t.Fatalf("marshal b: %v", err)
	}
	// Repo IDs chosen so "repo-a" sorts before "repo-b".
	if err := config.SaveRepoInstances("repo-b", bJSON); err != nil {
		t.Fatalf("save b: %v", err)
	}
	if err := config.SaveRepoInstances("repo-a", aJSON); err != nil {
		t.Fatalf("save a: %v", err)
	}

	got, err := listSessions("")
	if err != nil {
		t.Fatalf("listSessions: %v", err)
	}
	// (repoID, title) order: repo-a/a1, repo-a/a2, repo-b/b1.
	if want := []string{"a1", "a2", "b1"}; !equalStrs(titlesOf(got), want) {
		t.Fatalf("disk fallback returned %v, want (repoID,title)-sorted %v", titlesOf(got), want)
	}
}

// TestListSessions_DiskFallbackRepoScoped verifies the repo-scoped disk fallback
// reads only the requested repo and sorts by title.
func TestListSessions_DiskFallbackRepoScoped(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	stubSnapshot(t, daemonUnavailable)

	aJSON, err := json.Marshal([]session.InstanceData{{Title: "z"}, {Title: "m"}, {Title: "a"}})
	if err != nil {
		t.Fatalf("marshal a: %v", err)
	}
	if err := config.SaveRepoInstances("repo-a", aJSON); err != nil {
		t.Fatalf("save a: %v", err)
	}
	if err := config.SaveRepoInstances("repo-b", json.RawMessage(`[{"title":"other"}]`)); err != nil {
		t.Fatalf("save b: %v", err)
	}

	got, err := listSessions("repo-a")
	if err != nil {
		t.Fatalf("listSessions: %v", err)
	}
	if want := []string{"a", "m", "z"}; !equalStrs(titlesOf(got), want) {
		t.Fatalf("scoped disk fallback returned %v, want title-sorted %v", titlesOf(got), want)
	}
}

// TestListSessions_DiskFallbackCorruptLoud verifies the loud corrupt-file
// behavior (#730) is preserved on the disk fallback path.
func TestListSessions_DiskFallbackCorruptLoud(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	_ = captureWarnings(t)
	stubSnapshot(t, daemonUnavailable)

	if err := config.SaveRepoInstances("bad-repo", json.RawMessage("{not json")); err != nil {
		t.Fatalf("save corrupt: %v", err)
	}

	_, err := listSessions("")
	if err == nil {
		t.Fatalf("expected loud corrupt-file error on disk fallback, got nil")
	}
	if !strings.Contains(err.Error(), "bad-repo") {
		t.Fatalf("expected error to name the corrupted repo, got: %v", err)
	}
}

// TestGetSessionByTitle_DaemonUp verifies get resolves from the live snapshot,
// and that a miss against a reachable daemon returns not-found without reading
// disk (the daemon is authoritative).
func TestGetSessionByTitle_DaemonUp(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	// Disk holds the title so a "not found" proves disk was not consulted.
	diskJSON, err := json.Marshal([]session.InstanceData{{Title: "ghost"}})
	if err != nil {
		t.Fatalf("marshal disk: %v", err)
	}
	if err := config.SaveRepoInstances("repo-x", diskJSON); err != nil {
		t.Fatalf("save disk: %v", err)
	}

	live := []session.InstanceData{{Title: "one"}, {Title: "two"}}
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) { return live, nil })

	got, err := getSessionByTitle("two")
	if err != nil {
		t.Fatalf("getSessionByTitle(two): %v", err)
	}
	if got.Title != "two" {
		t.Fatalf("got %q, want two", got.Title)
	}

	_, err = getSessionByTitle("ghost")
	if err == nil {
		t.Fatalf("expected not-found for a title absent from the live snapshot")
	}
	if !errors.Is(err, errTitleNotFound) {
		t.Fatalf("expected errTitleNotFound sentinel, got: %v", err)
	}
}

// TestGetSessionByTitle_DiskFallback verifies get falls back to the disk scan
// when no daemon is reachable.
func TestGetSessionByTitle_DiskFallback(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	stubSnapshot(t, daemonUnavailable)

	diskJSON, err := json.Marshal([]session.InstanceData{{Title: "findme"}})
	if err != nil {
		t.Fatalf("marshal disk: %v", err)
	}
	if err := config.SaveRepoInstances("repo-x", diskJSON); err != nil {
		t.Fatalf("save disk: %v", err)
	}

	got, err := getSessionByTitle("findme")
	if err != nil {
		t.Fatalf("getSessionByTitle disk fallback: %v", err)
	}
	if got.Title != "findme" {
		t.Fatalf("got %q, want findme", got.Title)
	}
}

// TestWhoamiSession_DaemonUp verifies whoami matches TmuxName against the live
// snapshot.
func TestWhoamiSession_DaemonUp(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	// Disk holds a different TmuxName so a match must have come from the snapshot.
	diskJSON, err := json.Marshal([]session.InstanceData{{Title: "d", TmuxName: "af_disk"}})
	if err != nil {
		t.Fatalf("marshal disk: %v", err)
	}
	if err := config.SaveRepoInstances("repo-x", diskJSON); err != nil {
		t.Fatalf("save disk: %v", err)
	}

	live := []session.InstanceData{{Title: "live", TmuxName: "af_live_agent"}}
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) { return live, nil })

	got, err := whoamiSession("af_live_agent")
	if err != nil {
		t.Fatalf("whoamiSession: %v", err)
	}
	if got.Title != "live" {
		t.Fatalf("got %q, want live", got.Title)
	}

	if _, err := whoamiSession("af_disk"); err == nil {
		t.Fatalf("expected no match for a tmux name absent from the live snapshot")
	}
}

// TestWhoamiSession_DiskFallback verifies whoami falls back to the disk scan
// when no daemon is reachable.
func TestWhoamiSession_DiskFallback(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	stubSnapshot(t, daemonUnavailable)

	diskJSON, err := json.Marshal([]session.InstanceData{{Title: "mine", TmuxName: "af_mine_agent"}})
	if err != nil {
		t.Fatalf("marshal disk: %v", err)
	}
	if err := config.SaveRepoInstances("repo-x", diskJSON); err != nil {
		t.Fatalf("save disk: %v", err)
	}

	got, err := whoamiSession("af_mine_agent")
	if err != nil {
		t.Fatalf("whoamiSession disk fallback: %v", err)
	}
	if got.Title != "mine" {
		t.Fatalf("got %q, want mine", got.Title)
	}
}

// --- Remote-target error surfacing (#1679, #1681) ------------------------------
//
// These lock in the contract from docs/remote-tcp-auth.md: when targeting a
// remote daemon, a snapshot error (bad token → 401, or unreachable daemon →
// network error) is surfaced verbatim rather than being masked by a
// same-machine disk scan (there is no local disk on the other end). The disk is
// deliberately seeded with a MATCHING session so a passing test proves the
// error won over the misleading local hit. Local (unix-socket) targets keep
// today's disk-fallback behavior unchanged — the paired *_LocalDiskFallback*
// tests prove that.

// TestSnapshotRead_RemoteVsLocalBranch is a direct test of the shared seam:
// remote → surface err, no disk; local → signal disk fallback; success → data.
func TestSnapshotRead_RemoteVsLocalBranch(t *testing.T) {
	snapErr := errors.New("boom")

	t.Run("remote surfaces error, forbids disk", func(t *testing.T) {
		remoteTarget(t)
		stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) { return nil, snapErr })
		data, fallBack, err := snapshotRead(daemon.SnapshotRequest{})
		if !errors.Is(err, snapErr) {
			t.Fatalf("want snapErr surfaced, got %v", err)
		}
		if fallBack {
			t.Fatal("remote target must NOT permit disk fallback")
		}
		if data != nil {
			t.Fatalf("want nil data on error, got %v", data)
		}
	})

	t.Run("local permits disk fallback", func(t *testing.T) {
		stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) { return nil, snapErr })
		_, fallBack, err := snapshotRead(daemon.SnapshotRequest{})
		if !errors.Is(err, snapErr) {
			t.Fatalf("want snapErr returned, got %v", err)
		}
		if !fallBack {
			t.Fatal("local target must permit disk fallback")
		}
	})

	t.Run("success returns data and forbids disk", func(t *testing.T) {
		live := []session.InstanceData{{Title: "live"}}
		stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) { return live, nil })
		data, fallBack, err := snapshotRead(daemon.SnapshotRequest{})
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if fallBack {
			t.Fatal("success must not trigger disk fallback")
		}
		if len(data) != 1 || data[0].Title != "live" {
			t.Fatalf("want live snapshot, got %v", data)
		}
	})
}

// TestGetSessionByTitleInScope_RemoteTargetSurfacesError verifies a remote 401
// surfaces even though a matching session sits on local disk (#1679, #1681).
func TestGetSessionByTitleInScope_RemoteTargetSurfacesError(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	remoteTarget(t)

	authErr := errors.New("unauthorized: invalid bearer token")
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) { return nil, authErr })

	diskJSON, err := json.Marshal([]session.InstanceData{{Title: "test-session"}})
	if err != nil {
		t.Fatalf("marshal disk: %v", err)
	}
	if err := config.SaveRepoInstances("some-repo-id", diskJSON); err != nil {
		t.Fatalf("save disk: %v", err)
	}

	got, err := getSessionByTitleInScope("some-repo-id", "test-session")
	if err == nil {
		t.Fatalf("expected error from daemon auth failure, got session %+v (disk fallback masked it)", got)
	}
	if !errors.Is(err, authErr) {
		t.Fatalf("expected daemon auth error to surface, got: %v", err)
	}
}

// TestGetSessionByTitleInScope_RemoteTargetSurfacesNetworkError verifies an
// unreachable remote daemon surfaces its network error, not "instance not found".
func TestGetSessionByTitleInScope_RemoteTargetSurfacesNetworkError(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	remoteTarget(t)

	netErr := errors.New("dial tcp 10.0.0.1:443: connect: connection refused")
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) { return nil, netErr })

	diskJSON, _ := json.Marshal([]session.InstanceData{{Title: "test-session"}})
	if err := config.SaveRepoInstances("some-repo-id", diskJSON); err != nil {
		t.Fatalf("save disk: %v", err)
	}

	_, err := getSessionByTitleInScope("some-repo-id", "test-session")
	if !errors.Is(err, netErr) {
		t.Fatalf("expected network error to surface, got: %v", err)
	}
	if errors.Is(err, errTitleNotFound) {
		t.Fatal("network error must not be masked as instance-not-found")
	}
}

// TestGetSessionByTitleInScope_LocalDiskFallbackUnchanged proves the local
// (unix-socket) target still falls back to disk on snapshot error exactly as
// before — the fix must not disturb local behavior.
func TestGetSessionByTitleInScope_LocalDiskFallbackUnchanged(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	// FlagDaemonURL empty => local target.
	stubSnapshot(t, daemonUnavailable)

	diskJSON, _ := json.Marshal([]session.InstanceData{{Title: "test-session"}})
	if err := config.SaveRepoInstances("some-repo-id", diskJSON); err != nil {
		t.Fatalf("save disk: %v", err)
	}

	got, err := getSessionByTitleInScope("some-repo-id", "test-session")
	if err != nil {
		t.Fatalf("local disk fallback should succeed: %v", err)
	}
	if got.Title != "test-session" {
		t.Fatalf("got %q, want test-session", got.Title)
	}
}

// TestWhoamiSession_RemoteTargetSurfacesError verifies whoami surfaces a remote
// 401 even though a matching TmuxName sits on local disk (#1681).
func TestWhoamiSession_RemoteTargetSurfacesError(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	remoteTarget(t)

	authErr := errors.New("unauthorized: invalid bearer token")
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) { return nil, authErr })

	diskJSON, _ := json.Marshal([]session.InstanceData{{Title: "mine", TmuxName: "af_mine_agent"}})
	if err := config.SaveRepoInstances("repo-x", diskJSON); err != nil {
		t.Fatalf("save disk: %v", err)
	}

	got, err := whoamiSession("af_mine_agent")
	if err == nil {
		t.Fatalf("expected error from daemon auth failure, got session %+v (disk fallback masked it)", got)
	}
	if !errors.Is(err, authErr) {
		t.Fatalf("expected daemon auth error to surface, got: %v", err)
	}
}

// TestWhoamiSession_RemoteTargetSurfacesNetworkError verifies an unreachable
// remote daemon surfaces its network error from whoami, not "no session found".
func TestWhoamiSession_RemoteTargetSurfacesNetworkError(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	remoteTarget(t)

	netErr := errors.New("dial tcp 10.0.0.1:443: connect: connection refused")
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) { return nil, netErr })

	diskJSON, _ := json.Marshal([]session.InstanceData{{Title: "mine", TmuxName: "af_mine_agent"}})
	if err := config.SaveRepoInstances("repo-x", diskJSON); err != nil {
		t.Fatalf("save disk: %v", err)
	}

	_, err := whoamiSession("af_mine_agent")
	if !errors.Is(err, netErr) {
		t.Fatalf("expected network error to surface, got: %v", err)
	}
	if strings.Contains(err.Error(), "no Agent Factory session found") {
		t.Fatal("network error must not be masked as no-session-found")
	}
}

// TestWhoamiSession_LocalDiskFallbackUnchanged proves the local target still
// resolves whoami from disk on snapshot error (mirrors TestWhoamiSession_DiskFallback,
// paired here with the remote cases to document the intended contrast).
func TestWhoamiSession_LocalDiskFallbackUnchanged(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	// FlagDaemonURL empty => local target.
	stubSnapshot(t, daemonUnavailable)

	diskJSON, _ := json.Marshal([]session.InstanceData{{Title: "mine", TmuxName: "af_mine_agent"}})
	if err := config.SaveRepoInstances("repo-x", diskJSON); err != nil {
		t.Fatalf("save disk: %v", err)
	}

	got, err := whoamiSession("af_mine_agent")
	if err != nil {
		t.Fatalf("local disk fallback should succeed: %v", err)
	}
	if got.Title != "mine" {
		t.Fatalf("got %q, want mine", got.Title)
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

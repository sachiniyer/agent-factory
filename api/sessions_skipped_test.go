package api

import (
	"errors"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// stubSnapshotFn swaps snapshotViaDaemon for a stub returning the full
// (instances, skippedRepos, error) triple, so a test can exercise the
// corruption-signal path the wire fix added. The returned pointer captures every
// request so a test can assert --repo scoping is threaded through.
func stubSnapshotFn(t *testing.T, fn func(daemon.SnapshotRequest) ([]session.InstanceData, []daemon.SkippedRepo, error)) *[]daemon.SnapshotRequest {
	t.Helper()
	var reqs []daemon.SnapshotRequest
	prev := snapshotViaDaemon
	snapshotViaDaemon = func(req daemon.SnapshotRequest) ([]session.InstanceData, []daemon.SkippedRepo, error) {
		reqs = append(reqs, req)
		return fn(req)
	}
	t.Cleanup(func() { snapshotViaDaemon = prev })
	return &reqs
}

// corruptStub builds a stub mirroring the daemon's repo-scoping of SkippedRepos:
// it returns the canned instances plus only the skipped repos whose RepoID
// matches the request (empty RepoID = all). That is exactly what
// controlServer.snapshot produces, so the api layer's refuse/caveat logic is
// exercised against a faithful daemon answer.
func corruptStub(t *testing.T, data []session.InstanceData, allSkipped []daemon.SkippedRepo) *[]daemon.SnapshotRequest {
	t.Helper()
	return stubSnapshotFn(t, func(req daemon.SnapshotRequest) ([]session.InstanceData, []daemon.SkippedRepo, error) {
		skipped := allSkipped
		if req.RepoID != "" {
			skipped = nil
			for _, s := range allSkipped {
				if s.RepoID == req.RepoID {
					skipped = append(skipped, s)
				}
			}
		}
		return data, skipped, nil
	})
}

const skippedTestRepo = "corrupt-repo"

func skippedSet(repoIDs ...string) []daemon.SkippedRepo {
	out := make([]daemon.SkippedRepo, 0, len(repoIDs))
	for _, id := range repoIDs {
		out = append(out, daemon.SkippedRepo{RepoID: id, Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON})
	}
	return out
}

// --- listSessions ------------------------------------------------------

// TestListSessions_DaemonUpRefusesOnSkipped is the regression for the bug
// report's headline: with the daemon up and a repo dropped at startup, an
// all-repo list must refuse naming the dropped repo rather than silently
// returning a partial list as the complete answer (#730/#603 over the wire).
func TestListSessions_DaemonUpRefusesOnSkipped(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	live := []session.InstanceData{{Title: "survivor"}}
	corruptStub(t, live, skippedSet(skippedTestRepo))

	_, err := listSessions("")
	if err == nil {
		t.Fatal("all-repo list must refuse when a repo was dropped at startup, got nil error")
	}
	if !strings.Contains(err.Error(), skippedTestRepo) {
		t.Fatalf("error must name the dropped repo %q, got: %v", skippedTestRepo, err)
	}
	if !strings.Contains(err.Error(), "corrupted instances.json") {
		t.Fatalf("error must explain the cause, got: %v", err)
	}
}

// TestListSessions_DaemonUpScopedRefusesOnSkippedRepo pins the scoped misread:
// `af sessions list --repo X` against a running daemon whose startup dropped X
// must refuse naming X rather than return [] with err == nil.
func TestListSessions_DaemonUpScopedRefusesOnSkippedRepo(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	corruptStub(t, nil, skippedSet(skippedTestRepo))

	_, err := listSessions(skippedTestRepo)
	if err == nil {
		t.Fatalf("scoped list to a dropped repo must refuse, got nil error")
	}
	if !strings.Contains(err.Error(), skippedTestRepo) {
		t.Fatalf("error must name the dropped repo, got: %v", err)
	}
}

// TestListSessions_DaemonUpScopedIgnoresOtherRepoSkip proves a different repo's
// drop does not pollute a scoped read: `--repo valid-repo` returns its sessions
// untouched when only another repo was dropped (the daemon scopes SkippedRepos
// to the request, mirroring Instances).
func TestListSessions_DaemonUpScopedIgnoresOtherRepoSkip(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	live := []session.InstanceData{{Title: "ok"}}
	corruptStub(t, live, skippedSet("other-repo"))

	got, err := listSessions("valid-repo")
	if err != nil {
		t.Fatalf("a scoped list to a healthy repo must not refuse for another repo's drop: %v", err)
	}
	if want := []string{"ok"}; !equalStrs(titlesOf(got), want) {
		t.Fatalf("got %v, want the healthy repo's sessions %v", titlesOf(got), want)
	}
}

// --- getSessionByTitle (unscoped get) -----------------------------------

// TestGetSessionByTitle_DaemonUpMissCaveatsOnSkipped proves a miss against a
// live snapshot caveats naming the dropped repos instead of a clean not-found,
// mirroring findInstanceByTitle's corruption-tainted miss (#730, #603 over the
// wire) — a session hidden behind the corrupted file is not reported absent.
func TestGetSessionByTitle_DaemonUpMissCaveatsOnSkipped(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	live := []session.InstanceData{{Title: "someone-else"}}
	corruptStub(t, live, skippedSet(skippedTestRepo))

	_, _, err := getSessionByTitle("ghost")
	if err == nil {
		t.Fatal("miss against a dropped repo must caveat, not return nil")
	}
	if errors.Is(err, errTitleNotFound) {
		t.Fatalf("miss must NOT be a clean errTitleNotFound when a repo was dropped; got: %v", err)
	}
	if !strings.Contains(err.Error(), skippedTestRepo) || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error must be a not-found that names the dropped repo, got: %v", err)
	}
}

// TestGetSessionByTitle_DaemonUpPositiveMatchIgnoresSkipped proves a session
// found in the snapshot is returned without a corruption caveat — a PRESENCE
// the read can see is not weakened by a file it could not (mirrors
// findInstanceByTitle's positive branch, #730).
func TestGetSessionByTitle_DaemonUpPositiveMatchIgnoresSkipped(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	live := []session.InstanceData{{Title: "found", Path: "/repo"}}
	corruptStub(t, live, skippedSet(skippedTestRepo))

	got, notice, err := getSessionByTitle("found")
	if err != nil {
		t.Fatalf("positive match must resolve, got: %v", err)
	}
	if got.Title != "found" {
		t.Fatalf("got %q, want found", got.Title)
	}
	if notice != "" {
		t.Fatalf("positive match must carry no caveat, got notice %q", notice)
	}
}

// TestGetSessionByTitle_DaemonUpCleanMissUnchanged proves a clean snapshot (no
// repo dropped) still returns the bare errTitleNotFound sentinel so the
// corruption caveat only fires when there is something to caveat.
func TestGetSessionByTitle_DaemonUpCleanMissUnchanged(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	live := []session.InstanceData{{Title: "someone-else"}}
	corruptStub(t, live, nil)

	_, _, err := getSessionByTitle("ghost")
	if !errors.Is(err, errTitleNotFound) {
		t.Fatalf("clean miss must return errTitleNotFound, got: %v", err)
	}
}

// --- getSessionByTitleInScope (scoped get / watch) ---------------------

// TestGetSessionByTitleInScope_DaemonUpScopedRefusesOnSkipped proves a scoped
// miss refuses naming the dropped repo, mirroring the scoped disk path
// (loadRepoInstanceData's parse-error refusal) rather than a clean not-found.
func TestGetSessionByTitleInScope_DaemonUpScopedRefusesOnSkipped(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	corruptStub(t, nil, skippedSet(skippedTestRepo))

	_, _, err := getSessionByTitleInScope(skippedTestRepo, "missing")
	if err == nil {
		t.Fatal("scoped miss in a dropped repo must refuse, got nil")
	}
	if errors.Is(err, errTitleNotFound) {
		t.Fatalf("scoped miss in a dropped repo must NOT be a clean errTitleNotFound, got: %v", err)
	}
	if !strings.Contains(err.Error(), skippedTestRepo) {
		t.Fatalf("error must name the dropped repo, got: %v", err)
	}
}

// TestGetSessionByTitleInScope_DaemonUpCleanMissUnchanged proves a scoped miss
// in a healthy repo stays a clean errTitleNotFound when nothing was dropped.
func TestGetSessionByTitleInScope_DaemonUpCleanMissUnchanged(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	live := []session.InstanceData{{Title: "someone-else"}}
	corruptStub(t, live, nil)

	_, _, err := getSessionByTitleInScope("valid-repo", "missing")
	if !errors.Is(err, errTitleNotFound) {
		t.Fatalf("clean scoped miss must return errTitleNotFound, got: %v", err)
	}
}

// --- whoamiSession -----------------------------------------------------

// TestWhoamiSession_DaemonUpNoMatchCaveatsOnSkipped proves a no-match whoami
// caveats naming the dropped repos rather than a clean "no session found",
// mirroring diskWhoami's not-found-with-corruption behavior (#730, #603).
func TestWhoamiSession_DaemonUpNoMatchCaveatsOnSkipped(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	live := []session.InstanceData{{Title: "other", TmuxName: "af_other_agent"}}
	corruptStub(t, live, skippedSet(skippedTestRepo))

	_, err := whoamiSession("af_nobody_agent")
	if err == nil {
		t.Fatal("no-match whoami against a dropped repo must caveat, not return nil")
	}
	if !strings.Contains(err.Error(), skippedTestRepo) {
		t.Fatalf("error must name the dropped repo, got: %v", err)
	}
	if !strings.Contains(err.Error(), "no Agent Factory session found") {
		t.Fatalf("error must still read as a whoami not-found, got: %v", err)
	}
}

// TestWhoamiSession_DaemonUpPositiveMatchIgnoresSkipped proves a found session
// is returned even when another repo was dropped — a match short-circuits before
// the absence caveat (mirrors diskWhoami's match branch).
func TestWhoamiSession_DaemonUpPositiveMatchIgnoresSkipped(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	live := []session.InstanceData{{Title: "mine", TmuxName: "af_mine_agent"}}
	corruptStub(t, live, skippedSet(skippedTestRepo))

	got, err := whoamiSession("af_mine_agent")
	if err != nil {
		t.Fatalf("positive whoami match must resolve, got: %v", err)
	}
	if got.Title != "mine" {
		t.Fatalf("got %q, want mine", got.Title)
	}
}

// --- listSessionsInScope (fleet watch read path) -----------------------

// TestListSessionsInScope_DaemonUpRefusesOnSkipped proves the fleet-watch read
// path refuses naming the dropped repo rather than silently serving a partial
// fleet, mirroring listSessionsRequest; the watch cleanly exits on the error so
// a driver learns the fleet view is incomplete.
func TestListSessionsInScope_DaemonUpRefusesOnSkipped(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	live := []session.InstanceData{{Title: "survivor"}}
	corruptStub(t, live, skippedSet(skippedTestRepo))

	data, src, err := listSessionsInScope("")
	if err == nil {
		t.Fatal("fleet read must refuse when a repo was dropped, got nil error")
	}
	if !strings.Contains(err.Error(), skippedTestRepo) {
		t.Fatalf("error must name the dropped repo, got: %v", err)
	}
	if data != nil {
		t.Fatalf("no data on refuse, got %v", data)
	}
	if src != watchSourceNone {
		t.Fatalf("source must be none on refuse, got %v", src)
	}
}

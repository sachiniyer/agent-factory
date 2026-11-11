package api

import (
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// Titles are unique per-repo, so a bare title on the CLI can name more than one
// session. These pin the resolution contract: --repo (or the cwd repo) scopes the
// lookup; with no repo context a unique title still resolves, but a title held by
// several repos errors and names them instead of silently picking one.

// seedTwoReposSharingTitle writes a same-titled session into two repos, plus a
// title unique to one of them.
func seedTwoReposSharingTitle(t *testing.T) {
	t.Helper()
	aJSON, err := json.Marshal([]session.InstanceData{
		{Title: "foo", Path: "/repos/alpha"},
		{Title: "only-alpha", Path: "/repos/alpha"},
	})
	if err != nil {
		t.Fatalf("marshal a: %v", err)
	}
	bJSON, err := json.Marshal([]session.InstanceData{
		{Title: "foo", Path: "/repos/beta"},
	})
	if err != nil {
		t.Fatalf("marshal b: %v", err)
	}
	if err := config.SaveRepoInstances("repo-alpha", aJSON); err != nil {
		t.Fatalf("save alpha: %v", err)
	}
	if err := config.SaveRepoInstances("repo-beta", bJSON); err != nil {
		t.Fatalf("save beta: %v", err)
	}
}

// TestFindInstanceByTitle_AmbiguousAcrossRepos is the core of the contract: an
// unscoped bare title held by two repos must report ErrAmbiguousTitle naming both
// repo paths, never resolve one. The all-repos scan walks a Go map, so "first
// match" was nondeterministic across runs.
func TestFindInstanceByTitle_AmbiguousAcrossRepos(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	seedTwoReposSharingTitle(t)

	_, _, err := findInstanceByTitle("foo")
	if err == nil {
		t.Fatalf("a title held by two repos must not resolve to one of them")
	}
	if !errors.Is(err, session.ErrAmbiguousTitle) {
		t.Fatalf("expected ErrAmbiguousTitle, got: %v", err)
	}
	for _, want := range []string{"/repos/alpha", "/repos/beta"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name repo %q so the user can pass --repo, got: %v", want, err)
		}
	}
	if !strings.Contains(err.Error(), "--repo") {
		t.Errorf("error should tell the user how to disambiguate, got: %v", err)
	}
}

// TestFindInstanceByTitle_UniqueTitleStillResolves keeps the bare-title
// convenience for the common single-match case.
func TestFindInstanceByTitle_UniqueTitleStillResolves(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	seedTwoReposSharingTitle(t)

	data, repoID, err := findInstanceByTitle("only-alpha")
	if err != nil {
		t.Fatalf("a globally unique title must still resolve with no --repo: %v", err)
	}
	if data.Title != "only-alpha" || repoID != "repo-alpha" {
		t.Errorf("resolved wrong session: title=%q repo=%q", data.Title, repoID)
	}
}

// TestFindInstanceByTitleInScope_RepoDisambiguates verifies --repo picks the
// intended repo's session when both hold the title.
func TestFindInstanceByTitleInScope_RepoDisambiguates(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	seedTwoReposSharingTitle(t)

	for _, tc := range []struct{ repoID, wantPath string }{
		{"repo-alpha", "/repos/alpha"},
		{"repo-beta", "/repos/beta"},
	} {
		data, repoID, err := findInstanceByTitleInScope(tc.repoID, "foo")
		if err != nil {
			t.Fatalf("--repo %s must disambiguate %q, got: %v", tc.repoID, "foo", err)
		}
		if repoID != tc.repoID || data.Path != tc.wantPath {
			t.Errorf("scope %s resolved repo=%q path=%q, want repo=%q path=%q",
				tc.repoID, repoID, data.Path, tc.repoID, tc.wantPath)
		}
	}
}

// TestFindInstanceByTitle_NotFoundUnchanged guards that adding the ambiguity
// branch did not disturb the clean-miss sentinel callers rely on (#861).
func TestFindInstanceByTitle_NotFoundUnchanged(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	seedTwoReposSharingTitle(t)

	_, _, err := findInstanceByTitle("nope")
	if !errors.Is(err, errTitleNotFound) {
		t.Fatalf("a clean miss must still wrap errTitleNotFound, got: %v", err)
	}
	if errors.Is(err, session.ErrAmbiguousTitle) {
		t.Errorf("a miss is not an ambiguity: %v", err)
	}
}

// TestFindInstanceByTitle_DuplicateRowsInOneRepoAreNotAmbiguous guards the
// edge the distinct-repo grouping exists for: two rows with the same title
// inside ONE repo's instances.json is a corruption artifact (dedupeInstanceData
// territory), not a cross-project collision. Reporting it as "exists in multiple
// projects" would name a single repo twice and send the user chasing a --repo
// flag that cannot help.
func TestFindInstanceByTitle_DuplicateRowsInOneRepoAreNotAmbiguous(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	dupJSON, err := json.Marshal([]session.InstanceData{
		{Title: "foo", Path: "/repos/alpha"},
		{Title: "foo", Path: "/repos/alpha"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := config.SaveRepoInstances("repo-alpha", dupJSON); err != nil {
		t.Fatalf("save: %v", err)
	}

	data, repoID, err := findInstanceByTitle("foo")
	if err != nil {
		t.Fatalf("duplicate rows within one repo must resolve, not report ambiguity: %v", err)
	}
	if repoID != "repo-alpha" || data.Title != "foo" {
		t.Errorf("resolved wrong session: repo=%q title=%q", repoID, data.Title)
	}
}

// TestResolveLookupScope_RemoteIsNotScopedByClientCwd pins the remote rule:
// --daemon-url points af at a daemon on ANOTHER machine, where the client's cwd
// names a repo that exists HERE, not there. Auto-scoping by it would ask the
// remote for a repo ID it does not have — and the remote read path has no disk
// fallback — turning a working bare-title lookup into a spurious not-found.
func TestResolveLookupScope_RemoteIsNotScopedByClientCwd(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	// cwd is a real git repo, so CurrentRepo() would happily produce a scope.
	repo := t.TempDir()
	if err := exec.Command("git", "init", repo).Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	t.Chdir(repo)

	// Local target: the cwd repo DOES scope the lookup.
	localScope, err := resolveLookupScope()
	if err != nil {
		t.Fatalf("local resolve: %v", err)
	}
	local := localScope.RepoID
	if local == "" {
		t.Fatalf("a LOCAL lookup should still scope by the cwd repo")
	}

	// Remote target: the client's cwd must NOT scope it.
	remoteTarget(t)
	got, err := resolveLookupScope()
	if err != nil {
		t.Fatalf("remote resolve: %v", err)
	}
	if got != (lookupScope{}) {
		t.Errorf("a REMOTE lookup must not be scoped by the client's cwd, got %+v", got)
	}

	// But the WRITE-path resolver must STAY cwd-scoped even against a remote:
	// kill/archive/send-prompt talk to the LOCAL daemon over the control socket
	// regardless of --daemon-url, so dropping their scope would send an unscoped
	// destructive request to the local daemon and let it resolve a same-titled
	// session in a different LOCAL repo.
	write, err := resolveRepoID()
	if err != nil {
		t.Fatalf("write resolve: %v", err)
	}
	if write == "" {
		t.Errorf("resolveRepoID must stay cwd-scoped for a remote target: an unscoped local kill/archive could hit the wrong repo")
	}
	if write != local {
		t.Errorf("write scope changed with the target: got %q want %q", write, local)
	}
}

// TestSessionsGet_RemoteSendsUnscopedRequest is the behavioral half of the rule:
// against a remote daemon the request must carry no RepoID, so a bare title that
// resolved before this change keeps resolving.
func TestSessionsGet_RemoteSendsUnscopedRequest(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := t.TempDir()
	if err := exec.Command("git", "init", repo).Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	t.Chdir(repo)
	remoteTarget(t)

	reqs := stubSnapshot(t, func(req daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return []session.InstanceData{{Title: "foo", Path: "/remote/alpha"}}, nil
	})

	scope, err := resolveLookupScope()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got, _, err := getSessionByTitleInScope(scope.RepoID, "foo")
	if err != nil {
		t.Fatalf("remote bare-title lookup must resolve, got: %v", err)
	}
	if got.Title != "foo" {
		t.Errorf("resolved %q", got.Title)
	}
	if len(*reqs) == 0 {
		t.Fatalf("expected a snapshot request")
	}
	for _, r := range *reqs {
		if r.RepoID != "" {
			t.Errorf("remote request must be unscoped, carried RepoID %q", r.RepoID)
		}
	}
}

// TestGetSessionByTitle_SnapshotUnionsDiskRows closes the read-path twin of the
// daemon's partial-restore gap: the snapshot only mirrors the daemon's live
// instances, and refresh SKIPS rows it cannot restore. A lone snapshot match is
// therefore not proof of uniqueness — a second repo holding the title on disk
// only would be invisible, and a bare `sessions get foo` would name the wrong
// project.
func TestGetSessionByTitle_SnapshotUnionsDiskRows(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	// The daemon can only restore repo alpha's session.
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return []session.InstanceData{{Title: "foo", Path: "/repos/alpha"}}, nil
	})
	// But repo beta holds the same title on disk (its worktree/tmux is gone, so
	// refresh skipped it and it never reached the snapshot).
	seedTwoReposSharingTitle(t)

	_, _, err := getSessionByTitle("foo")
	if err == nil {
		t.Fatalf("a lone snapshot match must not resolve while another repo holds the title on disk")
	}
	if !errors.Is(err, session.ErrAmbiguousTitle) {
		t.Fatalf("expected ErrAmbiguousTitle, got: %v", err)
	}
	for _, want := range []string{"/repos/alpha", "/repos/beta"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name repo %q, got: %v", want, err)
		}
	}
}

// TestGetSessionByTitle_RemoteIgnoresLocalDisk guards the other side: a remote
// target's sessions have nothing to do with this machine's instances.json, so a
// same-titled LOCAL session must never make a remote lookup ambiguous.
func TestGetSessionByTitle_RemoteIgnoresLocalDisk(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	remoteTarget(t)
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return []session.InstanceData{{Title: "foo", Path: "/remote/alpha"}}, nil
	})
	// Local disk also has "foo" in two repos — irrelevant to the remote.
	seedTwoReposSharingTitle(t)

	got, _, err := getSessionByTitle("foo")
	if err != nil {
		t.Fatalf("local disk must not affect a remote lookup, got: %v", err)
	}
	if got.Path != "/remote/alpha" {
		t.Errorf("resolved %q, want the remote session", got.Path)
	}
}

// TestGetSessionByTitle_AmbiguousFromSnapshot covers the daemon-snapshot path
// (the one `sessions get` actually takes when a daemon is up), not just the disk
// fallback exercised above.
func TestGetSessionByTitle_AmbiguousFromSnapshot(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	stubSnapshot(t, func(req daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return []session.InstanceData{
			{Title: "foo", Path: "/repos/alpha"},
			{Title: "foo", Path: "/repos/beta"},
			{Title: "solo", Path: "/repos/alpha"},
		}, nil
	})

	_, _, err := getSessionByTitle("foo")
	if err == nil || !errors.Is(err, session.ErrAmbiguousTitle) {
		t.Fatalf("snapshot path must report ambiguity for a duplicated title, got: %v", err)
	}
	for _, want := range []string{"/repos/alpha", "/repos/beta"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name repo %q, got: %v", want, err)
		}
	}

	got, _, err := getSessionByTitle("solo")
	if err != nil {
		t.Fatalf("unique title must resolve from the snapshot: %v", err)
	}
	if got.Title != "solo" {
		t.Errorf("resolved wrong session: %q", got.Title)
	}
}

// ---------------------------------------------------------------------------
// #6882: the none-held half of the contract. Outside a git repository a bare
// title is looked up across EVERY project, so a miss is not "the session is
// gone" but "no project holds it" — the error must say that, say why the
// lookup had no scope, and say how to give it one. A bare "not found" reads
// as data loss to a user standing in the wrong directory.
// ---------------------------------------------------------------------------

// TestFindInstanceByTitle_UnscopedMissExplainsNoScope pins the none-held half
// on the disk path: the all-repos scan already covered every project, so the
// miss names the absent context and the remedy instead of pointing at the
// name. errTitleNotFound must survive inside it so the send-prompt exists
// check still classifies the miss as clean (#861).
func TestFindInstanceByTitle_UnscopedMissExplainsNoScope(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	seedTwoReposSharingTitle(t)

	_, _, err := findInstanceByTitle("nope")
	if err == nil {
		t.Fatal("a title held by no project must not resolve")
	}
	if !errors.Is(err, errTitleNotFound) {
		t.Fatalf("a clean miss must still classify as errTitleNotFound, got: %v", err)
	}
	for _, want := range []string{"held by no project", "outside a git repository", "--repo"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the unscoped miss must contain %q, got: %v", want, err)
		}
	}
}

// TestGetSessionByTitle_SnapshotMissUnionsDiskRows is the miss-side twin of
// TestGetSessionByTitle_SnapshotUnionsDiskRows: the snapshot only mirrors the
// daemon's live projection, and a persisted row refresh cannot restore never
// reaches it — a startup rows-failed-to-load skip is even withheld from the
// wire skipped set (daemon/snapshot.go). So a snapshot miss cannot support
// "no project holds it" on its own: the disk answers first, and a title one
// project still holds there resolves — the same bare-title convenience a
// daemon-down lookup grants.
func TestGetSessionByTitle_SnapshotMissUnionsDiskRows(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	// The daemon's snapshot knows only someone-else: repo-alpha's row failed
	// to restore at startup (worktree/tmux gone), so it never reached the
	// projection — but it IS still held by a project. Its persisted LiveReady
	// is last-saved state, not an observation: the row resolves, but reported
	// as Lost — the live daemon could not confirm it — so watch can never
	// complete on the stale "ready" (#6882 review).
	saveOneRepoInstances(t, "repo-alpha", session.InstanceData{Title: "ghost", Path: "/repos/alpha", BackendType: "docker", Liveness: session.LiveReady})
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return []session.InstanceData{{Title: "someone-else", Path: "/repos/alpha"}}, nil
	})

	got, _, err := getSessionByTitle("ghost")
	if err != nil {
		t.Fatalf("a title one project holds on disk must resolve despite the snapshot miss: %v", err)
	}
	if got.Title != "ghost" || got.Path != "/repos/alpha" {
		t.Fatalf("resolved the wrong row: %+v", got)
	}
	if got.Liveness != session.LiveLost {
		t.Fatalf("a disk-only row's live liveness must project as LiveLost, got %v", got.Liveness)
	}
}

// TestGetSessionByTitle_SnapshotMissAmbiguityFromDisk is the several-held half
// on the miss path: two disk-held rows the snapshot serves neither of must
// still refuse to pick — reporting the collision beats claiming "no project
// holds it" in EITHER direction.
func TestGetSessionByTitle_SnapshotMissAmbiguityFromDisk(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	seedTwoReposSharingTitle(t)
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return []session.InstanceData{{Title: "someone-else", Path: "/repos/x"}}, nil
	})

	_, _, err := getSessionByTitle("foo")
	if err == nil {
		t.Fatal("a title two projects hold on disk must not resolve, not even to 'not found'")
	}
	if !errors.Is(err, session.ErrAmbiguousTitle) {
		t.Fatalf("expected ErrAmbiguousTitle, got: %v", err)
	}
	for _, want := range []string{"/repos/alpha", "/repos/beta", "--repo"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ambiguity must name %q, got: %v", want, err)
		}
	}
}

// TestGetSessionByTitle_SnapshotMissExplainsNoScope is the #6882 headline on
// the live-read path: with the snapshot AND the disk both missing the title,
// the miss says no project holds it and why the lookup was unscoped — never a
// bare "not found" pointing at the name.
func TestGetSessionByTitle_SnapshotMissExplainsNoScope(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	seedTwoReposSharingTitle(t)
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return []session.InstanceData{{Title: "someone-else", Path: "/repos/alpha"}}, nil
	})

	_, _, err := getSessionByTitle("nope")
	if err == nil {
		t.Fatal("a title held by no project must not resolve")
	}
	if !errors.Is(err, errTitleNotFound) {
		t.Fatalf("a clean miss must still classify as errTitleNotFound, got: %v", err)
	}
	for _, want := range []string{"held by no project", "outside a git repository", "--repo"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the unscoped miss must contain %q, got: %v", want, err)
		}
	}
}

// TestGetSessionByTitle_RemoteMissNamesDaemon pins the remote twin: against a
// remote daemon the client's cwd is ignored by design, so "outside a git
// repository" is the wrong story — the miss names the daemon it searched and
// the --repo that scopes the next lookup.
func TestGetSessionByTitle_RemoteMissNamesDaemon(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	remoteTarget(t)
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return []session.InstanceData{{Title: "someone-else", Path: "/remote/alpha"}}, nil
	})

	_, _, err := getSessionByTitle("ghost")
	if err == nil {
		t.Fatal("a title held by no project must not resolve")
	}
	if !errors.Is(err, errTitleNotFound) {
		t.Fatalf("a clean miss must still classify as errTitleNotFound, got: %v", err)
	}
	if !strings.Contains(err.Error(), "remote.example.com") {
		t.Errorf("a remote miss must name the daemon it searched, got: %v", err)
	}
	if !strings.Contains(err.Error(), "--repo") {
		t.Errorf("a remote miss must name the scope flag, got: %v", err)
	}
	if strings.Contains(err.Error(), "outside a git repository") {
		t.Errorf("a remote miss must not blame the client's cwd, got: %v", err)
	}
}

// TestGetSessionByTitleInScope_ScopedMissStaysBare pins property 3: inside a
// repo the miss keeps its bare not-found shape — the outside-repo explanation
// only applies where project context was actually absent.
func TestGetSessionByTitleInScope_ScopedMissStaysBare(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return []session.InstanceData{{Title: "someone-else", Path: "/repos/alpha"}}, nil
	})

	_, _, err := getSessionByTitleInScope("repo-alpha", "nope")
	if err == nil {
		t.Fatal("a scoped miss must still error")
	}
	if got := err.Error(); got != `session "nope" not found` {
		t.Errorf("a scoped miss must keep its bare not-found shape, got: %v", got)
	}
}

// TestGetSessionByTitleInScope_SnapshotMissUnionsDiskRows is the scoped twin
// of the unscoped union: a persisted row the daemon's projection has not
// caught up to yet still resolves inside its own repo — the same disk check
// the daemon's scoped resolver (findInstanceDataByTitle) makes after an
// in-memory miss.
func TestGetSessionByTitleInScope_SnapshotMissUnionsDiskRows(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	saveOneRepoInstances(t, "repo-alpha", session.InstanceData{Title: "ghost", Path: "/repos/alpha", BackendType: "docker", Liveness: session.LiveReady})
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return []session.InstanceData{{Title: "someone-else", Path: "/repos/alpha"}}, nil
	})

	got, _, err := getSessionByTitleInScope("repo-alpha", "ghost")
	if err != nil {
		t.Fatalf("a title the repo holds on disk must resolve despite the scoped snapshot miss: %v", err)
	}
	if got.Title != "ghost" || got.Path != "/repos/alpha" {
		t.Fatalf("resolved the wrong row: %+v", got)
	}
	if got.Liveness != session.LiveLost {
		t.Fatalf("a disk-only row's live liveness must project as LiveLost, got %v", got.Liveness)
	}

	// The scope still confines it: repo-beta does not hold the title, on disk
	// or in the snapshot, so it must not resolve out of scope.
	if _, _, err := getSessionByTitleInScope("repo-beta", "ghost"); err == nil {
		t.Fatal("a title held only by repo-alpha must not resolve under repo-beta")
	}
}

// TestSessionsList_OutsideRepoPrintsScopeHintOnStderr is the #6882 list half:
// stdout stays the parseable bare JSON array, and exactly one stderr line
// explains that no project context scoped it — so `[]` cannot be misread as
// "all sessions are gone".
func TestSessionsList_OutsideRepoPrintsScopeHintOnStderr(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	resetScopeFlags(t)
	warn := captureWarnWriter(t)
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return []session.InstanceData{{Title: "foo", Path: "/repos/alpha"}}, nil
	})
	t.Chdir(t.TempDir()) // not a git repository

	out := captureJSON(t, func() error { return sessionsListCmd.RunE(sessionsListCmd, nil) })
	var got []session.InstanceData
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("stdout must stay the bare JSON array, got %q: %v", out, err)
	}
	if len(got) != 1 || got[0].Title != "foo" {
		t.Fatalf("the unscoped list must still span every project, got %+v", got)
	}

	lines := strings.Split(strings.TrimSpace(warn.String()), "\n")
	if len(lines) != 1 || strings.TrimSpace(lines[0]) == "" {
		t.Fatalf("outside a repo the list must print exactly one stderr line, got %q", warn.String())
	}
	for _, want := range []string{"not inside a project repo", "--repo", "--all"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("the scope hint must contain %q, got: %q", want, lines[0])
		}
	}
}

// TestSessionsList_ExplicitAllPrintsNoHint: a caller who asked for --all KNOWS
// the breadth is deliberate — the hint exists only for a scope the user did
// not choose.
func TestSessionsList_ExplicitAllPrintsNoHint(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	resetScopeFlags(t)
	warn := captureWarnWriter(t)
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return nil, nil
	})
	t.Chdir(t.TempDir()) // not a git repository
	sessionsListAllFlag = true

	out := captureJSON(t, func() error { return sessionsListCmd.RunE(sessionsListCmd, nil) })
	var got []session.InstanceData
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("stdout must stay the bare JSON array, got %q: %v", out, err)
	}
	if warn.String() != "" {
		t.Errorf("an explicit --all must not print the outside-repo hint, got %q", warn.String())
	}
}

// TestSessionsList_InsideRepoPrintsNoHint pins property 3 for the hint: scoped
// to a project, the outside-repo hint never prints. An EMPTY scoped list gets
// its own notice naming the project (#7092, scoped_lookup_notice_test.go), so
// this asserts the absence of the outside-repo claim, not an empty stderr.
func TestSessionsList_InsideRepoPrintsNoHint(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	warn := captureWarnWriter(t)
	repo := mkRepo(t, "alpha")
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return nil, nil
	})
	t.Chdir(repo)

	out := captureJSON(t, func() error { return sessionsListCmd.RunE(sessionsListCmd, nil) })
	if !json.Valid(out) {
		t.Fatalf("stdout must stay valid JSON, got %q", out)
	}
	if strings.Contains(warn.String(), "not inside a project repo") {
		t.Errorf("a scoped list must not print the outside-repo hint, got %q", warn.String())
	}
}

// TestSessionsList_HintSuppressedUnderJSON: under --json, stderr carries the
// {data,error} envelope, and a free-form line there hands automation invalid
// JSON on a SUCCESSFUL command (#3169). The hint waits for #3511's structural
// channel like every other caveat.
func TestSessionsList_HintSuppressedUnderJSON(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	resetScopeFlags(t)
	warn := captureWarnWriter(t)
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return nil, nil
	})
	t.Chdir(t.TempDir()) // not a git repository

	withEnvelope(t, func() {
		out := captureJSON(t, func() error { return sessionsListCmd.RunE(sessionsListCmd, nil) })
		if !json.Valid(out) {
			t.Fatalf("stdout must stay the envelope, got %q", out)
		}
	})
	if warn.String() != "" {
		t.Errorf("under --json the hint must not corrupt stderr, got %q", warn.String())
	}
}

// TestSendPrompt_UnscopedMissExplainsNoScope: the send-prompt exists check
// just searched every project, so its miss gets the same no-project-holds-it
// clause plus the verb's own --create affordance.
func TestSendPrompt_UnscopedMissExplainsNoScope(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	resetSendPromptState(t)
	resetScopeFlags(t)
	seedTwoReposSharingTitle(t)
	stubDaemon(t)
	t.Chdir(t.TempDir()) // not a git repository

	err := sessionsSendPromptCmd.RunE(sessionsSendPromptCmd, []string{"nope", "hi"})
	if err == nil {
		t.Fatal("a title held by no project must not send")
	}
	for _, want := range []string{"held by no project", "outside a git repository", "--repo", "--create"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the unscoped miss must contain %q, got: %v", want, err)
		}
	}
}

// TestSessionsGet_OutsideRepoMissExplainsNoScope is the command-level pin:
// `af sessions get nope` run from outside a repository must surface the
// scope-explaining miss, not a bare not-found pointing at the name.
func TestSessionsGet_OutsideRepoMissExplainsNoScope(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	resetScopeFlags(t)
	silenceCobraOutput()
	seedTwoReposSharingTitle(t)
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return []session.InstanceData{{Title: "someone-else", Path: "/repos/alpha"}}, nil
	})
	t.Chdir(t.TempDir()) // not a git repository

	err := sessionsGetCmd.RunE(sessionsGetCmd, []string{"nope"})
	if err == nil {
		t.Fatal("a title held by no project must not resolve")
	}
	for _, want := range []string{"held by no project", "outside a git repository", "--repo"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the unscoped miss must contain %q, got: %v", want, err)
		}
	}
}

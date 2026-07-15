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

// TestResolveRepoIDForLookup_RemoteIsNotScopedByClientCwd pins the remote rule:
// --daemon-url points af at a daemon on ANOTHER machine, where the client's cwd
// names a repo that exists HERE, not there. Auto-scoping by it would ask the
// remote for a repo ID it does not have — and the remote read path has no disk
// fallback — turning a working bare-title lookup into a spurious not-found.
func TestResolveRepoIDForLookup_RemoteIsNotScopedByClientCwd(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	// cwd is a real git repo, so CurrentRepo() would happily produce a scope.
	repo := t.TempDir()
	if err := exec.Command("git", "init", repo).Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	t.Chdir(repo)

	// Local target: the cwd repo DOES scope the lookup.
	local, err := resolveRepoIDForLookup()
	if err != nil {
		t.Fatalf("local resolve: %v", err)
	}
	if local == "" {
		t.Fatalf("a LOCAL lookup should still scope by the cwd repo")
	}

	// Remote target: the client's cwd must NOT scope it.
	remoteTarget(t)
	got, err := resolveRepoIDForLookup()
	if err != nil {
		t.Fatalf("remote resolve: %v", err)
	}
	if got != "" {
		t.Errorf("a REMOTE lookup must not be scoped by the client's cwd, got repoID %q", got)
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

	repoID, err := resolveRepoIDForLookup()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got, err := getSessionByTitleInScope(repoID, "foo")
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

	_, err := getSessionByTitle("foo")
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

	got, err := getSessionByTitle("foo")
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

	_, err := getSessionByTitle("foo")
	if err == nil || !errors.Is(err, session.ErrAmbiguousTitle) {
		t.Fatalf("snapshot path must report ambiguity for a duplicated title, got: %v", err)
	}
	for _, want := range []string{"/repos/alpha", "/repos/beta"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name repo %q, got: %v", want, err)
		}
	}

	got, err := getSessionByTitle("solo")
	if err != nil {
		t.Fatalf("unique title must resolve from the snapshot: %v", err)
	}
	if got.Title != "solo" {
		t.Errorf("resolved wrong session: %q", got.Title)
	}
}

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

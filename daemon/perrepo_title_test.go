package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Session titles are unique PER-REPO, not globally: the same name in two
// different projects is a supported, ordinary thing to do. Every name a session
// owns is already repo-scoped — the git branch (per-repo by nature), the tmux
// name (af_<repohash>_<title>), the archive dir (archived/<repoID>/<title>), and
// the manager key (daemonInstanceKey(repoID, title)) — so these tests pin the
// end-to-end property rather than any single check.

// TestCreateSessionAllowsSameTitleInDifferentRepos is the headline case: the
// same local title creates cleanly in two repos and the two sessions stay
// distinct in every structure that names them.
func TestCreateSessionAllowsSameTitleInDifferentRepos(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)

	repoAPath := setupControlRepo(t)
	repoBPath := setupControlRepo(t)
	repoA, err := config.RepoFromPath(repoAPath)
	if err != nil {
		t.Fatalf("RepoFromPath(A): %v", err)
	}
	repoB, err := config.RepoFromPath(repoBPath)
	if err != nil {
		t.Fatalf("RepoFromPath(B): %v", err)
	}
	if repoA.ID == repoB.ID {
		t.Fatalf("test premise broken: distinct repos must have distinct IDs")
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	const title = "foo"
	if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title: title, RepoPath: repoAPath, Program: "claude",
	}); err != nil {
		t.Fatalf("create %q in repo A: %v", title, err)
	}
	// The same title in a DIFFERENT repo must be accepted.
	if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title: title, RepoPath: repoBPath, Program: "claude",
	}); err != nil {
		t.Fatalf("create %q in repo B must be allowed (titles are per-repo), got: %v", title, err)
	}

	manager.mu.Lock()
	instA := manager.instances[daemonInstanceKey(repoA.ID, title)]
	instB := manager.instances[daemonInstanceKey(repoB.ID, title)]
	manager.mu.Unlock()

	if instA == nil || instB == nil {
		t.Fatalf("both sessions must be tracked under their own (repoID,title) key: A=%v B=%v", instA != nil, instB != nil)
	}
	if instA == instB {
		t.Fatalf("the two repos' sessions must be distinct instances")
	}
	// No cross-clobber: the tmux name each repo derives for this title embeds the
	// repo hash, so the two sessions can never target one tmux session. (Asserted
	// on the derivation rather than the instances: these run on a fake backend,
	// which owns no tmux session and so reports an empty name.)
	nameA := tmux.NewTmuxSessionForRepo(title, repoAPath, "claude").SanitizedName()
	nameB := tmux.NewTmuxSessionForRepo(title, repoBPath, "claude").SanitizedName()
	if nameA == nameB {
		t.Errorf("tmux names must be repo-scoped, both are %q", nameA)
	}
	if !strings.HasSuffix(nameA, "_"+title) || !strings.HasSuffix(nameB, "_"+title) {
		t.Errorf("tmux names should keep the af_<repohash>_<title> shape: %q / %q", nameA, nameB)
	}
	if instA.ID == instB.ID || instA.ID == "" || instB.ID == "" {
		t.Errorf("stable ids must be distinct and non-empty: A=%q B=%q", instA.ID, instB.ID)
	}
	if instA.Path == instB.Path {
		t.Errorf("sessions must point at their own repo: both %q", instA.Path)
	}
}

// TestPersistedStateSurvivesDuplicateTitlesAcrossRepos covers the storage side:
// on-disk records are keyed by repo (instances/<repoID>/instances.json), so two
// same-titled sessions must persist independently — neither overwriting the
// other's row nor bleeding into the other repo's file. Also pins that a scoped
// disk lookup resolves each to its OWN repo, and that stable ids (#1741) stay
// distinct across the duplicate titles.
func TestPersistedStateSurvivesDuplicateTitlesAcrossRepos(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)

	repoAPath := setupControlRepo(t)
	repoBPath := setupControlRepo(t)
	repoA, err := config.RepoFromPath(repoAPath)
	if err != nil {
		t.Fatalf("RepoFromPath(A): %v", err)
	}
	repoB, err := config.RepoFromPath(repoBPath)
	if err != nil {
		t.Fatalf("RepoFromPath(B): %v", err)
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	const title = "foo"
	for _, p := range []string{repoAPath, repoBPath} {
		if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
			Title: title, RepoPath: p, Program: "claude",
		}); err != nil {
			t.Fatalf("create %q in %s: %v", title, p, err)
		}
	}

	// Each repo's file holds exactly ONE row for the title — its own.
	for _, tc := range []struct{ repoID, wantPath string }{
		{repoA.ID, repoAPath},
		{repoB.ID, repoBPath},
	} {
		rows, err := loadRepoInstanceData(tc.repoID)
		if err != nil {
			t.Fatalf("loadRepoInstanceData(%s): %v", tc.repoID, err)
		}
		n := 0
		for i := range rows {
			if rows[i].Title == title {
				n++
				if rows[i].Path != tc.wantPath {
					t.Errorf("repo %s stored a row pointing at %q, want %q", tc.repoID, rows[i].Path, tc.wantPath)
				}
			}
		}
		if n != 1 {
			t.Errorf("repo %s must hold exactly one %q row, got %d", tc.repoID, title, n)
		}
	}

	// A repo-scoped disk lookup resolves each to its own session.
	dataA, ridA, err := findInstanceDataByTitle(title, repoA.ID)
	if err != nil {
		t.Fatalf("scoped disk lookup in A: %v", err)
	}
	dataB, ridB, err := findInstanceDataByTitle(title, repoB.ID)
	if err != nil {
		t.Fatalf("scoped disk lookup in B: %v", err)
	}
	if ridA != repoA.ID || dataA.Path != repoAPath {
		t.Errorf("A resolved repo=%q path=%q", ridA, dataA.Path)
	}
	if ridB != repoB.ID || dataB.Path != repoBPath {
		t.Errorf("B resolved repo=%q path=%q", ridB, dataB.Path)
	}
	// Stable ids (#1741) must stay distinct despite the shared title.
	if dataA.ID == "" || dataB.ID == "" || dataA.ID == dataB.ID {
		t.Errorf("stable ids must be distinct and non-empty: A=%q B=%q", dataA.ID, dataB.ID)
	}

	// And an UNSCOPED disk lookup is ambiguous rather than silently picking one.
	if _, _, err := findInstanceDataByTitle(title, ""); !errors.Is(err, session.ErrAmbiguousTitle) {
		t.Errorf("unscoped disk lookup of a duplicated title must be ambiguous, got: %v", err)
	}
}

// TestCreateSessionAllowsSameRemoteTitleInDifferentRepos is the regression for
// the ONE check that actually rejected a cross-repo duplicate: the daemon's
// remote-hook slug reservation was a GLOBAL map keyed by bare slug, so creating
// remote "foo" in repo A made remote "foo" in repo B fail with
// `remote hook name "foo" is already reserved`. Reservations are now keyed by
// (repoID, slug).
func TestCreateSessionAllowsSameRemoteTitleInDifferentRepos(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoAPath := setupControlRepo(t)
	repoBPath := setupControlRepo(t)
	repoA, err := config.RepoFromPath(repoAPath)
	if err != nil {
		t.Fatalf("RepoFromPath(A): %v", err)
	}
	repoB, err := config.RepoFromPath(repoBPath)
	if err != nil {
		t.Fatalf("RepoFromPath(B): %v", err)
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Use the #1636 title pair so this test exercises the SLUG reservation and
	// nothing else: "My_App" and "MyApp" derive DISTINCT git branches (Slugify
	// drops the underscore, branch sanitization keeps it as a dash) so the branch
	// check cannot fire, but both slugify to the hook name "myapp".
	const reservedTitle = "My_App"
	const newTitle = "MyApp"
	if manager.titlesCollide(reservedTitle, newTitle) {
		t.Fatalf("test premise broken: %q and %q collide on branch, so the slug path is unreachable", reservedTitle, newTitle)
	}
	if session.Slugify(reservedTitle) != session.Slugify(newTitle) {
		t.Fatalf("test premise broken: %q and %q must slugify to one hook name", reservedTitle, newTitle)
	}

	manager.mu.Lock()
	// Repo A holds an in-flight remote reservation for the slug "myapp".
	manager.reservedRemoteNames[daemonInstanceKey(repoA.ID, session.Slugify(reservedTitle))] = struct{}{}
	// Same slug, DIFFERENT repo: must be available.
	errOtherRepo := manager.validateTitleAvailableLocked(repoB.ID, repoB.Root, newTitle, "claude", true, false, nil)
	// Same slug, SAME repo: must still be rejected.
	errSameRepo := manager.validateTitleAvailableLocked(repoA.ID, repoA.Root, newTitle, "claude", true, false, nil)
	manager.mu.Unlock()

	if errOtherRepo != nil {
		t.Fatalf("remote hook slug %q in a different repo must be allowed, got: %v", session.Slugify(newTitle), errOtherRepo)
	}
	if errSameRepo == nil {
		t.Fatalf("remote hook slug %q in the SAME repo must still be rejected", session.Slugify(newTitle))
	}
	if !strings.Contains(errSameRepo.Error(), "hook name") {
		t.Errorf("same-repo rejection should name the hook-name collision, got: %v", errSameRepo)
	}
}

// TestCreateSessionStillRejectsSameTitleInSameRepo guards the other side of the
// change: per-repo uniqueness is still uniqueness.
func TestCreateSessionStillRejectsSameTitleInSameRepo(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	repoPath := setupControlRepo(t)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	const title = "foo"
	if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title: title, RepoPath: repoPath, Program: "claude",
	}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err = manager.CreateSession(context.Background(), CreateSessionRequest{
		Title: title, RepoPath: repoPath, Program: "claude",
	})
	if err == nil {
		t.Fatalf("duplicate title in the SAME repo must still be rejected")
	}
}

// TestFindSessionReportsAmbiguousTitleAcrossRepos pins the resolution contract:
// with no repo scope and the title held in several repos, the daemon must NOT
// pick one (the map walk made the winner nondeterministic) — it must name the
// candidate repos so the caller can disambiguate with --repo.
func TestFindSessionReportsAmbiguousTitleAcrossRepos(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)

	repoAPath := setupControlRepo(t)
	repoBPath := setupControlRepo(t)
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	const title = "foo"
	for _, p := range []string{repoAPath, repoBPath} {
		if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
			Title: title, RepoPath: p, Program: "claude",
		}); err != nil {
			t.Fatalf("create %q in %s: %v", title, p, err)
		}
	}

	// Unscoped lookup -> ambiguous.
	_, _, _, err = manager.findSession(title, "")
	if err == nil {
		t.Fatalf("unscoped lookup of a title held by two repos must be ambiguous, not resolved")
	}
	if !errors.Is(err, session.ErrAmbiguousTitle) {
		t.Fatalf("expected ErrAmbiguousTitle, got: %v", err)
	}
	for _, want := range []string{repoAPath, repoBPath} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ambiguity error must name repo %q so the user can pick one, got: %v", want, err)
		}
	}

	// Scoped lookup -> resolves to that repo's session.
	repoB, err := config.RepoFromPath(repoBPath)
	if err != nil {
		t.Fatalf("RepoFromPath(B): %v", err)
	}
	inst, rid, _, err := manager.findSession(title, repoB.ID)
	if err != nil {
		t.Fatalf("--repo must disambiguate, got: %v", err)
	}
	if rid != repoB.ID {
		t.Errorf("scoped lookup resolved the wrong repo: got %q want %q", rid, repoB.ID)
	}
	if inst == nil || inst.Path != repoBPath {
		t.Errorf("scoped lookup must return repo B's session, got path %v", inst)
	}
}

// TestFindSessionResolvesUniqueTitleUnscoped keeps the bare-title convenience:
// a title held by exactly ONE session anywhere still resolves with no --repo.
func TestFindSessionResolvesUniqueTitleUnscoped(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)

	repoAPath := setupControlRepo(t)
	repoBPath := setupControlRepo(t)
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Two repos, DIFFERENT titles.
	if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title: "only-in-a", RepoPath: repoAPath, Program: "claude",
	}); err != nil {
		t.Fatalf("create in A: %v", err)
	}
	if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title: "only-in-b", RepoPath: repoBPath, Program: "claude",
	}); err != nil {
		t.Fatalf("create in B: %v", err)
	}

	inst, _, _, err := manager.findSession("only-in-b", "")
	if err != nil {
		t.Fatalf("a globally unique title must still resolve without --repo, got: %v", err)
	}
	if inst == nil || inst.Path != repoBPath {
		t.Errorf("resolved the wrong session: %v", inst)
	}
}

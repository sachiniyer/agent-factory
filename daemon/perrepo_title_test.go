package daemon

import (
	"context"
	"encoding/json"
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

// Remote HOOK names are the one namespace that stays global while titles go
// per-repo: launch_cmd/delete_cmd receive `--name <slug>` verbatim with no repo
// component, so the slug is what external provisioners tag and reap real
// VMs/containers by. Two repos handing scripts the same name would clobber one
// sandbox and let either delete reap the other's.
//
// TestHookNameNamespaceIsGlobalAcrossRepos pins that, including the hole this
// closes: the in-flight reservation map is populated at reserve and dropped at
// release, so it only ever blocked CONCURRENT creates. The live/disk slug scans
// were repo-filtered, which let two repos take the same hook name SEQUENTIALLY
// and hand both sandboxes the identical --name.
func TestHookNameNamespaceIsGlobalAcrossRepos(t *testing.T) {
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

	// Use the #1636 title pair so this exercises the SLUG path and nothing else:
	// "My_App" and "MyApp" derive DISTINCT git branches (Slugify drops the
	// underscore, branch sanitization keeps it as a dash) so the branch check
	// cannot fire, but both slugify to the hook name "myapp".
	const existingTitle = "My_App"
	const newTitle = "MyApp"
	if manager.titlesCollide(existingTitle, newTitle) {
		t.Fatalf("test premise broken: %q and %q collide on branch, so the slug path is unreachable", existingTitle, newTitle)
	}
	if session.Slugify(existingTitle) != session.Slugify(newTitle) {
		t.Fatalf("test premise broken: %q and %q must slugify to one hook name", existingTitle, newTitle)
	}

	t.Run("concurrent create in another repo is refused", func(t *testing.T) {
		manager.mu.Lock()
		manager.reservedRemoteNames[session.Slugify(existingTitle)] = struct{}{}
		err := manager.validateTitleAvailableLocked(repoB.ID, repoB.Root, newTitle, "claude", true, false, nil)
		delete(manager.reservedRemoteNames, session.Slugify(existingTitle))
		manager.mu.Unlock()
		if err == nil {
			t.Fatalf("an in-flight hook name in another repo must block the create")
		}
		if !strings.Contains(err.Error(), "hook name") {
			t.Errorf("rejection should name the hook-name clash, got: %v", err)
		}
	})

	t.Run("settled live session in another repo is refused", func(t *testing.T) {
		inst, err := session.NewInstance(session.InstanceOptions{Title: existingTitle, Path: repoAPath, Program: "claude"})
		if err != nil {
			t.Fatalf("NewInstance: %v", err)
		}
		inst.SetBackend(remoteTypeBackend{session.NewFakeBackend()})
		inst.SetStartedForTest(true)
		manager.mu.Lock()
		manager.instances[daemonInstanceKey(repoA.ID, existingTitle)] = inst
		// No reservation entry — the create in repo A has SETTLED. This is the
		// sequential case the repo-filtered scan used to let through.
		err = manager.validateTitleAvailableLocked(repoB.ID, repoB.Root, newTitle, "claude", true, false, nil)
		delete(manager.instances, daemonInstanceKey(repoA.ID, existingTitle))
		manager.mu.Unlock()
		if err == nil {
			t.Fatalf("a settled hook session in another repo must block the name: both sandboxes would get --name %q", session.Slugify(newTitle))
		}
		if !strings.Contains(err.Error(), "hook name") {
			t.Errorf("rejection should name the hook-name clash, got: %v", err)
		}
	})

	t.Run("settled PERSISTED session in another repo is refused", func(t *testing.T) {
		// Repo A's hook session exists only on disk (daemon restarted, its
		// worktree/tmux gone so refresh could not restore it).
		rows, err := json.Marshal([]session.InstanceData{{
			Title: existingTitle, Path: repoAPath, Program: "claude", BackendType: "remote",
		}})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := config.SaveRepoInstances(repoA.ID, rows); err != nil {
			t.Fatalf("save: %v", err)
		}
		t.Cleanup(func() { _ = config.SaveRepoInstances(repoA.ID, json.RawMessage("[]")) })

		manager.mu.Lock()
		err = manager.validateTitleAvailableLocked(repoB.ID, repoB.Root, newTitle, "claude", true, false, nil)
		manager.mu.Unlock()
		if err == nil {
			t.Fatalf("a persisted hook session in another repo must block the name")
		}
		if !strings.Contains(err.Error(), "hook name") || !strings.Contains(err.Error(), repoAPath) {
			t.Errorf("rejection should name the clash and the owning project, got: %v", err)
		}
	})

	// ForceRemote is only ONE of three ways onto the hook backend: `--backend
	// hook` and a repo's `backend = "hook"` config both get there with
	// ForceRemote false. Gating the hook-name checks on ForceRemote alone let
	// those creates hand launch_cmd/delete_cmd a colliding --name.
	t.Run("--backend hook create is checked even with ForceRemote false", func(t *testing.T) {
		// Repo A's hook session is seeded on DISK: this goes through the full
		// reserveCreate (which refreshes first), and refresh only preserves an
		// in-memory instance while its repo directory is absent — an on-disk row
		// is the durable way to express "repo A already owns this hook name".
		rows, err := json.Marshal([]session.InstanceData{{
			Title: existingTitle, Path: repoAPath, Program: "claude", BackendType: "remote",
		}})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := config.SaveRepoInstances(repoA.ID, rows); err != nil {
			t.Fatalf("save: %v", err)
		}
		t.Cleanup(func() { _ = config.SaveRepoInstances(repoA.ID, json.RawMessage("[]")) })

		// Note ForceRemote is FALSE — the hook backend is selected by --backend.
		_, _, release, _, err := manager.reserveCreate(CreateSessionRequest{
			Title:       newTitle,
			RepoPath:    repoBPath,
			Program:     "claude",
			Backend:     string(session.BackendHook),
			ForceRemote: false,
		})
		if err == nil {
			release()
			t.Fatalf("a --backend hook create must run the hook-name checks: both sandboxes would get --name %q", session.Slugify(newTitle))
		}
		if !strings.Contains(err.Error(), "hook name") {
			t.Errorf("rejection should name the hook-name clash, got: %v", err)
		}
	})

	t.Run("a free hook name is still allowed", func(t *testing.T) {
		manager.mu.Lock()
		err := manager.validateTitleAvailableLocked(repoB.ID, repoB.Root, "totally-unused", "claude", true, false, nil)
		manager.mu.Unlock()
		if err != nil {
			t.Fatalf("an unused hook name must be available: %v", err)
		}
	})
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

// TestNextAvailableTitlePropagatesFatalHookCheckError guards the difference
// between "this candidate is taken" and "the check could not run". The cross-repo
// hook-name scan surfaces a corrupted instances.json as an error, and
// nextAvailableTitleLocked walks candidates treating any error as taken — so
// without the errTitleCheckFatal marker a title_base create would burn all 10,000
// suffixes under the manager lock and then report a misleading "could not find an
// available title", swallowing the actionable corruption message.
func TestNextAvailableTitlePropagatesFatalHookCheckError(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoBPath := setupControlRepo(t)
	repoB, err := config.RepoFromPath(repoBPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Another repo's instances.json is corrupted, so the hook-name scan cannot
	// prove the slug is free.
	if err := config.SaveRepoInstances("corrupt-repo", json.RawMessage("{not json")); err != nil {
		t.Fatalf("save: %v", err)
	}

	manager.mu.Lock()
	got, err := manager.nextAvailableTitleLocked(repoB.ID, repoB.Root, "base", "claude", true, nil)
	manager.mu.Unlock()

	if err == nil {
		t.Fatalf("an unverifiable hook name must not resolve to a suffixed title (%q)", got)
	}
	if !errors.Is(err, errTitleCheckFatal) {
		t.Fatalf("expected the fatal check error to propagate, got: %v", err)
	}
	if strings.Contains(err.Error(), "could not find an available title") {
		t.Errorf("the corruption cause must not be swallowed as exhaustion: %v", err)
	}
	if !strings.Contains(err.Error(), "corrupt-repo") {
		t.Errorf("error should name the corrupted repo so it is actionable, got: %v", err)
	}
}

// TestFindSessionAmbiguitySurvivesPartialRestore closes the gap where ONE live
// match looked like proof of uniqueness. A repo's row is skipped during refresh
// when it cannot be restored (worktree/tmux gone), so it never reaches
// m.instances — and an unscoped kill/archive would then act on the restored repo
// while the daemon-down disk path would correctly refuse to guess. The in-memory
// match must be unioned with the persisted rows before resolving.
func TestFindSessionAmbiguitySurvivesPartialRestore(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)

	repoAPath := setupControlRepo(t)
	repoBPath := setupControlRepo(t)
	repoB, err := config.RepoFromPath(repoBPath)
	if err != nil {
		t.Fatalf("RepoFromPath(B): %v", err)
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	const title = "foo"
	// Repo A's session is live and in memory.
	if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title: title, RepoPath: repoAPath, Program: "claude",
	}); err != nil {
		t.Fatalf("create in A: %v", err)
	}
	// Repo B holds the SAME title, but only on disk — as if its row could not be
	// restored during refresh. It is deliberately never put into m.instances.
	rows, err := json.Marshal([]session.InstanceData{{Title: title, Path: repoBPath, Program: "claude"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := config.SaveRepoInstances(repoB.ID, rows); err != nil {
		t.Fatalf("save B: %v", err)
	}

	_, _, _, err = manager.findSession(title, "")
	if err == nil {
		t.Fatalf("one live match must not resolve while another repo's row holds the title on disk")
	}
	if !errors.Is(err, session.ErrAmbiguousTitle) {
		t.Fatalf("expected ErrAmbiguousTitle, got: %v", err)
	}
	for _, want := range []string{repoAPath, repoBPath} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ambiguity error must name repo %q, got: %v", want, err)
		}
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

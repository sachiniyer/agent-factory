package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// seedUnreadableRepoHolding writes rows for repoID and then makes that repo's
// instances.json genuinely unreadable, returning the path. The rows are written
// first so the fixture models a repo that really does hold the title — the read
// failure is what hides it, not an empty file.
func seedUnreadableRepoHolding(t *testing.T, repoID, title, repoPath string) string {
	t.Helper()
	rows, err := json.Marshal([]session.InstanceData{{Title: title, Path: repoPath, Program: "claude"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := config.SaveRepoInstances(repoID, rows); err != nil {
		t.Fatalf("save %s: %v", repoID, err)
	}
	path, err := config.RepoInstancesPath(repoID)
	if err != nil {
		t.Fatalf("RepoInstancesPath: %v", err)
	}
	makeInstancesUnreadable(t, path)
	return path
}

func assertNamesFileAndCause(t *testing.T, err error, path string) {
	t.Helper()
	if !strings.Contains(err.Error(), path) {
		t.Errorf("refusal must name the unreadable file %s so it can be repaired, got: %v", path, err)
	}
	if !strings.Contains(err.Error(), "is a directory") && !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("refusal must carry the underlying I/O error, got: %v", err)
	}
}

// TestCollectTitleRepoPathsOnDiskKeepsReadableMatchesBesideTheGap is the
// counterpart to the refusal above, and the one the first cut of this PR got
// wrong: refusing EARLY threw away the matches the readable repos did yield.
//
// That is strictly worse than the fail-open it replaced. The caller unions this
// set with its live match to DETECT ambiguity, so discarding a readable second
// project's row removes a positive finding — and the caller then resolves the
// live match anyway. A gap in the evidence must be reported alongside the
// evidence, never instead of it.
func TestCollectTitleRepoPathsOnDiskKeepsReadableMatchesBesideTheGap(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	rows, err := json.Marshal([]session.InstanceData{{Title: "foo", Path: "/repos/visible", Program: "claude"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := config.SaveRepoInstances("visible-repo", rows); err != nil {
		t.Fatalf("save: %v", err)
	}
	path := seedUnreadableRepoHolding(t, "blocked-repo", "foo", "/repos/blocked")

	found, gaps, err := collectTitleRepoPathsOnDisk("foo")
	if err != nil {
		t.Fatalf("a per-repo read failure is a gap to report, not a reason to abandon the scan: %v", err)
	}
	if got := found["visible-repo"]; got != "/repos/visible" {
		t.Errorf("the readable repo's match must survive the gap; found = %v", found)
	}
	if len(gaps) != 1 || !strings.Contains(config.DescribeRepoInstancesSkips(gaps), path) {
		t.Errorf("the unreadable file must still be reported so the caller can refuse on it; gaps = %v", gaps)
	}
}

// TestFindInstanceDataByTitleRefusesUnscopedWhenARepoIsUnreadable is the
// destructive-resolution case. An unscoped lookup returns (row, repoID) and the
// caller treats that repoID as THE project — so "exactly one repo holds this
// title" is an absence claim about every other repo, and an unread file is
// precisely the evidence that would refute it.
func TestFindInstanceDataByTitleRefusesUnscopedWhenARepoIsUnreadable(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	rows, err := json.Marshal([]session.InstanceData{{Title: "foo", Path: "/repos/visible", Program: "claude"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := config.SaveRepoInstances("visible-repo", rows); err != nil {
		t.Fatalf("save: %v", err)
	}
	path := seedUnreadableRepoHolding(t, "blocked-repo", "foo", "/repos/blocked")

	data, rid, err := findInstanceDataByTitle("foo", "")
	if err == nil {
		t.Fatalf("an unscoped lookup resolved %q to repo %s (path %s) while another project's record could not be read", "foo", rid, data.Path)
	}
	assertNamesFileAndCause(t, err, path)

	// The refusal must stay escapable: --repo skips the cross-repo scan
	// entirely, so a scoped lookup keeps working with the same file unreadable.
	data, rid, err = findInstanceDataByTitle("foo", "visible-repo")
	if err != nil {
		t.Fatalf("--repo must remain a working escape hatch while another project's record is unreadable: %v", err)
	}
	if rid != "visible-repo" || data.Title != "foo" {
		t.Fatalf("scoped lookup returned the wrong row: rid=%q data=%+v", rid, data)
	}
}

// TestDaemonLoadGateRefusesUnreadableRepoBeforeAnyTitleResolution pins WHY the
// two refusals above are defence in depth rather than live-bug fixes, and keeps
// them from quietly becoming live bugs later.
//
// Every daemon path that reaches those helpers goes through findSession ->
// refreshLocked -> refreshDaemonInstances, which calls
// MigrateAllRepoInstancesForDaemonLoad FIRST — and that already refuses hard on
// a per-repo file it cannot read. So today the daemon never resolves a title
// while a record is unreadable; it errors earlier, naming the file. If that gate
// is ever relaxed to skip unreadable repos the way the loader does, this test
// goes red and the helpers behind it are already correct.
func TestDaemonLoadGateRefusesUnreadableRepoBeforeAnyTitleResolution(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installInstantBackend(t)
	repoAPath := setupControlRepo(t)

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
	repoA, err := config.RepoFromPath(repoAPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}

	// A second project holding the same title, on disk only, unreadable.
	path := seedUnreadableRepoHolding(t, "blocked-repo", title, "/repos/blocked")

	// Unscoped: must not resolve the one match it can see.
	if inst, rid, _, err := manager.findSession(title, ""); err == nil {
		t.Fatalf("unscoped lookup resolved %q to repo %s while a project record could not be read (inst=%v)", title, rid, inst != nil)
	} else {
		assertNamesFileAndCause(t, err, path)
	}

	// Scoped too: the refusal here comes from the LOAD gate, which runs before
	// any scoping, so --repo is not an escape from this particular one. That is
	// the daemon's existing posture, recorded rather than changed.
	if _, _, _, err := manager.findSession(title, repoA.ID); err == nil {
		t.Fatalf("the daemon load gate must refuse a scoped lookup too while a record is unreadable")
	} else {
		assertNamesFileAndCause(t, err, path)
	}
}

// withCollectTitleRepoPaths swaps findSession's disk-side ambiguity probe for
// the duration of one test. The daemon's load gate refuses an unreadable record
// before findSession runs, so the gap branch cannot be reached from a fixture;
// driving the seam is the only way to test the resolution logic that sits on top
// of it, and that logic is where the identity decision is actually made.
func withCollectTitleRepoPaths(t *testing.T, fn func(string) (map[string]string, []config.RepoInstancesSkip, error)) {
	t.Helper()
	prev := collectTitleRepoPaths
	collectTitleRepoPaths = fn
	t.Cleanup(func() { collectTitleRepoPaths = prev })
}

// managerWithLiveSession returns a manager holding exactly one live session, so
// findSession's unscoped branch reaches the one-live-match path where the
// cross-project ambiguity guard runs.
func managerWithLiveSession(t *testing.T, title string) (*Manager, string, string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installInstantBackend(t)
	repoPath := setupControlRepo(t)
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title: title, RepoPath: repoPath, Program: "claude",
	}); err != nil {
		t.Fatalf("create %q: %v", title, err)
	}
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	return manager, repoPath, repo.ID
}

// TestFindSessionPrefersDetectedAmbiguityOverTheGap keeps the gap refusal from
// swallowing a better answer. A collision the readable repos DID show is
// definitive — no unread file can make a title that is provably held twice
// unique again — so it must be reported as ambiguity, naming both projects,
// rather than downgraded to "could not verify".
func TestFindSessionPrefersDetectedAmbiguityOverTheGap(t *testing.T) {
	manager, _, _ := managerWithLiveSession(t, "foo")
	withCollectTitleRepoPaths(t, func(string) (map[string]string, []config.RepoInstancesSkip, error) {
		return map[string]string{"other-repo": "/repos/other"},
			[]config.RepoInstancesSkip{{RepoID: "blocked", Path: "/af/instances/blocked/instances.json", Err: os.ErrPermission}},
			nil
	})

	_, _, _, err := manager.findSession("foo", "")
	if err == nil {
		t.Fatalf("a title held by two projects must not resolve")
	}
	if !errors.Is(err, session.ErrAmbiguousTitle) {
		t.Fatalf("a collision the readable repos showed is definitive and must surface as ambiguity, got: %v", err)
	}
	if !strings.Contains(err.Error(), "/repos/other") {
		t.Errorf("ambiguity error must name the other project so --repo can disambiguate, got: %v", err)
	}
}

// TestFindSessionRefusesWhenUniquenessIsUnprovenByAGap is the fail-closed case:
// nothing readable holds the title twice, but the check did not see everything,
// so uniqueness is unproven rather than established. Resolving here is what an
// unscoped kill or archive would act on.
func TestFindSessionRefusesWhenUniquenessIsUnprovenByAGap(t *testing.T) {
	manager, _, _ := managerWithLiveSession(t, "foo")
	const blockedPath = "/af/instances/blocked/instances.json"
	withCollectTitleRepoPaths(t, func(string) (map[string]string, []config.RepoInstancesSkip, error) {
		return map[string]string{},
			[]config.RepoInstancesSkip{{RepoID: "blocked", Path: blockedPath, Err: os.ErrPermission}},
			nil
	})

	inst, rid, _, err := manager.findSession("foo", "")
	if err == nil {
		t.Fatalf("unscoped lookup resolved %q to repo %s though a project record could not be read (inst=%v)", "foo", rid, inst != nil)
	}
	if !strings.Contains(err.Error(), blockedPath) {
		t.Errorf("refusal must name the unreadable file so it can be repaired, got: %v", err)
	}
	if !strings.Contains(err.Error(), "--repo") {
		t.Errorf("refusal must point at the escape hatch that still works, got: %v", err)
	}
}

// TestFindSessionRefusesWhenAmbiguityCheckCannotEnumerate covers the remaining
// hole: failing to enumerate repos AT ALL is strictly less evidence than one
// unreadable file, so it cannot keep failing open while that case refuses.
func TestFindSessionRefusesWhenAmbiguityCheckCannotEnumerate(t *testing.T) {
	manager, _, _ := managerWithLiveSession(t, "foo")
	withCollectTitleRepoPaths(t, func(string) (map[string]string, []config.RepoInstancesSkip, error) {
		return nil, nil, errors.New("instances directory unreadable")
	})

	inst, rid, _, err := manager.findSession("foo", "")
	if err == nil {
		t.Fatalf("unscoped lookup resolved %q to repo %s though the ambiguity check could not run at all (inst=%v)", "foo", rid, inst != nil)
	}
	if !strings.Contains(err.Error(), "instances directory unreadable") {
		t.Errorf("refusal must carry the underlying cause, got: %v", err)
	}
}

// TestFindSessionResolvesWhenTheCheckIsCompleteAndClean is the non-regression
// floor: a complete check that finds no second project still resolves.
func TestFindSessionResolvesWhenTheCheckIsCompleteAndClean(t *testing.T) {
	manager, repoPath, _ := managerWithLiveSession(t, "foo")
	withCollectTitleRepoPaths(t, func(string) (map[string]string, []config.RepoInstancesSkip, error) {
		return map[string]string{}, nil, nil
	})

	inst, _, _, err := manager.findSession("foo", "")
	if err != nil {
		t.Fatalf("a complete, clean ambiguity check must resolve the live match: %v", err)
	}
	if inst == nil || inst.Path != repoPath {
		t.Fatalf("resolved the wrong session: %v", inst)
	}
}

// TestFindSessionIgnoresAGapInTheMatchedProject is the false-refusal boundary.
// Ambiguity is defined across DISTINCT repo IDs, and the live match already
// establishes the matched project's identity — so an unreadable file belonging
// to that same project cannot conceal a second project and must not refuse an
// operation it cannot affect. The same exclusion hookSlugOwnerInOtherRepos makes
// for its own repo (#3476).
func TestFindSessionIgnoresAGapInTheMatchedProject(t *testing.T) {
	manager, repoPath, repoID := managerWithLiveSession(t, "foo")
	withCollectTitleRepoPaths(t, func(string) (map[string]string, []config.RepoInstancesSkip, error) {
		return map[string]string{},
			[]config.RepoInstancesSkip{{RepoID: repoID, Path: "/af/instances/self/instances.json", Err: os.ErrPermission}},
			nil
	})

	inst, gotID, _, err := manager.findSession("foo", "")
	if err != nil {
		t.Fatalf("a gap in the matched project cannot hide a SECOND project, so it must not refuse: %v", err)
	}
	if inst == nil || inst.Path != repoPath || gotID != repoID {
		t.Fatalf("resolved the wrong session: inst=%v rid=%q", inst, gotID)
	}
}

// TestFindSessionStillRefusesAGapInAnotherProject guards the other side of that
// filter: excluding the matched project must not swallow a gap anywhere else.
func TestFindSessionStillRefusesAGapInAnotherProject(t *testing.T) {
	manager, _, repoID := managerWithLiveSession(t, "foo")
	const otherPath = "/af/instances/other/instances.json"
	withCollectTitleRepoPaths(t, func(string) (map[string]string, []config.RepoInstancesSkip, error) {
		return map[string]string{},
			[]config.RepoInstancesSkip{
				{RepoID: repoID, Path: "/af/instances/self/instances.json", Err: os.ErrPermission},
				{RepoID: "elsewhere", Path: otherPath, Err: os.ErrPermission},
			},
			nil
	})

	if _, _, _, err := manager.findSession("foo", ""); err == nil {
		t.Fatalf("a gap in ANOTHER project still leaves uniqueness unproven and must refuse")
	} else if !strings.Contains(err.Error(), otherPath) {
		t.Errorf("refusal must name the other project's unreadable file, got: %v", err)
	}
}

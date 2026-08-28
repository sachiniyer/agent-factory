package daemon

import (
	"context"
	"encoding/json"
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

// TestCollectTitleRepoPathsOnDiskRefusesUnreadableRepo pins the helper that
// backs the daemon's cross-project ambiguity guard. Its whole job is to widen a
// lone in-memory match with the persisted rows a partial restore hid, so an
// answer assembled from a partial READ is the one thing it must never return:
// the caller reads a short list as "no other project holds this title".
func TestCollectTitleRepoPathsOnDiskRefusesUnreadableRepo(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	path := seedUnreadableRepoHolding(t, "blocked-repo", "foo", "/repos/blocked")

	got, err := collectTitleRepoPathsOnDisk("foo")
	if err == nil {
		t.Fatalf("the ambiguity guard must not report a set it could not finish reading (got %v)", got)
	}
	assertNamesFileAndCause(t, err, path)
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

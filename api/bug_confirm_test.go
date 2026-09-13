package api

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// These tests pin the bug fix for "CLI: `af sessions create` incorrectly
// blocks reusing a title from an archived session". The client-side pre-check
// repoHasInstanceTitle (api/api.go) used a naive Title==title match with no
// liveness filter, so it reported an archived session as "already exists",
// aborting creates the daemon would allow (the archived-name-reuse feature,
// renameArchivedForReuseLocked). The fix skips archived rows so the create
// reaches the daemon, which performs its own race-safe refusal or reclaim.

func TestRepoHasInstanceTitle_ArchivedRowSkippedForReuse(t *testing.T) {
	tmp := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", tmp)
	repoID := "repo-archived"
	raw, err := json.Marshal([]session.InstanceData{
		{Title: "foo", Liveness: session.LiveArchived, Path: "/tmp/repo"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := config.SaveRepoInstances(repoID, raw); err != nil {
		t.Fatalf("save: %v", err)
	}

	exists, err := repoHasInstanceTitle(repoID, "foo")
	if err != nil {
		t.Fatalf("repoHasInstanceTitle: %v", err)
	}
	if exists {
		t.Fatalf("repoHasInstanceTitle reports archived row %q as existing; the CLI create pre-check must skip archived rows so the daemon can rename the archived row aside and allow reuse", "foo")
	}
}

func TestRepoHasInstanceTitle_LegacyArchivedRowSkippedForReuse(t *testing.T) {
	// A row persisted before #1195 added the Liveness field carries only the
	// legacy Status integer. RecordedLiveness must still resolve it as archived
	// so the pre-check skips it, matching the daemon's own behavior.
	tmp := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", tmp)
	repoID := "repo-legacy-archived"
	raw, err := json.Marshal([]session.InstanceData{
		{Title: "bar", Status: session.Archived, Path: "/tmp/repo"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := config.SaveRepoInstances(repoID, raw); err != nil {
		t.Fatalf("save: %v", err)
	}

	exists, err := repoHasInstanceTitle(repoID, "bar")
	if err != nil {
		t.Fatalf("repoHasInstanceTitle: %v", err)
	}
	if exists {
		t.Fatalf("repoHasInstanceTitle reports legacy archived row (Status=Archived, no Liveness) %q as existing; RecordedLiveness must resolve the legacy status to LiveArchived and skip it", "bar")
	}
}

func TestRepoHasInstanceTitle_MixedArchivedAndLiveSameTitle(t *testing.T) {
	// When both an archived and a live row hold the same title, the live row is
	// a genuine collision the daemon would refuse. The pre-check must report it
	// rather than skipping it because one of the rows is archived.
	tmp := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", tmp)
	repoID := "repo-mixed"
	raw, err := json.Marshal([]session.InstanceData{
		{Title: "dup", Liveness: session.LiveArchived, Path: "/tmp/repo"},
		{Title: "dup", Liveness: session.LiveReady, Path: "/tmp/repo"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := config.SaveRepoInstances(repoID, raw); err != nil {
		t.Fatalf("save: %v", err)
	}
	exists, err := repoHasInstanceTitle(repoID, "dup")
	if err != nil {
		t.Fatalf("repoHasInstanceTitle: %v", err)
	}
	if !exists {
		t.Fatalf("repoHasInstanceTitle must report a title held by both an archived and a live row; the live row is a real collision")
	}
}

func TestSessionsCreate_ArchivedRowReachesDaemon(t *testing.T) {
	// End-to-end: `af sessions create <title>` over a title held only by an
	// archived session must NOT abort at the pre-check, and must reach the
	// daemon so it can rename the archived row aside and allow reuse.
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	silenceStdio(t)

	repoRoot := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoRoot, 0755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	if out, err := exec.Command("git", "init", repoRoot).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	repo, err := config.RepoFromPath(repoRoot)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}

	archivedRow, err := json.Marshal([]session.InstanceData{
		{Title: "foo", Liveness: session.LiveArchived, Path: repoRoot},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := config.SaveRepoInstances(repo.ID, archivedRow); err != nil {
		t.Fatalf("save: %v", err)
	}

	daemonReached := false
	prevCreate := createSessionViaDaemon
	createSessionViaDaemon = func(req daemon.CreateSessionRequest) (*session.InstanceData, error) {
		daemonReached = true
		return &session.InstanceData{Title: req.Title}, nil
	}
	t.Cleanup(func() { createSessionViaDaemon = prevCreate })

	setSessionsCreateFlags(t, "", repoRoot, false, false)
	if err := sessionsCreateCmd.RunE(sessionsCreateCmd, []string{"foo"}); err != nil {
		t.Fatalf("archived-row create must reach the daemon and succeed, not abort at the pre-check: %v", err)
	}
	if !daemonReached {
		t.Fatal("archived-row create never reached the daemon; the pre-check must skip archived rows and let the round trip happen")
	}
}

func TestSessionsCreate_LiveRowStillAbortsBeforeDaemon(t *testing.T) {
	// No regression: a title held by a live session must still abort at the
	// pre-check with "already exists" and never reach the daemon.
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	silenceStdio(t)

	repoRoot := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoRoot, 0755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	if out, err := exec.Command("git", "init", repoRoot).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	repo, err := config.RepoFromPath(repoRoot)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}

	liveRow, err := json.Marshal([]session.InstanceData{
		{Title: "foo", Liveness: session.LiveReady, Path: repoRoot},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := config.SaveRepoInstances(repo.ID, liveRow); err != nil {
		t.Fatalf("save: %v", err)
	}

	daemonReached := false
	prevCreate := createSessionViaDaemon
	createSessionViaDaemon = func(req daemon.CreateSessionRequest) (*session.InstanceData, error) {
		daemonReached = true
		return &session.InstanceData{Title: req.Title}, nil
	}
	t.Cleanup(func() { createSessionViaDaemon = prevCreate })

	setSessionsCreateFlags(t, "", repoRoot, false, false)
	err = sessionsCreateCmd.RunE(sessionsCreateCmd, []string{"foo"})
	if err == nil {
		t.Fatal("live-row create must abort at the pre-check, not succeed")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected 'already exists' error, got: %v", err)
	}
	if daemonReached {
		t.Fatal("live-row create reached the daemon; the pre-check must abort a genuine live duplicate before the round trip")
	}
}

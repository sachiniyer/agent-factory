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

// These tests pin the Loading-ghost carve-out in repoHasLiveInstanceTitle, the
// sessions-create pre-check. The skip-set must mirror the daemon's authoritative
// findTitleConflictLocked: skip Status==Loading ghost rows (a legacy TUI #551
// artifact the daemon treats as overwritable) in addition to archived rows, but
// NOT Deleting rows (a persisted Deleting row is a real titleConflictDisk
// collision to the daemon). RecordedLiveness resolves a Loading row to
// LivenessUnset, not LiveArchived, so the archived carve-out alone never covered
// Loading; this separate skip is load-bearing.

// TestRepoHasLiveInstanceTitle_LoadingGhostSkippedForReuse pins the fix at the
// helper level: a persisted Status==Loading ghost must NOT be counted as a live
// collision, matching the daemon's findTitleConflictLocked which skips Loading
// rows so they do not "block title reuse forever".
func TestRepoHasLiveInstanceTitle_LoadingGhostSkippedForReuse(t *testing.T) {
	tmp := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", tmp)
	repoID := "repo-loading-ghost"
	raw, err := json.Marshal([]session.InstanceData{
		{Title: "ghost", Status: session.Loading, Path: "/tmp/repo"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := config.SaveRepoInstances(repoID, raw); err != nil {
		t.Fatalf("save: %v", err)
	}

	exists, err := repoHasLiveInstanceTitle(repoID, "ghost")
	if err != nil {
		t.Fatalf("repoHasLiveInstanceTitle: %v", err)
	}
	if exists {
		t.Fatalf("repoHasLiveInstanceTitle reports Loading ghost %q as existing; the pre-check must skip Loading rows to mirror the daemon's findTitleConflictLocked ghost skip", "ghost")
	}
}

// TestRepoHasLiveInstanceTitle_DeletingRowStillAborts pins the asymmetry the fix
// is explicit about: a persisted Deleting row is a real titleConflictDisk
// collision to the daemon (findTitleConflictLocked skips Loading but NOT
// Deleting), so the pre-check must keep counting it. Letting it through would
// hand the daemon a create it would then refuse, so this guards against a
// future "skip all transient statuses" widening.
func TestRepoHasLiveInstanceTitle_DeletingRowStillAborts(t *testing.T) {
	tmp := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", tmp)
	repoID := "repo-deleting"
	raw, err := json.Marshal([]session.InstanceData{
		{Title: "going", Status: session.Deleting, Path: "/tmp/repo"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := config.SaveRepoInstances(repoID, raw); err != nil {
		t.Fatalf("save: %v", err)
	}

	exists, err := repoHasLiveInstanceTitle(repoID, "going")
	if err != nil {
		t.Fatalf("repoHasLiveInstanceTitle: %v", err)
	}
	if !exists {
		t.Fatalf("repoHasLiveInstanceTitle must report a Deleting row %q as existing; the daemon treats a persisted Deleting row as a real titleConflictDisk collision, so the CLI must not skip it", "going")
	}
}

// TestSessionsCreate_LoadingGhostReachesDaemon is the end-to-end regression for
// the bug: `af sessions create <title>` over a title held only by a Loading
// ghost must NOT abort at the pre-check and must reach the daemon (whose
// findTitleConflictLocked skips the ghost and whose appendInstanceData overwrites
// it on the next save, reaping it). Mirrors bug_confirm_test.go's archived-row
// test.
func TestSessionsCreate_LoadingGhostReachesDaemon(t *testing.T) {
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

	ghostRow, err := json.Marshal([]session.InstanceData{
		{Title: "ghost", Status: session.Loading, Path: repoRoot},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := config.SaveRepoInstances(repo.ID, ghostRow); err != nil {
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
	if err := sessionsCreateCmd.RunE(sessionsCreateCmd, []string{"ghost"}); err != nil {
		t.Fatalf("Loading-ghost create must reach the daemon and succeed, not abort at the pre-check: %v", err)
	}
	if !daemonReached {
		t.Fatal("Loading-ghost create never reached the daemon; the pre-check must skip Loading ghosts and let the round trip happen")
	}
}

// TestSessionsCreate_DeletingRowStillAbortsBeforeDaemon is the end-to-end guard
// for the Deleting carve-out: a title held by a persisted Deleting row is a real
// collision to the daemon, so the create must still abort at the pre-check with
// "already exists" and never reach the daemon. The Loading skip must not widen
// into a Deleting skip at the call site.
func TestSessionsCreate_DeletingRowStillAbortsBeforeDaemon(t *testing.T) {
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

	deletingRow, err := json.Marshal([]session.InstanceData{
		{Title: "going", Status: session.Deleting, Path: repoRoot},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := config.SaveRepoInstances(repo.ID, deletingRow); err != nil {
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
	err = sessionsCreateCmd.RunE(sessionsCreateCmd, []string{"going"})
	if err == nil {
		t.Fatal("Deleting-row create must abort at the pre-check, not succeed")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected 'already exists' error, got: %v", err)
	}
	if daemonReached {
		t.Fatal("Deleting-row create reached the daemon; the pre-check must abort a persisted Deleting row as a genuine live collision before the round trip")
	}
}

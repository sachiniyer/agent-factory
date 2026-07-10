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

// TestSessionsArchive_SurfacesDaemonError: a daemon-side rejection (e.g. remote
// or in-place session) is surfaced as a JSON error, not a silent success.
func TestSessionsArchive_SurfacesDaemonError(t *testing.T) {
	setupRepoForCmd(t)

	prev := archiveSessionViaDaemon
	archiveSessionViaDaemon = func(daemon.ArchiveSessionRequest) (string, error) {
		return "", errors.New("cannot archive remote session")
	}
	defer func() { archiveSessionViaDaemon = prev }()

	_, err := runCmdCaptureStdout(t, sessionsArchiveCmd, []string{"faraway"})
	if err == nil {
		t.Fatal("archive must surface a daemon rejection as an error, not a silent success")
	}
	if !strings.Contains(err.Error(), "cannot archive remote session") {
		t.Fatalf("error = %v, want the daemon's rejection message", err)
	}
}

// stubCurrentTmuxName swaps the tmux-name seam so `--self`/`whoami` tests can
// resolve a session without a real tmux server, restoring it on cleanup.
func stubCurrentTmuxName(t *testing.T, fn func() (string, error)) {
	t.Helper()
	prev := currentTmuxName
	currentTmuxName = fn
	t.Cleanup(func() { currentTmuxName = prev })
}

// TestSessionsArchiveSelf_ResolvesAndArchivesCurrentSession: `archive --self`
// resolves the caller's own session via whoami and archives that title, scoped
// by the resolved session's OWN repo.
func TestSessionsArchiveSelf_ResolvesAndArchivesCurrentSession(t *testing.T) {
	setupRepoForCmd(t)

	const sessionRoot = "/home/agent/src/myrepo"
	stubCurrentTmuxName(t, func() (string, error) { return "af_me_agent", nil })
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return []session.InstanceData{{
			Title:    "me",
			TmuxName: "af_me_agent",
			Worktree: session.GitWorktreeData{RepoPath: sessionRoot},
		}}, nil
	})

	var gotReq daemon.ArchiveSessionRequest
	prev := archiveSessionViaDaemon
	archiveSessionViaDaemon = func(req daemon.ArchiveSessionRequest) (string, error) {
		gotReq = req
		return "/home/u/.agent-factory/archived/" + req.RepoID + "/me", nil
	}
	defer func() { archiveSessionViaDaemon = prev }()

	sessionsArchiveSelf = true
	defer func() { sessionsArchiveSelf = false }()

	out, err := runCmdCaptureStdout(t, sessionsArchiveCmd, nil)
	if err != nil {
		t.Fatalf("archive --self returned error: %v", err)
	}
	if gotReq.Title != "me" {
		t.Fatalf("archive --self resolved Title = %q, want %q (whoami's title)", gotReq.Title, "me")
	}
	if want := config.RepoIDFromRoot(sessionRoot); gotReq.RepoID != want {
		t.Fatalf("archive --self RepoID = %q, want resolved-session repo %q", gotReq.RepoID, want)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output is not JSON (%q): %v", string(out), err)
	}
	if parsed["ok"] != true || parsed["title"] != "me" {
		t.Fatalf("JSON ok/title wrong: %v", parsed)
	}
}

// TestSessionsArchiveSelf_RejectsTitleArg: `--self` and a positional <title>
// are mutually exclusive.
func TestSessionsArchiveSelf_RejectsTitleArg(t *testing.T) {
	setupRepoForCmd(t)

	sessionsArchiveSelf = true
	defer func() { sessionsArchiveSelf = false }()

	_, err := runCmdCaptureStdout(t, sessionsArchiveCmd, []string{"worker"})
	if err == nil {
		t.Fatal("archive --self <title> must error: the two are mutually exclusive")
	}
	if !strings.Contains(err.Error(), "cannot combine --self with a <title>") {
		t.Fatalf("error = %v, want the mutual-exclusion message", err)
	}
}

// TestSessionsArchiveSelf_ScopesByResolvedRepoNotCwd: an agent that has cd'd
// into a DIFFERENT repo must still archive ITS OWN session — scoped by the
// resolved session's repo, never the cwd/--repo repo. Otherwise --self would
// hit a same-titled namesake in the wrong repo or fail "not found" while
// leaving the caller's real session alive.
func TestSessionsArchiveSelf_ScopesByResolvedRepoNotCwd(t *testing.T) {
	cwdRepoID := setupRepoForCmd(t) // repoFlag points at the cwd repo (repo-a)

	// The caller's real session lives in a DIFFERENT repo than the cwd.
	const sessionRoot = "/home/agent/src/other-repo"
	sessionRepoID := config.RepoIDFromRoot(sessionRoot)
	if sessionRepoID == cwdRepoID {
		t.Fatal("test setup: resolved-session repo must differ from cwd repo")
	}

	stubCurrentTmuxName(t, func() (string, error) { return "af_me_agent", nil })
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return []session.InstanceData{{
			Title:    "me",
			TmuxName: "af_me_agent",
			Worktree: session.GitWorktreeData{RepoPath: sessionRoot},
		}}, nil
	})

	var gotReq daemon.ArchiveSessionRequest
	prev := archiveSessionViaDaemon
	archiveSessionViaDaemon = func(req daemon.ArchiveSessionRequest) (string, error) {
		gotReq = req
		return "/archived/me", nil
	}
	defer func() { archiveSessionViaDaemon = prev }()

	sessionsArchiveSelf = true
	defer func() { sessionsArchiveSelf = false }()

	if _, err := runCmdCaptureStdout(t, sessionsArchiveCmd, nil); err != nil {
		t.Fatalf("archive --self returned error: %v", err)
	}
	if gotReq.RepoID != sessionRepoID {
		t.Fatalf("archive --self RepoID = %q, want resolved-session repo %q (NOT cwd repo %q)", gotReq.RepoID, sessionRepoID, cwdRepoID)
	}
	if gotReq.Title != "me" {
		t.Fatalf("archive --self Title = %q, want %q", gotReq.Title, "me")
	}
}

// TestSessionsArchiveSelf_OutsideSessionErrors: `--self` run outside an af
// session surfaces an actionable error instead of archiving nothing.
func TestSessionsArchiveSelf_OutsideSessionErrors(t *testing.T) {
	setupRepoForCmd(t)

	stubCurrentTmuxName(t, func() (string, error) {
		return "", errors.New("not running inside a tmux session")
	})

	sessionsArchiveSelf = true
	defer func() { sessionsArchiveSelf = false }()

	_, err := runCmdCaptureStdout(t, sessionsArchiveCmd, nil)
	if err == nil {
		t.Fatal("archive --self outside a session must error, not silently succeed")
	}
	if !strings.Contains(err.Error(), "--self must be run from inside an af session") {
		t.Fatalf("error = %v, want the actionable outside-a-session message", err)
	}
}

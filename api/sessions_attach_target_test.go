package api

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// These tests pin resolveAttachTarget, the resolution step `sessions attach`
// runs before dialing the daemon's WS PTY stream (#1974).

// clientWithoutTheSession is the shape of the #1974 bug report: a CLIENT machine
// whose cwd is a real git repo (so cwd-scoping would happily produce a repo ID)
// and whose AF home holds no session at all — which is exactly what the client
// looks like when the session lives on the remote daemon's machine. It returns
// the repo root.
func clientWithoutTheSession(t *testing.T) string {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := t.TempDir()
	if err := exec.Command("git", "init", repoRoot).Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	t.Chdir(repoRoot)
	return repoRoot
}

// TestResolveAttachTarget_RemoteDoesNotResolveOnLocalDisk is the #1974
// regression: against --daemon-url, attach resolved the session on the CLIENT's
// disk and failed "session not found" before ever reaching the daemon, so a
// session that exists only on the remote was unattachable from the CLI.
func TestResolveAttachTarget_RemoteDoesNotResolveOnLocalDisk(t *testing.T) {
	clientWithoutTheSession(t)
	remoteTarget(t)

	title, repoID, err := resolveAttachTarget("remote-only")
	if err != nil {
		t.Fatalf("a remote attach must be resolved BY the daemon, not on the client's disk; got: %v", err)
	}
	if title != "remote-only" {
		t.Errorf("title = %q, want the bare title handed to the daemon", title)
	}
	// The client's cwd names a repo on THIS machine, which says nothing about
	// the daemon's projects — scoping by it asks the remote for an ID it has
	// never seen (the resolveRepoIDForLookup contract).
	if repoID != "" {
		t.Errorf("repoID = %q, want empty: the client's cwd must not scope a remote attach", repoID)
	}
}

// TestSessionsAttachCmd_RemoteReachesTheDaemon is the behavioral half, and the
// guard that the fix is WIRED INTO the command rather than sitting in a helper
// nothing calls: running attach's RunE against a remote target must get past
// resolution and fail at the dial. Before #1974 it failed with "not found"
// having never opened a connection.
//
// The target is a closed local port, so the dial fails immediately with no
// network involved. Only the REMOTE path is driven here: the local path calls
// daemon.EnsureDaemon, which spawns a real daemon and has no place in a unit
// test.
func TestSessionsAttachCmd_RemoteReachesTheDaemon(t *testing.T) {
	clientWithoutTheSession(t)
	prev := apiclient.FlagDaemonURL
	apiclient.FlagDaemonURL = "http://127.0.0.1:1"
	t.Cleanup(func() { apiclient.FlagDaemonURL = prev })

	cmd := *sessionsAttachCmd
	cmd.SetContext(context.Background())
	err := cmd.RunE(&cmd, []string{"remote-only"})
	if err == nil {
		t.Fatal("dialing a closed port must fail")
	}
	if strings.Contains(err.Error(), "not found") {
		t.Fatalf("attach resolved the session on the client's disk instead of the remote daemon: %v", err)
	}
	if !strings.Contains(err.Error(), "failed to attach") {
		t.Fatalf("expected the failure to come from the daemon dial, got: %v", err)
	}
}

// TestResolveAttachTarget_RemoteStillHonorsExplicitRepo keeps --repo meaningful
// against a remote: an explicitly named repo is the user stating the scope, not
// the cwd being inherited, so it is threaded through to the daemon.
func TestResolveAttachTarget_RemoteStillHonorsExplicitRepo(t *testing.T) {
	repoRoot := clientWithoutTheSession(t)
	repo, err := config.RepoFromPath(repoRoot)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	remoteTarget(t)

	prev := repoFlag
	repoFlag = repoRoot
	t.Cleanup(func() { repoFlag = prev })

	_, repoID, err := resolveAttachTarget("remote-only")
	if err != nil {
		t.Fatalf("resolveAttachTarget: %v", err)
	}
	if repoID != repo.ID {
		t.Errorf("repoID = %q, want the explicitly named repo %q", repoID, repo.ID)
	}
}

// TestResolveAttachTarget_LocalStillResolvesOnDisk is the other half of the
// contract, and the guard against over-correcting #1974 into "never look at
// disk". A LOCAL attach must still resolve (and restore) the instance from this
// machine's records — that restore is what gives the local daemon a session to
// attach to — so an unknown title still fails locally rather than being handed
// blindly to the daemon.
func TestResolveAttachTarget_LocalStillResolvesOnDisk(t *testing.T) {
	repoRoot := clientWithoutTheSession(t)
	// No remoteTarget(t): this is the local unix-socket path.

	_, _, err := resolveAttachTarget("nonexistent")
	if err == nil {
		t.Fatal("a local attach must still resolve the title on disk, and fail when it is not there")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected the local disk lookup to report not-found, got: %v", err)
	}

	// A title that IS on this machine's records gets PAST the scoped lookup —
	// it fails later, in the restore, on this deliberately thin record. That
	// difference is the point: the local path reads disk and its answer depends
	// on what disk holds, which is precisely what the remote path must not do.
	repo, err := config.RepoFromPath(repoRoot)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	raw, err := json.Marshal([]session.InstanceData{{Title: "local-one", Path: repoRoot}})
	if err != nil {
		t.Fatalf("marshal instances: %v", err)
	}
	if err := config.SaveRepoInstances(repo.ID, raw); err != nil {
		t.Fatalf("save instances: %v", err)
	}

	_, _, err = resolveAttachTarget("local-one")
	if err != nil && strings.Contains(err.Error(), "not found") {
		t.Fatalf("a title present in this repo's records must resolve past the scoped lookup, got: %v", err)
	}
}

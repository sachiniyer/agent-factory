package api

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"
)

// TestSessionsRetryLimit_UsesSharedDaemonPath pins the surface-parity fix: the
// CLI must hand the selected session to the existing ResumeFromLimit daemon
// action, scoped exactly like the other title-addressed session mutations. The
// daemon owns every resume detail (respawn, pending-prompt delivery, and durable
// liveness clearing); the CLI must not grow a second implementation of them.
func TestSessionsRetryLimit_UsesSharedDaemonPath(t *testing.T) {
	repoID := setupRepoForCmd(t)

	var gotReq daemon.ResumeFromLimitRequest
	prev := resumeFromLimitViaDaemon
	resumeFromLimitViaDaemon = func(req daemon.ResumeFromLimitRequest) error {
		gotReq = req
		return nil
	}
	t.Cleanup(func() { resumeFromLimitViaDaemon = prev })

	out, err := runCmdCaptureStdout(t, sessionsRetryLimitCmd, []string{"worker"})
	if err != nil {
		t.Fatalf("retry-limit returned error: %v", err)
	}
	if gotReq.Title != "worker" || gotReq.RepoID != repoID || gotReq.ID != "" {
		t.Fatalf("ResumeFromLimit request = %+v, want title %q scoped to repo %q with no web-only id", gotReq, "worker", repoID)
	}

	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("output is not JSON (%q): %v", out, err)
	}
	if payload["ok"] != true || payload["title"] != "worker" {
		t.Fatalf("JSON output = %v, want ok=true and title=worker", payload)
	}
}

// TestSessionsRetryLimit_SurfacesDaemonRejection keeps the CLI from claiming a
// retry happened when the shared daemon path says the target is not limit-
// blocked (or rejects it for any other reason).
func TestSessionsRetryLimit_SurfacesDaemonRejection(t *testing.T) {
	setupRepoForCmd(t)

	prev := resumeFromLimitViaDaemon
	resumeFromLimitViaDaemon = func(daemon.ResumeFromLimitRequest) error {
		return errors.New("session \"worker\" is not blocked on a usage limit")
	}
	t.Cleanup(func() { resumeFromLimitViaDaemon = prev })

	_, err := runCmdCaptureStdout(t, sessionsRetryLimitCmd, []string{"worker"})
	if err == nil {
		t.Fatal("retry-limit must surface the daemon rejection")
	}
	if !strings.Contains(err.Error(), "not blocked on a usage limit") {
		t.Fatalf("error = %v, want the daemon's rejection message", err)
	}
}

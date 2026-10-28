package integration_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// TestDaemonReadyWithUnreachableRemoteDependencies is the #5815 regression: the
// daemon must reach ready promptly when everything outside the box it could wait
// on is unreachable, because a user whose sandbox host or Docker engine is slow
// or down would otherwise see "starting (restoring sessions)" on every client.
//
// Both dependencies are made to hang rather than refuse (an unroutable TEST-NET-1
// address), the case that costs a client timeout per attempt:
//
//   - `sandbox_ssh` names an ssh host that never answers;
//   - DOCKER_HOST names a Docker engine that never answers, which the startup
//     orphan-container sweep used to wait on for 30s per `docker` call.
//
// startDaemon's health wait fails the test after 10s with the daemon log dumped,
// so reaching it at all is the assertion; the explicit bound below is the same
// promise stated in the test body.
func TestDaemonReadyWithUnreachableRemoteDependencies(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real daemon; skipped under -short — see #2052")
	}
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv("DOCKER_HOST", "tcp://192.0.2.1:2375")

	bin := buildBinary(t)
	repo := setupGitRepo(t)
	writeFile(t, filepath.Join(repo, ".agent-factory", "config.json"), `{"backend":"sandbox"}`, 0644)
	writeFile(t, filepath.Join(home, config.ConfigFileName),
		mustJSON(t, map[string]any{
			"auto_update": false,
			"sandbox_ssh": "ssh -o BatchMode=yes -o ConnectTimeout=30 root@192.0.2.1",
		}), 0644)

	h := &harness{t: t, bin: bin, home: home, repo: repo}
	t.Cleanup(func() { stopDaemons(t, bin, home) })

	start := time.Now()
	h.startDaemon()
	if took := time.Since(start); took > 8*time.Second {
		t.Fatalf("daemon took %s to become ready with an unreachable sandbox host and Docker engine; readiness must not wait on remote dependencies (#5815)%s",
			took.Round(time.Millisecond), h.daemonLogTail())
	}
}

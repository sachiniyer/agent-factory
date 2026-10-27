package integration_test

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// TestSandboxDaemonE2E is the W10.3 regression net for backend = "sandbox"
// (epic #5571): a real `af --daemon` on a DEFAULT config — the only operator
// setting is `sandbox_ssh`, there is no program_overrides and the program is the
// bare name `claude` — drives a session on a containerized sshd host through the
// daemon's own RPCs:
//
//	create → prompt → preview → tab → archive → restore → kill, then a
//	task-fired create.
//
// Every step asserts on BOTH sides: the daemon's Snapshot and the host's
// processes, session dirs and pushed branch. The older round-trips call
// ArchiveSandbox/RestoreSandbox directly and run with a wrapper program, which
// is where a default-config blocker (AR-01, AR-05) or a create blocker a
// task-fired session inherits (AR-26) would hide.
//
// CI only (.github/workflows/sandbox-e2e.yml); SKIPS cleanly without docker.
func TestSandboxDaemonE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("real-backend E2E; skipped under -short — see #2052")
	}
	requireDocker(t)

	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	// The daemon streams ITS OWN binary onto the host, so it has to run a static
	// build the alpine sshd can execute.
	afBin := buildStaticBinary(t)

	repo := setupGitRepo(t)
	host := startSandboxHost(t, repo)
	writeFile(t, filepath.Join(repo, ".agent-factory", "config.json"), `{"backend":"sandbox"}`, 0644)

	writeConfig := func(extra map[string]any) {
		cfg := map[string]any{"auto_update": false, "sandbox_ssh": host.sshCommand()}
		for k, v := range extra {
			cfg[k] = v
		}
		writeFile(t, filepath.Join(home, config.ConfigFileName), mustJSON(t, cfg), 0644)
	}
	writeConfig(nil)

	h := &harness{t: t, bin: afBin, home: home, repo: repo}
	t.Cleanup(func() { stopDaemons(t, afBin, home) })
	h.startDaemon()

	repoID := config.RepoIDForPath(repo)
	const title = "sbx-e2e"

	snapshot := func() []session.InstanceData {
		var resp daemon.SnapshotResponse
		if err := h.tryHTTPPost("/v1/Snapshot", daemon.SnapshotRequest{}, &resp); err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		return resp.Instances
	}
	find := func(name string) *session.InstanceData {
		for _, in := range snapshot() {
			if in.Title == name {
				in := in
				return &in
			}
		}
		return nil
	}
	post := func(path string, req, dst any) {
		t.Helper()
		if err := h.tryHTTPPost(path, req, dst); err != nil {
			t.Fatalf("%v", err)
		}
	}
	preview := func(name string, tab int) string {
		var resp daemon.PreviewResponse
		post("/v1/Preview", daemon.PreviewRequest{Title: name, RepoID: repoID, Tab: tab}, &resp)
		return resp.Content
	}
	// The shared helper execs pgrep directly: a `sh -c "pgrep -f …"` would match
	// its own command line and never read zero.
	agentServers := func() int { return sshdAgentServerProcs(t, host.container) }
	sessionDirs := func() string { return host.exec(t, "ls -1 /root/.af-sessions 2>/dev/null") }

	// --- create ---------------------------------------------------------------
	//
	// First on the DEFAULT config. Today that is refused: the daemon listens on
	// loopback with no token, so it will not hand a sandbox a callback credential
	// (AR-01; W3's -R tunnel removes the need). The refusal is pinned by name and
	// logged, not skipped past — when W3 lands the default create succeeds, this
	// branch stops running and the test proves the real default. Until then the
	// rest of the lifecycle runs under the one workaround an operator has: a
	// non-loopback network.listen_addr with network.require_token on.
	var created daemon.CreateSessionResponse
	err := h.tryHTTPPost("/v1/CreateSession", daemon.CreateSessionRequest{Title: title, RepoPath: repo, Program: "claude"}, &created)
	if err != nil {
		if !strings.Contains(err.Error(), "refusing to give this sandbox a callback credential") {
			t.Fatalf("default-config CreateSession failed for a reason other than the known callback refusal: %v", err)
		}
		t.Logf("KNOWN BLOCKER (AR-01, fixed by W3): default-config create is refused: %v", err)
		ip := nonLoopbackIPv4(t)
		stopDaemons(t, afBin, home)
		// The first daemon start migrated config.json to config.toml, which then
		// wins; drop it so the rewritten JSON is the one that loads.
		_ = os.Remove(filepath.Join(home, "config.toml"))
		writeConfig(map[string]any{"listen_addr": ip + ":" + freeTCPPort(t), "require_token": true})
		h.startDaemon()
		post("/v1/CreateSession", daemon.CreateSessionRequest{Title: title, RepoPath: repo, Program: "claude"}, &created)
	}
	in := find(title)
	if in == nil {
		t.Fatalf("created session %q is not in the daemon snapshot", title)
	}
	if in.BackendType != "sandbox" {
		t.Fatalf("backend_type = %q, want sandbox", in.BackendType)
	}
	if agentServers() == 0 || sessionDirs() == "" {
		t.Fatalf("host shows no agent-server (%d) or session dir (%q) after create", agentServers(), sessionDirs())
	}

	// --- prompt → preview -----------------------------------------------------
	var sent daemon.SendPromptResponse
	post("/v1/SendPrompt", daemon.SendPromptRequest{Title: title, RepoID: repoID, Prompt: "hello-from-sandbox-e2e"}, &sent)
	waitFor(t, "the prompt to reach the sandbox agent's preview", 60*time.Second, func() bool {
		return strings.Contains(preview(title, 0), "hello-from-sandbox-e2e")
	})

	// --- tab ------------------------------------------------------------------
	// An external URL: a loopback target is refused on a sandbox until W5 (#3062).
	before := len(find(title).Tabs)
	var tab daemon.CreateTabResponse
	post("/v1/CreateTab", daemon.CreateTabRequest{Title: title, RepoID: repoID, Kind: "web", URL: "https://example.com", Name: "dev"}, &tab)
	if got := len(find(title).Tabs); got != before+1 {
		t.Fatalf("tab count = %d after CreateTab, want %d", got, before+1)
	}

	// --- session work: a distinctive commit in the sandbox workspace ----------------
	// The agent works in the clone's LINKED worktree (the main clone stays on the
	// default branch); that worktree is where its work and the pushed branch live.
	workspace := func() string {
		return host.exec(t, "main=$(dirname $(find /root/.af-sessions -maxdepth 4 -name .git | head -1)); "+
			"git -C $main worktree list --porcelain | awk '/^worktree /{p=$2} END{print p}'")
	}
	ws := workspace()
	committed := host.exec(t, "cd "+ws+" && echo e2e > e2e-work.txt && git add e2e-work.txt && git -c user.name=e2e -c user.email=e2e@example.com commit -q -m sbx-e2e-work 2>&1; git log --oneline -3 2>&1; git branch --show-current")
	if !strings.Contains(committed, "sbx-e2e-work") {
		t.Fatalf("could not commit in the sandbox workspace %q:\n%s", ws, committed)
	}
	t.Logf("sandbox workspace %s: %s", ws, committed)

	// --- archive: the host is reaped, the work survives on origin ---------------
	post("/v1/ArchiveSession", daemon.ArchiveSessionRequest{Title: title, RepoID: repoID}, &daemon.ArchiveSessionResponse{})
	if a := find(title); a == nil || a.Liveness != session.LiveArchived {
		t.Fatalf("after ArchiveSession the session is %+v, want an archived row", a)
	}
	waitFor(t, "the archived session's agent-server to be reaped", 30*time.Second, func() bool { return agentServers() == 0 })
	if log := host.exec(t, "git -c safe.directory='*' -C /repo.git log --all --format=%s 2>&1"); !strings.Contains(log, "sbx-e2e-work") {
		t.Fatalf("origin lacks the session's commit after archive; log:\n%s\nbranches:\n%s", log,
			host.exec(t, "git -c safe.directory='*' -C /repo.git branch -a 2>&1"))
	}

	// --- restore: a fresh sandbox, drivable ------------------------------------
	post("/v1/RestoreArchived", daemon.RestoreArchivedRequest{Title: title, RepoID: repoID}, &daemon.RestoreArchivedResponse{})
	waitFor(t, "the restored session to be live", 90*time.Second, func() bool {
		r := find(title)
		return r != nil && r.Liveness != session.LiveArchived && agentServers() > 0
	})
	if got := host.exec(t, "cat "+workspace()+"/e2e-work.txt"); got != "e2e" {
		t.Fatalf("restored workspace lost the session's file; e2e-work.txt = %q", got)
	}
	post("/v1/SendPrompt", daemon.SendPromptRequest{Title: title, RepoID: repoID, Prompt: "after-restore-marker"}, &sent)
	waitFor(t, "the restored agent to echo a prompt", 60*time.Second, func() bool {
		return strings.Contains(preview(title, 0), "after-restore-marker")
	})

	// --- kill: nothing is left behind on the host ----------------------------
	post("/v1/KillSession", daemon.KillSessionRequest{Title: title, RepoID: repoID}, &daemon.KillSessionResponse{})
	if find(title) != nil {
		t.Fatalf("session %q still in the snapshot after KillSession", title)
	}
	waitFor(t, "the host to be clean after kill", 30*time.Second, func() bool {
		return agentServers() == 0 && sessionDirs() == ""
	})

	// --- task-fired create (AR-26) ---------------------------------------------
	// Fired through the daemon's scheduler path on the same default config: any
	// create blocker a task-fired session inherits fails here.
	post("/v1/AddTask", daemon.AddTaskRequest{Task: task.Task{
		ID:          "sbx-task",
		Name:        "sbx-task-run",
		Prompt:      "task-fired-marker",
		CronExpr:    manualTriggerFixtureCron(time.Now()),
		ProjectPath: repo,
		Program:     "claude",
		Enabled:     true,
		CreatedAt:   time.Now(),
	}}, &daemon.AddTaskResponse{})
	post("/v1/TriggerTask", daemon.TriggerTaskRequest{ID: "sbx-task"}, &daemon.TriggerTaskResponse{})
	waitFor(t, "the task-fired sandbox session", 120*time.Second, func() bool {
		in := find("sbx-task-run")
		return in != nil && in.BackendType == "sandbox"
	})
	waitFor(t, "the task prompt to reach the sandbox agent", 60*time.Second, func() bool {
		return strings.Contains(preview("sbx-task-run", 0), "task-fired-marker")
	})
	post("/v1/KillSession", daemon.KillSessionRequest{Title: "sbx-task-run", RepoID: repoID}, &daemon.KillSessionResponse{})
	waitFor(t, "the host to be clean after the task session is killed", 30*time.Second, func() bool {
		return agentServers() == 0 && sessionDirs() == ""
	})
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

// nonLoopbackIPv4 finds an address of this host a sandbox could dial; the test
// skips on a host with none.
func nonLoopbackIPv4(t *testing.T) string {
	t.Helper()
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
			return ipn.IP.String()
		}
	}
	t.Skip("no non-loopback IPv4 address for the daemon's callback listener")
	return ""
}

func freeTCPPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick a free port: %v", err)
	}
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return port
}

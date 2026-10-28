package integration_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// TestSandboxDisruptionsE2E is the W10.4 regression net (epic #5571): the same
// real daemon and containerized sshd host as TestSandboxDaemonE2E, but the point
// is what survives a disruption, not the happy lifecycle. One live session takes
// three hits in turn:
//
//  1. The daemon restarts mid-session. The SAME agent-server process, the same
//     tab roster and the conversation stay live, and nothing is re-provisioned.
//  2. The daemon-to-sandbox ssh connection is killed. The session reconnects with
//     no operator action, the agent-server is still the same process, and a new
//     prompt round-trips.
//  3. The host restarts, which kills every process on it. The work committed in
//     the sandbox's workspace is still recoverable: the session comes back live
//     with that file in its workspace.
//
// CI only (.github/workflows/sandbox-e2e.yml); SKIPS cleanly without docker.
func TestSandboxDisruptionsE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("real-backend E2E; skipped under -short — see #2052")
	}
	requireDocker(t)

	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	afBin := buildStaticBinary(t)

	repo := setupGitRepo(t)
	host := startSandboxHost(t, repo)
	writeFile(t, filepath.Join(repo, ".agent-factory", "config.json"), `{"backend":"sandbox"}`, 0644)
	writeFile(t, filepath.Join(home, config.ConfigFileName),
		mustJSON(t, map[string]any{"auto_update": false, "sandbox_ssh": host.sshCommand()}), 0644)

	h := &harness{t: t, bin: afBin, home: home, repo: repo}
	t.Cleanup(func() { stopDaemons(t, afBin, home) })
	h.startDaemon()

	repoID := config.RepoIDForPath(repo)
	const title = "sbx-disrupt"

	find := func() *session.InstanceData {
		var resp daemon.SnapshotResponse
		if err := h.tryHTTPPost("/v1/Snapshot", daemon.SnapshotRequest{}, &resp); err != nil {
			return nil
		}
		for _, in := range resp.Instances {
			if in.Title == title {
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
	preview := func() string {
		var resp daemon.PreviewResponse
		if err := h.tryHTTPPost("/v1/Preview", daemon.PreviewRequest{Title: title, RepoID: repoID, Tab: 0}, &resp); err != nil {
			return ""
		}
		return resp.Content
	}
	prompt := func(marker string) {
		t.Helper()
		post("/v1/SendPrompt", daemon.SendPromptRequest{Title: title, RepoID: repoID, Prompt: marker}, &daemon.SendPromptResponse{})
		waitFor(t, "the prompt "+marker+" to reach the sandbox agent", 90*time.Second, func() bool {
			return strings.Contains(preview(), marker)
		})
	}
	// The agent-server pids on the host, sorted: "the same process" is the same set.
	agentPIDs := func() string {
		out, _ := exec.Command("docker", "exec", host.container, "pgrep", "-f", "agent-server --listen").CombinedOutput()
		pids := strings.Fields(string(out))
		sort.Strings(pids)
		return strings.Join(pids, ",")
	}
	sessionDirs := func() string { return host.exec(t, "ls -1 /root/.af-sessions 2>/dev/null") }
	roster := func() string {
		in := find()
		if in == nil {
			return ""
		}
		var names []string
		for _, tab := range in.Tabs {
			names = append(names, strconv.Itoa(int(tab.Kind))+":"+tab.Name)
		}
		return strings.Join(names, ",")
	}
	live := func() bool {
		in := find()
		return in != nil && (in.Liveness == session.LiveRunning || in.Liveness == session.LiveReady)
	}
	workspace := func() string {
		return host.exec(t, "main=$(dirname $(find /root/.af-sessions -maxdepth 4 -name .git | head -1)); "+
			"git -C $main worktree list --porcelain | awk '/^worktree /{p=$2} END{print p}'")
	}

	// --- a live session with a roster, a conversation and committed work -------
	post("/v1/CreateSession", daemon.CreateSessionRequest{Title: title, RepoPath: repo, Program: "claude"}, &daemon.CreateSessionResponse{})
	post("/v1/CreateTab", daemon.CreateTabRequest{Title: title, RepoID: repoID, Kind: "web", URL: "https://example.com", Name: "dev"}, &daemon.CreateTabResponse{})
	prompt("before-restart-marker")
	if out := host.exec(t, "cd "+workspace()+" && echo survives > e2e-disrupt.txt && git add e2e-disrupt.txt && "+
		"git -c user.name=e2e -c user.email=e2e@example.com commit -q -m sbx-disrupt-work 2>&1; git log --oneline -1"); !strings.Contains(out, "sbx-disrupt-work") {
		t.Fatalf("could not commit in the sandbox workspace:\n%s", out)
	}
	pids, dirs, tabs := agentPIDs(), sessionDirs(), roster()
	if pids == "" || dirs == "" || !strings.HasSuffix(tabs, ":dev") {
		t.Fatalf("setup: agent-server pids %q, session dirs %q, roster %q", pids, dirs, tabs)
	}

	// --- 1. daemon restart mid-session ------------------------------------------
	stopDaemons(t, afBin, home)
	h.startDaemon()
	waitFor(t, "the session to be live again after the daemon restart", 120*time.Second, live)
	if got := agentPIDs(); got != pids {
		t.Fatalf("daemon restart replaced the agent-server: pids %q -> %q", pids, got)
	}
	if got := sessionDirs(); got != dirs {
		t.Fatalf("daemon restart re-provisioned the session: dirs %q -> %q", dirs, got)
	}
	if got := roster(); got != tabs {
		t.Fatalf("daemon restart changed the tab roster: %q -> %q", tabs, got)
	}
	if !strings.Contains(preview(), "before-restart-marker") {
		t.Fatalf("the conversation did not survive the daemon restart; preview:\n%s", preview())
	}
	prompt("after-daemon-restart-marker")

	// --- 2. the ssh connection dies ---------------------------------------------
	killed := killTunnelSSH(t, host.privKey)
	if len(killed) == 0 {
		t.Fatalf("found no daemon-owned ssh -L tunnel for %s to kill", host.privKey)
	}
	waitFor(t, "the session to be live after the tunnel loss", 120*time.Second, live)
	prompt("after-tunnel-loss-marker")
	// Reconnected, not abandoned: a prompt only round-trips through a live tunnel.
	if len(tunnelSSHPIDs(host.privKey)) == 0 {
		t.Fatalf("the prompt round-tripped but no ssh tunnel is running")
	}
	if got := agentPIDs(); got != pids {
		t.Fatalf("tunnel loss replaced the agent-server: pids %q -> %q", pids, got)
	}
	if got := sessionDirs(); got != dirs {
		t.Fatalf("tunnel loss re-provisioned the session: dirs %q -> %q", dirs, got)
	}

	// --- 3. the host restarts ----------------------------------------------------
	host.restart(t)
	waitFor(t, "the session to be recovered after the host restart", 240*time.Second, func() bool {
		return live() && agentPIDs() != ""
	})
	if got := host.exec(t, "cat "+workspace()+"/e2e-disrupt.txt"); got != "survives" {
		t.Fatalf("the sandbox's work was lost across the host restart; e2e-disrupt.txt = %q", got)
	}
	prompt("after-host-restart-marker")

	post("/v1/KillSession", daemon.KillSessionRequest{Title: title, RepoID: repoID}, &daemon.KillSessionResponse{})
	waitFor(t, "the host to be clean after kill", 60*time.Second, func() bool {
		return agentPIDs() == "" && sessionDirs() == ""
	})
}

// tunnelSSHPIDs lists the daemon-host `ssh -L` processes that carry a session's
// tunnel to this fixture's host: the ssh binary itself (not the sh or lifeline
// wrapper around it), with a forward, using the fixture's key.
func tunnelSSHPIDs(privKey string) []int {
	out, _ := exec.Command("pgrep", "-f", privKey).Output()
	var pids []int
	for _, f := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(f)
		if err != nil {
			continue
		}
		comm, _ := os.ReadFile("/proc/" + f + "/comm")
		cmdline, _ := os.ReadFile("/proc/" + f + "/cmdline")
		args := strings.Split(string(cmdline), "\x00")
		if strings.TrimSpace(string(comm)) == "ssh" && hasForward(args) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// killTunnelSSH SIGKILLs those processes — a dropped connection, not a daemon
// shutdown — and returns the pids it killed.
func killTunnelSSH(t *testing.T, privKey string) []int {
	t.Helper()
	pids := tunnelSSHPIDs(privKey)
	for _, pid := range pids {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
		}
	}
	return pids
}

func containsInt(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// hasForward reports whether an ssh argv carries a -L port forward, spelled
// `-L spec` or `-Lspec`.
func hasForward(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "-L") {
			return true
		}
	}
	return false
}

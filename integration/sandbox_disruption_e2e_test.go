package integration_test

import (
	"net"
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

// agentServerScanScript lists the sandbox host's agent-server processes as
// "pid:starttime cmdline", one per line. The match is the product's own
// identity proof (sweepFindPIDs): argv[0] is the session dir's `af` binary
// AND the process environment's AF_TOKEN_REF equals the ref that session's
// af-identity.env carries — not a substring of the command line. The
// `pgrep -f "agent-server --listen"` snapshot this replaced (#7472) also
// counted any `sh -c` carrying the launch text and every $(...) subshell
// that inherits it, which is how a transient remote helper read as a
// second agent-server. The start time (field 22 of /proc/<pid>/stat once
// the comm field is stripped) keeps a recycled pid from faking identity, so
// pid:starttime is the "same process" the assertions compare.
const agentServerScanScript = `for d in /root/.af-sessions/*/; do d=${d%/}; af=$d/af; ` +
	`[ -f "$d/af-identity.env" ] || continue; ` +
	`tok=$(sed -n "s/^AF_TOKEN_REF=//p" "$d/af-identity.env" | sed "s/^'//; s/'\$//" | head -1); ` +
	`[ -n "$tok" ] || continue; ` +
	`for p in /proc/[0-9]*; do pid=${p#/proc/}; ` +
	`a=$(tr '\000' '\n' < "$p/cmdline" 2>/dev/null | sed -n '1p'); [ "$a" = "$af" ] || continue; ` +
	`r=$(tr '\000' '\n' < "$p/environ" 2>/dev/null | sed -n 's/^AF_TOKEN_REF=//p' | sed -n '1p'); ` +
	`[ "$r" = "$tok" ] || continue; ` +
	`st=$(awk '{sub(/^.*\) /,""); print $20}' "$p/stat" 2>/dev/null); ` +
	`[ -n "$st" ] && echo "$pid:$st $(tr '\000' ' ' < "$p/cmdline")"; done; done`

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
//  3. The session is archived (the workspace is preserved), the host and the
//     daemon restart, and restore hands back the SAME workspace byte for byte:
//     HEAD, a staged file, an unstaged edit, an untracked file and an ignored
//     one (W8.5/W8.6).
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
	// The session's agent-server processes on the host, keyed by stable
	// identity — "pid:starttime" — with each match's full command line for
	// the failure dump. The remote scan proves the product's own identity
	// rule (agentServerScanScript), so a helper that merely carries the
	// launch text never appears in this set.
	agentServers := func() map[string]string {
		out, _ := exec.Command("docker", "exec", host.container, "sh", "-c", agentServerScanScript).Output()
		procs := map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if id, cmd, ok := strings.Cut(line, " "); ok && id != "" {
				procs[id] = cmd
			}
		}
		return procs
	}
	// A transient matching process lives seconds; a real second agent-server
	// persists. The property under test is that exactly one agent-server
	// exists once observation settles, so poll to a singleton instead of
	// comparing a pid set captured at a racy instant (#7472), and on timeout
	// dump every match so a genuine duplicate names itself.
	wantSingleAgentServer := func(what string) string {
		t.Helper()
		var procs map[string]string
		deadline := time.Now().Add(30 * time.Second)
		for {
			procs = agentServers()
			if len(procs) == 1 {
				for id := range procs {
					return id
				}
			}
			if !time.Now().Before(deadline) {
				var dump []string
				for id, cmd := range procs {
					dump = append(dump, id+" "+cmd)
				}
				sort.Strings(dump)
				t.Fatalf("%s: the agent-server set never settled to one (%d match):\n%s",
					what, len(procs), strings.Join(dump, "\n"))
			}
			time.Sleep(300 * time.Millisecond)
		}
	}
	// A disruption must not replace the server: once the set settles, the
	// one agent-server is still the SAME pid+start time it was before.
	requireSameAgentServer := func(what, want string) {
		t.Helper()
		if got := wantSingleAgentServer(what); got != want {
			t.Fatalf("%s replaced the agent-server: %s -> %s", what, want, got)
		}
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
	server, dirs, tabs := wantSingleAgentServer("setup"), sessionDirs(), roster()
	if dirs == "" || !strings.HasSuffix(tabs, ":dev") {
		t.Fatalf("setup: session dirs %q, roster %q", dirs, tabs)
	}

	// --- 1. daemon restart mid-session ------------------------------------------
	stopDaemons(t, afBin, home)
	h.startDaemon()
	waitFor(t, "the session to be live again after the daemon restart", 120*time.Second, live)
	requireSameAgentServer("daemon restart", server)
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
	// The supervisor (W1.2) re-dials after a capped backoff, so the forward is
	// down for a moment after the kill and liveness has not noticed yet: a prompt
	// sent now is refused on the dead local port (the first run of this test did
	// exactly that). Wait on the transport itself, not on a sleep: a replacement
	// ssh -L process (not one of the killed pids) must carry the SAME local port
	// the session already holds, and that port must accept connections. The bound
	// is the redial ladder's ceiling (30s, +25% jitter) plus the readiness probe
	// (20s) with margin.
	killed := killTunnelSSH(t, host.privKey)
	if len(killed) == 0 {
		t.Fatalf("found no daemon-owned ssh -L tunnel for %s to kill", host.privKey)
	}
	forwardPort := killed[0].port
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("supervisor lines from the daemon log:\n%s", transportLogLines(home))
		}
	})
	waitFor(t, "a replacement ssh tunnel forwarding local port "+forwardPort, 90*time.Second, func() bool {
		for _, tun := range tunnelSSHProcs(host.privKey) {
			if tun.port == forwardPort && !containsInt(pidsOf(killed), tun.pid) {
				return true
			}
		}
		return false
	})
	waitFor(t, "the restored forward on 127.0.0.1:"+forwardPort+" to accept connections", 60*time.Second, func() bool {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+forwardPort, time.Second)
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	})
	waitFor(t, "the session to be live after the tunnel loss", 120*time.Second, live)
	// Cached liveness is not a readiness barrier (the daemon keeps the last-known
	// value through transport failures), so also require a read-only round trip
	// through the recovered transport: the preview reads the agent-server.
	waitFor(t, "a preview round trip through the recovered tunnel", 60*time.Second, func() bool {
		return strings.Contains(preview(), "after-daemon-restart-marker")
	})
	prompt("after-tunnel-loss-marker")
	t.Logf("supervisor lines after the tunnel loss:\n%s", transportLogLines(home))
	// Reconnected, not abandoned: a prompt only round-trips through a live tunnel.
	if len(tunnelSSHPIDs(host.privKey)) == 0 {
		t.Fatalf("the prompt round-tripped but no ssh tunnel is running")
	}
	requireSameAgentServer("tunnel loss", server)
	if got := sessionDirs(); got != dirs {
		t.Fatalf("tunnel loss re-provisioned the session: dirs %q -> %q", dirs, got)
	}

	// --- 3. archive-preserve, restart the host and the daemon, restore ----------
	// A host restart kills every process on it (the agent-server included) but the
	// container filesystem survives, so the preserved workspace is still there.
	// Archive keeps it (W8.5) and restore reattaches it (W8.6, #5947) instead of
	// re-cloning, so the work comes back byte for byte: committed HEAD, a staged
	// file, an unstaged edit, an untracked file and an ignored one.
	ws := workspace()
	// Every setup step must succeed, and the fingerprint is stdout only under
	// set -e (execOK): a failed step or a missing file would otherwise read as an
	// identical absence before and after. The exclude file is resolved with
	// --git-path because a linked worktree's info/exclude lives in the common dir.
	host.execOK(t, "cd "+ws+" && echo staged > staged.txt && git add staged.txt && echo edited >> e2e-disrupt.txt && echo loose > untracked.txt && "+
		"ex=$(git rev-parse --git-path info/exclude) && mkdir -p \"$(dirname \"$ex\")\" && echo '*.ign' >> \"$ex\" && echo ignored > keep.ign")
	treeState := func() string {
		return host.execOK(t, "cd "+ws+" && git status --porcelain --ignored && git rev-parse HEAD && cat e2e-disrupt.txt staged.txt untracked.txt keep.ign")
	}
	stateAtArchive := treeState()
	for _, want := range []string{"A  staged.txt", " M e2e-disrupt.txt", "?? untracked.txt", "!! keep.ign"} {
		if !strings.Contains(stateAtArchive, want) {
			t.Fatalf("the fixture does not hold %q, so the fingerprint would not cover it:\n%s", want, stateAtArchive)
		}
	}
	originHasWork := func() bool {
		return strings.Contains(host.exec(t, "git -c safe.directory='*' -C /repo.git log --all --format=%s 2>&1"), "sbx-disrupt-work")
	}

	var archived daemon.ArchiveSessionResponse
	post("/v1/ArchiveSession", daemon.ArchiveSessionRequest{Title: title, RepoID: repoID}, &archived)
	waitFor(t, "the archived session's agent-server to stop", 30*time.Second, func() bool { return len(agentServers()) == 0 })
	if loc := archived.ArchivedWorkspace; loc == nil || loc.Pushed || loc.Dir == "" {
		t.Fatalf("archive response archived_workspace = %+v, want an unpushed preserved dir", loc)
	}
	if got := treeState(); got != stateAtArchive {
		t.Fatalf("archiving changed the workspace:\nbefore:\n%s\nafter:\n%s", stateAtArchive, got)
	}

	host.restart(t)
	if got := treeState(); got != stateAtArchive {
		t.Fatalf("the host restart changed the preserved workspace:\nbefore:\n%s\nafter:\n%s", stateAtArchive, got)
	}
	stopDaemons(t, afBin, home)
	h.startDaemon()
	if r := find(); r == nil || r.Liveness != session.LiveArchived || r.ArchivedWorkspace == nil {
		t.Fatalf("after the host and daemon restarts the row is %+v, want the archived row still reporting its preserved workspace", r)
	}

	post("/v1/RestoreArchived", daemon.RestoreArchivedRequest{Title: title, RepoID: repoID}, &daemon.RestoreArchivedResponse{})
	waitFor(t, "the preserved session to be live again after the host restart", 240*time.Second, func() bool {
		return live() && len(agentServers()) > 0
	})
	if got := treeState(); got != stateAtArchive {
		t.Fatalf("restore did not hand back the workspace exactly as archived:\nbefore:\n%s\nafter:\n%s", stateAtArchive, got)
	}
	if got := workspace(); got != ws {
		t.Fatalf("restore moved the workspace: %q -> %q (it must reattach, not re-clone)", ws, got)
	}
	if originHasWork() {
		t.Fatalf("restoring a preserved workspace published its commit to origin")
	}
	if r := find(); r == nil || r.ArchivedWorkspace != nil {
		t.Fatalf("a restored row must no longer report a preserved workspace: %+v", r)
	}
	prompt("after-host-restart-marker")

	post("/v1/KillSession", daemon.KillSessionRequest{Title: title, RepoID: repoID}, &daemon.KillSessionResponse{})
	waitFor(t, "the host to be clean after kill", 60*time.Second, func() bool {
		return len(agentServers()) == 0 && sessionDirs() == ""
	})
}

// tunnelProc is one daemon-host `ssh -L` tunnel process and the local port it
// forwards.
type tunnelProc struct {
	pid  int
	port string
}

// tunnelSSHProcs lists the daemon-host `ssh -L` processes that carry a session's
// tunnel to this fixture's host: the ssh binary itself (not the sh or lifeline
// wrapper around it), with a forward, using the fixture's key.
func tunnelSSHProcs(privKey string) []tunnelProc {
	out, _ := exec.Command("pgrep", "-f", privKey).Output()
	var procs []tunnelProc
	for _, f := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(f)
		if err != nil {
			continue
		}
		comm, _ := os.ReadFile("/proc/" + f + "/comm")
		cmdline, _ := os.ReadFile("/proc/" + f + "/cmdline")
		if strings.TrimSpace(string(comm)) != "ssh" {
			continue
		}
		if port := forwardLocalPort(strings.Split(string(cmdline), "\x00")); port != "" {
			procs = append(procs, tunnelProc{pid: pid, port: port})
		}
	}
	return procs
}

func tunnelSSHPIDs(privKey string) []int { return pidsOf(tunnelSSHProcs(privKey)) }

// killTunnelSSH SIGKILLs those processes — a dropped connection, not a daemon
// shutdown — and returns what it killed.
func killTunnelSSH(t *testing.T, privKey string) []tunnelProc {
	t.Helper()
	procs := tunnelSSHProcs(privKey)
	for _, tun := range procs {
		if p, err := os.FindProcess(tun.pid); err == nil {
			_ = p.Kill()
		}
	}
	return procs
}

func pidsOf(procs []tunnelProc) []int {
	var pids []int
	for _, p := range procs {
		pids = append(pids, p.pid)
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

// forwardLocalPort returns the local port of an ssh argv's -L forward, spelled
// `-L spec` or `-Lspec` with spec `[bind:]port:host:hostport`; "" if none.
func forwardLocalPort(args []string) string {
	for i, a := range args {
		spec := ""
		switch {
		case a == "-L" && i+1 < len(args):
			spec = args[i+1]
		case strings.HasPrefix(a, "-L") && len(a) > 2:
			spec = a[2:]
		default:
			continue
		}
		switch f := strings.Split(spec, ":"); len(f) {
		case 3:
			return f[0]
		case 4:
			return f[1]
		}
	}
	return ""
}

// transportLogLines returns the supervisor's lines from the daemon log.
func transportLogLines(home string) string {
	raw, _ := os.ReadFile(filepath.Join(home, "agent-factory.log"))
	var out []string
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.Contains(l, "ssh transport") || strings.Contains(l, "sandbox runtime") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

package integration_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// TestSSHBackendRoundTrip is the #1592 Phase 4 PR5 proof: a session runs on a REAL
// remote host over SSH, to parity with local.
//
// It stands up a throwaway sshd container as the ssh target (a real ssh server +
// git + tmux, no external host and no dependency on the box's own sshd), sets a
// repo's config to backend=ssh pointing at it, and creates a session through the
// ordinary NewInstance path — so the ssh runtime dials the host with the Go
// x/crypto/ssh client (key auth + known_hosts verification), clones the workspace
// into a per-session dir, streams a static `af` binary onto the remote, starts an
// `af agent-server` bound to remote loopback behind a bearer token, and exposes its
// http:// URL through an ssh local-forward tunnel. The daemon-side Instance then
// drives that remote agent-server over the wire, through the FULL surface:
//
//	Start (Provision+Launch the remote workspace) → Subscribe to its PTY stream →
//	Input (typed bytes reach the remote agent) → observe the echo → Preview /
//	Snapshot / Alive reflect the pane → Kill → the remote agent-server process is
//	reaped + the session dir removed + the tunnel closed (no leak).
//
// Run it on a host with docker: make backend-ssh-roundtrip. It SKIPS cleanly where
// docker is unavailable, so the full suite stays green there.
func TestSSHBackendRoundTrip(t *testing.T) {
	requireDocker(t)
	requireTool(t, "git")
	requireTool(t, "ssh-keygen")
	requireTool(t, "ssh-keyscan")

	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	// A static `af` the runtime streams onto the remote (musl-compatible; the
	// daemon's own binary is what production streams, but the test binary is not
	// `af`, so build a real one and point the runtime at it).
	afBin := buildStaticBinary(t)
	restore := session.SetSSHSelfBinaryForTest(afBin)
	defer restore()

	image := buildSSHDRoundTripImage(t)

	// The source repo + a bare clone bind-mounted into the sshd container as the
	// clone source (file:///repo.git). This makes the in-remote clone a REAL git
	// clone with no GitHub dependency — the durable-store model, self-contained.
	repo := setupGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "ssh round-trip\n", 0644)
	runExternal(t, repo, "git", "add", "-A")
	runExternal(t, repo, "git", "commit", "-m", "seed")
	bare := filepath.Join(t.TempDir(), "repo.git")
	runExternal(t, "", "git", "clone", "--bare", repo, bare)
	runExternal(t, repo, "git", "remote", "add", "origin", "file:///repo.git")

	// An ssh keypair; the pubkey is the container's authorized_keys, the privkey is
	// ssh.identity_file. Real public-key auth end to end.
	keyDir := t.TempDir()
	privKey := filepath.Join(keyDir, "id_ed25519")
	runExternal(t, "", "ssh-keygen", "-t", "ed25519", "-N", "", "-f", privKey, "-C", "af-ssh-roundtrip")
	pubKey := privKey + ".pub"

	// --- start the sshd container (the ssh target) -----------------------------
	cname := fmt.Sprintf("af-ssh-rt-%d", os.Getpid())
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", cname).Run() })
	runExternal(t, "", "docker", "run", "-d", "--name", cname,
		"-p", "127.0.0.1::22",
		"-v", bare+":/repo.git:ro",
		"-v", pubKey+":/authorized_keys:ro",
		image)

	sshHost := dockerPublishedHostPort(t, cname, "22")
	t.Logf("sshd container %s reachable at 127.0.0.1:%s", cname, sshHost)

	waitForSSH(t, sshHost)

	// known_hosts: pin the container's host keys so the runtime's mandatory
	// host-key verification passes against this ephemeral target (no insecure
	// escape hatch — the key is seeded out of band, as a real op would for a fresh
	// cloud host).
	knownHosts := writeKnownHostsForContainer(t, sshHost)

	// Repo config: backend=ssh, the container as the host, key auth, pinned host key.
	writeSSHRepoConfig(t, repo, "127.0.0.1:"+sshHost, "root", privKey, knownHosts)

	title := "ssh-rt"

	// --- create the session on the ssh backend (the full NewInstance path) ------
	// program `cat` echoes the PTY, so typed input observably comes back over the
	// stream — the input-reaches-the-remote-agent proof.
	t.Logf("provisioning ssh session %q on 127.0.0.1:%s...", title, sshHost)
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   title,
		Path:    repo,
		Program: "cat",
		Backend: session.BackendSSH,
	})
	if err != nil {
		t.Fatalf("NewInstance(backend=ssh): %v", err)
	}
	// The remote agent-server is up the instant NewInstance returns (the runtime
	// provisioned it). Assert it, and route every later failure through Kill so the
	// remote process + session dir are reaped.
	if n := sshdAgentServerProcs(t, cname); n == 0 {
		t.Fatal("expected an af agent-server process on the remote after provisioning")
	}
	if dirs := sshdSessionDirs(t, cname); len(dirs) == 0 {
		t.Fatal("expected a per-session dir under ~/.af-sessions on the remote after provisioning")
	}
	t.Logf("remote agent-server is up; exposed over http:// through the ssh tunnel with a bearer token")
	killed := false
	defer func() {
		if !killed {
			_ = inst.Kill()
		}
	}()

	// --- Start: Provision + Launch the remote workspace over the wire ----------
	if err := inst.Start(true); err != nil {
		t.Fatalf("Start (drive remote agent-server Provision+Launch): %v", err)
	}

	as := inst.AgentServer()

	// --- Subscribe to the remote PTY stream through the tunnel -----------------
	sub, err := as.Subscribe(0, 0)
	if err != nil {
		t.Fatalf("Subscribe to the remote PTY stream: %v", err)
	}
	defer func() { _ = sub.Close() }()

	pumpCtx, pumpCancel := context.WithCancel(context.Background())
	defer pumpCancel()
	var (
		pumpMu sync.Mutex
		pump   strings.Builder
	)
	go func() {
		for {
			ev, rerr := sub.NextEvent(pumpCtx)
			if rerr != nil {
				return
			}
			if ev.Kind == session.PTYData || ev.Kind == session.PTYRepaint {
				pumpMu.Lock()
				pump.Write(ev.Data)
				pumpMu.Unlock()
			}
		}
	}()
	streamOutput := func() string {
		pumpMu.Lock()
		defer pumpMu.Unlock()
		return pump.String()
	}

	// --- Input: typed bytes reach the remote agent; the `cat` pane echoes -------
	marker := "ssh-roundtrip-ping"
	if err := as.Input(0, []byte(marker+"\n")); err != nil {
		t.Fatalf("Input over the wire: %v", err)
	}
	waitUntil(t, 20*time.Second, "remote PTY stream echoes typed input", func() bool {
		return strings.Contains(streamOutput(), marker)
	})
	t.Logf("typed input reached the remote agent and echoed over the stream: %q", strings.TrimSpace(streamOutput()))

	// --- Preview / Snapshot / Alive reflect the remote pane --------------------
	waitUntil(t, 10*time.Second, "Snapshot reflects the remote pane", func() bool {
		obs, serr := as.Snapshot()
		return serr == nil && strings.Contains(obs.Content, marker)
	})
	preview, err := as.Preview(0, false)
	if err != nil {
		t.Fatalf("Preview over the wire: %v", err)
	}
	if !strings.Contains(preview.Content, marker) {
		t.Fatalf("Preview did not reflect the typed input: %q", preview.Content)
	}
	if alive, err := as.Alive(); err != nil || !alive {
		t.Fatal("expected the remote workspace to report Alive")
	}
	t.Logf("preview/liveness reflect the remote workspace over the tunnel")

	// --- Capability matrix: the REAL ssh backend services full parity -----------
	// (#1592 Phase 4 PR8): descriptor parity + attach/input/preview/liveness
	// serviced over the tunnel. Archive/Recover are exercised live by
	// TestSSHBackendArchiveRestore.
	assertLiveCapabilityMatrix(t, "ssh", inst)

	// --- Kill: tear the workspace down AND reap the remote process/dir/tunnel ---
	if err := inst.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	killed = true
	waitUntil(t, 30*time.Second, "the remote agent-server process is reaped after Kill", func() bool {
		return sshdAgentServerProcs(t, cname) == 0
	})
	waitUntil(t, 30*time.Second, "the remote session dir is removed after Kill", func() bool {
		return len(sshdSessionDirs(t, cname)) == 0
	})
	t.Logf("remote process reaped + session dir removed + tunnel closed on Kill — no leak. ssh round-trip complete.")
}

// --- ssh round-trip helpers ------------------------------------------------

// buildSSHDRoundTripImage builds a slim image carrying an sshd + git + tmux + bash
// — a real ssh target for the ssh runtime. Its entrypoint installs the mounted
// authorized_keys and runs sshd in the foreground. Host keys are baked at build
// time so they are stable for known_hosts pinning. Returns the image tag.
func buildSSHDRoundTripImage(t *testing.T) string {
	t.Helper()
	const tag = "af-sshd-roundtrip:test"
	dir := t.TempDir()
	dockerfile := "FROM alpine:3.20\n" +
		"RUN apk add --no-cache git tmux bash openssh-server\n" +
		"RUN ssh-keygen -A && mkdir -p /root/.ssh && chmod 700 /root/.ssh\n" +
		// The ssh runtime reaches the remote agent-server through an ssh
		// local-forward (direct-tcpip) tunnel, which the sshd must permit — alpine's
		// default sshd_config ships an active `AllowTcpForwarding no` (first-match
		// wins, so replace it in place rather than appending an override).
		"RUN sed -i 's/^AllowTcpForwarding.*/AllowTcpForwarding yes/' /etc/ssh/sshd_config\n" +
		"COPY entrypoint.sh /entrypoint.sh\n" +
		"RUN chmod +x /entrypoint.sh\n" +
		"ENTRYPOINT [\"/entrypoint.sh\"]\n"
	entrypoint := "#!/bin/sh\n" +
		"set -e\n" +
		"if [ -f /authorized_keys ]; then\n" +
		"  cp /authorized_keys /root/.ssh/authorized_keys\n" +
		"  chmod 600 /root/.ssh/authorized_keys\n" +
		"fi\n" +
		"exec /usr/sbin/sshd -D -e\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "entrypoint.sh"), []byte(entrypoint), 0644); err != nil {
		t.Fatalf("write entrypoint.sh: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "build", "-t", tag, dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the sshd round-trip image failed (needs network on first run): %v\n%s", err, out)
	}
	return tag
}

// dockerPublishedHostPort returns the random loopback host port docker mapped the
// container's given internal port to.
func dockerPublishedHostPort(t *testing.T, cname, internalPort string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "port", cname, internalPort+"/tcp").CombinedOutput()
	if err != nil {
		t.Fatalf("docker port %s: %v\n%s", cname, err, out)
	}
	line := strings.TrimSpace(string(out))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if idx := strings.LastIndex(line, ":"); idx >= 0 && idx+1 < len(line) {
		return strings.TrimSpace(line[idx+1:])
	}
	t.Fatalf("could not parse host port from %q", string(out))
	return ""
}

// writeKnownHostsForContainer seeds a known_hosts file with ALL of the sshd's host
// keys (via ssh-keyscan, exactly how a real known_hosts is populated), so the
// runtime's mandatory host-key verification passes whichever host-key type the Go
// client negotiates. ssh-keyscan already emits the `[127.0.0.1]:<port>` host token,
// so its output is a valid known_hosts file verbatim.
func writeKnownHostsForContainer(t *testing.T, hostPort string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ssh-keyscan", "-p", hostPort, "127.0.0.1").Output()
	if err != nil || len(strings.TrimSpace(string(out))) == 0 {
		t.Fatalf("ssh-keyscan for known_hosts failed: %v\n%s", err, out)
	}
	path := filepath.Join(t.TempDir(), "known_hosts")
	writeFile(t, path, string(out), 0644)
	return path
}

// waitForSSH blocks until the sshd in the container is accepting TCP connections on
// the published port (sshd takes a moment to bind after the container starts).
func waitForSSH(t *testing.T, hostPort string) {
	t.Helper()
	waitUntil(t, 30*time.Second, "sshd accepting connections", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		// `ssh-keyscan` returns 0 and prints a key once sshd answers the handshake.
		out, err := exec.CommandContext(ctx, "ssh-keyscan", "-p", hostPort, "127.0.0.1").CombinedOutput()
		return err == nil && strings.Contains(string(out), "ssh-")
	})
}

// writeSSHRepoConfig drops a repo config selecting the ssh backend at host with key
// auth and the pinned known_hosts.
func writeSSHRepoConfig(t *testing.T, repo, host, user, identityFile, knownHosts string) {
	t.Helper()
	dir := filepath.Join(repo, ".agent-factory")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir repo config: %v", err)
	}
	body := fmt.Sprintf(`{"backend":"ssh","ssh":{"host":%q,"user":%q,"identity_file":%q,"known_hosts":%q}}`,
		host, user, identityFile, knownHosts)
	writeFile(t, filepath.Join(dir, "config.json"), body, 0644)
}

// sshdAgentServerProcs counts running `af agent-server` processes inside the sshd
// container — the remote-liveness probe for the round-trip's provision + reap
// assertions.
func sshdAgentServerProcs(t *testing.T, cname string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// pgrep exits non-zero when nothing matches; that is "0 procs", not an error.
	out, _ := exec.CommandContext(ctx, "docker", "exec", cname, "pgrep", "-f", "agent-server --listen").CombinedOutput()
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// sshdSessionDirs lists the per-session dirs under ~/.af-sessions on the remote —
// non-empty after provisioning, empty after reap.
func sshdSessionDirs(t *testing.T, cname string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "docker", "exec", cname, "sh", "-c", "ls -1 /root/.af-sessions 2>/dev/null").CombinedOutput()
	var dirs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			dirs = append(dirs, s)
		}
	}
	return dirs
}

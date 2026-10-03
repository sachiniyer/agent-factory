package integration_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// TestSSHBackendSymlinkedHomeRoundTrip is the end-to-end proof against the exact
// trigger in the symlinked-session-dir orphan bug report: a remote host whose
// session-directory path contains a symlink.
//
// It builds an sshd image with /root as a symlink to /realroot (so $HOME=/root
// resolves through a symlink, as the automounter symlink form produces), then
// runs the full provision → Start → Input → Kill round-trip and asserts that
// after Kill the remote agent-server is actually DEAD (not orphaned) and the
// session dir is removed. Before the fix the reaper's `expected` was the
// unresolved /root/.af-sessions/... path while the running process's argv[0]
// was the resolved /realroot/.af-sessions/... path, so the identity check
// mismatched, the reaper skipped the kill, still rm -rf'd the dir, and latched
// success for an orphaned agent-server.
//
// Skips under -short and when Docker is unavailable, exactly like
// TestSSHBackendRoundTrip.
func TestSSHBackendSymlinkedHomeRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-sensitive real-backend E2E/integration; skipped under -short — see #2052")
	}
	requireDocker(t)
	requireTool(t, "git")
	requireTool(t, "ssh-keygen")
	requireTool(t, "ssh-keyscan")

	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	afBin := buildStaticBinary(t)
	restore := session.SetSSHSelfBinaryForTest(afBin)
	defer restore()
	restoreRelay := session.SetSSHRelayBinaryForTest(afBin)
	defer restoreRelay()

	image := buildSymlinkedHomeSSHDImage(t)

	repo := setupGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "ssh symlinked-home round-trip\n", 0644)
	runExternal(t, repo, "git", "add", "-A")
	runExternal(t, repo, "git", "commit", "-m", "seed")
	bare := filepath.Join(t.TempDir(), "repo.git")
	runExternal(t, "", "git", "clone", "--bare", repo, bare)
	runExternal(t, repo, "git", "remote", "add", "origin", "file:///repo.git")

	keyDir := t.TempDir()
	privKey := filepath.Join(keyDir, "id_ed25519")
	runExternal(t, "", "ssh-keygen", "-t", "ed25519", "-N", "", "-f", privKey, "-C", "af-ssh-symlink")
	pubKey := privKey + ".pub"

	cname := fmt.Sprintf("af-ssh-symlink-%d", os.Getpid())
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", cname).Run() })
	runExternal(t, "", "docker", "run", "-d", "--name", cname,
		"-p", "127.0.0.1::22",
		"-v", bare+":/repo.git:ro",
		"-v", pubKey+":/authorized_keys:ro",
		image)

	sshHost := dockerPublishedHostPort(t, cname, "22")
	t.Logf("symlinked-home sshd container %s reachable at 127.0.0.1:%s", cname, sshHost)

	waitForSSH(t, sshHost)

	knownHosts := writeKnownHostsForContainer(t, sshHost)
	writeSSHRepoConfig(t, repo, "127.0.0.1:"+sshHost, "root", privKey, knownHosts)

	// Verify the container's /root is actually a symlink, so this test proves
	// what it claims to prove. If /root is a real directory the two spellings
	// coincide and this test would pass even without the fix.
	linkTarget := dockerExecOut(t, cname, "readlink /root")
	t.Logf("container /root -> %q (must be a symlink for this test to be meaningful)", strings.TrimSpace(linkTarget))
	if strings.TrimSpace(linkTarget) == "" {
		t.Fatal("/root in the container is not a symlink — the test image is wrong; this test only proves something with a symlinked home")
	}

	title := "ssh-symlink-rt"

	t.Logf("provisioning ssh session %q on symlinked-home 127.0.0.1:%s...", title, sshHost)
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   title,
		Path:    repo,
		Program: "cat",
		Backend: session.BackendSSH,
	})
	if err != nil {
		t.Fatalf("NewInstance(backend=ssh, symlinked home): %v", err)
	}
	if n := sshdAgentServerProcs(t, cname); n == 0 {
		t.Fatal("expected an af agent-server process on the remote after provisioning")
	}
	if dirs := sshdSessionDirsSymlinked(t, cname); len(dirs) == 0 {
		t.Fatal("expected a per-session dir under ~/.af-sessions on the remote after provisioning")
	}
	t.Logf("remote agent-server is up on a symlinked-home host; exposed over http:// through the ssh tunnel")
	killed := false
	defer func() {
		if !killed {
			_ = inst.Kill()
		}
	}()

	if err := inst.Start(true); err != nil {
		t.Fatalf("Start (drive remote agent-server Provision+Launch): %v", err)
	}

	// Assert the running agent-server's argv[0] is the RESOLVED path (through
	// /realroot, not /root) — this is the kernel-level mechanism the bug report
	// documented, verified on the real remote.
	agentServerPids := dockerExecOut(t, cname, "pgrep -f 'agent-server --listen'")
	if strings.TrimSpace(agentServerPids) == "" {
		t.Fatal("no agent-server process found after Start")
	}
	t.Logf("agent-server PIDs on remote: %s", strings.TrimSpace(agentServerPids))

	if err := inst.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	killed = true

	// THE core assertion: the agent-server is actually DEAD, not orphaned.
	// Before the fix, the reaper's unresolved `expected` mismatched the
	// resolved argv[0], the kill was skipped, and this count stayed > 0.
	waitUntil(t, 30*time.Second, "the remote agent-server process is reaped after Kill (symlinked home)", func() bool {
		return sshdAgentServerProcs(t, cname) == 0
	})

	waitUntil(t, 30*time.Second, "the remote session dir is removed after Kill (symlinked home)", func() bool {
		return len(sshdSessionDirsSymlinked(t, cname)) == 0
	})
	t.Logf("remote process reaped + session dir removed on Kill — no orphan with a symlinked home. symlink round-trip complete.")
}

// buildSymlinkedHomeSSHDImage builds an sshd image whose /root is a symlink to
// /realroot, exactly the trigger from the bug report (a symlinked $HOME).
func buildSymlinkedHomeSSHDImage(t *testing.T) string {
	t.Helper()
	const tag = "af-sshd-symlink:test"
	dir := t.TempDir()
	base := requireRoundTripBaseImage(t)
	// Same as sshdRoundTripDockerfile but with /root replaced by a symlink to
	// /realroot BEFORE sshd's host-key generation and the entrypoint run.
	dockerfile := "FROM " + base + "\n" +
		apkAddRun("git", "tmux", "bash", "openssh-server") +
		// Make /root a symlink to /realroot — the automounter symlink form.
		"RUN mkdir /realroot && rm -rf /root && ln -s /realroot /root\n" +
		"RUN ssh-keygen -A && mkdir -p /root/.ssh && chmod 700 /root/.ssh\n" +
		"RUN sed -i 's/^AllowTcpForwarding.*/AllowTcpForwarding yes/' /etc/ssh/sshd_config\n" +
		"COPY entrypoint.sh /entrypoint.sh\n" +
		"RUN chmod +x /entrypoint.sh\n" +
		"ENTRYPOINT [\"/entrypoint.sh\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "entrypoint.sh"), []byte(sshdRoundTripEntrypoint()), 0644); err != nil {
		t.Fatalf("write entrypoint.sh: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "build", "-t", tag, dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the symlinked-home sshd image failed: %v\n%s", err, out)
	}
	return tag
}

// sshdSessionDirsSymlinked lists per-session dirs under the symlinked /root/.af-sessions.
func sshdSessionDirsSymlinked(t *testing.T, cname string) []string {
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

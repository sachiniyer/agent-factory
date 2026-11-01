package integration_test

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sandboxHost is a containerized mock ssh host for the backend=sandbox end-to-end
// (W10.3): sshd + git + tmux and a stub agent installed as a BARE `claude` on the
// non-interactive PATH, with a bare repo mounted as the clone source. Later W10
// cases reuse it rather than rebuilding the fixture.
type sandboxHost struct {
	container string
	port      string
	privKey   string
	known     string
	repo      string
	// bare is the host-side path of the bare repository mounted at /repo.git.
	bare string
}

// stubAgentScript is the stand-in agent: it prints the ready prompt the create
// path waits for, then echoes stdin so a delivered prompt is visible in the
// preview. It swallows every argument af appends to a real claude command line.
const stubAgentScript = "#!/bin/sh\nprintf '❯ '\nexec cat\n"

// sshCommand is the operator's free-form `sandbox_ssh` for this host — what a
// person would already type in a terminal. Host keys are pinned out of band.
func (h *sandboxHost) sshCommand() string {
	return fmt.Sprintf("ssh -i %s -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=%s -p %s root@127.0.0.1",
		h.privKey, h.known, h.port)
}

// startSandboxHost builds the image, starts the container and returns once sshd
// answers. repo is the local source repo; its bare clone becomes file:///repo.git
// inside the container and is set as the repo's origin.
func startSandboxHost(t *testing.T, repo string) *sandboxHost {
	t.Helper()
	return startSandboxHostWith(t, repo, sandboxImageExtra{})
}

// sandboxImageExtra is what a later W10 case adds to the mock host: files copied
// into the image's build context (context name → source path on disk), Dockerfile
// lines appended after the stub agent, and extra `docker run` arguments. A case
// with extras builds its own tag, so it never replaces the shared image.
type sandboxImageExtra struct {
	tag        string
	files      map[string]string
	dockerfile string
	runArgs    []string
}

// startSandboxHostWith is startSandboxHost with extras.
func startSandboxHostWith(t *testing.T, repo string, extra sandboxImageExtra) *sandboxHost {
	t.Helper()
	requireDocker(t)
	requireTool(t, "git")
	requireTool(t, "ssh")
	requireTool(t, "ssh-keygen")
	requireTool(t, "ssh-keyscan")

	base := buildSSHDRoundTripImage(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "claude"), stubAgentScript, 0755)
	for name, src := range extra.files {
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("reading sandbox image extra %s: %v", src, err)
		}
		writeFile(t, filepath.Join(dir, name), string(data), 0755)
	}
	tag := "af-sandbox-e2e:test"
	if extra.tag != "" {
		tag = extra.tag
	}
	writeFile(t, filepath.Join(dir, "Dockerfile"),
		"FROM "+base+"\nCOPY claude /usr/local/bin/claude\nRUN chmod +x /usr/local/bin/claude\n"+extra.dockerfile, 0644)
	if out, err := exec.Command("docker", "build", "-t", tag, dir).CombinedOutput(); err != nil {
		t.Fatalf("building the sandbox e2e image: %v\n%s", err, out)
	}

	writeFile(t, filepath.Join(repo, "README.md"), "sandbox e2e\n", 0644)
	runExternal(t, repo, "git", "add", "-A")
	runExternal(t, repo, "git", "commit", "-m", "seed")
	bareDir := t.TempDir()
	// The container pushes as root into this bind mount; hand the files back
	// before TempDir's RemoveAll or cleanup fails with EPERM.
	reownTempViaDocker(t, tag, bareDir)
	bare := filepath.Join(bareDir, "repo.git")
	runExternal(t, "", "git", "clone", "--bare", repo, bare)
	runExternal(t, repo, "git", "remote", "add", "origin", "file:///repo.git")

	keyDir := t.TempDir()
	priv := filepath.Join(keyDir, "id_ed25519")
	runExternal(t, "", "ssh-keygen", "-t", "ed25519", "-N", "", "-f", priv, "-C", "af-sandbox-e2e")

	h := &sandboxHost{
		container: fmt.Sprintf("af-sandbox-e2e-%d", os.Getpid()),
		privKey:   priv,
		repo:      repo,
		bare:      bare,
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", h.container).Run() })
	// A FIXED host port, not an ephemeral one: restart() must come back on the
	// address the operator's sandbox_ssh already names, and Docker may hand an
	// ephemeral port out again differently after a restart.
	fixedPort := freeLoopbackPort(t)
	args := []string{"run", "-d", "--name", h.container,
		"-p", "127.0.0.1:" + fixedPort + ":22",
		"-v", bare + ":/repo.git",
		"-v", priv + ".pub:/authorized_keys:ro"}
	args = append(args, extra.runArgs...)
	runExternal(t, "", "docker", append(args, tag)...)
	h.port = dockerPublishedHostPort(t, h.container, "22")
	waitForSSH(t, h.port)
	h.known = writeKnownHostsForContainer(t, h.port)
	return h
}

// freeLoopbackPort asks the kernel for a free loopback TCP port and releases it.
func freeLoopbackPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick a free port: %v", err)
	}
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return port
}

// restart bounces the container (docker restart): every process in it dies, the
// filesystem and the published ssh port survive, and it returns once sshd answers.
func (h *sandboxHost) restart(t *testing.T) {
	t.Helper()
	runExternal(t, "", "docker", "restart", "-t", "2", h.container)
	waitForSSH(t, h.port)
}

// exec runs a shell snippet inside the container.
func (h *sandboxHost) exec(t *testing.T, script string) string {
	t.Helper()
	out, _ := exec.Command("docker", "exec", h.container, "sh", "-c", script).CombinedOutput()
	return strings.TrimSpace(string(out))
}

// execOK runs a shell snippet inside the container under `set -e` and fails the
// test on any nonzero exit, returning stdout only. exec above discards the exit
// status and interleaves stderr into its result, which is right for polling but
// wrong for fixture setup and for fingerprints: a setup step that silently failed
// leaves the assertion built on it testing nothing (#5947). Leading whitespace is
// preserved (hostStdout).
func (h *sandboxHost) execOK(t *testing.T, script string) string {
	t.Helper()
	cmd := exec.Command("docker", "exec", h.container, "sh", "-c", "set -e; "+script)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("host command failed: %v\nscript: %s\nstdout: %s\nstderr: %s", err, script, out, stderr.String())
	}
	return hostStdout(out)
}

// hostStdout drops only the trailing newline(s). A leading TrimSpace would eat the
// first column of `git status --porcelain`, where ' M f' (unstaged) and 'M  f'
// (staged) differ only by position, so a fingerprint taken that way cannot tell a
// restore that staged someone's edit from one that left it alone.
func hostStdout(out []byte) string { return strings.TrimRight(string(out), "\r\n") }

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

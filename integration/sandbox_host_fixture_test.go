package integration_test

import (
	"fmt"
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
	requireDocker(t)
	requireTool(t, "git")
	requireTool(t, "ssh")
	requireTool(t, "ssh-keygen")
	requireTool(t, "ssh-keyscan")

	base := buildSSHDRoundTripImage(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "claude"), stubAgentScript, 0755)
	writeFile(t, filepath.Join(dir, "Dockerfile"),
		"FROM "+base+"\nCOPY claude /usr/local/bin/claude\nRUN chmod +x /usr/local/bin/claude\n", 0644)
	if out, err := exec.Command("docker", "build", "-t", "af-sandbox-e2e:test", dir).CombinedOutput(); err != nil {
		t.Fatalf("building the sandbox e2e image: %v\n%s", err, out)
	}

	writeFile(t, filepath.Join(repo, "README.md"), "sandbox e2e\n", 0644)
	runExternal(t, repo, "git", "add", "-A")
	runExternal(t, repo, "git", "commit", "-m", "seed")
	bareDir := t.TempDir()
	// The container pushes as root into this bind mount; hand the files back
	// before TempDir's RemoveAll or cleanup fails with EPERM.
	reownTempViaDocker(t, "af-sandbox-e2e:test", bareDir)
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
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", h.container).Run() })
	runExternal(t, "", "docker", "run", "-d", "--name", h.container,
		"-p", "127.0.0.1::22",
		"-v", bare+":/repo.git",
		"-v", priv+".pub:/authorized_keys:ro",
		"af-sandbox-e2e:test")
	h.port = dockerPublishedHostPort(t, h.container, "22")
	waitForSSH(t, h.port)
	h.known = writeKnownHostsForContainer(t, h.port)
	return h
}

// exec runs a shell snippet inside the container.
func (h *sandboxHost) exec(t *testing.T, script string) string {
	t.Helper()
	out, _ := exec.Command("docker", "exec", h.container, "sh", "-c", script).CombinedOutput()
	return strings.TrimSpace(string(out))
}

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

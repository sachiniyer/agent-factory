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

// TestDockerBackendArchiveRestore is the #1592 Phase 4 PR6 proof for docker: a
// session's state survives ARCHIVE → RESTORE via GitHub, and the restored session
// is drivable again.
//
// The model (epic decision 4): the container is disposable, the branch on origin
// is the durable workspace. So archive PUSHES the session branch to origin then
// REAPS the container, and restore RE-PROVISIONS a fresh container that CLONES the
// pushed branch back. This test drives that end to end against a real container:
//
//	create (docker backend) → Start → commit real work on the session branch →
//	ArchiveSandbox (assert: branch pushed to origin, container reaped) →
//	RestoreSandbox (assert: FRESH container, branch cloned back, the commit is
//	present) → drive the restored session (typed input echoes) → Kill (reaped).
//
// Run on a host with docker: make backend-docker-roundtrip (it matches
// TestDockerBackend*). SKIPS cleanly where docker is unavailable.
func TestDockerBackendArchiveRestore(t *testing.T) {
	requireDocker(t)
	requireTool(t, "git")

	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	afBin := buildStaticBinary(t)
	restore := session.SetDockerSelfBinaryForTest(afBin)
	defer restore()
	image := buildDockerRoundTripImage(t)

	// Source repo + a bare clone bind-mounted as the clone source. The mount is
	// READ-WRITE (unlike the round-trip's :ro) so the in-container archive can push
	// the branch to it — the bare repo IS "GitHub" for this self-contained test.
	repo := setupGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "docker archive/restore\n", 0644)
	runExternal(t, repo, "git", "add", "-A")
	runExternal(t, repo, "git", "commit", "-m", "seed")
	bareDir := t.TempDir()
	bare := filepath.Join(bareDir, "repo.git")
	runExternal(t, "", "git", "clone", "--bare", repo, bare)
	runExternal(t, repo, "git", "remote", "add", "origin", "file:///repo.git")
	writeDockerRepoConfig(t, repo, image, []string{"-v", bare + ":/repo.git"})
	// The container pushes (as root) into the bind-mounted bare repo, so reown it
	// before t.TempDir's RemoveAll runs (LIFO: registered after the TempDir).
	reownTempViaDocker(t, bareDir)

	title := "docker-ar"
	slug := session.Slugify(title)
	t.Cleanup(func() { reapByLabel(slug) })

	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   title,
		Path:    repo,
		Program: "cat",
		Backend: session.BackendDocker,
	})
	if err != nil {
		t.Fatalf("NewInstance(backend=docker): %v", err)
	}
	killed := false
	defer func() {
		if !killed {
			_ = inst.Kill()
		}
	}()
	if err := inst.Start(true); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The real docker backend services the full capability matrix (#1592 Phase 4
	// PR8) before we exercise its Archive/Recover below.
	assertLiveCapabilityMatrix(t, "docker", inst)

	ids := containersForLabel(t, slug)
	if len(ids) == 0 {
		t.Fatal("expected a container after provisioning")
	}
	container := ids[0]
	worktree, branch := linkedWorktree(t, container, "/workspace")
	t.Logf("session worktree %s on branch %s", worktree, branch)

	// Make a DISTINCT commit on the session branch (standing in for the agent's
	// work) so we can prove it survives archive→restore through GitHub.
	marker := "archive-restore-marker-42"
	dockerExec(t, container, fmt.Sprintf("cd %s && printf %%s %s > proof.txt && git add proof.txt && git commit -m %s",
		shellQ(worktree), shellQ(marker), shellQ(marker)))
	commitSHA := strings.TrimSpace(dockerExecOut(t, container, fmt.Sprintf("git -C %s rev-parse HEAD", shellQ(worktree))))
	if len(commitSHA) < 12 {
		t.Fatalf("could not read the session branch HEAD: %q", commitSHA)
	}
	t.Logf("committed %s on the session branch", commitSHA[:12])

	// --- ARCHIVE: push the branch to origin, reap the container -----------------
	pushed, err := inst.ArchiveSandbox()
	if err != nil {
		t.Fatalf("ArchiveSandbox: %v", err)
	}
	if pushed != branch {
		t.Fatalf("archive pushed branch %q, want %q", pushed, branch)
	}
	waitUntil(t, 30*time.Second, "the container is reaped after archive", func() bool {
		return len(containersForLabel(t, slug)) == 0
	})
	// The branch (with our commit) is now on origin — the durable proof.
	bareLog := runExternal(t, "", "git", "-C", bare, "log", "--format=%H", branch)
	if !strings.Contains(bareLog, commitSHA) {
		t.Fatalf("archived branch %q on origin is missing commit %s:\n%s", branch, commitSHA, bareLog)
	}
	t.Logf("ARCHIVE ✓ branch %q pushed to origin with commit %s; container reaped", branch, commitSHA[:12])

	// --- RESTORE: fresh container, branch cloned back, commit present ----------
	if err := inst.RestoreSandbox(); err != nil {
		t.Fatalf("RestoreSandbox: %v", err)
	}
	ids2 := containersForLabel(t, slug)
	if len(ids2) == 0 {
		t.Fatal("expected a fresh container after restore")
	}
	newContainer := ids2[0]
	if newContainer == container {
		t.Fatalf("restore reused the reaped container %s instead of provisioning a fresh one", shortID(container))
	}
	newWorktree, _ := linkedWorktree(t, newContainer, "/workspace")
	restoredLog := dockerExecOut(t, newContainer, fmt.Sprintf("git -C %s log --format=%%H", shellQ(newWorktree)))
	if !strings.Contains(restoredLog, commitSHA) {
		t.Fatalf("restored worktree is missing the archived commit %s:\n%s", commitSHA, restoredLog)
	}
	restoredProof := strings.TrimSpace(dockerExecOut(t, newContainer, fmt.Sprintf("cat %s/proof.txt", shellQ(newWorktree))))
	if restoredProof != marker {
		t.Fatalf("restored proof.txt = %q, want %q", restoredProof, marker)
	}
	t.Logf("RESTORE ✓ fresh container %s; branch cloned back; commit %s present (proof.txt=%q)",
		shortID(newContainer), commitSHA[:12], restoredProof)

	// --- the restored session is drivable again --------------------------------
	assertDrivable(t, inst.AgentServer(), "post-restore-ping")
	t.Logf("restored session is drivable: typed input echoes over the fresh container's stream")

	if err := inst.Kill(); err != nil {
		t.Fatalf("Kill after restore: %v", err)
	}
	killed = true
	waitUntil(t, 30*time.Second, "the restored container is reaped on Kill", func() bool {
		return len(containersForLabel(t, slug)) == 0
	})
	t.Logf("docker archive→restore round-trip complete: state survived via GitHub, no leak.")
}

// TestSSHBackendArchiveRestore is the #1592 Phase 4 PR6 proof for ssh: identical
// archive→restore-survives-via-GitHub round-trip against a REAL remote host over
// ssh (a throwaway sshd container as the target). The mechanics are written ONCE
// against the Runtime interface, so this exercises the same push-then-teardown /
// re-provision-then-clone flow the docker test does, over the ssh transport.
func TestSSHBackendArchiveRestore(t *testing.T) {
	requireDocker(t)
	requireTool(t, "git")
	requireTool(t, "ssh-keygen")
	requireTool(t, "ssh-keyscan")

	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	afBin := buildStaticBinary(t)
	restore := session.SetSSHSelfBinaryForTest(afBin)
	defer restore()
	image := buildSSHDRoundTripImage(t)

	repo := setupGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "ssh archive/restore\n", 0644)
	runExternal(t, repo, "git", "add", "-A")
	runExternal(t, repo, "git", "commit", "-m", "seed")
	bareDir := t.TempDir()
	bare := filepath.Join(bareDir, "repo.git")
	runExternal(t, "", "git", "clone", "--bare", repo, bare)
	runExternal(t, repo, "git", "remote", "add", "origin", "file:///repo.git")
	// The sshd container pushes (as root) into the bind-mounted bare repo, so reown
	// it before t.TempDir's RemoveAll runs (LIFO: registered after the TempDir).
	reownTempViaDocker(t, bareDir)

	keyDir := t.TempDir()
	privKey := filepath.Join(keyDir, "id_ed25519")
	runExternal(t, "", "ssh-keygen", "-t", "ed25519", "-N", "", "-f", privKey, "-C", "af-ssh-ar")
	pubKey := privKey + ".pub"

	cname := fmt.Sprintf("af-ssh-ar-%d", os.Getpid())
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", cname).Run() })
	// READ-WRITE clone-source mount so the remote archive can push the branch back.
	runExternal(t, "", "docker", "run", "-d", "--name", cname,
		"-p", "127.0.0.1::22",
		"-v", bare+":/repo.git",
		"-v", pubKey+":/authorized_keys:ro",
		image)

	sshHost := dockerPublishedHostPort(t, cname, "22")
	waitForSSH(t, sshHost)
	knownHosts := writeKnownHostsForContainer(t, sshHost)
	writeSSHRepoConfig(t, repo, "127.0.0.1:"+sshHost, "root", privKey, knownHosts)

	title := "ssh-ar"

	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   title,
		Path:    repo,
		Program: "cat",
		Backend: session.BackendSSH,
	})
	if err != nil {
		t.Fatalf("NewInstance(backend=ssh): %v", err)
	}
	killed := false
	defer func() {
		if !killed {
			_ = inst.Kill()
		}
	}()
	if err := inst.Start(true); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The real ssh backend services the full capability matrix (#1592 Phase 4
	// PR8) before we exercise its Archive/Recover below.
	assertLiveCapabilityMatrix(t, "ssh", inst)

	dirs := sshdSessionDirs(t, cname)
	if len(dirs) == 0 {
		t.Fatal("expected a per-session dir on the remote after provisioning")
	}
	workspace := "/root/.af-sessions/" + dirs[0] + "/workspace"
	worktree, branch := linkedWorktree(t, cname, workspace)
	t.Logf("remote session worktree %s on branch %s", worktree, branch)

	marker := "archive-restore-marker-77"
	dockerExec(t, cname, fmt.Sprintf("cd %s && printf %%s %s > proof.txt && git add proof.txt && git commit -m %s",
		shellQ(worktree), shellQ(marker), shellQ(marker)))
	commitSHA := strings.TrimSpace(dockerExecOut(t, cname, fmt.Sprintf("git -C %s rev-parse HEAD", shellQ(worktree))))
	if len(commitSHA) < 12 {
		t.Fatalf("could not read the remote session branch HEAD: %q", commitSHA)
	}
	t.Logf("committed %s on the remote session branch", commitSHA[:12])

	// --- ARCHIVE: push the branch to origin, reap the remote sandbox -----------
	pushed, err := inst.ArchiveSandbox()
	if err != nil {
		t.Fatalf("ArchiveSandbox: %v", err)
	}
	if pushed != branch {
		t.Fatalf("archive pushed branch %q, want %q", pushed, branch)
	}
	waitUntil(t, 30*time.Second, "the remote agent-server is reaped after archive", func() bool {
		return sshdAgentServerProcs(t, cname) == 0
	})
	waitUntil(t, 30*time.Second, "the remote session dir is removed after archive", func() bool {
		return len(sshdSessionDirs(t, cname)) == 0
	})
	bareLog := runExternal(t, "", "git", "-C", bare, "log", "--format=%H", branch)
	if !strings.Contains(bareLog, commitSHA) {
		t.Fatalf("archived branch %q on origin is missing commit %s:\n%s", branch, commitSHA, bareLog)
	}
	t.Logf("ARCHIVE ✓ branch %q pushed to origin with commit %s; remote sandbox reaped", branch, commitSHA[:12])

	// --- RESTORE: fresh remote sandbox, branch cloned back, commit present -----
	if err := inst.RestoreSandbox(); err != nil {
		t.Fatalf("RestoreSandbox: %v", err)
	}
	dirs2 := sshdSessionDirs(t, cname)
	if len(dirs2) == 0 {
		t.Fatal("expected a fresh per-session dir on the remote after restore")
	}
	if dirs2[0] == dirs[0] {
		t.Fatalf("restore reused the reaped session dir %q instead of provisioning a fresh one", dirs[0])
	}
	newWorkspace := "/root/.af-sessions/" + dirs2[0] + "/workspace"
	newWorktree, _ := linkedWorktree(t, cname, newWorkspace)
	restoredLog := dockerExecOut(t, cname, fmt.Sprintf("git -C %s log --format=%%H", shellQ(newWorktree)))
	if !strings.Contains(restoredLog, commitSHA) {
		t.Fatalf("restored remote worktree is missing the archived commit %s:\n%s", commitSHA, restoredLog)
	}
	restoredProof := strings.TrimSpace(dockerExecOut(t, cname, fmt.Sprintf("cat %s/proof.txt", shellQ(newWorktree))))
	if restoredProof != marker {
		t.Fatalf("restored proof.txt = %q, want %q", restoredProof, marker)
	}
	t.Logf("RESTORE ✓ fresh remote dir %q; branch cloned back; commit %s present (proof.txt=%q)",
		dirs2[0], commitSHA[:12], restoredProof)

	assertDrivable(t, inst.AgentServer(), "post-restore-ping")
	t.Logf("restored ssh session is drivable: typed input echoes over the fresh tunnel's stream")

	if err := inst.Kill(); err != nil {
		t.Fatalf("Kill after restore: %v", err)
	}
	killed = true
	waitUntil(t, 30*time.Second, "the restored remote agent-server is reaped on Kill", func() bool {
		return sshdAgentServerProcs(t, cname) == 0
	})
	t.Logf("ssh archive→restore round-trip complete: state survived via GitHub, no leak.")
}

// --- archive/restore shared helpers ----------------------------------------

// shellQ single-quotes s for a POSIX sh -c command (the paths and markers here
// are simple, but quoting keeps the exec robust).
func shellQ(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// reownTempViaDocker chowns dir back to the invoking user via a throwaway root
// container, registered so it runs BEFORE t.TempDir's RemoveAll (LIFO). The
// sandbox containers run as root and push git objects into the bind-mounted bare
// repo, leaving root-owned files the non-root test user cannot delete; without
// this, cleanup fails with EPERM.
func reownTempViaDocker(t *testing.T, dir string) {
	t.Helper()
	uid, gid := os.Getuid(), os.Getgid()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "docker", "run", "--rm",
			"-v", dir+":/reown", "alpine:3.20",
			"chown", "-R", fmt.Sprintf("%d:%d", uid, gid), "/reown").CombinedOutput()
		if err != nil {
			t.Logf("reown %s failed (leaked root-owned files may remain): %v\n%s", dir, err, out)
		}
	})
}

// shortID trims a container id to the 12-char short form for logs.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// dockerExec runs `sh -c <script>` inside container and fails the test on error,
// returning nothing — used for state-mutating steps (making the marker commit).
func dockerExec(t *testing.T, container, script string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "exec", container, "sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("docker exec %q failed: %v\n%s", script, err, out)
	}
}

// dockerExecOut runs `sh -c <script>` inside container and returns its combined
// output (used to read git state). It does NOT fail on a non-zero exit — callers
// assert on the returned text.
func dockerExecOut(t *testing.T, container, script string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "docker", "exec", container, "sh", "-c", script).CombinedOutput()
	return string(out)
}

// linkedWorktree returns the (path, branch) of the session's LINKED git worktree
// inside container — the one `git -C <mainRepo> worktree list` reports that is not
// the main clone itself. It is where the agent's work + the pushed branch live.
func linkedWorktree(t *testing.T, container, mainRepo string) (string, string) {
	t.Helper()
	out := dockerExecOut(t, container, fmt.Sprintf("git -C %s worktree list --porcelain", shellQ(mainRepo)))
	var path, branch string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "worktree "):
			p := strings.TrimSpace(strings.TrimPrefix(line, "worktree "))
			if p == mainRepo {
				path = "" // main clone; skip until its branch line passes
				continue
			}
			path = p
		case strings.HasPrefix(line, "branch ") && path != "":
			branch = strings.TrimSpace(strings.TrimPrefix(line, "branch "))
			branch = strings.TrimPrefix(branch, "refs/heads/")
			return path, branch
		}
	}
	t.Fatalf("could not find the session's linked worktree in %s:\n%s", mainRepo, out)
	return "", ""
}

// assertDrivable proves a (freshly restored) agent-server is live and interactive:
// it subscribes to the PTY stream, types a marker, and waits for the `cat` pane to
// echo it back over the stream.
func assertDrivable(t *testing.T, as session.AgentServer, marker string) {
	t.Helper()
	sub, err := as.Subscribe(0, 0)
	if err != nil {
		t.Fatalf("Subscribe after restore: %v", err)
	}
	defer func() { _ = sub.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var (
		mu   sync.Mutex
		buf  strings.Builder
		read = func() string { mu.Lock(); defer mu.Unlock(); return buf.String() }
	)
	go func() {
		for {
			ev, rerr := sub.NextEvent(ctx)
			if rerr != nil {
				return
			}
			if ev.Kind == session.PTYData || ev.Kind == session.PTYRepaint {
				mu.Lock()
				buf.Write(ev.Data)
				mu.Unlock()
			}
		}
	}()
	if err := as.Input(0, []byte(marker+"\n")); err != nil {
		t.Fatalf("Input after restore: %v", err)
	}
	waitUntil(t, 20*time.Second, "restored PTY stream echoes typed input", func() bool {
		return strings.Contains(read(), marker)
	})
}

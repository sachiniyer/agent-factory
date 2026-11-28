package integration_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// TestDockerBackendArchiveRestoreCollidingDefaultBranch is the #1666 restore-
// fetch bug's integration proof. The bug: fetchRestoreBranch runs
// `git fetch origin <b>:<b>` into a fresh non-bare clone whose main worktree
// has the origin's DEFAULT branch checked out, so whenever the archived branch
// name equals the origin's default branch name, git refuses the fetch (exit
// 128) and restore fails forever — even though the branch is fully pushed.
//
// This test reproduces the report's realistic trigger: the session is created
// while the origin's default branch is a NON-colliding name (so the fresh
// create's own `git worktree add` succeeds), then the operator renames the
// origin's default branch to match the session branch (the master→main
// migration pattern) BEFORE the loss/restore. The restore's fresh clone now
// checks out the session branch, the restore fetch collides, and — without
// the detach guard — restore fails with the misleading "was it pushed to
// origin?" while the branch is fully pushed. With the guard, restore
// succeeds and the session's commit is present.
//
// It drives the docker backend end-to-end against a real container; SKIPS
// cleanly where docker is unavailable. See TestDockerBackendArchiveRestore
// for the non-colliding happy path.
func TestDockerBackendArchiveRestoreCollidingDefaultBranch(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-sensitive real-backend E2E/integration; skipped under -short — see #2052")
	}
	requireDocker(t)
	requireTool(t, "git")

	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	afBin := buildStaticBinary(t)
	restore := session.SetDockerSelfBinaryForTest(afBin)
	defer restore()
	image := buildDockerRoundTripImage(t)

	repo := setupGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "docker colliding archive/restore\n", 0644)
	runExternal(t, repo, "git", "add", "-A")
	runExternal(t, repo, "git", "commit", "-m", "seed")
	bareDir := t.TempDir()
	bare := filepath.Join(bareDir, "repo.git")
	runExternal(t, "", "git", "clone", "--bare", repo, bare)
	runExternal(t, repo, "git", "remote", "add", "origin", "file:///repo.git")
	writeDockerRepoConfig(t, repo, image, []string{"-v", bare + ":/repo.git"})
	reownTempViaDocker(t, image, bareDir)

	title := "docker-collide"
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

	ids := containersForLabel(t, slug)
	if len(ids) == 0 {
		t.Fatal("expected a container after provisioning")
	}
	container := ids[0]
	worktree, branch := linkedWorktree(t, container, "/workspace")
	t.Logf("session worktree %s on branch %s (origin default is still master, so create did not collide)", worktree, branch)

	// A distinct commit on the session branch standing in for the agent's work.
	marker := "colliding-restore-marker-99"
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
	bareLog := runExternal(t, "", "git", "-C", bare, "log", "--format=%H", branch)
	if !strings.Contains(bareLog, commitSHA) {
		t.Fatalf("archived branch %q on origin is missing commit %s:\n%s", branch, commitSHA, bareLog)
	}
	t.Logf("ARCHIVE ✓ branch %q pushed to origin with commit %s; container reaped", branch, commitSHA[:12])

	// --- SIMULATE the origin default-branch rename to match the session branch --
	// The report's realistic trigger: the operator renames origin's default
	// branch to the session branch name AFTER the session was created on a
	// non-colliding default. The bare origin's HEAD now points at the session
	// branch, so the restore's fresh clone checks it out — and the restore
	// fetch `<branch>:<branch>` collides with the checked-out branch. Before the
	// fix this was the exit-128 refusal; with the detach guard it succeeds.
	runExternal(t, "", "git", "--git-dir", bare, "symbolic-ref", "HEAD", "refs/heads/"+branch)
	gotHead := strings.TrimSpace(runExternal(t, "", "git", "--git-dir", bare, "symbolic-ref", "HEAD"))
	if gotHead != "refs/heads/"+branch {
		t.Fatalf("renaming the bare origin's default to %q failed; HEAD=%q", branch, gotHead)
	}
	t.Logf("origin default branch renamed to %q (the collision is now live for restore)", branch)

	// --- RESTORE: fresh container whose clone checks out the colliding branch ---
	// Without the detach guard, fetchRestoreBranch's `git fetch origin <b>:<b>`
	// fails here with `fatal: refusing to fetch into branch 'refs/heads/<b>'
	// checked out at .../workspace` (exit 128), and RestoreSandbox returns the
	// misleading "was it pushed to origin?".
	if err := inst.RestoreSandbox(); err != nil {
		t.Fatalf("RestoreSandbox with a colliding default branch failed (was it pushed to origin?): %v", err)
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
	t.Logf("RESTORE ✓ fresh container %s; colliding branch cloned back; commit %s present (proof.txt=%q)",
		shortID(newContainer), commitSHA[:12], restoredProof)

	if err := inst.Kill(); err != nil {
		t.Fatalf("Kill after restore: %v", err)
	}
	killed = true
	waitUntil(t, 30*time.Second, "the restored container is reaped on Kill", func() bool {
		return len(containersForLabel(t, slug)) == 0
	})
	t.Logf("docker archive→restore with colliding default branch complete: the restore-fetch collision is handled.")
}

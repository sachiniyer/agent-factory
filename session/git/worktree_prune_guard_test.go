package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newPruneGuardRepo builds a real repo with one committed file and returns its
// path — the smallest origin the bounded git probes can run against.
func newPruneGuardRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, exec.Command("git", "init", "-b", "main", repo).Run())
	require.NoError(t, os.WriteFile(filepath.Join(repo, "a.txt"), []byte("x"), 0o644))
	commit := exec.Command("git", "-C", repo, "add", "-A")
	require.NoError(t, commit.Run())
	commit = exec.Command("git", "-C", repo,
		"-c", "user.email=t@t", "-c", "user.name=t",
		"commit", "-qm", "init")
	require.NoError(t, commit.Run())
	return repo
}

// TestWorktreeDirtyFiles pins the prune gate's clean/dirty polarity: a clean
// registered worktree counts zero, one untracked file counts one, and an
// unanswerable path (dead gitdir) is an error — never silently clean.
func TestWorktreeDirtyFiles(t *testing.T) {
	repo := newPruneGuardRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, exec.Command("git", "-C", repo, "worktree", "add", "-b", "af/guard", wt).Run())

	n, err := WorktreeDirtyFiles(wt)
	require.NoError(t, err)
	assert.Zero(t, n, "a fresh worktree must read clean")

	require.NoError(t, os.WriteFile(filepath.Join(wt, "wip.txt"), []byte("only copy"), 0o644))
	n, err = WorktreeDirtyFiles(wt)
	require.NoError(t, err)
	assert.Greater(t, n, 0, "an untracked file must read dirty")

	require.NoError(t, os.RemoveAll(filepath.Join(repo, ".git", "worktrees")))
	_, err = WorktreeDirtyFiles(wt)
	assert.Error(t, err, "a dead gitdir is an error, not a clean answer")
}

// TestWorktreeDirtyFiles_CountsIgnored: a gitignored file is invisible to a
// plain --porcelain status yet is still the only copy of itself — a
// gitignored .env lives nowhere but the worktree, and the kept branch cannot
// restore what it never tracked. It must read dirty like any uncommitted
// file (#5136 Codex round 5).
func TestWorktreeDirtyFiles_CountsIgnored(t *testing.T) {
	repo := newPruneGuardRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("secret.env\n"), 0o644))
	add := exec.Command("git", "-C", repo, "add", ".gitignore")
	require.NoError(t, add.Run())
	require.NoError(t, exec.Command("git", "-C", repo,
		"-c", "user.email=t@t", "-c", "user.name=t",
		"commit", "-qm", "ignore secrets").Run())
	wt := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, exec.Command("git", "-C", repo, "worktree", "add", "-b", "af/ign", wt).Run())

	n, err := WorktreeDirtyFiles(wt)
	require.NoError(t, err)
	assert.Zero(t, n, "precondition: the fresh worktree is clean")

	require.NoError(t, os.WriteFile(filepath.Join(wt, "secret.env"), []byte("TOKEN=x"), 0o644))
	n, err = WorktreeDirtyFiles(wt)
	require.NoError(t, err)
	assert.Greater(t, n, 0, "an ignored file is the only copy — it must read dirty")
}

// TestWorktreeDirtyFiles_DisablesFsmonitor: --no-optional-locks does NOT stop
// `git status` consulting a configured core.fsmonitor hook or builtin monitor
// — that consultation can spawn the monitor or write its cookie, a write a
// read-only probe must not cause (#5136 Codex round 6). The invocation
// carries `-c core.fsmonitor=` so a configured hook never runs; the marker
// file is the observable proof.
func TestWorktreeDirtyFiles_DisablesFsmonitor(t *testing.T) {
	repo := newPruneGuardRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, exec.Command("git", "-C", repo, "worktree", "add", "-b", "af/fsm", wt).Run())

	marker := filepath.Join(t.TempDir(), "fsmonitor-ran")
	hook := filepath.Join(t.TempDir(), "fsm-hook.sh")
	require.NoError(t, os.WriteFile(hook, []byte(
		"#!/bin/sh\necho ran >> \""+marker+"\"\nprintf '2\\n\\n\\n'\n"), 0o755))
	require.NoError(t, exec.Command("git", "-C", repo, "config", "core.fsmonitor", hook).Run())

	// Precondition: a status WITHOUT the override invokes the hook — the
	// fixture has to prove it can observe the write it is testing for.
	plain := exec.Command("git", "-C", wt, "status", "--porcelain")
	require.NoError(t, plain.Run())
	_, err := os.Stat(marker)
	require.NoError(t, err, "the fixture must prove a configured hook fires")
	require.NoError(t, os.Remove(marker))

	n, err := WorktreeDirtyFiles(wt)
	require.NoError(t, err)
	assert.Zero(t, n)
	_, statErr := os.Stat(marker)
	assert.Error(t, statErr, "the read-only probe must never invoke the fsmonitor hook")
}

// TestWorktreeDirtyFiles_SeesPastSubmoduleIgnore: `submodule.<name>.ignore=all`
// makes `git status` suppress EVERYTHING inside the submodule — even with
// --ignored — so unrecoverable work parked there would read clean and list as
// reclaimable. The probe carries --ignore-submodules=none (#5136 Codex round
// 7); a file only inside the ignored submodule must read dirty.
func TestWorktreeDirtyFiles_SeesPastSubmoduleIgnore(t *testing.T) {
	repo := newPruneGuardRepo(t)
	sub := newPruneGuardRepo(t)

	add := exec.Command("git", "-C", repo,
		"-c", "protocol.file.allow=always",
		"submodule", "add", sub, "vendor/sub")
	add.Env = append(os.Environ(), "GIT_ALLOW_PROTOCOL=file")
	out, err := add.CombinedOutput()
	require.NoError(t, err, string(out))
	require.NoError(t, exec.Command("git", "-C", repo,
		"-c", "user.email=t@t", "-c", "user.name=t",
		"commit", "-qm", "add submodule").Run())
	// ignore=all is the suppressive configuration Codex reproduced: status
	// reports nothing inside the submodule at all.
	require.NoError(t, exec.Command("git", "-C", repo,
		"config", "submodule.vendor/sub.ignore", "all").Run())

	wt := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, exec.Command("git", "-C", repo, "worktree", "add", "-b", "af/sub", wt).Run())
	init := exec.Command("git", "-C", wt, "-c", "protocol.file.allow=always",
		"submodule", "update", "--init")
	init.Env = append(os.Environ(), "GIT_ALLOW_PROTOCOL=file")
	require.NoError(t, init.Run())
	require.NoError(t, os.WriteFile(filepath.Join(wt, "vendor", "sub", "wip.txt"), []byte("only copy"), 0o644))

	// Precondition: WITHOUT the override the ignore rule hides the work —
	// the fixture has to prove the hazard it is testing against.
	plain := exec.Command("git", "-C", wt, "--no-optional-locks",
		"-c", "core.fsmonitor=",
		"status", "--porcelain", "--untracked-files=normal", "--ignored")
	porcelain, err := plain.Output()
	require.NoError(t, err)
	assert.Empty(t, strings.TrimSpace(string(porcelain)),
		"submodule.<name>.ignore=all must actually suppress the uncommitted file")

	n, err := WorktreeDirtyFiles(wt)
	require.NoError(t, err)
	assert.Greater(t, n, 0,
		"a submodule file visible only via --ignore-submodules=none must read dirty")
}

// TestVerifyRegisteredWorktreeOccupantBranch pins the session-identity half:
// the pointer proves same-repo, and the branch proves THIS session — a
// same-repo worktree on a different branch parked at a recycled path is a
// different session's checkout and must be refused (#5136 Codex round 2).
func TestVerifyRegisteredWorktreeOccupantBranch(t *testing.T) {
	repo := newPruneGuardRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, exec.Command("git", "-C", repo, "worktree", "add", "-b", "af/mine", wt).Run())

	require.NoError(t, VerifyRegisteredWorktreeOccupantBranch(wt, repo, "af/mine"),
		"the occupant on the recorded branch must verify")
	require.NoError(t, VerifyRegisteredWorktreeOccupantBranch(wt, repo, ""),
		"no recorded branch falls back to the repo-level binding")

	err := VerifyRegisteredWorktreeOccupantBranch(wt, repo, "af/someone-else")
	require.Error(t, err, "a same-repo occupant on a different branch is a different session")
	assert.Contains(t, err.Error(), "different session")

	// A foreign repo's worktree parked at the path is refused before the
	// branch is even compared — the pointer does not bind into this origin.
	foreign := newPruneGuardRepo(t)
	foreignWt := filepath.Join(t.TempDir(), "fwt")
	require.NoError(t, exec.Command("git", "-C", foreign, "worktree", "add", "-b", "af/x", foreignWt).Run())
	err = VerifyRegisteredWorktreeOccupantBranch(foreignWt, repo, "af/x")
	assert.Error(t, err, "a foreign repo's worktree must not satisfy the binding")
}

// TestVerifyWorktreeRegistrationPredates pins the non-reusable half of the
// binding: repo+branch+path can all be RE-created after the original archive
// is deleted, but a re-created worktree's registration leaf is always younger
// than the archive it impersonates — and pruning on the reusable spellings
// alone would delete the replacement (#5136 Codex round 4).
func TestVerifyWorktreeRegistrationPredates(t *testing.T) {
	repo := newPruneGuardRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, exec.Command("git", "-C", repo, "worktree", "add", "-b", "af/orig", wt).Run())

	// The original's leaf predates any archive time that comes later.
	require.NoError(t, VerifyWorktreeRegistrationPredates(wt, time.Now()),
		"a leaf created before the archive bound must verify")

	// The same check against a bound EARLIER than the leaf's creation is
	// the recreation scenario: leaf now > bound then.
	err := VerifyWorktreeRegistrationPredates(wt, time.Now().Add(-time.Hour))
	require.Error(t, err, "a leaf younger than the bound is a recreated worktree")
	assert.Contains(t, err.Error(), "not the archived original")

	// The literal recreation: remove the original, prune the stale
	// registration, and `git worktree add` the same path on the same
	// retained branch — repo, path, and branch all match again.
	archivedAt := time.Now()
	require.NoError(t, os.RemoveAll(wt))
	require.NoError(t, exec.Command("git", "-C", repo, "worktree", "prune", "--expire=now").Run())
	require.NoError(t, exec.Command("git", "-C", repo, "worktree", "add", wt, "af/orig").Run())
	require.NoError(t, VerifyRegisteredWorktreeOccupantBranch(wt, repo, "af/orig"),
		"the reusable spellings DO match — that is the hazard this test pins")
	err = VerifyWorktreeRegistrationPredates(wt, archivedAt)
	assert.Error(t, err, "the recreated occupant's leaf cannot predate the archive")
}

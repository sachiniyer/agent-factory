package api

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// #1842: the CLI resolved user-supplied repo paths with a bare filepath.Abs,
// which treats "~" as an ordinary directory name. Whenever the shell did not
// expand the tilde first — a single-quoted '~/repo', or a "$VAR" holding
// "~/repo" — the path was silently rewritten to "<cwd>/~/repo", surfacing as a
// confusing "not a valid git repository: <cwd>/~/repo" error on sessions/tasks
// and as a wrong target for projects delete. These tests pin every CLI repo-path
// entry point against that regression.

// initRepoUnderHome points HOME at a temp dir and git-inits a repo inside it, so
// "~/<name>" is a real repository only reachable when the tilde is expanded.
func initRepoUnderHome(t *testing.T, name string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	repoRoot := filepath.Join(home, name)
	if out, err := exec.Command("git", "init", repoRoot).CombinedOutput(); err != nil {
		t.Fatalf("git init %s: %v (%s)", repoRoot, err, out)
	}
	return repoRoot
}

// canonical resolves symlinks so comparisons survive platforms where the temp
// dir is itself a symlink (git reports the resolved form).
func canonical(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return resolved
}

// assertNoTildeSegment fails if a resolved path kept a literal "~" component —
// the exact shape of the #1842 corruption.
func assertNoTildeSegment(t *testing.T, in, got string) {
	t.Helper()
	for _, seg := range strings.Split(got, string(filepath.Separator)) {
		if seg == "~" {
			t.Errorf("resolved %q to %q; kept a literal %q segment (the #1842 corruption)", in, got, "~")
		}
	}
}

// TestRepoFromFlagExpandsTilde covers `--repo '~/repo'` on sessions/tasks.
func TestRepoFromFlagExpandsTilde(t *testing.T) {
	repoRoot := initRepoUnderHome(t, "myrepo")

	prev := repoFlag
	t.Cleanup(func() { repoFlag = prev })
	repoFlag = "~/myrepo"

	repo, err := repoFromFlag()
	if err != nil {
		t.Fatalf("repoFromFlag() with --repo %q returned error: %v\nthe tilde was left unexpanded (#1842)", repoFlag, err)
	}
	assertNoTildeSegment(t, repoFlag, repo.Root)
	if got, want := canonical(t, repo.Root), canonical(t, repoRoot); got != want {
		t.Errorf("repoFromFlag() with --repo %q resolved to %q, want %q", repoFlag, got, want)
	}
}

// TestRepoFromFlagBareTilde pins that a bare "~" resolves to the home directory
// rather than "<cwd>/~".
func TestRepoFromFlagBareTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if out, err := exec.Command("git", "init", home).CombinedOutput(); err != nil {
		t.Fatalf("git init %s: %v (%s)", home, err, out)
	}

	prev := repoFlag
	t.Cleanup(func() { repoFlag = prev })
	repoFlag = "~"

	repo, err := repoFromFlag()
	if err != nil {
		t.Fatalf("repoFromFlag() with --repo %q returned error: %v (#1842)", repoFlag, err)
	}
	assertNoTildeSegment(t, repoFlag, repo.Root)
	if got, want := canonical(t, repo.Root), canonical(t, home); got != want {
		t.Errorf("repoFromFlag() with --repo %q resolved to %q, want home %q", repoFlag, got, want)
	}
}

// TestResolveProjectDeleteTargetExpandsTilde covers `projects delete '~/proj'`.
// Targeting is destructive, so a corrupted path here does not merely error — it
// aims the request at the wrong project.
func TestResolveProjectDeleteTargetExpandsTilde(t *testing.T) {
	repoRoot := initRepoUnderHome(t, "proj")

	req, name, err := resolveProjectDeleteTarget([]string{"~/proj"})
	if err != nil {
		t.Fatalf("resolveProjectDeleteTarget([\"~/proj\"]) returned error: %v (#1842)", err)
	}
	assertNoTildeSegment(t, "~/proj", req.RepoPath)
	if got, want := canonical(t, req.RepoPath), canonical(t, repoRoot); got != want {
		t.Errorf("resolveProjectDeleteTarget([\"~/proj\"]) targeted %q, want %q", got, want)
	}
	if name != "proj" {
		t.Errorf("resolveProjectDeleteTarget([\"~/proj\"]) named the project %q, want %q", name, "proj")
	}
	if req.RepoID == "" {
		t.Error("resolveProjectDeleteTarget([\"~/proj\"]) left RepoID empty; the path did not resolve to a git repo")
	}
}

// TestResolveProjectDeleteTargetTildeNonRepoFallback pins the non-repo fallback
// (a moved/removed project). It must still expand the tilde: the fallback path
// is what the daemon sweeps, so "<cwd>/~/gone" would sweep the wrong entry.
func TestResolveProjectDeleteTargetTildeNonRepoFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	req, name, err := resolveProjectDeleteTarget([]string{"~/gone"})
	if err != nil {
		t.Fatalf("resolveProjectDeleteTarget([\"~/gone\"]) returned error: %v (#1842)", err)
	}
	assertNoTildeSegment(t, "~/gone", req.RepoPath)
	if want := filepath.Join(home, "gone"); req.RepoPath != want {
		t.Errorf("resolveProjectDeleteTarget([\"~/gone\"]) targeted %q, want %q", req.RepoPath, want)
	}
	if name != "gone" {
		t.Errorf("resolveProjectDeleteTarget([\"~/gone\"]) named the project %q, want %q", name, "gone")
	}
}

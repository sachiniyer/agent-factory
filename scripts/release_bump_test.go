package main

import (
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// --- release-bump.sh: the tag holds what was built (#7018) -----------------
//
// stable-release.yml builds every artifact from github.sha, then runs
// release-bump.sh and tags the commit HEAD is left on. v1.0.349 showed what
// goes wrong when that commit is not the built one: #7017 merged during the
// build, the bump was rebased onto it, and the tag and its generated notes
// carried a change the binaries did not. These tests pin the property from
// the issue, on real git: the tag's tree is the built tree plus the main.go
// edit, the notes range for this release excludes a mid-build merge while the
// next release's range includes it, master still gets the bump, and the tag
// stays reachable from master so previews and GitHub's notes still find it.

// isAncestor reports whether a is an ancestor of (or equal to) b in dir.
func isAncestor(t *testing.T, dir string, env []string, a, b string) bool {
	t.Helper()
	cmd := exec.Command("git", "merge-base", "--is-ancestor", a, b)
	cmd.Dir = dir
	cmd.Env = env
	err := cmd.Run()
	if err == nil {
		return true
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false
	}
	t.Fatalf("git merge-base --is-ancestor %s %s (in %s): %v", a, b, dir, err)
	return false
}

// landOnMaster commits file=content on origin's current master from a fresh
// clone named name, the way a PR squash-merge lands, and returns the new sha.
func landOnMaster(t *testing.T, base string, env []string, origin, name, file, content, msg string) string {
	t.Helper()
	clone := filepath.Join(base, name)
	runGitEnv(t, base, env, "clone", "-q", origin, clone)
	writeFile(t, filepath.Join(clone, file), content)
	runGitEnv(t, clone, env, "add", file)
	runGitEnv(t, clone, env, "commit", "-q", "-m", msg)
	runGitEnv(t, clone, env, "push", "-q", "origin", "master")
	return gitOutEnv(t, clone, env, "rev-parse", "HEAD")
}

// checkoutBuild stands in for a fresh "Bump and publish" runner: a full clone
// of origin, detached at the built commit, as actions/checkout leaves it.
func checkoutBuild(t *testing.T, base string, env []string, origin, name, built string) string {
	t.Helper()
	dir := filepath.Join(base, name)
	runGitEnv(t, base, env, "clone", "-q", origin, dir)
	runGitEnv(t, dir, env, "checkout", "-q", "--detach", built)
	return dir
}

// runBump runs release-bump.sh in dir and fails the test if it does not exit 0.
func runBump(t *testing.T, dir string, env []string, version string) string {
	t.Helper()
	out, timedOut, err := runReleaseScript(t, dir, env, version)
	if timedOut {
		t.Fatalf("release-bump.sh did not terminate\n%s", out)
	}
	if err != nil {
		t.Fatalf("release-bump.sh failed: %v\n%s", err, out)
	}
	return out
}

// assertTagIsTheBuild checks the commit release-bump.sh left at HEAD in dir —
// the sha the workflow passes to `gh release create --target` — against the
// built commit: its tree is the built tree apart from main.go, main.go carries
// version, and it sits on origin's master. Returns that sha.
func assertTagIsTheBuild(t *testing.T, dir, origin string, env []string, built, version string) string {
	t.Helper()
	tag := gitOutEnv(t, dir, env, "rev-parse", "HEAD")
	if tag != built {
		if parent := gitOutEnv(t, dir, env, "rev-parse", tag+"^"); parent != built {
			t.Fatalf("the tagged commit %s is parented on %s, not on the built commit %s (#7018)", tag, parent, built)
		}
	}
	if diff := gitOutEnv(t, dir, env, "diff", "--name-only", built, tag); diff != "main.go" && diff != "" {
		t.Fatalf("the tagged tree differs from the built tree outside main.go (#7018):\n%s", diff)
	}
	if got := gitOutEnv(t, dir, env, "show", tag+":main.go"); !strings.Contains(got, `version = "`+version+`"`) {
		t.Fatalf("the tagged main.go does not carry %s:\n%s", version, got)
	}
	if !isAncestor(t, origin, env, tag, "master") {
		t.Fatalf("the tagged commit %s is not reachable from master: previews and GitHub's generated notes would not find the new stable", tag)
	}
	return tag
}

// TestReleaseBumpTagsTheBuiltCommitWhenMasterMoves is the #7018 regression,
// shaped like v1.0.349: one PR shipped before the build, another merged during
// it. The tag must hold the first and not the second, the notes range from the
// previous release must list only what was built, and the mid-build PR must
// fall into the next release's range instead.
func TestReleaseBumpTagsTheBuiltCommitWhenMasterMoves(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("release-bump.sh is POSIX sh; not run on Windows")
	}
	base := t.TempDir()
	env := isolatedGitEnv(filepath.Join(base, "home"))

	origin, seedWork := seedReleaseRemote(t, base, env, "1.0.0")
	previous := gitOutEnv(t, seedWork, env, "rev-parse", "HEAD") // v1.0.0 was released here
	built := landOnMaster(t, base, env, origin, "shipped", "SHIPPED.md", "in the build\n", "feat: shipped in this release")
	work := checkoutBuild(t, base, env, origin, "build", built)
	midBuild := landOnMaster(t, base, env, origin, "other", "CONCURRENT.md", "landed mid-build\n", "feat: merged during build")

	out := runBump(t, work, env, "1.0.1")
	if !strings.Contains(out, "master advanced during the build") {
		t.Fatalf("a non-fast-forward push was not reported as master advancing:\n%s", out)
	}

	tag := assertTagIsTheBuild(t, work, origin, env, built, "1.0.1")
	if isAncestor(t, origin, env, midBuild, tag) {
		t.Fatalf("the mid-build commit %s is in the tagged history (#7018)", midBuild)
	}

	// --generate-notes lists previous..tag. It must name what was built and
	// nothing that merged after the build started.
	notes := gitOutEnv(t, origin, env, "log", "--format=%s", previous+".."+tag)
	if !strings.Contains(notes, "feat: shipped in this release") || strings.Contains(notes, "feat: merged during build") {
		t.Fatalf("this release's notes range is wrong (#7018):\n%s", notes)
	}
	// The next release's range starts at this tag, so the mid-build PR is
	// listed there, once, and nothing already shipped is listed again.
	next := gitOutEnv(t, origin, env, "log", "--format=%s", tag+"..master")
	if !strings.Contains(next, "feat: merged during build") || strings.Contains(next, "feat: shipped in this release") {
		t.Fatalf("the next release's notes range is wrong (#7018):\n%s", next)
	}

	// master got the bump, kept the concurrent work, and gained nothing else:
	// the commit that landed differs from the mid-build tip only in main.go.
	if got := gitOutEnv(t, origin, env, "show", "master:main.go"); !strings.Contains(got, `version = "1.0.1"`) {
		t.Fatalf("main.go on master was not bumped to 1.0.1:\n%s", got)
	}
	if diff := gitOutEnv(t, origin, env, "diff", "--name-only", midBuild, "master"); diff != "main.go" {
		t.Fatalf("landing the bump changed more than main.go on master:\n%s", diff)
	}
	if tags := gitOutEnv(t, origin, env, "tag", "-l", "v*"); tags != "" {
		t.Fatalf("a tag was pushed — tagging belongs to the release itself (#5159): %q", tags)
	}

	// Previews find the new stable (#1041): auto-release.yml checks out master
	// and asks latest-released-tag.sh for its baseline. Create the tags the two
	// releases would have created and ask it from master's tip.
	preview := checkoutBuild(t, base, env, origin, "preview", "origin/master")
	runGitEnv(t, preview, env, "tag", "v1.0.0", previous)
	runGitEnv(t, preview, env, "tag", "v1.0.1", tag)
	if got, err := runLatestReleasedTag(t, preview, env, []string{"v1.0.0", "v1.0.1"}); err != nil || got != "v1.0.1" {
		t.Fatalf("latest released tag from master = %q (err %v), want v1.0.1", got, err)
	}
}

// TestReleaseBumpFastForwardsWhenMasterHasNotMoved is the common case: nothing
// merged during the build, so the bump commit lands on master as-is. History
// stays linear — the merge exists only for a master that moved.
func TestReleaseBumpFastForwardsWhenMasterHasNotMoved(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("release-bump.sh is POSIX sh; not run on Windows")
	}
	base := t.TempDir()
	env := isolatedGitEnv(filepath.Join(base, "home"))

	origin, work := seedReleaseRemote(t, base, env, "1.0.0")
	built := gitOutEnv(t, work, env, "rev-parse", "HEAD")

	out := runBump(t, work, env, "1.0.1")
	if strings.Contains(out, "master advanced") {
		t.Fatalf("a push that landed first time was reported as master advancing:\n%s", out)
	}
	tag := assertTagIsTheBuild(t, work, origin, env, built, "1.0.1")
	if tip := gitOutEnv(t, origin, env, "rev-parse", "master"); tip != tag {
		t.Fatalf("master is %s, want the bump commit %s itself", tip, tag)
	}
	if merges := gitOutEnv(t, origin, env, "rev-list", "--merges", "master"); merges != "" {
		t.Fatalf("a merge commit landed although master never moved:\n%s", merges)
	}
}

// TestReleaseBumpRerunReproducesTheTag covers re-running after a run that landed
// the bump on master and then died before publishing. "Re-run failed jobs"
// keeps github.sha but runs on a fresh runner at a later time; the re-run must
// tag the same commit and push nothing new — no second bump, no second merge.
// A fresh dispatch instead builds master's new tip, which already carries the
// bump, so it makes no commit and tags the built commit itself.
func TestReleaseBumpRerunReproducesTheTag(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("release-bump.sh is POSIX sh; not run on Windows")
	}
	for _, moved := range []bool{false, true} {
		name := "master still"
		if moved {
			name = "master moved"
		}
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			home := filepath.Join(base, "home")
			env := isolatedGitEnv(home)

			origin, work := seedReleaseRemote(t, base, env, "1.0.0")
			built := gitOutEnv(t, work, env, "rev-parse", "HEAD")
			if moved {
				landOnMaster(t, base, env, origin, "other", "CONCURRENT.md", "landed mid-build\n", "feat: merged during build")
			}

			// The two runs happen at different times; nothing about the
			// commit they tag may depend on when.
			firstEnv := isolatedGitEnv(home, "GIT_AUTHOR_DATE=2021-01-01T00:00:00", "GIT_COMMITTER_DATE=2021-01-01T00:00:00")
			runBump(t, work, firstEnv, "1.0.1")
			firstTag := assertTagIsTheBuild(t, work, origin, env, built, "1.0.1")
			landed := gitOutEnv(t, origin, env, "rev-parse", "master")

			rerun := checkoutBuild(t, base, env, origin, "rerun", built)
			rerunEnv := isolatedGitEnv(home, "GIT_AUTHOR_DATE=2021-06-01T00:00:00", "GIT_COMMITTER_DATE=2021-06-01T00:00:00")
			runBump(t, rerun, rerunEnv, "1.0.1")
			if tag := assertTagIsTheBuild(t, rerun, origin, env, built, "1.0.1"); tag != firstTag {
				t.Fatalf("the re-run tags %s, the first run tagged %s: a re-run must reproduce the release commit", tag, firstTag)
			}
			if tip := gitOutEnv(t, origin, env, "rev-parse", "master"); tip != landed {
				t.Fatalf("the re-run moved master from %s to %s; it must find the bump already landed", landed, tip)
			}
			if log := gitOutEnv(t, origin, env, "log", "--format=%s", "master"); strings.Count(log, "chore: release v1.0.1") != 1 {
				t.Fatalf("expected exactly one bump commit on master:\n%s", log)
			}

			dispatch := checkoutBuild(t, base, env, origin, "dispatch", landed)
			runBump(t, dispatch, env, "1.0.1")
			if tag := assertTagIsTheBuild(t, dispatch, origin, env, landed, "1.0.1"); tag != landed {
				t.Fatalf("a fresh dispatch of the landed bump tags %s, want the built commit %s", tag, landed)
			}
			if tip := gitOutEnv(t, origin, env, "rev-parse", "master"); tip != landed {
				t.Fatalf("a fresh dispatch of the landed bump moved master from %s to %s", landed, tip)
			}
		})
	}
}

// TestReleaseBumpRefusesABuildThatIsNotOnMaster: landing the bump brings the
// tagged commit's history into master, so that history must already be
// master's. A build of a commit master does not contain — a dispatch from
// another branch — must fail before pushing, not land that branch's commits.
func TestReleaseBumpRefusesABuildThatIsNotOnMaster(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("release-bump.sh is POSIX sh; not run on Windows")
	}
	for _, moved := range []bool{false, true} {
		name := "master still"
		if moved {
			name = "master moved"
		}
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			env := isolatedGitEnv(filepath.Join(base, "home"))

			origin, work := seedReleaseRemote(t, base, env, "1.0.0")
			writeFile(t, filepath.Join(work, "SIDE.md"), "never reviewed\n")
			runGitEnv(t, work, env, "add", "SIDE.md")
			runGitEnv(t, work, env, "commit", "-q", "-m", "feat: unreviewed side work")
			if moved {
				landOnMaster(t, base, env, origin, "other", "CONCURRENT.md", "landed mid-build\n", "feat: merged during build")
			}
			before := gitOutEnv(t, origin, env, "rev-parse", "master")

			out, timedOut, err := runReleaseScript(t, work, env, "1.0.1")
			if timedOut {
				t.Fatalf("release-bump.sh did not terminate\n%s", out)
			}
			if err == nil {
				t.Fatalf("a build that is not on master was released:\n%s", out)
			}
			if !strings.Contains(out, "not on master") {
				t.Fatalf("the refusal does not say the build is not on master:\n%s", out)
			}
			if tip := gitOutEnv(t, origin, env, "rev-parse", "master"); tip != before {
				t.Fatalf("master moved from %s to %s for a build that is not on master", before, tip)
			}
		})
	}
}

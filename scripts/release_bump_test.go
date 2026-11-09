package main

import (
	"errors"
	"os"
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

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
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
	runGitEnv(t, clone, env, "push", "-q", "origin", "HEAD:refs/heads/master")
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
	if !isAncestor(t, origin, env, tag, "refs/heads/master") {
		t.Fatalf("the tagged commit %s is not reachable from master: previews and GitHub's generated notes would not find the new stable", tag)
	}
	return tag
}

// assertLandedAsMerge checks how the bump reached a master that moved: one
// merge commit whose FIRST parent is the tip it found and whose SECOND parent
// is the tagged commit. The order is load-bearing — master's first-parent line
// must stay master's own history, with the release joined in from the side.
func assertLandedAsMerge(t *testing.T, origin string, env []string, tip, tag string) {
	t.Helper()
	parents := strings.Fields(gitOutEnv(t, origin, env, "rev-list", "--parents", "-n", "1", "refs/heads/master"))
	if len(parents) != 3 || parents[1] != tip || parents[2] != tag {
		t.Fatalf("master's tip is not a merge of (%s, %s) in that order: commit and parents %v", tip, tag, parents)
	}
	if merges := strings.Fields(gitOutEnv(t, origin, env, "rev-list", "--merges", "refs/heads/master")); len(merges) != 1 {
		t.Fatalf("want exactly one merge commit on master, got %v", merges)
	}
	if diff := gitOutEnv(t, origin, env, "diff", "--name-only", tip, "refs/heads/master"); diff != "main.go" {
		t.Fatalf("landing the bump changed more than main.go on master:\n%s", diff)
	}
}

// assertCheckoutOnBuild checks a FAILED run put the checkout back on the built
// commit, clean. The workflow's `if: failure()` step runs
// release-forget-version.sh from this working tree, and every job pins
// github.sha: a run that died on master's tip or on its own bump commit would
// run some other commit's scripts.
func assertCheckoutOnBuild(t *testing.T, dir string, env []string, built string) {
	t.Helper()
	if head := gitOutEnv(t, dir, env, "rev-parse", "HEAD"); head != built {
		t.Fatalf("the failed run left the checkout on %s, not on the built commit %s", head, built)
	}
	if status := gitOutEnv(t, dir, env, "status", "--porcelain"); status != "" {
		t.Fatalf("the failed run left the checkout dirty:\n%s", status)
	}
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

	if !strings.Contains(out, "Merging the release commit") {
		t.Fatalf("the log does not say the bump is being merged into the moved master:\n%s", out)
	}

	tag := assertTagIsTheBuild(t, work, origin, env, built, "1.0.1")
	if isAncestor(t, origin, env, midBuild, tag) {
		t.Fatalf("the mid-build commit %s is in the tagged history (#7018)", midBuild)
	}
	assertLandedAsMerge(t, origin, env, midBuild, tag)

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
			out := runBump(t, rerun, rerunEnv, "1.0.1")
			// The log says what the re-run does: finds the commit landed — git's
			// own "up-to-date" when master is still on it, the script's line when
			// master moved on since — and never claims to be merging.
			landedMsg := strings.Contains(out, "master already contains") || strings.Contains(out, "Everything up-to-date")
			if !landedMsg || strings.Contains(out, "erging") {
				t.Fatalf("the re-run's log does not say it found the release commit already landed:\n%s", out)
			}
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
			side := gitOutEnv(t, work, env, "rev-parse", "HEAD")
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
			assertCheckoutOnBuild(t, work, env, side)
		})
	}
}

// TestReleaseBumpRetriesAMasterThatMovesAgain is a second real race: master
// moves during the build, and again between the merge and its push. The retry
// must merge into the NEWEST tip — the first, never-pushed merge is discarded —
// so master ends with one merge whose parents are that tip and the tag. A fake
// `git` lands the second racer just before the second push, then hands the
// push to the real binary, which the remote rejects for real.
func TestReleaseBumpRetriesAMasterThatMovesAgain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("release-bump.sh is POSIX sh; not run on Windows")
	}
	base := t.TempDir()
	home := filepath.Join(base, "home")
	env := isolatedGitEnv(home)

	origin, work := seedReleaseRemote(t, base, env, "1.0.0")
	built := gitOutEnv(t, work, env, "rev-parse", "HEAD")
	first := landOnMaster(t, base, env, origin, "other", "CONCURRENT.md", "landed mid-build\n", "feat: merged during build")

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("locate real git: %v", err)
	}
	pushes := filepath.Join(base, "pushes")
	writeFile(t, pushes, "0\n")
	racer := filepath.Join(base, "racer")
	fakeBin := filepath.Join(base, "fakebin")
	writeExecutable(t, filepath.Join(fakeBin, "git"), `#!/bin/sh
if [ "$1" = "push" ]; then
	n=$(cat "$FAKE_PUSHES")
	n=$((n + 1))
	echo "$n" > "$FAKE_PUSHES"
	if [ "$n" -eq 2 ]; then
		"$REAL_GIT" clone -q "$FAKE_ORIGIN" "$FAKE_RACER" &&
		echo "second race" > "$FAKE_RACER/RACE2.md" &&
		"$REAL_GIT" -C "$FAKE_RACER" add RACE2.md &&
		"$REAL_GIT" -C "$FAKE_RACER" commit -q -m "feat: merged during the retry" &&
		"$REAL_GIT" -C "$FAKE_RACER" push -q origin HEAD:refs/heads/master || exit 99
	fi
fi
exec "$REAL_GIT" "$@"
`)

	runBump(t, work, isolatedGitEnv(home,
		"REAL_GIT="+realGit,
		"FAKE_PUSHES="+pushes,
		"FAKE_ORIGIN="+origin,
		"FAKE_RACER="+racer,
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH")), "1.0.1")

	second := gitOutEnv(t, racer, env, "rev-parse", "HEAD")
	tag := assertTagIsTheBuild(t, work, origin, env, built, "1.0.1")
	assertLandedAsMerge(t, origin, env, second, tag)
	if !isAncestor(t, origin, env, first, "refs/heads/master") {
		t.Fatalf("the first mid-build commit %s was lost from master", first)
	}
	if n := strings.TrimSpace(readFile(t, pushes)); n != "3" {
		t.Fatalf("want 3 pushes (rejected, rejected, landed), got %s", n)
	}
}

// TestReleaseBumpQualifiesMastersRef: the fetch and push must name
// refs/heads/master. With a tag named `master` on origin, a bare `master` is
// ambiguous — the push fails "matches more than one", and a fetch could take
// the tag for the branch.
func TestReleaseBumpQualifiesMastersRef(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("release-bump.sh is POSIX sh; not run on Windows")
	}
	base := t.TempDir()
	env := isolatedGitEnv(filepath.Join(base, "home"))

	origin, work := seedReleaseRemote(t, base, env, "1.0.0")
	built := gitOutEnv(t, work, env, "rev-parse", "HEAD")
	runGitEnv(t, work, env, "tag", "master", built)
	runGitEnv(t, work, env, "push", "-q", "origin", "refs/tags/master")
	midBuild := landOnMaster(t, base, env, origin, "other", "CONCURRENT.md", "landed mid-build\n", "feat: merged during build")

	runBump(t, work, env, "1.0.1")
	tag := assertTagIsTheBuild(t, work, origin, env, built, "1.0.1")
	assertLandedAsMerge(t, origin, env, midBuild, tag)
	if got := gitOutEnv(t, origin, env, "rev-parse", "refs/tags/master"); got != built {
		t.Fatalf("the tag named master moved to %s", got)
	}
}

// TestReleaseBumpConflictNextToTheVersionLine: git conflicts on ADJACENT lines,
// not only the same one, and in the real main.go the version line is followed
// by `rootCommand = commands.NewRootCommand`. A mid-build PR that edits only
// that line fails the release; the error must not blame a rival version bump,
// and the checkout must go back to the build.
func TestReleaseBumpConflictNextToTheVersionLine(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("release-bump.sh is POSIX sh; not run on Windows")
	}
	base := t.TempDir()
	env := isolatedGitEnv(filepath.Join(base, "home"))

	origin, _ := seedReleaseRemote(t, base, env, "1.0.0")
	mainGo := func(root string) string {
		return "package main\n\nvar (\n\tversion     = \"1.0.0\"\n\trootCommand = " + root + "\n)\n"
	}
	built := landOnMaster(t, base, env, origin, "shape", "main.go", mainGo("commands.NewRootCommand"), "refactor: the real main.go shape")
	work := checkoutBuild(t, base, env, origin, "build", built)
	midBuild := landOnMaster(t, base, env, origin, "other", "main.go", mainGo("commands.NewRootCommandWithOptions"), "refactor: rename the root command")

	out, timedOut, err := runReleaseScript(t, work, env, "1.0.1")
	if timedOut {
		t.Fatalf("release-bump.sh did not terminate\n%s", out)
	}
	if err == nil {
		t.Fatalf("an adjacent-line conflict merged cleanly?\n%s", out)
	}
	if !strings.Contains(out, "at or next to the version line") || strings.Contains(out, "rival") {
		t.Fatalf("the conflict error does not describe an edit next to the version line:\n%s", out)
	}
	if tip := gitOutEnv(t, origin, env, "rev-parse", "refs/heads/master"); tip != midBuild {
		t.Fatalf("master moved from %s to %s on a conflict", midBuild, tip)
	}
	assertCheckoutOnBuild(t, work, env, built)
}

// TestReleaseBumpRefusesWhatItCannotCheck: the not-on-master guard fails
// closed, before pushing anything, when the checkout cannot answer the
// question — and says why, rather than claiming the build is off master.
func TestReleaseBumpRefusesWhatItCannotCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("release-bump.sh is POSIX sh; not run on Windows")
	}
	cases := []struct {
		name string
		// prepare returns the checkout to run in, standing in for a job whose
		// actions/checkout lost the history the check needs.
		prepare func(t *testing.T, base string, env []string, origin, work string) string
		want    string
	}{
		{
			name: "no origin/master",
			prepare: func(t *testing.T, _ string, env []string, _, work string) string {
				runGitEnv(t, work, env, "update-ref", "-d", "refs/remotes/origin/master")
				return work
			},
			want: "has no refs/remotes/origin/master",
		},
		{
			// A depth-1 checkout whose origin/master was also fetched at depth
			// 1: the ancestry walk stops at a grafted-away parent, and "not an
			// ancestor" would be the shallow history's answer, not master's.
			name: "shallow checkout",
			prepare: func(t *testing.T, base string, env []string, origin, _ string) string {
				shallow := filepath.Join(base, "shallow")
				runGitEnv(t, base, env, "clone", "-q", "--depth", "1", "file://"+origin, shallow)
				runGitEnv(t, shallow, env, "checkout", "-q", "--detach", "HEAD")
				landOnMaster(t, base, env, origin, "other", "CONCURRENT.md", "landed mid-build\n", "feat: merged during build")
				runGitEnv(t, shallow, env, "fetch", "-q", "--depth", "1", "origin")
				return shallow
			},
			want: "shallow",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base := t.TempDir()
			env := isolatedGitEnv(filepath.Join(base, "home"))
			origin, work := seedReleaseRemote(t, base, env, "1.0.0")
			dir := c.prepare(t, base, env, origin, work)
			built := gitOutEnv(t, dir, env, "rev-parse", "HEAD")
			before := gitOutEnv(t, origin, env, "rev-parse", "refs/heads/master")

			out, timedOut, err := runReleaseScript(t, dir, env, "1.0.1")
			if timedOut {
				t.Fatalf("release-bump.sh did not terminate\n%s", out)
			}
			if err == nil {
				t.Fatalf("released from a checkout that cannot prove the build is on master:\n%s", out)
			}
			if !strings.Contains(out, c.want) || strings.Contains(out, "is not on master") {
				t.Fatalf("the refusal does not name the cause (%q):\n%s", c.want, out)
			}
			if tip := gitOutEnv(t, origin, env, "rev-parse", "refs/heads/master"); tip != before {
				t.Fatalf("master moved from %s to %s", before, tip)
			}
			assertCheckoutOnBuild(t, dir, env, built)
		})
	}
}

// TestRequireOnMasterScript drives the guard the validate job runs on its own,
// before the preflight, with github.sha as the argument (#7018 follow-up).
func TestRequireOnMasterScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("require-on-master.sh is POSIX sh; not run on Windows")
	}
	base := t.TempDir()
	env := isolatedGitEnv(filepath.Join(base, "home"))
	origin, work := seedReleaseRemote(t, base, env, "1.0.0")
	behind := gitOutEnv(t, work, env, "rev-parse", "HEAD")
	landOnMaster(t, base, env, origin, "other", "CONCURRENT.md", "landed\n", "feat: later")
	runGitEnv(t, work, env, "fetch", "-q", "origin")
	writeFile(t, filepath.Join(work, "SIDE.md"), "side\n")
	runGitEnv(t, work, env, "add", "SIDE.md")
	runGitEnv(t, work, env, "commit", "-q", "-m", "feat: side")
	side := gitOutEnv(t, work, env, "rev-parse", "HEAD")

	script := filepath.Join(repoRoot(t), ".github", "scripts", "require-on-master.sh")
	for _, c := range []struct {
		name, commit, want string
		ok                 bool
	}{
		{name: "a commit master has moved past", commit: behind, ok: true},
		{name: "master's tip", commit: "origin/master", ok: true},
		{name: "a commit off master", commit: side, want: "is not on master"},
	} {
		cmd := exec.Command(script, c.commit)
		cmd.Dir = work
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if ok := err == nil; ok != c.ok {
			t.Errorf("%s: ok=%v, want %v\n%s", c.name, ok, c.ok, out)
		}
		if c.want != "" && !strings.Contains(string(out), c.want) {
			t.Errorf("%s: output missing %q:\n%s", c.name, c.want, out)
		}
	}
}

// workflowJob returns the lines of one job under `jobs:`, from its `  <name>:`
// key to the next job key, prefixed with "jobs:" so workflowSteps can split it.
func workflowJob(text, name string) string {
	var out []string
	in := false
	for _, line := range strings.Split(text, "\n") {
		isKey := strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") &&
			strings.HasSuffix(strings.TrimSpace(line), ":") && !strings.HasPrefix(strings.TrimSpace(line), "#")
		if isKey {
			in = strings.TrimSpace(line) == name+":"
		}
		if in {
			out = append(out, line)
		}
	}
	return "jobs:\n" + strings.Join(out, "\n")
}

// TestStableReleaseRefusesABuildOffMasterBeforeThePreflight: publish refuses
// a build that is not on master, but only after the 30-minute preflight and
// four builds. The validate job runs the same check first, on a checkout with
// the history it needs.
func TestStableReleaseRefusesABuildOffMasterBeforeThePreflight(t *testing.T) {
	text := readFile(t, filepath.Join(repoRoot(t), ".github", "workflows", "stable-release.yml"))
	steps := workflowSteps(workflowJob(text, "validate"))
	checkout, guard, preflight := -1, -1, -1
	for i, s := range steps {
		switch {
		case strings.Contains(s.title, "actions/checkout") && checkout < 0:
			checkout = i
		case strings.Contains(s.body, ".github/scripts/require-on-master.sh"):
			guard = i
		case s.title == "- name: Release preflight":
			preflight = i
		}
	}
	if checkout < 0 || preflight < 0 {
		t.Fatalf("the validate job lost its checkout (%d) or preflight (%d) step", checkout, preflight)
	}
	if guard < 0 || guard < checkout || guard > preflight {
		t.Fatalf("validate must run require-on-master.sh after its checkout and before the preflight (checkout %d, guard %d, preflight %d)", checkout, guard, preflight)
	}
	if !strings.Contains(steps[checkout].body, "fetch-depth: 0") {
		t.Fatalf("validate's checkout must fetch full history for the ancestry check:\n%s", steps[checkout].body)
	}
	if !strings.Contains(steps[guard].body, "github.sha") {
		t.Fatalf("validate's guard must check github.sha, the commit everything is built from:\n%s", steps[guard].body)
	}
}

// TestReleaseBumpRefusesADirtyCheckout: the script commits the index and, on a
// failing exit, force-checks-out the built commit — both are only safe on a
// tree whose tracked files hold nothing but the script's own edit. A run in a
// checkout with uncommitted tracked changes (a maintainer trying it locally)
// must refuse before touching anything: on master the staged file would ride
// into the release commit, and off master the failure cleanup would wipe the
// edits. Untracked files — the workflow's dist/ — are not changes to the build
// and stay allowed, untouched.
func TestReleaseBumpRefusesADirtyCheckout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("release-bump.sh is POSIX sh; not run on Windows")
	}
	for _, onMaster := range []bool{true, false} {
		name := "on master"
		if !onMaster {
			name = "not on master"
		}
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			env := isolatedGitEnv(filepath.Join(base, "home"))
			origin, _ := seedReleaseRemote(t, base, env, "1.0.0")
			built := landOnMaster(t, base, env, origin, "readme", "README.md", "original\n", "docs: readme")
			work := checkoutBuild(t, base, env, origin, "build", built)
			if !onMaster {
				writeFile(t, filepath.Join(work, "SIDE.md"), "side\n")
				runGitEnv(t, work, env, "add", "SIDE.md")
				runGitEnv(t, work, env, "commit", "-q", "-m", "feat: side")
			}
			head := gitOutEnv(t, work, env, "rev-parse", "HEAD")
			before := gitOutEnv(t, origin, env, "rev-parse", "refs/heads/master")

			writeFile(t, filepath.Join(work, "README.md"), "edited, not committed\n")
			writeFile(t, filepath.Join(work, "NEW.md"), "staged, not committed\n")
			runGitEnv(t, work, env, "add", "NEW.md")
			status := gitOutEnv(t, work, env, "status", "--porcelain")

			out, timedOut, err := runReleaseScript(t, work, env, "1.0.1")
			if timedOut {
				t.Fatalf("release-bump.sh did not terminate\n%s", out)
			}
			if err == nil {
				t.Fatalf("released from a checkout with uncommitted tracked changes:\n%s", out)
			}
			if got := readFile(t, filepath.Join(work, "README.md")); got != "edited, not committed\n" {
				t.Fatalf("the uncommitted README.md edit was lost: %q\n%s", got, out)
			}
			if got := gitOutEnv(t, work, env, "status", "--porcelain"); got != status {
				t.Fatalf("the refusal changed the checkout:\nbefore:\n%s\nafter:\n%s", status, got)
			}
			if !strings.Contains(out, "uncommitted changes") {
				t.Fatalf("the refusal does not name the uncommitted changes:\n%s", out)
			}
			if got := gitOutEnv(t, work, env, "rev-parse", "HEAD"); got != head {
				t.Fatalf("HEAD moved from %s to %s", head, got)
			}
			if tip := gitOutEnv(t, origin, env, "rev-parse", "refs/heads/master"); tip != before {
				t.Fatalf("master moved from %s to %s", before, tip)
			}
		})
	}

	t.Run("untracked files only", func(t *testing.T) {
		base := t.TempDir()
		env := isolatedGitEnv(filepath.Join(base, "home"))
		origin, work := seedReleaseRemote(t, base, env, "1.0.0")
		built := gitOutEnv(t, work, env, "rev-parse", "HEAD")
		writeFile(t, filepath.Join(work, "dist", "agent-factory-linux-amd64.tar.gz"), "artifact\n")

		runBump(t, work, env, "1.0.1")
		assertTagIsTheBuild(t, work, origin, env, built, "1.0.1")
		if got := readFile(t, filepath.Join(work, "dist", "agent-factory-linux-amd64.tar.gz")); got != "artifact\n" {
			t.Fatalf("the untracked artifact changed: %q", got)
		}
	})
}

// TestReleaseBumpAlreadyBumpedBuildOnAMovedMaster: a fresh dispatch whose
// built commit already carries the version makes no commit and tags the build
// itself. If master moved during that build, the push is rejected and the
// fetch finds the build already in master's history — and no earlier run
// landed anything, so the log must not say one did.
func TestReleaseBumpAlreadyBumpedBuildOnAMovedMaster(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("release-bump.sh is POSIX sh; not run on Windows")
	}
	base := t.TempDir()
	env := isolatedGitEnv(filepath.Join(base, "home"))
	origin, work := seedReleaseRemote(t, base, env, "1.0.1")
	built := gitOutEnv(t, work, env, "rev-parse", "HEAD")
	midBuild := landOnMaster(t, base, env, origin, "other", "CONCURRENT.md", "landed mid-build\n", "feat: merged during build")

	out := runBump(t, work, env, "1.0.1")
	if tag := assertTagIsTheBuild(t, work, origin, env, built, "1.0.1"); tag != built {
		t.Fatalf("an already-bumped build tagged %s, want the built commit %s", tag, built)
	}
	if tip := gitOutEnv(t, origin, env, "rev-parse", "refs/heads/master"); tip != midBuild {
		t.Fatalf("master moved from %s to %s; there was nothing to push", midBuild, tip)
	}
	if !strings.Contains(out, "master already contains") || strings.Contains(out, "earlier run") {
		t.Fatalf("the log does not say plainly that master already contains the build:\n%s", out)
	}
}

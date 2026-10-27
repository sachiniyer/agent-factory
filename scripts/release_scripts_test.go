package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// These tests exercise the release version-computation scripts used by the
// GitHub workflows (#1041). They are hermetic: tags are fed on stdin, no git
// or network involved.

// repoRoot returns the repository root even when this package is tested from
// scripts/.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get cwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			if info, err := os.Stat(filepath.Join(dir, ".github", "scripts")); err == nil && info.IsDir() {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find repo root from %s", dir)
		}
		dir = parent
	}
}

// runVersionScript runs .github/scripts/<script> with args, feeding tags
// (one per line) on stdin. Returns trimmed stdout and any error.
func runVersionScript(t *testing.T, script string, args []string, tags []string) (string, error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("release scripts are POSIX sh; not run on Windows")
	}
	path := filepath.Join(repoRoot(t), ".github", "scripts", script)
	cmd := exec.Command("sh", append([]string{path}, args...)...)
	cmd.Stdin = strings.NewReader(strings.Join(tags, "\n") + "\n")
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func TestNextPreviewVersionScript(t *testing.T) {
	cases := []struct {
		name string
		tags []string
		want string
	}{
		{
			name: "first preview after a stable",
			tags: []string{"v1.0.136", "v1.0.137"},
			want: "1.0.138-preview-1",
		},
		{
			name: "increments z within a base",
			tags: []string{"v1.0.137", "v1.0.138-preview-1", "v1.0.138-preview-2"},
			want: "1.0.138-preview-3",
		},
		{
			name: "z compares numerically not lexically",
			tags: []string{"v1.0.137", "v1.0.138-preview-9", "v1.0.138-preview-10"},
			want: "1.0.138-preview-11",
		},
		{
			name: "stable compares numerically not lexically",
			tags: []string{"v1.0.9", "v1.0.10"},
			want: "1.0.11-preview-1",
		},
		{
			name: "z resets when a new stable changes the base",
			tags: []string{"v1.0.137", "v1.0.138-preview-5", "v1.1.0"},
			want: "1.1.1-preview-1",
		},
		{
			name: "promoted preview base moves previews to the next patch",
			tags: []string{"v1.0.137", "v1.0.138-preview-3", "v1.0.138"},
			want: "1.0.139-preview-1",
		},
		{
			name: "ignores tags matching neither channel",
			tags: []string{"foo", "v1.2", "v1.0.138-rc-1", "v1.0.137", "v1.0.138-preview-x"},
			want: "1.0.138-preview-1",
		},
		{
			name: "no tags at all",
			tags: []string{},
			want: "0.0.1-preview-1",
		},
	}
	for _, c := range cases {
		got, err := runVersionScript(t, "next-preview-version.sh", nil, c.tags)
		if err != nil {
			t.Errorf("%s: script failed: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: next preview = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestValidateStableVersionScript(t *testing.T) {
	cases := []struct {
		name    string
		version string
		tags    []string
		wantOK  bool
	}{
		{
			name:    "valid bump over latest stable",
			version: "1.1.0",
			tags:    []string{"v1.0.136", "v1.0.137"},
			wantOK:  true,
		},
		{
			name:    "promoting the current preview base is allowed",
			version: "1.0.138",
			tags:    []string{"v1.0.137", "v1.0.138-preview-9"},
			wantOK:  true,
		},
		{
			name:    "equal to latest stable is rejected",
			version: "1.0.137",
			tags:    []string{"v1.0.136", "v1.0.137"},
			wantOK:  false,
		},
		{
			name:    "lower than latest stable is rejected",
			version: "1.0.100",
			tags:    []string{"v1.0.137"},
			wantOK:  false,
		},
		{
			name:    "leading v is rejected",
			version: "v1.1.0",
			tags:    []string{"v1.0.137"},
			wantOK:  false,
		},
		{
			name:    "two-part version is rejected",
			version: "1.1",
			tags:    []string{"v1.0.137"},
			wantOK:  false,
		},
		{
			name:    "already-tagged version is rejected",
			version: "1.1.0",
			tags:    []string{"v1.0.137", "v1.1.0"},
			wantOK:  false,
		},
		{
			name:    "numeric comparison against latest stable",
			version: "1.0.11",
			tags:    []string{"v1.0.9", "v1.0.10"},
			wantOK:  true,
		},
	}
	for _, c := range cases {
		_, err := runVersionScript(t, "validate-stable-version.sh", []string{c.version}, c.tags)
		if ok := err == nil; ok != c.wantOK {
			t.Errorf("%s: validate %q ok=%v, want ok=%v (err: %v)", c.name, c.version, ok, c.wantOK, err)
		}
	}
}

// --- release-bump.sh: the #1927 mid-build merge race ------------------------
//
// The stable-release "Bump and publish" job checks out github.sha, builds for
// ~4 minutes, then bumps main.go and pushes to master. master advances during
// that window (the auto-gate squash-merges CI-green PRs continuously), so a
// plain `git push origin HEAD:master` races that traffic and dies
// non-fast-forward after four good builds (#1927). These tests stand up a real
// bare "remote" + working checkout, land a commit on master mid-window, and
// assert the release still lands the bump on the moved tip.
//
// The script no longer tags (#5159): `gh release create --target` creates the
// tag atomically with the published release, so the tests assert the bump is
// pushed and HEAD — the sha the workflow tags — sits on origin's master tip,
// with NO tag on the remote either way.

// isolatedGitEnv returns an environment that pins git to a throwaway HOME (no
// user/system config — no signing, no hooks) and a deterministic identity, so
// the release script's commits are reproducible and hermetic. Extra "KEY=val"
// entries (e.g. GIT_*_DATE) may be appended.
func isolatedGitEnv(home string, extra ...string) []string {
	env := append(os.Environ(),
		"HOME="+home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Release Bot",
		"GIT_AUTHOR_EMAIL=release@example.com",
		"GIT_COMMITTER_NAME=Release Bot",
		"GIT_COMMITTER_EMAIL=release@example.com",
		"GIT_TERMINAL_PROMPT=0",
	)
	return append(env, extra...)
}

func runGitEnv(t *testing.T, dir string, env []string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

func gitOutEnv(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s (in %s): %v", strings.Join(args, " "), dir, err)
	}
	return strings.TrimSpace(string(out))
}

// seedReleaseRemote creates a bare "origin" repo whose master carries a main.go
// pinned to seedVersion, plus a detached "work" checkout of that tip standing
// in for the stable-release job's `checkout ref: github.sha`. It returns the
// bare remote path and the work-tree path.
func seedReleaseRemote(t *testing.T, base string, env []string, seedVersion string) (origin, work string) {
	t.Helper()
	origin = filepath.Join(base, "origin.git")
	seed := filepath.Join(base, "seed")
	work = filepath.Join(base, "work")

	runGitEnv(t, base, env, "init", "--bare", "-b", "master", origin)
	runGitEnv(t, base, env, "init", "-b", "master", seed)
	writeFile(t, filepath.Join(seed, "main.go"),
		"package main\n\nconst (\n\tversion = \""+seedVersion+"\"\n)\n")
	runGitEnv(t, seed, env, "add", "main.go")
	runGitEnv(t, seed, env, "commit", "-m", "seed")
	runGitEnv(t, seed, env, "remote", "add", "origin", origin)
	runGitEnv(t, seed, env, "push", "origin", "master")

	runGitEnv(t, base, env, "clone", origin, work)
	// The job checks out an immutable github.sha, not a tracking branch.
	runGitEnv(t, work, env, "checkout", "--detach", "HEAD")
	return origin, work
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestReleaseBumpToleratesMidBuildMerge is the #1927 regression: a PR merges to
// master while the release is building, and the version-bump push must land on
// the moved tip instead of aborting non-fast-forward.
func TestReleaseBumpToleratesMidBuildMerge(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("release-bump.sh is POSIX sh; not run on Windows")
	}
	script := filepath.Join(repoRoot(t), ".github", "scripts", "release-bump.sh")
	base := t.TempDir()
	home := filepath.Join(base, "home")
	env := isolatedGitEnv(home)

	origin, work := seedReleaseRemote(t, base, env, "1.0.0")

	// A PR squash-merges to master DURING the build window — after `work` was
	// checked out. It touches a different file, so the one-line bump rebases
	// cleanly (as any real merge would; only a rival release touches the
	// version line).
	other := filepath.Join(base, "other")
	runGitEnv(t, base, env, "clone", origin, other)
	writeFile(t, filepath.Join(other, "CONCURRENT.md"), "landed mid-build\n")
	runGitEnv(t, other, env, "add", "CONCURRENT.md")
	runGitEnv(t, other, env, "commit", "-m", "feat: merged during build")
	runGitEnv(t, other, env, "push", "origin", "master")

	// Run the release bump against the now-stale checkout.
	cmd := exec.Command(script, "1.0.1")
	cmd.Dir = work
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("release-bump.sh failed on a mid-build merge (#1927): %v\n%s", err, out)
	}

	// The bump landed on the NEW tip: master carries both commits, main.go is
	// bumped, and the concurrent work survived.
	masterLog := gitOutEnv(t, origin, env, "log", "--format=%s", "master")
	if !strings.Contains(masterLog, "feat: merged during build") {
		t.Fatalf("concurrent commit was lost from master:\n%s", masterLog)
	}
	if !strings.Contains(masterLog, "chore: release v1.0.1") {
		t.Fatalf("bump commit did not land on master:\n%s", masterLog)
	}
	if got := gitOutEnv(t, origin, env, "show", "master:main.go"); !strings.Contains(got, `version = "1.0.1"`) {
		t.Fatalf("main.go on master was not bumped to 1.0.1:\n%s", got)
	}
	if got := gitOutEnv(t, origin, env, "show", "master:CONCURRENT.md"); !strings.Contains(got, "landed mid-build") {
		t.Fatalf("concurrent file content missing from master: %q", got)
	}

	// HEAD is exactly what landed on master — the sha the workflow passes to
	// `gh release create --target` — and NO tag was pushed: the tag is created
	// atomically with the release (#5159).
	if head, tip := gitOutEnv(t, work, env, "rev-parse", "HEAD"),
		gitOutEnv(t, origin, env, "rev-parse", "master"); head != tip {
		t.Fatalf("HEAD (%s) does not match landed master tip (%s)", head, tip)
	}
	if tags := gitOutEnv(t, origin, env, "tag", "-l", "v*"); tags != "" {
		t.Fatalf("a tag was pushed — tagging moved to the release itself (#5159): %q", tags)
	}
}

// TestReleaseBumpIdempotentWhenBumpAlreadyLanded covers re-running the job after
// a previous run landed the bump commit on master but died before the release
// was published: the re-run must converge (drop its duplicate bump, report the
// landed commit) rather than double-bump or abort.
func TestReleaseBumpIdempotentWhenBumpAlreadyLanded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("release-bump.sh is POSIX sh; not run on Windows")
	}
	script := filepath.Join(repoRoot(t), ".github", "scripts", "release-bump.sh")
	base := t.TempDir()
	home := filepath.Join(base, "home")
	env := isolatedGitEnv(home)

	origin, work := seedReleaseRemote(t, base, env, "1.0.0")

	// A previous release run already landed the bump on master (no release
	// published — e.g. the run died between push and publish).
	// Give it a fixed EARLIER date so its commit hash differs from the re-run's
	// bump — otherwise both bumps collide to one object and the rebase-drop
	// path is never exercised.
	other := filepath.Join(base, "other")
	runGitEnv(t, base, env, "clone", origin, other)
	writeFile(t, filepath.Join(other, "main.go"),
		"package main\n\nconst (\n\tversion = \"1.0.1\"\n)\n")
	prevEnv := isolatedGitEnv(home,
		"GIT_AUTHOR_DATE=2020-01-01T00:00:00", "GIT_COMMITTER_DATE=2020-01-01T00:00:00")
	runGitEnv(t, other, prevEnv, "add", "main.go")
	runGitEnv(t, other, prevEnv, "commit", "-m", "chore: release v1.0.1")
	runGitEnv(t, other, prevEnv, "push", "origin", "master")

	// Re-run the job (later fixed date → distinct bump commit).
	cmd := exec.Command(script, "1.0.1")
	cmd.Dir = work
	cmd.Env = isolatedGitEnv(home,
		"GIT_AUTHOR_DATE=2020-12-31T00:00:00", "GIT_COMMITTER_DATE=2020-12-31T00:00:00")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("release-bump.sh failed on an idempotent re-run: %v\n%s", err, out)
	}

	// Exactly one bump commit on master (no double-bump), HEAD sits on that
	// landed commit, and no tag was pushed — tagging happens at release
	// publish (#5159).
	masterLog := gitOutEnv(t, origin, env, "log", "--format=%s", "master")
	if n := strings.Count(masterLog, "chore: release v1.0.1"); n != 1 {
		t.Fatalf("expected exactly one bump commit on master, got %d:\n%s", n, masterLog)
	}
	if got := gitOutEnv(t, origin, env, "show", "master:main.go"); !strings.Contains(got, `version = "1.0.1"`) {
		t.Fatalf("main.go on master is not 1.0.1:\n%s", got)
	}
	if head, tip := gitOutEnv(t, work, env, "rev-parse", "HEAD"),
		gitOutEnv(t, origin, env, "rev-parse", "master"); head != tip {
		t.Fatalf("HEAD (%s) does not match landed master tip (%s)", head, tip)
	}
	if tags := gitOutEnv(t, origin, env, "tag", "-l", "v*"); tags != "" {
		t.Fatalf("a tag was pushed — tagging moved to the release itself (#5159): %q", tags)
	}
}

// runReleaseScript runs release-bump.sh under a hard timeout so a
// regressed unbounded retry loop is killed and reported rather than hanging the
// whole suite. Returns combined output, whether the deadline was hit, and the
// run error.
func runReleaseScript(t *testing.T, dir string, env []string, version string) (out string, timedOut bool, err error) {
	t.Helper()
	script := filepath.Join(repoRoot(t), ".github", "scripts", "release-bump.sh")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, script, version)
	cmd.Dir = dir
	cmd.Env = env
	b, runErr := cmd.CombinedOutput()
	return string(b), ctx.Err() == context.DeadlineExceeded, runErr
}

// TestReleaseBumpBoundsRetriesOnPersistentReject locks the retry BOUND: with a
// remote that rejects every push to master (a racer that never lets the push
// through) and RELEASE_PUSH_MAX_ATTEMPTS=1, the script must give up non-zero —
// not spin forever — and must leave the remote pristine (no bump on master, no
// tag). This fails CI if a future edit drops the MAX_ATTEMPTS guard and
// reintroduces an unbounded loop (the run would hang and hit the timeout).
func TestReleaseBumpBoundsRetriesOnPersistentReject(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("release-bump.sh is POSIX sh; not run on Windows")
	}
	base := t.TempDir()
	home := filepath.Join(base, "home")
	env := isolatedGitEnv(home)

	origin, work := seedReleaseRemote(t, base, env, "1.0.0")

	// A pre-receive hook that declines every push stands in for a racer that
	// keeps the master push rejecting no matter how often we rebase and retry.
	writeExecutable(t, filepath.Join(origin, "hooks", "pre-receive"),
		"#!/bin/sh\necho 'push rejected by test (persistent racer)' >&2\nexit 1\n")
	masterBefore := gitOutEnv(t, origin, env, "rev-parse", "master")

	out, timedOut, err := runReleaseScript(t, work,
		isolatedGitEnv(home, "RELEASE_PUSH_MAX_ATTEMPTS=1"), "1.0.1")
	if timedOut {
		t.Fatalf("script did not terminate — the retry loop is unbounded?\n%s", out)
	}
	if err == nil {
		t.Fatalf("expected non-zero exit when the push keeps rejecting; got success\n%s", out)
	}

	// The remote stayed pristine: master unmoved, no bump commit, no tag.
	if tip := gitOutEnv(t, origin, env, "rev-parse", "master"); tip != masterBefore {
		t.Fatalf("master moved (%s → %s) despite every push being rejected", masterBefore, tip)
	}
	if log := gitOutEnv(t, origin, env, "log", "--format=%s", "master"); strings.Contains(log, "chore: release") {
		t.Fatalf("a bump commit reached master despite exhaustion:\n%s", log)
	}
	if tags := gitOutEnv(t, origin, env, "tag", "-l", "v*"); tags != "" {
		t.Fatalf("a tag was pushed despite the push never landing: %q", tags)
	}
}

// TestReleaseBumpFailsLoudlyOnGenuineConflict locks the fail-loud behavior when
// the rebase genuinely conflicts — a rival commit rewrote main.go's version
// line, so the one-line bump cannot replay. The script must abort non-zero and
// must NOT clobber the rival's commit (no force-push) or push a tag. This fails
// CI if someone "hardens" the loop into a silent `git push --force` over
// another release.
func TestReleaseBumpFailsLoudlyOnGenuineConflict(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("release-bump.sh is POSIX sh; not run on Windows")
	}
	base := t.TempDir()
	home := filepath.Join(base, "home")
	env := isolatedGitEnv(home)

	origin, work := seedReleaseRemote(t, base, env, "1.0.0")

	// A rival release lands on master mid-build and rewrites the SAME version
	// line to a different value, so our bump's rebase must conflict.
	rival := filepath.Join(base, "rival")
	runGitEnv(t, base, env, "clone", origin, rival)
	writeFile(t, filepath.Join(rival, "main.go"),
		"package main\n\nconst (\n\tversion = \"2.0.0\"\n)\n")
	runGitEnv(t, rival, env, "add", "main.go")
	runGitEnv(t, rival, env, "commit", "-m", "chore: release v2.0.0")
	runGitEnv(t, rival, env, "push", "origin", "master")
	rivalTip := gitOutEnv(t, origin, env, "rev-parse", "master")

	out, timedOut, err := runReleaseScript(t, work, env, "1.0.1")
	if timedOut {
		t.Fatalf("script hung on a conflict instead of failing fast\n%s", out)
	}
	if err == nil {
		t.Fatalf("expected non-zero exit on a genuine version-line conflict; got success\n%s", out)
	}

	// The rival was preserved — master was neither overwritten nor advanced by
	// our bump, and no tag was pushed.
	if tip := gitOutEnv(t, origin, env, "rev-parse", "master"); tip != rivalTip {
		t.Fatalf("master was force-overwritten: %s (rival tip was %s)", tip, rivalTip)
	}
	if got := gitOutEnv(t, origin, env, "show", "master:main.go"); !strings.Contains(got, `version = "2.0.0"`) {
		t.Fatalf("rival's version was clobbered:\n%s", got)
	}
	if log := gitOutEnv(t, origin, env, "log", "--format=%s", "master"); strings.Contains(log, "chore: release v1.0.1") {
		t.Fatalf("our bump commit reached master despite the conflict:\n%s", log)
	}
	if tags := gitOutEnv(t, origin, env, "tag", "-l", "v*"); tags != "" {
		t.Fatalf("a tag was pushed despite the conflict: %q", tags)
	}
}

// --- latest-released-tag.sh: a stranded tag is not a baseline (#5159) -------
//
// The auto-release check resolves "latest" as the nearest tag with a
// PUBLISHED release, not the nearest tag on origin. A tag stranded without
// its release shipped no binaries, so commits must be counted from the last
// tag that actually shipped. These tests build a real repo whose tip tags are
// absent from the released-tags file (standing in for a pushed-but-released-
// never-happened tag) and assert the script walks past them.

// seedTagRepo creates a repo where commit i carries the tags tagsAt[i].
func seedTagRepo(t *testing.T, base string, env []string, commits []string, tagsAt map[int][]string) string {
	t.Helper()
	repo := filepath.Join(base, "repo")
	runGitEnv(t, base, env, "init", "-b", "master", repo)
	for i, msg := range commits {
		writeFile(t, filepath.Join(repo, "file.txt"), msg+"\n")
		runGitEnv(t, repo, env, "add", "file.txt")
		runGitEnv(t, repo, env, "commit", "-m", msg)
		for _, tag := range tagsAt[i] {
			runGitEnv(t, repo, env, "tag", tag)
		}
	}
	return repo
}

func runLatestReleasedTag(t *testing.T, repo string, env []string, released []string) (string, error) {
	t.Helper()
	script := filepath.Join(repoRoot(t), ".github", "scripts", "latest-released-tag.sh")
	releasedFile := filepath.Join(t.TempDir(), "released-tags.txt")
	content := ""
	if len(released) > 0 {
		content = strings.Join(released, "\n") + "\n"
	}
	writeFile(t, releasedFile, content)
	cmd := exec.Command(script, releasedFile)
	cmd.Dir = repo
	cmd.Env = env
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func TestLatestReleasedTag(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("latest-released-tag.sh is POSIX sh; not run on Windows")
	}
	base := t.TempDir()
	home := filepath.Join(base, "home")
	env := isolatedGitEnv(home)

	cases := []struct {
		name     string
		commits  []string
		tagsAt   map[int][]string
		released []string
		want     string
	}{
		{
			// The #5159 stranding: the tip carries a tag with no release. The
			// baseline must be the last tag that actually shipped.
			name:     "orphan tag at tip is skipped",
			commits:  []string{"c1", "c2"},
			tagsAt:   map[int][]string{0: {"v1.0.1"}, 1: {"v1.0.2-preview-1"}},
			released: []string{"v1.0.1"},
			want:     "v1.0.1",
		},
		{
			name:     "released tag at tip wins",
			commits:  []string{"c1", "c2"},
			tagsAt:   map[int][]string{0: {"v1.0.1"}, 1: {"v1.0.2-preview-1"}},
			released: []string{"v1.0.1", "v1.0.2-preview-1"},
			want:     "v1.0.2-preview-1",
		},
		{
			name:     "stacked orphans are all walked past",
			commits:  []string{"c1", "c2", "c3"},
			tagsAt:   map[int][]string{0: {"v1.0.1"}, 1: {"v1.0.2-preview-1"}, 2: {"v1.0.2-preview-2"}},
			released: []string{"v1.0.1"},
			want:     "v1.0.1",
		},
		{
			name:     "no released tags means no baseline",
			commits:  []string{"c1"},
			tagsAt:   map[int][]string{0: {"v1.0.1"}},
			released: nil,
			want:     "",
		},
		{
			name:     "no tags at all",
			commits:  []string{"c1"},
			released: []string{"v1.0.1"},
			want:     "",
		},
	}
	for _, c := range cases {
		repo := seedTagRepo(t, t.TempDir(), env, c.commits, c.tagsAt)
		got, err := runLatestReleasedTag(t, repo, env, c.released)
		if err != nil {
			t.Errorf("%s: script failed: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: latest released tag = %q, want %q", c.name, got, c.want)
		}
	}
}

// --- release-forget-version.sh: forget drafts and orphan refs, never --------
// --- published releases ------------------------------------------------------
//
// The publish jobs call this before `gh release create` (self-heal after a
// cancelled run) and on failure (leave nothing behind). It must delete a
// leftover draft release and an orphaned tag ref — but never a tag backed by
// a published release. A fake `gh` on PATH serves the release/ref lookups and
// records the DELETE calls.

// fakeGHEnv builds a fake `gh` executable on PATH answering the three calls
// release-forget-version.sh makes: the release-list lookup (fixture lines are
// "tag_name id draft"), the git-ref probe (names in refs exist; others 404),
// and any DELETE (recorded in callsFile).
func fakeGHEnv(t *testing.T, base string, releasesFixture, refs string) (env []string, callsFile string) {
	t.Helper()
	fakeBin := filepath.Join(base, "fakebin")
	fixture := filepath.Join(base, "releases.txt")
	callsFile = filepath.Join(base, "gh-calls")
	writeFile(t, fixture, releasesFixture)
	writeFile(t, callsFile, "")
	writeExecutable(t, filepath.Join(fakeBin, "gh"), `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_GH_CALLS"
[ "$1" = "api" ] || exit 2
shift
case "$*" in
	"-X DELETE"*)
		exit 0
		;;
	*"git/ref/tags/"*)
		tag="${*##*tags/}"
		for r in $FAKE_GH_TAG_REFS; do
			if [ "$r" = "$tag" ]; then
				printf '{"ref":"refs/tags/%s"}\n' "$tag"
				exit 0
			fi
		done
		echo "gh: Not Found (HTTP 404)" >&2
		exit 1
		;;
	*"releases?per_page="*)
		# The caller's --jq selects one tag_name and prints "id draft"; the
		# fixture is "tag_name id draft" per line, so emit the matching tail.
		tag=$(printf '%s\n' "$*" | sed -n 's/.*tag_name == "\([^"]*\)".*/\1/p')
		awk -v t="$tag" '$1 == t { print $2, $3 }' "$FAKE_GH_RELEASES"
		;;
	*)
		exit 2
		;;
esac
`)
	return append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_GH_CALLS="+callsFile,
		"FAKE_GH_RELEASES="+fixture,
		"FAKE_GH_TAG_REFS="+refs,
		"GH_REPO=owner/repo",
		"GH_TOKEN=fake",
	), callsFile
}

func TestReleaseForgetVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("release-forget-version.sh is POSIX sh; not run on Windows")
	}
	script := filepath.Join(repoRoot(t), ".github", "scripts", "release-forget-version.sh")

	cases := []struct {
		name       string
		version    string
		fixture    string // "tag_name id draft" lines the fake API lists
		refs       string // space-separated tag names that exist as git refs
		wantDelete []string
		wantLog    string // substring expected on stdout, "" for none
	}{
		{
			name:    "published release is left entirely alone",
			version: "1.0.1",
			fixture: "v1.0.1 777 false\n",
			refs:    "v1.0.1",
			wantLog: "nothing to forget",
		},
		{
			name:       "leftover draft release is deleted",
			version:    "1.0.2-preview-1",
			fixture:    "v1.0.2-preview-1 888 true\n",
			wantDelete: []string{"releases/888"},
			wantLog:    "Deleting leftover draft",
		},
		{
			// tag_name is not unique across drafts — every matching draft
			// must go, or the leftover still collides with the create.
			name:       "several drafts sharing a tag are all deleted",
			version:    "1.0.2-preview-1",
			fixture:    "v1.0.2-preview-1 888 true\nv1.0.2-preview-1 889 true\n",
			wantDelete: []string{"releases/888", "releases/889"},
		},
		{
			name:       "orphaned tag ref with no release is deleted",
			version:    "1.0.2-preview-1",
			refs:       "v1.0.2-preview-1",
			wantDelete: []string{"git/refs/tags/v1.0.2-preview-1"},
			wantLog:    "Deleting orphaned tag ref",
		},
		{
			name:       "draft plus stray ref deletes both",
			version:    "1.0.3",
			fixture:    "v1.0.3 999 true\n",
			refs:       "v1.0.3",
			wantDelete: []string{"releases/999", "git/refs/tags/v1.0.3"},
		},
		{
			name:    "no release and no ref is a no-op",
			version: "1.0.4",
		},
		{
			// The fixture carries other tags; only v1.0.5 is touched.
			name:       "only the named version is forgotten",
			version:    "1.0.5",
			fixture:    "v1.0.5 555 true\nv1.0.4 444 false\n",
			refs:       "v1.0.4 v1.0.5",
			wantDelete: []string{"releases/555", "git/refs/tags/v1.0.5"},
		},
	}
	for _, c := range cases {
		base := t.TempDir()
		env, callsFile := fakeGHEnv(t, base, c.fixture, c.refs)
		cmd := exec.Command(script, c.version)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Errorf("%s: script failed: %v\n%s", c.name, err, out)
			continue
		}
		if c.wantLog != "" && !strings.Contains(string(out), c.wantLog) {
			t.Errorf("%s: output missing %q:\n%s", c.name, c.wantLog, out)
		}
		calls, err := os.ReadFile(callsFile)
		if err != nil {
			t.Fatalf("%s: read calls file: %v", c.name, err)
		}
		for _, want := range c.wantDelete {
			if !strings.Contains(string(calls), "-X DELETE") ||
				!strings.Contains(string(calls), want) {
				t.Errorf("%s: expected DELETE %s; calls:\n%s", c.name, want, calls)
			}
		}
		if len(c.wantDelete) == 0 && strings.Contains(string(calls), "-X DELETE") {
			t.Errorf("%s: unexpected DELETE call:\n%s", c.name, calls)
		}
	}
}

func TestInstallScriptRestartsDaemonWithInstalledBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is POSIX sh; not run on Windows")
	}
	dir := t.TempDir()
	fakeBin := filepath.Join(dir, "fakebin")
	installDir := filepath.Join(dir, "install")
	callsFile := filepath.Join(dir, "af-calls")

	writeExecutable(t, filepath.Join(fakeBin, "uname"), `#!/bin/sh
case "$1" in
	-s) echo Linux ;;
	-m) echo x86_64 ;;
	*) exit 2 ;;
esac
`)
	writeExecutable(t, filepath.Join(fakeBin, "curl"), `#!/bin/sh
out=
while [ "$#" -gt 0 ]; do
	if [ "$1" = "-o" ]; then
		out="$2"
		break
	fi
	shift
done
[ -n "$out" ] || exit 2
case "$out" in
	*/sha256sums.txt)
		printf '%s\n' '746e8948ec0a7b6e150cc34446bad2bd0d0e2e3f0bd8f03f0edef7467a38e3b6  agent-factory-linux-amd64.tar.gz' > "$out"
		;;
	*)
		printf fake-tarball > "$out"
		;;
esac
`)
	writeExecutable(t, filepath.Join(fakeBin, "tar"), `#!/bin/sh
case "$1" in
	tzf)
		exit 0
		;;
	xzf)
		outdir=
		shift 2
		while [ "$#" -gt 0 ]; do
			if [ "$1" = "-C" ]; then
				outdir="$2"
				break
			fi
			shift
		done
		[ -n "$outdir" ] || exit 2
		cat > "$outdir/agent-factory" <<'AF'
#!/bin/sh
printf '%s\n' "$*" >> "$AF_FAKE_AF_CALLS"
if [ "$1" = "version" ]; then
	echo "agent-factory version fake"
fi
exit 0
AF
		chmod +x "$outdir/agent-factory"
		;;
	*)
		exit 2
		;;
esac
`)

	root := repoRoot(t)
	cmd := exec.Command("sh", filepath.Join(root, "install.sh"), "--version", "v9.9.9")
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"AF_INSTALL_DIR="+installDir,
		"AF_FAKE_AF_CALLS="+callsFile,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install.sh failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "warning: "+installDir+" is not on your PATH") {
		t.Errorf("install path notice must use the established lowercase warning marker:\n%s", out)
	}
	if strings.Contains(string(out), "NOTE:") {
		t.Errorf("install path notice contains caps-shouting:\n%s", out)
	}

	assertScriptRestartCall(t, callsFile)
}

func TestDevInstallScriptRestartsDaemonWithInstalledBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("dev-install.sh is POSIX sh; not run on Windows")
	}
	dir := t.TempDir()
	fakeBin := filepath.Join(dir, "fakebin")
	installDir := filepath.Join(dir, "install")
	callsFile := filepath.Join(dir, "af-calls")

	writeExecutable(t, filepath.Join(fakeBin, "go"), `#!/bin/sh
out=
while [ "$#" -gt 0 ]; do
	if [ "$1" = "-o" ]; then
		out="$2"
		break
	fi
	shift
done
[ -n "$out" ] || exit 2
cat > "$out" <<'AF'
#!/bin/sh
printf '%s\n' "$*" >> "$AF_FAKE_AF_CALLS"
if [ "$1" = "version" ]; then
	echo "agent-factory version fake"
fi
exit 0
AF
chmod +x "$out"
`)

	cmd := exec.Command("sh", filepath.Join(repoRoot(t), "dev-install.sh"))
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"BIN_DIR="+installDir,
		"AF_FAKE_AF_CALLS="+callsFile,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dev-install.sh failed: %v\n%s", err, out)
	}

	assertScriptRestartCall(t, callsFile)
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir fake tool dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		t.Fatalf("write fake tool %s: %v", path, err)
	}
}

func assertScriptRestartCall(t *testing.T, callsFile string) {
	t.Helper()
	raw, err := os.ReadFile(callsFile)
	if err != nil {
		t.Fatalf("read fake af calls: %v", err)
	}
	calls := string(raw)
	if !strings.Contains(calls, "version\n") {
		t.Fatalf("installed af version command was not called; calls:\n%s", calls)
	}
	if !strings.Contains(calls, "daemon restart --quiet\n") {
		t.Fatalf("installed af daemon restart --quiet was not called; calls:\n%s", calls)
	}
}

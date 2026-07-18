package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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

// --- release-bump-and-tag.sh: the #1927 mid-build merge race ---------------
//
// The stable-release "Tag and publish" job checks out github.sha, builds for
// ~4 minutes, then bumps main.go and pushes to master. master advances during
// that window (the auto-gate squash-merges CI-green PRs continuously), so a
// plain `git push origin HEAD:master` races that traffic and dies
// non-fast-forward after four good builds (#1927). These tests stand up a real
// bare "remote" + working checkout, land a commit on master mid-window, and
// assert the release still lands the bump on the moved tip.

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
		t.Skip("release-bump-and-tag.sh is POSIX sh; not run on Windows")
	}
	script := filepath.Join(repoRoot(t), ".github", "scripts", "release-bump-and-tag.sh")
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
		t.Fatalf("release-bump-and-tag.sh failed on a mid-build merge (#1927): %v\n%s", err, out)
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

	// The tag points at the released commit and is reachable from master, so a
	// tag never dangles ahead of what shipped.
	if tag, tip := gitOutEnv(t, origin, env, "rev-list", "-n", "1", "v1.0.1"),
		gitOutEnv(t, origin, env, "rev-parse", "master"); tag != tip {
		t.Fatalf("tag v1.0.1 (%s) does not point at master tip (%s)", tag, tip)
	}
}

// TestReleaseBumpIdempotentWhenBumpAlreadyLanded covers re-running the job after
// a previous run landed the bump commit on master but died before pushing the
// tag: the re-run must converge (drop its duplicate bump, tag the landed
// commit) rather than double-bump or abort.
func TestReleaseBumpIdempotentWhenBumpAlreadyLanded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("release-bump-and-tag.sh is POSIX sh; not run on Windows")
	}
	script := filepath.Join(repoRoot(t), ".github", "scripts", "release-bump-and-tag.sh")
	base := t.TempDir()
	home := filepath.Join(base, "home")
	env := isolatedGitEnv(home)

	origin, work := seedReleaseRemote(t, base, env, "1.0.0")

	// A previous release run already landed the bump on master (no tag pushed).
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
		t.Fatalf("release-bump-and-tag.sh failed on an idempotent re-run: %v\n%s", err, out)
	}

	// Exactly one bump commit on master (no double-bump), and the tag lands on
	// the already-published commit.
	masterLog := gitOutEnv(t, origin, env, "log", "--format=%s", "master")
	if n := strings.Count(masterLog, "chore: release v1.0.1"); n != 1 {
		t.Fatalf("expected exactly one bump commit on master, got %d:\n%s", n, masterLog)
	}
	if got := gitOutEnv(t, origin, env, "show", "master:main.go"); !strings.Contains(got, `version = "1.0.1"`) {
		t.Fatalf("main.go on master is not 1.0.1:\n%s", got)
	}
	if tag, tip := gitOutEnv(t, origin, env, "rev-list", "-n", "1", "v1.0.1"),
		gitOutEnv(t, origin, env, "rev-parse", "master"); tag != tip {
		t.Fatalf("tag v1.0.1 (%s) does not point at master tip (%s)", tag, tip)
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
printf fake-tarball > "$out"
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

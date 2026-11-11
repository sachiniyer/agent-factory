package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// These tests cover how testbox.sh builds its images (#7096).
//
// The bug: every routine build ran `$ENGINE build -q … >/dev/null`. `-q` drops
// the build's own log, so when a RUN failed the CI log carried the Dockerfile
// excerpt and `exit code: 100` and nothing else. On 2026-11-09 and again on
// 2026-11-11 (run 43033894945) the web-selftest image failed in `apt-get update`
// on all three attempts, and nobody could say whether the Ubuntu archive, its
// security pocket, or the NodeSource source the Playwright base ships was the
// one that failed — the lines that said so had been thrown away.
//
// The property: a build that succeeds stays quiet, and a build that fails prints
// what the build printed, so the failing layer's apt output is in the CI log.
//
// Both halves are asserted, because a fix that only made the builds loud would
// also put the failing lines in the log — and would bury every green run under
// a few hundred lines of layer-pull progress.

// fakeBuildEngine is a stand-in docker. A `build` prints apt-shaped lines to
// both streams (a real build writes its progress to stderr and the image ID to
// stdout, and a fix that captured only one would lose the other), records the
// build context it was handed, and fails when FAKE_BUILD_FAIL is set. Every
// other subcommand succeeds, and `inspect` answers "true" for the image-lease
// watcher, as in testbox_network_test.go.
const fakeBuildEngine = `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$FAKE_ENGINE_LOG"
case "${1:-}" in
build)
	ctx="${!#}"
	if [ "$ctx" = - ]; then
		echo "context: stdin" >> "$FAKE_ENGINE_LOG"
	else
		{
			printf 'context: %s\n' "$ctx"
			(cd "$ctx" && ls -A)
		} >> "$FAKE_ENGINE_LOG"
		mkdir -p "$FAKE_CONTEXT_COPY"
		cp -R "$ctx"/. "$FAKE_CONTEXT_COPY"/
	fi
	# -q is BuildKit's quiet progress: no step log on either stream, only the
	# image ID on success and the bare ERROR line on failure. That is what hid
	# the apt lines in run 43033894945, so the stand-in has to do it too.
	quiet=no
	for a in "$@"; do
		case "$a" in -q | --quiet) quiet=yes ;; esac
	done
	if [ "$quiet" = no ]; then
		echo "#6 [2/6] RUN apt-get update"
		echo "#6 1.203 Hit:1 http://archive.ubuntu.com/ubuntu noble InRelease" >&2
	fi
	if [ -n "${FAKE_BUILD_FAIL:-}" ]; then
		if [ "$quiet" = no ]; then
			echo "#6 1.871 E: Failed to fetch https://deb.nodesource.com/node_22.x/dists/nodistro/InRelease  403  Forbidden [IP: 192.0.2.7 443]" >&2
			echo "#6 1.872 E: The repository 'https://deb.nodesource.com/node_22.x nodistro InRelease' is no longer signed." >&2
		fi
		echo 'ERROR: failed to build: failed to solve: process "/bin/sh -c apt-get update" did not complete successfully: exit code: 100' >&2
		exit 1
	fi
	if [ "$quiet" = no ]; then
		echo "#9 writing image sha256:0123456789abcdef done"
	fi
	echo "sha256:0123456789abcdef"
	;;
inspect) printf 'true\n' ;;
esac
exit 0
`

type testboxRun struct {
	output string
	err    error
	// engine holds the fake engine's log: one argv line per call, plus the
	// build context each `build` was given.
	engine string
	// context is a copy of the last build context, for asserting what an image
	// can COPY from it.
	context string
}

func runTestboxWithFakeBuild(t *testing.T, fail bool, args ...string) testboxRun {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("testbox.sh requires bash")
	}
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", binDir, err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte(fakeBuildEngine), 0o755); err != nil {
		t.Fatalf("write fake engine: %v", err)
	}
	logPath := filepath.Join(tmp, "engine.log")
	contextCopy := filepath.Join(tmp, "context")

	cmd := exec.Command("bash", append([]string{filepath.Join(repoRoot(t), "scripts", "testbox.sh")}, args...)...)
	env := append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_ENGINE_LOG="+logPath,
		"FAKE_CONTEXT_COPY="+contextCopy,
		// A distinct tag, and never the box-wide image lease: see
		// testbox_network_test.go.
		"AF_TESTBOX_IMAGE=agent-factory-testbox-buildtest",
		"AF_TESTBOX_IMAGE_LOCK="+filepath.Join(tmp, "image.lock"),
		// The retry backoff is 5s then 10s; a unit test has no registry blip
		// to wait out.
		"AF_TESTBOX_BUILD_RETRY_DELAY=0",
		// Keep the build context, and any temp file the harness makes, under
		// this test's own directory so a leak is visible below.
		"TMPDIR="+tmp,
	)
	if fail {
		env = append(env, "FAKE_BUILD_FAIL=1")
	}
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()

	engine, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("read engine log (did the harness invoke the engine at all?): %v\n%s", readErr, out.String())
	}
	return testboxRun{output: out.String(), err: err, engine: string(engine), context: contextCopy}
}

// The bug in #7096, exactly: the web-selftest image fails in apt, and the job
// log must say which source failed.
func TestTestbox_AFailedImageBuildPrintsTheBuildOutput(t *testing.T) {
	run := runTestboxWithFakeBuild(t, true, "web-selftest")
	if run.err == nil {
		t.Fatalf("testbox.sh succeeded although every image build failed:\n%s", run.output)
	}
	for _, want := range []string{
		// stderr: the failing source, which is the line #7096 could not see.
		"403  Forbidden",
		"is no longer signed",
		// stdout: the step that was running.
		"RUN apt-get update",
	} {
		if !strings.Contains(run.output, want) {
			t.Errorf("a failed image build hid %q from the log; output:\n%s", want, run.output)
		}
	}
	// Every attempt's output, not just the last: a retry can fail differently
	// (a 403, then a timeout), and the first failure is often the informative one.
	if got := strings.Count(run.output, "403  Forbidden"); got != 3 {
		t.Errorf("printed the failing build's output %d time(s), want once per attempt (3); output:\n%s", got, run.output)
	}
}

// The other half: a build that succeeds says nothing. The routine builds were
// quiet on purpose, and a green run should not grow by a few hundred lines of
// layer-pull progress to buy diagnostics for a red one.
func TestTestbox_ASuccessfulImageBuildStaysQuiet(t *testing.T) {
	run := runTestboxWithFakeBuild(t, false, "test", "./nonexistent")
	if run.err != nil {
		t.Fatalf("testbox.sh failed against an engine whose every call succeeds: %v\n%s", run.err, run.output)
	}
	if !strings.Contains(run.engine, "build ") {
		t.Fatalf("the harness never built an image; this test proved nothing:\n%s", run.engine)
	}
	for _, chatter := range []string{"RUN apt-get update", "Hit:1", "writing image", "sha256:"} {
		if strings.Contains(run.output, chatter) {
			t.Errorf("a successful build printed %q; it should be quiet:\n%s", chatter, run.output)
		}
	}
}

// The lifecycle gate builds its own uniquely-tagged image through the same
// path, and it is the one that runs nightly with nobody watching — so it needs
// the failure output just as much.
func TestTestbox_TheLifecycleImageBuildAlsoPrintsItsFailure(t *testing.T) {
	run := runTestboxWithFakeBuild(t, true, "lifecycle")
	if run.err == nil {
		t.Fatalf("testbox.sh lifecycle succeeded although every image build failed:\n%s", run.output)
	}
	if !strings.Contains(run.output, "403  Forbidden") {
		t.Errorf("the lifecycle image build hid its failure; output:\n%s", run.output)
	}
}

// Every image build goes through retry_image_build and states whether it is
// quiet or loud. A `-q` or a `>/dev/null` on a build is exactly how the failing
// lines were lost, and a new target written that way would compile, work, and
// silently bring #7096 back for its image — so the structure is pinned here.
func TestTestbox_NoImageBuildDiscardsItsOwnOutput(t *testing.T) {
	source, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "testbox.sh"))
	if err != nil {
		t.Fatalf("read testbox.sh: %v", err)
	}
	inHelper := regexp.MustCompile(`(?s)\nretry_image_build\(\) \{.*?\n\}`)
	outside := inHelper.ReplaceAllString(string(source), "")

	callSite := regexp.MustCompile(`(?m)^\s*retry_image_build\s.*$`)
	calls := callSite.FindAllString(outside, -1)
	if len(calls) < 4 {
		t.Fatalf("found %d retry_image_build call sites, want at least 4 (testbox, lifecycle, and the web image twice); "+
			"the search is looking in the wrong place:\n%v", len(calls), calls)
	}
	for _, call := range calls {
		fields := strings.Fields(call)
		if len(fields) < 2 || (fields[1] != "quiet" && fields[1] != "loud") {
			t.Errorf("image build does not say whether it is quiet or loud: %s", strings.TrimSpace(call))
		}
		for _, f := range fields {
			if f == "-q" || f == "--quiet" || strings.HasSuffix(f, "/dev/null") {
				t.Errorf("image build discards its own output, so a failure cannot be attributed (#7096): %s", strings.TrimSpace(call))
			}
		}
	}
	directBuild := regexp.MustCompile(`"\$ENGINE" build\s`)
	if directBuild.MatchString(outside) {
		for i, line := range strings.Split(outside, "\n") {
			if directBuild.MatchString(line) {
				t.Errorf("testbox.sh:%d builds an image outside retry_image_build: %s", i+1, strings.TrimSpace(line))
			}
		}
	}
}

// The web-selftest image installs through scripts/ci-apt-install.sh (#7096), so
// its build context has to carry that script — the same bytes the workflows
// run, not a copy that can drift — next to the Dockerfile.
func TestTestbox_TheBuildContextCarriesTheAptWrapper(t *testing.T) {
	run := runTestboxWithFakeBuild(t, false, "test", "./nonexistent")
	if run.err != nil {
		t.Fatalf("testbox.sh failed: %v\n%s", run.err, run.output)
	}
	if strings.Contains(run.engine, "context: stdin") {
		t.Fatalf("the image was built from stdin with no context, so it cannot COPY the apt wrapper:\n%s", run.engine)
	}
	for name, src := range map[string]string{
		"Dockerfile":        filepath.Join(repoRoot(t), "scripts", "container", "Dockerfile.test"),
		"ci-apt-install.sh": filepath.Join(repoRoot(t), "scripts", "ci-apt-install.sh"),
	} {
		got, err := os.ReadFile(filepath.Join(run.context, name))
		if err != nil {
			t.Errorf("build context has no %s: %v\nengine log:\n%s", name, err, run.engine)
			continue
		}
		want, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("read %s: %v", src, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("build context's %s differs from %s", name, src)
		}
	}
	// Nothing else: the context is what the image can see, and the source tree
	// reaches the container through the read-only bind mount at run time.
	entries, err := os.ReadDir(run.context)
	if err != nil {
		t.Fatalf("read context copy: %v", err)
	}
	if len(entries) != 2 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("build context holds %v, want exactly Dockerfile and ci-apt-install.sh", names)
	}
}

// The context is a temp directory, and it must not outlive the build — on the
// failure path too, which is the one that returns early.
func TestTestbox_TheBuildContextIsRemovedAfterTheBuild(t *testing.T) {
	for _, fail := range []bool{false, true} {
		args := []string{"test", "./nonexistent"}
		if fail {
			args = []string{"web-selftest"}
		}
		run := runTestboxWithFakeBuild(t, fail, args...)
		var dirs []string
		for _, line := range strings.Split(run.engine, "\n") {
			if dir, ok := strings.CutPrefix(line, "context: "); ok && dir != "stdin" {
				dirs = append(dirs, dir)
			}
		}
		if len(dirs) == 0 {
			t.Fatalf("fail=%v: no build context recorded:\n%s", fail, run.engine)
		}
		for _, dir := range dirs {
			if _, err := os.Stat(dir); err == nil {
				t.Errorf("fail=%v: build context %s was left behind", fail, dir)
			}
		}
	}
}

package integration_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// These tests cover the apk layer's mirror tolerance (#3779).
//
// The bug: `Registry-free image build` went red because dl-cdn answered
// "temporary error (try again later)" for about a minute, so `apk add` exited 4
// while building the very image the job uses to demonstrate its property — a
// property the same log had already proved one step earlier.
//
// Unlike #3774 there is no unrelated source to drop: the packages genuinely have
// to come from a mirror, and TestImageBuildsWithRegistryBlocked deliberately
// builds the real Dockerfile text. So the contract has two halves, and BOTH are
// asserted below, because getting only the first is how you end up with a build
// that cannot fail:
//
//  1. one mirror's bad minute does not fail the build — a second mirror on a
//     different operator, plus a bounded retry;
//  2. an outage on every mirror STILL fails the build, loudly, naming the
//     packages.
//
// apk is stubbed, so these are hermetic: no docker, no network, no mirror. The
// behaviour against a real apk and real mirrors is proven separately in a
// container (see the PR for #3779).

// apkStub stands in for apk. It records each invocation's argv and fails a
// configurable number of leading attempts, so the retry is measured rather than
// assumed.
const apkStub = `#!/usr/bin/env sh
count=$(cat "$APK_COUNT" 2>/dev/null || echo 0)
count=$((count + 1))
printf '%s' "$count" > "$APK_COUNT"
printf '%s\n' "$*" >> "$APK_ARGV"
if [ "$count" -le "${APK_FAIL_TIMES:-0}" ]; then
	echo "WARNING: fetching https://dl-cdn.alpinelinux.org/alpine/v3.20/main: temporary error (try again later)" >&2
	echo "ERROR: unable to select packages:" >&2
	exit 4
fi
echo "OK: packages installed"
exit 0
`

// sleepStub keeps the real backoff in the emitted script honest while costing
// the test nothing: the script's `sleep $((attempt * 3))` is exercised, but
// returns immediately.
const sleepStub = `#!/usr/bin/env sh
exit 0
`

type apkRun struct {
	output   string
	err      error
	attempts int
	argv     []string
}

// runEmittedApkScript executes what apkAddRun() puts in the Dockerfile.
//
// It converts the RUN instruction to a shell script the way the Dockerfile
// parser does — drop the `RUN ` prefix, then join every backslash-newline pair —
// so what runs here is the text docker would hand to /bin/sh, not a paraphrase
// of it.
func runEmittedApkScript(t *testing.T, failTimes int, pkgs ...string) apkRun {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the emitted apk layer is a POSIX shell script")
	}

	run := apkAddRun(pkgs...)
	script, ok := strings.CutPrefix(run, "RUN ")
	if !ok {
		t.Fatalf("apkAddRun did not emit a RUN instruction:\n%s", run)
	}
	script = strings.ReplaceAll(script, "\\\n", "")

	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	for name, body := range map[string]string{"apk": apkStub, "sleep": sleepStub} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write %s stub: %v", name, err)
		}
	}
	countFile := filepath.Join(tmp, "count")
	argvFile := filepath.Join(tmp, "argv")

	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = tmp
	cmd.Env = []string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"APK_COUNT=" + countFile,
		"APK_ARGV=" + argvFile,
		"APK_FAIL_TIMES=" + strconv.Itoa(failTimes),
	}
	out, err := cmd.CombinedOutput()

	result := apkRun{output: string(out), err: err}
	if data, readErr := os.ReadFile(countFile); readErr == nil {
		result.attempts, _ = strconv.Atoi(strings.TrimSpace(string(data)))
	}
	if data, readErr := os.ReadFile(argvFile); readErr == nil {
		for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
			if line != "" {
				result.argv = append(result.argv, line)
			}
		}
	}
	return result
}

// The happy path must stay quiet and must not retry.
func TestApkLayerInstallsOnTheFirstAttempt(t *testing.T) {
	run := runEmittedApkScript(t, 0, "git", "tmux", "bash")
	if run.err != nil {
		t.Fatalf("the apk layer failed with a healthy mirror: %v\n%s", run.err, run.output)
	}
	if run.attempts != 1 {
		t.Errorf("apk ran %d times on the happy path, want 1", run.attempts)
	}
	if strings.Contains(run.output, "retrying") {
		t.Errorf("a successful install still announced a retry:\n%s", run.output)
	}
}

// The bug in #3779, exactly: a mirror answers "temporary error" and the build
// must still complete.
func TestApkLayerSurvivesAMirrorsBadMinute(t *testing.T) {
	run := runEmittedApkScript(t, apkAddAttempts-1, "git", "tmux", "bash")
	if run.err != nil {
		t.Fatalf("the apk layer did not survive %d transient failures: %v\n%s",
			apkAddAttempts-1, run.err, run.output)
	}
	if run.attempts != apkAddAttempts {
		t.Errorf("apk ran %d times, want %d (the retry budget was not spent)", run.attempts, apkAddAttempts)
	}
	// The failures must stay visible: a mirror that is quietly unwell for a
	// minute looks identical to a healthy one once the build goes green.
	if !strings.Contains(run.output, "temporary error") {
		t.Errorf("the mirror's own error was swallowed:\n%s", run.output)
	}
}

// The other half of the contract, and the reason this is not a
// skip-with-reason: an outage on every mirror still fails the build.
func TestApkLayerStillFailsWhenEveryMirrorIsDown(t *testing.T) {
	run := runEmittedApkScript(t, apkAddAttempts+5, "git", "tmux", "bash")
	if run.err == nil {
		t.Fatalf("the apk layer SUCCEEDED with every attempt failing — the build cannot fail:\n%s", run.output)
	}
	if run.attempts != apkAddAttempts {
		t.Errorf("apk ran %d times, want exactly the %d-attempt budget", run.attempts, apkAddAttempts)
	}
	if !strings.Contains(run.output, "FAILED after") || !strings.Contains(run.output, "git tmux bash") {
		t.Errorf("the final failure did not name the attempts and the packages:\n%s", run.output)
	}
}

// Every attempt must offer apk the fallback repository; without it a retry is
// just the same unwell mirror three times.
func TestApkLayerOffersTheFallbackMirrorOnEveryAttempt(t *testing.T) {
	run := runEmittedApkScript(t, apkAddAttempts-1, "git", "tmux", "bash")
	if len(run.argv) != apkAddAttempts {
		t.Fatalf("recorded %d apk invocations, want %d", len(run.argv), apkAddAttempts)
	}
	branch := alpineBranch()
	for i, argv := range run.argv {
		for _, want := range []string{
			"-X " + alpineFallbackMirror + "/" + branch + "/main",
			"-X " + alpineFallbackMirror + "/" + branch + "/community",
			"git tmux bash",
			"--no-cache",
		} {
			if !strings.Contains(argv, want) {
				t.Errorf("attempt %d did not pass %q; argv was: %s", i+1, want, argv)
			}
		}
	}
}

// A mirror URL carrying a hardcoded release would keep serving the previous one
// after a base bump, and the build would still pass — installing from the wrong
// branch.
func TestApkMirrorBranchTracksTheBase(t *testing.T) {
	if got, want := alpineBranch(), "v3.20"; got != want {
		t.Errorf("alpineBranch() = %q, want %q", got, want)
	}
	if !strings.HasSuffix(roundTripBaseUpstream, strings.TrimPrefix(alpineBranch(), "v")) {
		t.Errorf("alpineBranch() %q does not track roundTripBaseUpstream %q",
			alpineBranch(), roundTripBaseUpstream)
	}
	for _, df := range []string{
		dockerRoundTripDockerfile(roundTripBaseImage),
		sshdRoundTripDockerfile(roundTripBaseImage),
	} {
		if !strings.Contains(df, alpineFallbackMirror+"/"+alpineBranch()+"/main") {
			t.Errorf("a round-trip Dockerfile does not name the fallback mirror for the pinned branch:\n%s", df)
		}
	}
}

// The drift guard. Both round-trip images must install through apkAddRun; a
// second, hand-written `apk add` would be exactly the un-hardened layer this
// issue is about, and TestImageBuildsWithRegistryBlocked builds whatever text
// these functions return.
func TestRoundTripDockerfilesInstallThroughTheHardenedLayer(t *testing.T) {
	for name, df := range map[string]string{
		"docker round-trip": dockerRoundTripDockerfile(roundTripBaseImage),
		"sshd round-trip":   sshdRoundTripDockerfile(roundTripBaseImage),
	} {
		if !strings.Contains(df, "for attempt in $(seq 1 "+strconv.Itoa(apkAddAttempts)+")") {
			t.Errorf("%s image does not go through the retry loop:\n%s", name, df)
		}
		// Exactly one apk INVOCATION, inside the loop — not a second bare
		// one. Counted as `apk add --no-cache` rather than `apk add`, because
		// the layer's own retry/failure messages quote the phrase and would
		// make a bare-phrase count read 3 on a correct Dockerfile.
		if got := strings.Count(df, "apk add --no-cache"); got != 1 {
			t.Errorf("%s image has %d apk invocations, want exactly 1 (the hardened layer)", name, got)
		}
		if strings.Contains(df, "RUN apk add") {
			t.Errorf("%s image installs with a bare `RUN apk add`, which fails on one mirror's bad minute (#3779):\n%s", name, df)
		}
	}
}

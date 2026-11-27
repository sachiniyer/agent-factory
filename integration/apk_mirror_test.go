package integration_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// These tests cover the apk layer's mirror tolerance (#3779) and the #7896
// layering that keeps that layer out of every per-test build.
//
// The bug: `Registry-free image build` went red because dl-cdn answered
// "temporary error (try again later)" for about a minute, so `apk add` exited 4
// while building the very image the job uses to demonstrate its property — a
// property the same log had already proved one step earlier.
//
// Unlike #3774 there is no unrelated source to drop: the packages genuinely have
// to come from a mirror. So the contract has two halves, and BOTH are asserted
// below, because getting only the first is how you end up with a build that
// cannot fail:
//
//  1. one mirror's bad minute does not fail the build — a second mirror on a
//     different operator, plus a bounded retry;
//  2. an outage on every mirror STILL fails the build, loudly, naming the
//     packages.
//
// #7896 then moved WHERE the layer runs: once per test binary, inside the base
// image's build (roundTripBaseDockerfile), under its own labelled
// infrastructure budget — never inside a per-test `docker build`'s 300s. The
// drift guards below pin both halves of that: the base is the only Dockerfile
// that may carry an apk invocation, and every per-test Dockerfile must fetch
// nothing at all.
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
	// The only Dockerfile left with a mirror in it is the base's (#7896).
	if df := roundTripBaseDockerfile(roundTripBaseUpstream); !strings.Contains(df, alpineFallbackMirror+"/"+alpineBranch()+"/main") {
		t.Errorf("the base image Dockerfile does not name the fallback mirror for the pinned branch:\n%s", df)
	}
}

// The drift guard, updated for the #7896 layering: packages install in exactly
// ONE place — the base image's hardened apk layer. A second, hand-written
// `apk add` (in the base, or back in any per-test Dockerfile) would be exactly
// the un-hardened fetch this family of issues keeps paying for, and
// TestImageBuildsWithRegistryBlocked builds whatever text these functions
// return.
func TestBaseDockerfileInstallsThroughTheHardenedLayer(t *testing.T) {
	df := roundTripBaseDockerfile(roundTripBaseUpstream)
	if !strings.Contains(df, "for attempt in $(seq 1 "+strconv.Itoa(apkAddAttempts)+")") {
		t.Errorf("the base image does not go through the retry loop:\n%s", df)
	}
	// Exactly one apk INVOCATION, inside the loop — not a second bare
	// one. Counted as `apk add --no-cache` rather than `apk add`, because
	// the layer's own retry/failure messages quote the phrase and would
	// make a bare-phrase count read 3 on a correct Dockerfile.
	if got := strings.Count(df, "apk add --no-cache"); got != 1 {
		t.Errorf("the base image has %d apk invocations, want exactly 1 (the hardened layer)", got)
	}
	if strings.Contains(df, "RUN apk add") {
		t.Errorf("the base image installs with a bare `RUN apk add`, which fails on one mirror's bad minute (#3779):\n%s", df)
	}
	// The labels dockerBaseImageCurrent verifies are emitted too — without
	// them a stale bare retag of the upstream could pass for a built base.
	for _, want := range []string{
		basePackagesLabel + `="` + strings.Join(roundTripBasePackages, " ") + `"`,
		baseDigestLabel + `="` + baseDockerfileDigest(roundTripBaseUpstream) + `"`,
	} {
		if !strings.Contains(df, want) {
			t.Errorf("the base image Dockerfile does not carry %s:\n%s", want, df)
		}
	}
}

// The #7896 property, checked textually with no docker: once the packages are
// baked into the base image, every per-test Dockerfile must contain NO network
// fetch — no package manager, no downloader, no URL — and its FROM must name a
// local-only image (af-*), so buildkit has nothing to resolve remotely either.
// If a new image genuinely needs another package, extend roundTripBasePackages
// — never add a fetch to a per-test Dockerfile, which is how a mirror got a
// vote inside a 300s build budget in the first place.
func TestPerTestDockerfilesFetchNothingOverTheNetwork(t *testing.T) {
	for name, df := range map[string]string{
		"docker round-trip":   dockerRoundTripDockerfile(roundTripBaseImage),
		"sshd round-trip":     sshdRoundTripDockerfile(roundTripBaseImage),
		"account alias":       accountAliasDockerfile(roundTripBaseImage),
		"symlinked-home sshd": symlinkedHomeDockerfile(roundTripBaseImage),
		"sandbox host":        sandboxHostDockerfile("af-sshd-roundtrip:test"),
	} {
		from, _, _ := strings.Cut(df, "\n")
		if !strings.HasPrefix(from, "FROM af-") {
			t.Errorf("%s Dockerfile does not FROM a local-only (af-*) image: %s\n%s", name, from, df)
		}
		for _, fetch := range []string{
			"apk", "apt", "wget", "curl", "pip", "npm", "yarn", "pnpm",
			"git clone", "go get", "go install", "svn", "ftp://", "http://", "https://",
		} {
			if strings.Contains(df, fetch) {
				t.Errorf("%s Dockerfile contains a network fetch %q — packages install once through the base image's hardened layer (#7896), never in a per-test build:\n%s", name, fetch, df)
			}
		}
	}
}

// The base build's failure label must come from the signature of the step that
// actually failed (#7896 R2) — never from a fetch-adjacent string that happens
// to be in the log. Both directions are pinned: a lone "WARNING: fetching" is
// only one mirror complaining while the fallback serves, so it must NOT claim
// the label; and a FROM-resolve failure must not be labelled a plain build
// error.
func TestBaseBuildFailureLabelsTheFailingStep(t *testing.T) {
	buildErr := errors.New("exit status 1")
	for _, tc := range []struct {
		name     string
		ctxErr   error
		output   string
		want     []string
		dontWant []string
	}{
		{
			name:   "deadline means a slow fetch",
			ctxErr: context.DeadlineExceeded,
			output: "#8 [2/2] RUN apk add ...",
			want:   []string{"infrastructure budget", "slow alpine mirror"},
		},
		{
			name:   "every mirror attempt exhausted",
			output: "WARNING: fetching https://dl-cdn.alpinelinux.org/...\napk add FAILED after 3 attempts: git tmux bash openssh-server",
			want:   []string{"mirror outage"},
		},
		{
			name:     "a lone one-mirror warning never decides it",
			output:   "WARNING: fetching https://dl-cdn.alpinelinux.org/... temporary error\nno space left on device",
			want:     []string{"real build error"},
			dontWant: []string{"mirror outage"},
		},
		{
			name:     "warning present but the failure was elsewhere",
			output:   "WARNING: fetching https://dl-cdn.alpinelinux.org/... temporary error\nERROR: failed to export layer: write /var/lib/docker: no space left",
			want:     []string{"real build error"},
			dontWant: []string{"mirror outage"},
		},
		{
			name:     "FROM resolve failure is the registry",
			output:   "failed to resolve source metadata for docker.io/library/alpine:3.20",
			want:     []string{"registry outage"},
			dontWant: []string{"a real build error, not a mirror or registry outage"},
		},
		{
			name:   "non-deadline ctx error with generic failure",
			ctxErr: context.Canceled,
			output: "process killed",
			want:   []string{"real build error"},
		},
		{
			name:     "a plain build failure stays a build failure",
			output:   "Dockerfile parse error line 3: unknown instruction",
			want:     []string{"real build error"},
			dontWant: []string{"a mirror outage", "registry outage signature"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := classifyBaseBuildFailure(tc.ctxErr, buildErr, tc.output).Error()
			for _, w := range tc.want {
				if !strings.Contains(msg, w) {
					t.Errorf("classified message lacks %q:\n%s", w, msg)
				}
			}
			for _, dw := range tc.dontWant {
				if strings.Contains(msg, dw) {
					t.Errorf("classified message wrongly claims %q:\n%s", dw, msg)
				}
			}
		})
	}
}

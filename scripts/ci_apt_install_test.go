package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// These tests cover scripts/ci-apt-install.sh and the workflow lint that keeps
// it from being bypassed (#3774).
//
// The bug: `apt-get update` exits 100 when ANY configured source fails, so a
// 403 from packages.microsoft.com — a repository the ubuntu-latest image ships
// and this project never installs from — short-circuited the `&&` in six
// workflow steps and reddened master's Build, the required Test check, and both
// release preflights.
//
// The script's contract has two halves, and BOTH are asserted below, because
// getting only the first is how you end up with `|| true` on the compound
// command and a check that reports clean when it did not run:
//
//  1. a failing update does not fail the step, and its errors are printed;
//  2. a failing install DOES fail the step — even when the update failed too.
//
// apt-get is stubbed, so these are hermetic and need no network, no root, and
// no apt. The script's behaviour against a real archive with a real broken
// source is proven separately in a container (see the PR for #3774).

// aptStub is a stand-in apt-get. It records each invocation's argv verbatim
// (\x1f-separated, one line per call) and fails update/install on demand.
const aptStub = `#!/usr/bin/env bash
{
	line=""
	for a in "$@"; do line="$line$a"$'\x1f'; done
	printf '%s\n' "$line"
} >> "$APT_CALL_LOG"
case "${1:-}" in
update)
	echo "Hit:1 http://azure.archive.ubuntu.com/ubuntu noble InRelease"
	if [ -n "${APT_UPDATE_FAIL:-}" ]; then
		echo "E: Failed to fetch https://packages.microsoft.com/repos/azure-cli/dists/noble/InRelease  403  Forbidden [IP: 13.107.246.40 443]" >&2
		echo "E: The repository 'https://packages.microsoft.com/repos/azure-cli noble InRelease' is no longer signed." >&2
		exit 100
	fi
	;;
install)
	if [ -n "${APT_INSTALL_FAIL:-}" ]; then
		echo "E: Unable to locate package" >&2
		exit 100
	fi
	echo "Setting up the requested packages"
	;;
esac
exit 0
`

// sudoStub stands in for passwordless sudo on a runner. The script resolves
// sudo only when it is not root, so this makes the test behave the same way
// whether or not it runs as root (containers) — either way the apt-get stub is
// what ends up being executed.
const sudoStub = `#!/usr/bin/env bash
exec "$@"
`

type aptRun struct {
	output string
	err    error
	// dir is the script's working directory, so a test can look for side
	// effects a shell would have produced if it had re-parsed an argument.
	dir string
	// calls holds the argv of each apt-get invocation, program name excluded.
	calls [][]string
}

func (r aptRun) exitCode(t *testing.T) int {
	t.Helper()
	if r.err == nil {
		return 0
	}
	var ee *exec.ExitError
	if !errors.As(r.err, &ee) {
		t.Fatalf("expected an exit error, got %T: %v\n%s", r.err, r.err, r.output)
	}
	return ee.ExitCode()
}

// runAptInstall executes scripts/ci-apt-install.sh with apt-get and sudo
// stubbed onto PATH. env carries the stub's failure switches.
func runAptInstall(t *testing.T, env map[string]string, args ...string) aptRun {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("ci-apt-install.sh requires bash")
	}

	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	for name, body := range map[string]string{"apt-get": aptStub, "sudo": sudoStub} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write %s stub: %v", name, err)
		}
	}
	callLog := filepath.Join(tmp, "apt-calls.log")

	script := filepath.Join(repoRoot(t), "scripts", "ci-apt-install.sh")
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Dir = tmp
	cmd.Env = []string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + tmp,
		"APT_CALL_LOG=" + callLog,
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()

	run := aptRun{output: string(out), err: err, dir: tmp}
	if data, readErr := os.ReadFile(callLog); readErr == nil {
		for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
			if line == "" {
				continue
			}
			argv := strings.Split(strings.TrimSuffix(line, "\x1f"), "\x1f")
			run.calls = append(run.calls, argv)
		}
	} else if !os.IsNotExist(readErr) {
		t.Fatalf("read apt call log: %v", readErr)
	}
	return run
}

func assertCalls(t *testing.T, run aptRun, want [][]string) {
	t.Helper()
	if len(run.calls) != len(want) {
		t.Fatalf("apt-get invoked %d time(s), want %d: %v\n%s", len(run.calls), len(want), run.calls, run.output)
	}
	for i, wantArgv := range want {
		got := run.calls[i]
		if len(got) != len(wantArgv) {
			t.Fatalf("call %d argv = %q, want %q\n%s", i, got, wantArgv, run.output)
		}
		for j := range wantArgv {
			if got[j] != wantArgv[j] {
				t.Fatalf("call %d arg %d = %q, want %q (full argv %q)\n%s", i, j, got[j], wantArgv[j], got, run.output)
			}
		}
	}
}

// The bug in #3774, exactly: the update fails because of a source we do not
// install from, and the step must still succeed.
func TestCIAptInstallSurvivesASourceItDoesNotInstallFrom(t *testing.T) {
	run := runAptInstall(t, map[string]string{"APT_UPDATE_FAIL": "1"}, "zsh")
	if run.err != nil {
		t.Fatalf("script failed on a tolerable update failure: %v\n%s", run.err, run.output)
	}
	assertCalls(t, run, [][]string{
		{"update"},
		{"install", "-y", "--no-install-recommends", "zsh"},
	})
	// The failure has to stay visible: a real archive outage looks identical in
	// the exit status, and these lines are the only thing that tells them apart.
	if !strings.Contains(run.output, "403  Forbidden") {
		t.Errorf("the update's own error lines were swallowed; output:\n%s", run.output)
	}
	if !strings.Contains(run.output, "::warning::") {
		t.Errorf("a tolerated update failure produced no warning annotation; output:\n%s", run.output)
	}
}

// The other half of the contract, and the reason this is not `|| true` on the
// compound command: the install is the assertion.
func TestCIAptInstallFailsWhenThePackageIsGenuinelyUnavailable(t *testing.T) {
	run := runAptInstall(t, map[string]string{"APT_INSTALL_FAIL": "1"}, "zsh")
	if run.err == nil {
		t.Fatalf("script succeeded despite a failing install; output:\n%s", run.output)
	}
	if code := run.exitCode(t); code != 100 {
		t.Errorf("exit code = %d, want apt-get's 100 (propagated, not rewritten)", code)
	}
	if !strings.Contains(run.output, "::error::") {
		t.Errorf("a failing install produced no error annotation; output:\n%s", run.output)
	}
}

// A tolerated update failure must not become a tolerated install failure. This
// is the regression that a `|| true` on the whole compound command would cause,
// and it is the one the issue explicitly warned against.
func TestCIAptInstallStillFailsWhenBothUpdateAndInstallFail(t *testing.T) {
	run := runAptInstall(t, map[string]string{"APT_UPDATE_FAIL": "1", "APT_INSTALL_FAIL": "1"}, "zsh")
	if run.err == nil {
		t.Fatalf("a failing update swallowed a failing install; output:\n%s", run.output)
	}
	if code := run.exitCode(t); code != 100 {
		t.Errorf("exit code = %d, want 100", code)
	}
	// Both causes should be legible, since the update failure may well be why
	// the package could not be found.
	if !strings.Contains(run.output, "403  Forbidden") {
		t.Errorf("update errors missing from a double failure; output:\n%s", run.output)
	}
}

func TestCIAptInstallIsQuietWhenTheUpdateSucceeds(t *testing.T) {
	run := runAptInstall(t, nil, "zsh")
	if run.err != nil {
		t.Fatalf("script failed on the happy path: %v\n%s", run.err, run.output)
	}
	if strings.Contains(run.output, "::warning::") {
		t.Errorf("a clean update still warned; output:\n%s", run.output)
	}
}

func TestCIAptInstallRefusesAnEmptyPackageList(t *testing.T) {
	run := runAptInstall(t, nil)
	if run.err == nil {
		t.Fatalf("script accepted zero packages; output:\n%s", run.output)
	}
	if code := run.exitCode(t); code != 2 {
		t.Errorf("exit code = %d, want 2 (usage)", code)
	}
	if len(run.calls) != 0 {
		t.Errorf("apt-get was invoked despite a usage error: %v", run.calls)
	}
}

// Package names reach apt-get as argv entries and are never re-parsed by a
// shell. That is what makes it safe for a call site to pass literal words —
// and it is why a call site must never interpolate `${{ }}` into the command
// line, which the lint below enforces.
func TestCIAptInstallPassesPackageNamesAsLiteralArgv(t *testing.T) {
	hostile := "zsh; touch pwned"
	run := runAptInstall(t, nil, "tmux", "jq", "curl", hostile, "$(touch pwned)")
	if run.err != nil {
		t.Fatalf("script failed: %v\n%s", run.err, run.output)
	}
	assertCalls(t, run, [][]string{
		{"update"},
		{"install", "-y", "--no-install-recommends", "tmux", "jq", "curl", hostile, "$(touch pwned)"},
	})
	if _, err := os.Stat(filepath.Join(run.dir, "pwned")); err == nil {
		t.Errorf("a package name was evaluated by a shell")
	}
}

func TestCIAptInstallScriptIsExecutable(t *testing.T) {
	// The workflows invoke `scripts/ci-apt-install.sh` directly, so the mode
	// bit committed to git is load-bearing: without it every one of the six
	// steps dies with "Permission denied".
	info, err := os.Stat(filepath.Join(repoRoot(t), "scripts", "ci-apt-install.sh"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("mode = %v, want the executable bit set", info.Mode().Perm())
	}
}

// ── the drift lint ───────────────────────────────────────────────────────────
//
// Six copies of the same one-liner is how this bug reached four workflows, two
// of which cut releases. One script fixes it once; this lint is what stops a
// seventh call site from being written as a bare `apt-get update && …` again.

// lintAptUsage reports problems in CI definition files, keyed name -> content.
// Comment lines are stripped first: a `#` line is inert in both YAML and in a
// `run:` shell block, and the call sites deliberately explain the fix in prose.
func lintAptUsage(files map[string]string) []string {
	var findings []string
	for name, content := range files {
		for i, line := range strings.Split(content, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "#") {
				continue
			}
			if strings.Contains(line, "apt-get") {
				findings = append(findings, name+":"+strconv.Itoa(i+1)+
					": calls apt-get directly; use `scripts/ci-apt-install.sh <pkg>...` instead."+
					" A bare `apt-get update` fails the whole step when any configured source"+
					" fails, including third-party ones the runner image ships and we never"+
					" install from (#3774).")
			}
			if strings.Contains(line, "ci-apt-install.sh") && strings.Contains(line, "${{") {
				findings = append(findings, name+":"+strconv.Itoa(i+1)+
					": interpolates `${{ }}` into the ci-apt-install.sh command line."+
					" That is script injection, not a variable reference — pass package"+
					" names as literal arguments (#3774).")
			}
		}
	}
	return findings
}

func TestAptLintRejectsAReintroducedBareAptGetUpdate(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name:    "the exact one-liner this issue removed",
			content: "      - name: Install zsh\n        run: sudo apt-get update && sudo apt-get install -y --no-install-recommends zsh\n",
			want:    true,
		},
		{
			name:    "the multi-line shape, which has the same defect",
			content: "        run: |\n          sudo apt-get update\n          sudo apt-get install -y --no-install-recommends tmux\n",
			want:    true,
		},
		{
			name:    "an update on its own, still exit 100 on any bad source",
			content: "        run: sudo apt-get update\n",
			want:    true,
		},
		{
			name:    "the wrapper",
			content: "        run: scripts/ci-apt-install.sh zsh\n",
			want:    false,
		},
		{
			name:    "prose about the old shape in a comment is inert",
			content: "      # Was: sudo apt-get update && sudo apt-get install -y zsh (#3774).\n        run: scripts/ci-apt-install.sh zsh\n",
			want:    false,
		},
		{
			name:    "a workflow expression on the wrapper's command line",
			content: "        run: scripts/ci-apt-install.sh ${{ inputs.packages }}\n",
			want:    true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			findings := lintAptUsage(map[string]string{"fixture.yml": tc.content})
			if got := len(findings) > 0; got != tc.want {
				t.Fatalf("flagged = %v, want %v (findings: %v)", got, tc.want, findings)
			}
		})
	}
}

func TestAptLintOverTheRealCIDefinitions(t *testing.T) {
	root := filepath.Join(repoRoot(t), ".github")
	files := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		rel, relErr := filepath.Rel(repoRoot(t), path)
		if relErr != nil {
			rel = path
		}
		files[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("walk .github: %v", err)
	}

	// Anti-vacuity: a lint that scanned the wrong directory would also report
	// nothing. Name two files it must have read.
	for _, must := range []string{
		filepath.Join(".github", "workflows", "pr.yml"),
		filepath.Join(".github", "workflows", "build.yml"),
	} {
		if content, ok := files[must]; !ok || len(content) == 0 {
			t.Fatalf("scan did not read %s — the lint is not looking where it thinks it is", must)
		}
	}

	if findings := lintAptUsage(files); len(findings) > 0 {
		t.Fatalf("CI definitions call apt-get outside the wrapper:\n  %s", strings.Join(findings, "\n  "))
	}

	// The six steps that installed packages must actually route through the
	// script; otherwise the lint above is satisfied by having deleted them.
	callSites := 0
	for _, content := range files {
		callSites += strings.Count(content, "run: scripts/ci-apt-install.sh")
	}
	if callSites < 6 {
		t.Errorf("found %d ci-apt-install.sh call sites, want the 6 from #3774 "+
			"(build.yml, pr.yml ×2, auto-release.yml, stable-release.yml, lifecycle.yml). "+
			"If a step was removed on purpose, lower this number deliberately.", callSites)
	}
}

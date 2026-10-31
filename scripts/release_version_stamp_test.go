package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ── the release version on the static sandbox-host artifacts (#5666) ─────────
//
// A release binary gets its version at link time —
// `go build -ldflags "-X main.version=${VERSION}"` — and ${VERSION} comes from
// the workflow step. A step that never defines it expands it to the empty
// string, which fails nothing: commands.NewRootCommand ignores an empty
// version, so `af version` falls back to "dev" and exits 0. That is how
// stable-release.yml shipped agent-factory-linux-{amd64,arm64}-static.tar.gz
// reporting "dev": its static build step was copied from auto-release.yml
// without the line that set VERSION, and its smoke test only checked the exit
// status.
//
// Two guards, because they fail in different places:
//   - assert-binary-version.sh is the runtime check the smoke tests run against
//     the just-built binary. It is exercised below against fake binaries that
//     print what a stamped, an unstamped and a misstamped build print.
//   - the lint below is the structural check, derived from the workflow files:
//     every step that stamps a version defines VERSION itself, and every
//     static smoke test calls the assertion with it. The arm64 artifact cannot
//     be executed on the amd64 runner, so for that leg this is the only guard —
//     it is the same step, with a different GOARCH, as the leg that is run.

const assertVersionScript = ".github/scripts/assert-binary-version.sh"

// afVersionOutput is what `af version` prints for a binary stamped with v
// (commands/root.go): the version line, then the release-tag URL.
func afVersionOutput(v string) string {
	return "agent-factory version " + v + "\n" +
		"https://github.com/sachiniyer/agent-factory/releases/tag/v" + v + "\n"
}

// fakeAF writes an executable that answers `version` with printed and exits
// with exitCode. Any other invocation fails, so a script that did not run the
// `version` subcommand cannot pass.
func fakeAF(t *testing.T, printed string, exitCode int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent-factory")
	writeExecutable(t, path, "#!/bin/sh\n"+
		"[ \"$1\" = version ] || { echo \"unexpected args: $*\" >&2; exit 64; }\n"+
		"cat <<'EOF'\n"+printed+"EOF\n"+
		"exit "+strconv.Itoa(exitCode)+"\n")
	return path
}

func runAssertVersion(t *testing.T, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{filepath.Join(repoRoot(t), assertVersionScript)}, args...)...)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &exitErr):
		return string(out), exitErr.ExitCode()
	default:
		t.Fatalf("run %s: %v", assertVersionScript, err)
		return "", -1
	}
}

func TestAssertBinaryVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("assert-binary-version.sh is POSIX sh; not run on Windows")
	}
	cases := []struct {
		name     string
		printed  string // what the fake binary prints for `version`
		exitCode int    // what the fake binary exits with
		expected string // the version the smoke test passes in
		wantExit int
		wantOut  string // substring of the combined output
	}{
		{
			name:    "stable release, stamped",
			printed: afVersionOutput("1.2.3"), expected: "1.2.3",
			wantOut: "ok:",
		},
		{
			name:    "preview release, stamped",
			printed: afVersionOutput("1.2.4-preview-7"), expected: "1.2.4-preview-7",
			wantOut: "ok:",
		},
		{
			// The #5666 shape: the stamp was empty, so the binary fell back to
			// "dev" and exited 0 — exactly what the old exit-status-only smoke
			// test accepted.
			name:    "stamp lost, binary fell back to dev",
			printed: afVersionOutput("dev"), expected: "1.2.3",
			wantExit: 1,
			wantOut:  "want 'agent-factory version 1.2.3'. It fell back to 'dev'",
		},
		{
			name:    "a different release",
			printed: afVersionOutput("1.2.2"), expected: "1.2.3",
			wantExit: 1,
			wantOut:  "reports 'agent-factory version 1.2.2'",
		},
		{
			name:    "expected version is a prefix of the reported one",
			printed: afVersionOutput("1.2.30"), expected: "1.2.3",
			wantExit: 1,
			wantOut:  "want 'agent-factory version 1.2.3'",
		},
		{
			name:    "reported version is a prefix of the expected one",
			printed: afVersionOutput("1.2.3"), expected: "1.2.30",
			wantExit: 1,
			wantOut:  "want 'agent-factory version 1.2.30'",
		},
		{
			name:    "a preview build where the stable release was expected",
			printed: afVersionOutput("1.2.3-preview-1"), expected: "1.2.3",
			wantExit: 1,
			wantOut:  "reports 'agent-factory version 1.2.3-preview-1'",
		},
		{
			// A smoke-test step that never received VERSION must not "pass" by
			// comparing the unstamped binary's "dev" to nothing.
			name:    "expected version empty, binary on the dev fallback",
			printed: afVersionOutput("dev"), expected: "",
			wantExit: 2,
			wantOut:  "not a release version",
		},
		{
			name:    "expected version is dev",
			printed: afVersionOutput("dev"), expected: "dev",
			wantExit: 2,
			wantOut:  "not a release version",
		},
		{
			name:    "right version but the binary exits non-zero",
			printed: afVersionOutput("1.2.3"), exitCode: 3, expected: "1.2.3",
			wantExit: 1,
			wantOut:  "failed: the artifact cannot run",
		},
		{
			name:    "binary prints nothing",
			printed: "", expected: "1.2.3",
			wantExit: 1,
			wantOut:  "reports ''",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, exit := runAssertVersion(t, fakeAF(t, tc.printed, tc.exitCode), tc.expected)
			if exit != tc.wantExit {
				t.Fatalf("exit = %d, want %d; output:\n%s", exit, tc.wantExit, out)
			}
			if !strings.Contains(out, tc.wantOut) {
				t.Fatalf("output missing %q:\n%s", tc.wantOut, out)
			}
		})
	}
}

func TestAssertBinaryVersionUsage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("assert-binary-version.sh is POSIX sh; not run on Windows")
	}
	for _, args := range [][]string{nil, {"only-a-binary"}, {"a", "b", "c"}} {
		out, exit := runAssertVersion(t, args...)
		if exit != 2 || !strings.Contains(out, "usage:") {
			t.Errorf("args %q: exit = %d, output %q; want exit 2 with a usage line", args, exit, out)
		}
	}
}

func TestAssertBinaryVersionScriptIsExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode bits are not meaningful on Windows")
	}
	// The workflows invoke the script directly, and they only run at release
	// time — so the mode bit committed to git is load-bearing, and a missing one
	// would surface as "Permission denied" in the middle of a release.
	info, err := os.Stat(filepath.Join(repoRoot(t), assertVersionScript))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("mode = %v, want the executable bit set", info.Mode().Perm())
	}
}

// ── the structural lint ──────────────────────────────────────────────────────

const stampFlag = "-X main.version="

var (
	// `VERSION: <value>` as a step env entry. The value must be non-empty — an
	// empty quoted string reproduces the bug — but what it evaluates to is the
	// runner's business.
	envVersion = regexp.MustCompile(`(?m)^\s+VERSION:[ \t]*(?:[^\s"']|"[^"]|'[^'])`)
	// `VERSION=<value>` assigned in the step's run script, same rule.
	assignedVersion = regexp.MustCompile(`(?m)^\s+VERSION=(?:[^\s"']|"[^"]|'[^'])`)
	// The smoke test's call: the script, a binary, then "$VERSION".
	assertVersionCall = regexp.MustCompile(`assert-binary-version\.sh\s+\S+\s+"\$\{?VERSION\}?"`)
)

// workflowStep is one entry of a job's `steps:` list.
type workflowStep struct {
	title string // the step's first line, e.g. "- name: Build"
	body  string // every line of the step, full-line comments removed
}

// workflowSteps splits a workflow file into steps. The repo's workflows list
// steps as "    - " under a job, so a step runs from that line to the next one, or
// to the next line indented no deeper than the list (the next job key). A
// workflow that indents its steps differently yields none, which is why the
// real-files test below counts what it expects to find in the two release
// workflows. Full-line comments are dropped on purpose: a pin must not be
// satisfiable by prose that merely mentions what it is looking for.
func workflowSteps(text string) []workflowStep {
	var (
		steps  []workflowStep
		title  string
		body   []string
		inStep bool
		inJobs bool
	)
	flush := func() {
		if inStep {
			steps = append(steps, workflowStep{title: title, body: strings.Join(body, "\n")})
		}
		inStep, title, body = false, "", nil
	}
	for _, line := range strings.Split(text, "\n") {
		if !inJobs {
			// `on.schedule` lists "    - cron:" entries that are not steps.
			inJobs = line == "jobs:"
			continue
		}
		trimmed := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " "))
		switch {
		case trimmed == "" || strings.HasPrefix(trimmed, "#"):
		case strings.HasPrefix(line, "    - "):
			flush()
			inStep, title, body = true, trimmed, []string{line}
		case indent <= 4:
			flush()
		case inStep:
			body = append(body, line)
		}
	}
	flush()
	return steps
}

// definesVersionBefore reports whether the step gives VERSION a non-empty value
// that is in force when the command at byte offset `at` runs: a step env entry
// always is; an assignment in the script only if it comes first. Another step's
// env does not count, which is the point — the dynamic build step defined
// VERSION all along, and the static one next to it did not.
func definesVersionBefore(body string, at int) bool {
	if envVersion.MatchString(body) {
		return true
	}
	loc := assignedVersion.FindStringIndex(body)
	return loc != nil && loc[0] < at
}

type versionStampScan struct {
	stampers map[string]int // steps that link -X main.version, per file
	smokes   map[string]int // steps that unpack a static archive, per file
	findings []string
}

func scanVersionStamps(files map[string]string) versionStampScan {
	scan := versionStampScan{stampers: map[string]int{}, smokes: map[string]int{}}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, step := range workflowSteps(files[name]) {
			if at := strings.Index(step.body, stampFlag); at >= 0 {
				scan.stampers[name]++
				if !definesVersionBefore(step.body, at) {
					scan.findings = append(scan.findings, fmt.Sprintf(
						"%s: %q links %s${VERSION} but defines no VERSION before the build — an empty stamp falls back to \"dev\" (#5666)",
						name, step.title, stampFlag))
				}
			}
			if strings.Contains(step.body, "tar xzf") && strings.Contains(step.body, "-static.tar.gz") {
				scan.smokes[name]++
				loc := assertVersionCall.FindStringIndex(step.body)
				if loc == nil || !definesVersionBefore(step.body, loc[0]) {
					scan.findings = append(scan.findings, fmt.Sprintf(
						"%s: %q unpacks a static archive but does not run %s <binary> \"$VERSION\" with a VERSION it defines — exit status alone passes a \"dev\" binary (#5666)",
						name, step.title, assertVersionScript))
				}
			}
		}
	}
	return scan
}

func TestVersionStampLintFixtures(t *testing.T) {
	const head = "on:\n  schedule:\n    - cron: '0 */3 * * *'\njobs:\n  build:\n    runs-on: ubuntu-24.04\n    steps:\n"
	const dynamicBuild = `    - name: Build
      env:
        VERSION: ${{ inputs.version }}
      run: |
        go build -v -ldflags "-X main.version=${VERSION}" -o dist/agent-factory
`
	const smokeWith = `    - name: Smoke-test the static sandbox-host artifact
      env:
        VERSION: ${{ inputs.version }}
      run: |
        tar xzf "agent-factory-linux-amd64-static.tar.gz" -C "$tmp"
        .github/scripts/assert-binary-version.sh "$tmp/agent-factory" "$VERSION"
`
	cases := []struct {
		name  string
		steps string
		want  string // substring of the single expected finding; "" for none
	}{
		{
			name: "build step with VERSION in its env",
			steps: `    - name: Build the static sandbox-host binary
      env:
        CGO_ENABLED: "0"
        VERSION: ${{ inputs.version }}
      run: |
        go build -v -ldflags "-X main.version=${VERSION}" -o dist-static/agent-factory
`,
		},
		{
			name: "build step assigning VERSION in its script",
			steps: `    - name: Build the static sandbox-host binary
      run: |
        VERSION="${{ needs.check.outputs.new_version }}"
        go build -v -ldflags "-X main.version=${VERSION}" -o dist-static/agent-factory
`,
		},
		{
			// The #5666 shape, verbatim: VERSION is defined for the dynamic build
			// one step up and is simply absent from the static one.
			name: "static build step with no VERSION beside a dynamic one that has it",
			steps: dynamicBuild + `    - name: Build the static sandbox-host binary
      env:
        CGO_ENABLED: "0"
      run: |
        go build -v -ldflags "-X main.version=${VERSION}" -o dist-static/agent-factory
`,
			want: `"- name: Build the static sandbox-host binary" links -X main.version=${VERSION} but defines no VERSION`,
		},
		{
			name: "VERSION set to an empty string in the env",
			steps: `    - name: Build
      env:
        VERSION: ""
      run: |
        go build -ldflags "-X main.version=${VERSION}" -o dist/agent-factory
`,
			want: "defines no VERSION",
		},
		{
			name: "VERSION assigned an empty string in the script",
			steps: `    - name: Build
      run: |
        VERSION=""
        go build -ldflags "-X main.version=${VERSION}" -o dist/agent-factory
`,
			want: "defines no VERSION",
		},
		{
			name: "VERSION only mentioned in comments",
			steps: `    - name: Build
      # VERSION: ${{ inputs.version }}
      run: |
        # VERSION=1.2.3
        go build -ldflags "-X main.version=${VERSION}" -o dist/agent-factory
`,
			want: "defines no VERSION",
		},
		{
			name: "VERSION assigned after the build already ran",
			steps: `    - name: Build
      run: |
        go build -ldflags "-X main.version=${VERSION}" -o dist/agent-factory
        VERSION=1.2.3
`,
			want: "defines no VERSION",
		},
		{
			name:  "smoke test that asserts the version",
			steps: smokeWith,
		},
		{
			name: "smoke test that only checks the exit status",
			steps: `    - name: Smoke-test the static sandbox-host artifact
      run: |
        tar xzf "agent-factory-linux-amd64-static.tar.gz" -C "$tmp"
        "$tmp/agent-factory" version
`,
			want: "exit status alone passes a \"dev\" binary",
		},
		{
			name: "smoke test that asserts a version it never defined",
			steps: `    - name: Smoke-test the static sandbox-host artifact
      run: |
        tar xzf "agent-factory-linux-amd64-static.tar.gz" -C "$tmp"
        .github/scripts/assert-binary-version.sh "$tmp/agent-factory" "$VERSION"
`,
			want: "exit status alone passes a \"dev\" binary",
		},
		{
			name: "smoke test whose assertion is only in a comment",
			steps: `    - name: Smoke-test the static sandbox-host artifact
      env:
        VERSION: ${{ inputs.version }}
      run: |
        tar xzf "agent-factory-linux-amd64-static.tar.gz" -C "$tmp"
        # .github/scripts/assert-binary-version.sh "$tmp/agent-factory" "$VERSION"
        "$tmp/agent-factory" version
`,
			want: "exit status alone passes a \"dev\" binary",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			findings := scanVersionStamps(map[string]string{"fixture.yml": head + tc.steps}).findings
			switch {
			case tc.want == "" && len(findings) != 0:
				t.Fatalf("unexpected findings: %q", findings)
			case tc.want != "" && len(findings) != 1:
				t.Fatalf("findings = %q, want exactly one containing %q", findings, tc.want)
			case tc.want != "" && !strings.Contains(findings[0], tc.want):
				t.Fatalf("finding %q does not contain %q", findings[0], tc.want)
			}
		})
	}
}

// TestReleaseWorkflowsStampAndAssertTheVersion is the lint over the real
// workflow files. It scans every workflow, so a new lane that stamps a version
// cannot skip the VERSION definition either.
func TestReleaseWorkflowsStampAndAssertTheVersion(t *testing.T) {
	dir := filepath.Join(repoRoot(t), ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	files := map[string]string{}
	for _, entry := range entries {
		if entry.IsDir() || (!strings.HasSuffix(entry.Name(), ".yml") && !strings.HasSuffix(entry.Name(), ".yaml")) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("read workflow: %v", err)
		}
		files[filepath.Join(".github", "workflows", entry.Name())] = string(data)
	}

	scan := scanVersionStamps(files)

	// Anti-vacuity: a lint that read the wrong files, or whose step splitter
	// stopped recognising the steps, would also report nothing. Each release
	// workflow builds twice with a stamp (the installed binary and the static
	// sandbox-host one) and smoke-tests the static archive once; if that
	// changes on purpose, change these with it.
	for _, wf := range []string{"stable-release.yml", "auto-release.yml"} {
		name := filepath.Join(".github", "workflows", wf)
		if got := scan.stampers[name]; got != 2 {
			t.Errorf("%s: found %d steps that link %s, want 2 — the lint is not seeing what it guards", name, got, stampFlag)
		}
		if got := scan.smokes[name]; got != 1 {
			t.Errorf("%s: found %d steps that unpack a static archive, want 1 — the lint is not seeing what it guards", name, got)
		}
	}

	if len(scan.findings) > 0 {
		t.Fatalf("release workflows can ship a binary that reports \"dev\":\n  %s", strings.Join(scan.findings, "\n  "))
	}
}

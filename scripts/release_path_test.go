package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ── the release path restores no cache and takes no untrusted input (#7194) ──
//
// A job that holds contents: write can publish a release, and can merge a pull
// request. Code it runs that a cache supplied, rather than the commit, can
// therefore reach users or land a change no one reviewed. Repository code
// can write such a cache:
//   - Build (build.yml) runs the merged tree's `go test` with setup-go's cache
//     on the same runner image and Go version as the release workflows. After a
//     go.sum change it saves a GOCACHE under the key they would restore,
//     holding whatever that test code put in it.
//   - Any job's code can reach ACTIONS_RUNTIME_TOKEN and write a key that does
//     not exist yet in the default branch's scope.
// Code merged and then reverted could keep running through that cache.
//
// What this pins is a data-flow property. The jobs it applies to (the release
// path) are:
//   - every job that holds contents: write, names a secret, or publishes;
//   - every job whose outputs a release-path job reads, transitively;
//   - every job of the workflow, when a release-path job downloads the run's
//     artifacts (any job of the run can upload one).
// Each release-path job:
//   - restores no cache: setup-go says `with: cache: false` (its default is
//     true, and the same key under `env:` does nothing);
//   - uses no action outside an allowlist known to restore none;
//   - downloads only named artifacts;
//   - interpolates no step output, input or event field into shell (a value
//     carried there runs as code; env: carries it as data);
//   - keeps no git credentials in a checkout unless it writes contents (#7200).
// An action this lint does not know is a finding, not a pass.
//
// The workflows are read with yaml.v3, so `with:` and `env:` stay distinct (a
// line scanner took `cache: false` under env: for the real input, #7200 R4).

// knownActions restore no cache, given the inputs checked below.
var knownActions = map[string]bool{
	"actions/checkout":          true,
	"actions/setup-go":          true,
	"actions/upload-artifact":   true,
	"actions/download-artifact": true,
	"actions/github-script":     true,
}

var (
	secretRef      = regexp.MustCompile(`secrets\.(\w+)`)
	readsOutputsOf = regexp.MustCompile(`needs\.([\w-]+)\.outputs`)
	// ${{ }} carrying a value that a step, a job, an input or the event chose.
	untrustedExpr = regexp.MustCompile(`\$\{\{[^}]*\b(needs|steps|inputs|github\.event)\b`)
)

type ghStep struct {
	Name string         `yaml:"name"`
	ID   string         `yaml:"id"`
	Uses string         `yaml:"uses"`
	With map[string]any `yaml:"with"`
	Env  map[string]any `yaml:"env"`
	Run  string         `yaml:"run"`
}

func (s ghStep) title() string {
	for _, t := range []string{s.Name, s.ID, s.Uses} {
		if t != "" {
			return t
		}
	}
	return "run step"
}

func (s ghStep) action() string { return strings.SplitN(s.Uses, "@", 2)[0] }

type ghJob struct {
	Permissions any      `yaml:"permissions"`
	Steps       []ghStep `yaml:"steps"`
	text        string   // the job re-marshalled, for expression scans
}

type ghWorkflow struct {
	Permissions any               `yaml:"permissions"`
	Jobs        map[string]*ghJob `yaml:"jobs"`
}

func parseWorkflow(data []byte) (*ghWorkflow, error) {
	var wf ghWorkflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		return nil, err
	}
	var raw struct {
		Jobs map[string]any `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	for id, job := range wf.Jobs {
		text, err := yaml.Marshal(raw.Jobs[id])
		if err != nil {
			return nil, err
		}
		job.text = string(text)
	}
	return &wf, nil
}

// writesContents reports whether a permissions value grants contents: write.
// Unset permissions mean the repository default, which may: that fails closed.
func writesContents(perms any) bool {
	switch p := perms.(type) {
	case nil:
		return true
	case string:
		return p == "write-all"
	case map[string]any:
		return p["contents"] == "write"
	}
	return false
}

func (wf *ghWorkflow) permissionsOf(job *ghJob) any {
	if job.Permissions != nil {
		return job.Permissions
	}
	return wf.Permissions
}

func isFalse(v any) bool { return v == false || v == "false" }

// releasePath returns the ids of the jobs the lint applies to (see above).
func releasePath(wf *ghWorkflow) map[string]bool {
	var queue []string
	for id, job := range wf.Jobs {
		seed := writesContents(wf.permissionsOf(job))
		for _, m := range secretRef.FindAllStringSubmatch(job.text, -1) {
			seed = seed || m[1] != "GITHUB_TOKEN"
		}
		for _, step := range job.Steps {
			seed = seed || strings.Contains(step.Run, "gh release")
		}
		if seed {
			queue = append(queue, id)
		}
	}
	path := map[string]bool{}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		job, ok := wf.Jobs[id]
		if !ok || path[id] {
			continue
		}
		path[id] = true
		for _, m := range readsOutputsOf.FindAllStringSubmatch(job.text, -1) {
			queue = append(queue, m[1])
		}
		for _, step := range job.Steps {
			if step.action() == "actions/download-artifact" {
				for other := range wf.Jobs {
					queue = append(queue, other)
				}
			}
		}
	}
	return path
}

type releasePathScan struct {
	path      map[string][]string // release-path job ids, per file
	setupGo   map[string]int      // setup-go steps on the path, per file
	downloads map[string]int      // artifact downloads on the path, per file
	findings  []string
}

func scanReleasePaths(files map[string]string) releasePathScan {
	scan := releasePathScan{path: map[string][]string{}, setupGo: map[string]int{}, downloads: map[string]int{}}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		wf, err := parseWorkflow([]byte(files[name]))
		if err != nil {
			scan.findings = append(scan.findings, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		path := releasePath(wf)
		ids := make([]string, 0, len(path))
		for id := range path {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		scan.path[name] = ids
		for _, id := range ids {
			job := wf.Jobs[id]
			finding := func(step ghStep, format string, args ...any) {
				scan.findings = append(scan.findings,
					fmt.Sprintf("%s: job %s, %q: ", name, id, step.title())+fmt.Sprintf(format, args...))
			}
			for _, step := range job.Steps {
				if step.Uses != "" && !knownActions[step.action()] {
					finding(step, "uses %s, which this lint does not know to restore no cache", step.action())
				}
				switch step.action() {
				case "actions/setup-go":
					scan.setupGo[name]++
					if !isFalse(step.With["cache"]) {
						finding(step, "setup-go restores a Go cache (it caches unless `with: cache: false`)")
					}
				case "actions/checkout":
					if !writesContents(wf.permissionsOf(job)) && !isFalse(step.With["persist-credentials"]) {
						finding(step, "the checkout keeps git credentials in a job that does not write contents")
					}
				case "actions/download-artifact":
					scan.downloads[name]++
					if fmt.Sprint(step.With["pattern"]) == "<nil>" && fmt.Sprint(step.With["name"]) == "<nil>" {
						finding(step, "downloads every artifact of the run; name the ones the build uploads (`pattern:`)")
					}
				}
				if m := untrustedExpr.FindString(step.Run); m != "" {
					finding(step, "interpolates %q… into shell; pass it through env:", m)
				}
			}
		}
	}
	return scan
}

func TestReleasePathLintFixtures(t *testing.T) {
	const head = "on:\n  workflow_dispatch:\npermissions:\n  contents: read\njobs:\n"
	const checkout = "      - uses: actions/checkout@v6\n        with:\n          persist-credentials: false\n"
	setupGo := func(with string) string {
		return "      - name: Set up Go\n        uses: actions/setup-go@v6\n        with:\n          go-version: '1.25'\n" + with
	}
	const upload = "      - uses: actions/upload-artifact@v7\n        with:\n          name: agent-factory-linux\n"
	const publish = "  publish:\n    needs: [build]\n    permissions:\n      contents: write\n    steps:\n" +
		"      - uses: actions/download-artifact@v7\n        with:\n          pattern: agent-factory-*\n" +
		"      - run: gh release create v1 dist/*\n"
	job := func(id string, steps ...string) string {
		return "  " + id + ":\n    steps:\n" + strings.Join(steps, "")
	}
	for _, tc := range []struct {
		name     string
		jobs     string
		findings int
	}{
		{"a build that turns the cache off", job("build", checkout, setupGo("          cache: false\n"), upload) + publish, 0},
		{"a build that asks for the cache", job("build", checkout, setupGo("          cache: true\n"), upload) + publish, 1},
		{"a build that leaves setup-go at its default", job("build", checkout, setupGo(""), upload) + publish, 1},
		{"cache: false under env:, where setup-go never reads it", job("build", checkout,
			"      - uses: actions/setup-go@v6\n        env:\n          cache: false\n", upload) + publish, 1},
		{"a build that restores with actions/cache", job("build", checkout,
			"      - uses: actions/cache@v4\n        with:\n          path: ~/go\n", setupGo("          cache: false\n"), upload) + publish, 1},
		{"a new job that signs with a cached toolchain and uploads to the release", job("build", checkout,
			setupGo("          cache: false\n"), upload) + publish + "  sign:\n    permissions:\n      contents: write\n    steps:\n" +
			checkout + setupGo("") + "      - run: gh release upload v1 sig\n", 1},
		{"a preflight whose output the release reads, with a cache", job("check", checkout, setupGo("          cache: true\n"),
			"      - id: v\n        run: echo v=1 >> \"$GITHUB_OUTPUT\"\n") + "    outputs:\n      v: ${{ steps.v.outputs.v }}\n" +
			"  release:\n    needs: [check]\n    permissions:\n      contents: write\n    steps:\n" +
			"      - env:\n          V: ${{ needs.check.outputs.v }}\n        run: gh release create \"v$V\"\n", 1},
		{"a cached job in a run whose publisher downloads the run's artifacts", job("test", checkout, setupGo("")) +
			job("build", checkout, setupGo("          cache: false\n"), upload) + publish, 1},
		{"a cached job a writer takes nothing from", job("test", checkout, setupGo("")) +
			"  reconcile:\n    needs: [test]\n    permissions:\n      contents: write\n    steps:\n      - run: gh api repos/o/r/dispatches\n", 0},
		{"a publisher that downloads every artifact", job("build", checkout, setupGo("          cache: false\n"), upload) +
			"  publish:\n    permissions:\n      contents: write\n    steps:\n      - uses: actions/download-artifact@v7\n" +
			"        with:\n          path: dist\n      - run: gh release create v1 dist/*\n", 1},
		{"a step output interpolated into shell", job("build", checkout, setupGo("          cache: false\n"),
			"      - run: go build -ldflags \"-X main.version=${{ needs.check.outputs.v }}\"\n", upload) + publish, 1},
		{"a read-only release-path job that keeps its git credentials", job("build",
			"      - uses: actions/checkout@v6\n", setupGo("          cache: false\n"), upload) + publish, 1},
		{"an action the lint does not know", job("build", checkout, "      - uses: some/build-action@v1\n", upload) + publish, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scan := scanReleasePaths(map[string]string{"fixture.yml": head + tc.jobs})
			if len(scan.findings) != tc.findings {
				t.Errorf("findings = %q, want %d", scan.findings, tc.findings)
			}
		})
	}
}

func readWorkflows(t *testing.T) map[string]string {
	t.Helper()
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
	return files
}

// TestReleasePathRestoresNoCache is the lint over the real workflow files. It
// scans every workflow, so a new job that writes contents is held to it too.
func TestReleasePathRestoresNoCache(t *testing.T) {
	scan := scanReleasePaths(readWorkflows(t))

	// Anti-vacuity: in each release workflow the preflight, the build and the
	// publisher are on the path, and the path sets up Go twice (preflight and
	// build) and downloads once. A parser that stopped seeing them would pass.
	for wf, jobs := range map[string]string{
		"auto-release.yml":   "build check release",
		"stable-release.yml": "build publish validate",
	} {
		name := filepath.Join(".github", "workflows", wf)
		if got := strings.Join(scan.path[name], " "); got != jobs {
			t.Errorf("%s: release path = %q, want %q — the lint is not seeing what it guards", name, got, jobs)
		}
		if got := scan.setupGo[name]; got != 2 {
			t.Errorf("%s: found %d setup-go steps on the release path, want 2", name, got)
		}
		if got := scan.downloads[name]; got != 1 {
			t.Errorf("%s: found %d artifact downloads on the release path, want 1", name, got)
		}
	}

	if len(scan.findings) > 0 {
		t.Fatalf("the release path can run what a cache or an untrusted value supplied:\n  %s", strings.Join(scan.findings, "\n  "))
	}
}

// TestPreviewVersionIsCheckedBeforeItLeavesCheck runs auto-release.yml's own
// "Compute preview version" step. new_version becomes a tag, an archive name
// and a linker flag, so a value that is not a preview version must stop the
// run there, not travel on (#7200 R5).
func TestPreviewVersionIsCheckedBeforeItLeavesCheck(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", "auto-release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	wf, err := parseWorkflow(data)
	if err != nil {
		t.Fatal(err)
	}
	var script string
	for _, step := range wf.Jobs["check"].Steps {
		if step.ID == "compute" {
			script = step.Run
		}
	}
	if script == "" {
		t.Fatal(`auto-release.yml: check has no step with id "compute"`)
	}
	for _, tc := range []struct {
		printed string
		valid   bool
	}{
		{"1.0.352-preview-3", true},
		{"1.0.352-preview-3 -X main.other=x", false},
		{"1.0.352-preview-3\"; touch pwned; \"", false},
		{"1.0.352", false},
		{"", false},
	} {
		t.Run(tc.printed, func(t *testing.T) {
			dir := t.TempDir()
			scriptDir := filepath.Join(dir, ".github", "scripts")
			if err := os.MkdirAll(scriptDir, 0o755); err != nil {
				t.Fatal(err)
			}
			writeExecutable(t, filepath.Join(scriptDir, "next-preview-version.sh"),
				"#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '"+strings.ReplaceAll(tc.printed, "'", `'\''`)+"'\n")
			temp := t.TempDir()
			if err := os.WriteFile(filepath.Join(temp, "released-tags.txt"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(dir, "github-output")
			cmd := exec.Command("bash", "--noprofile", "--norc", "-eo", "pipefail", "-c", script)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "RUNNER_TEMP="+temp, "GITHUB_OUTPUT="+output)
			out, runErr := cmd.CombinedOutput()
			written, _ := os.ReadFile(output)
			if tc.valid {
				if runErr != nil || string(written) != "new_version="+tc.printed+"\n" {
					t.Fatalf("a valid version was refused: err=%v output=%q log=%s", runErr, written, out)
				}
				return
			}
			if runErr == nil || strings.Contains(string(written), "new_version") {
				t.Fatalf("an invalid version left check: err=%v output=%q", runErr, written)
			}
			if _, err := os.Stat(filepath.Join(dir, "pwned")); err == nil {
				t.Fatal("the version ran as shell")
			}
		})
	}
}

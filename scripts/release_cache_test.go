package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// ── release builds restore no cache (#7194) ─────────────────────────────────
//
// A release build that restores an Actions cache compiles from whatever that
// cache holds. Code in repository CI can write such a cache: Build (build.yml)
// runs the merged tree's `go test` with setup-go's cache on the same runner
// image and Go version as the release builds, so after a go.sum change it saves
// a GOCACHE under the very key they restore, with whatever that test code put
// in it. And any job's code can reach ACTIONS_RUNTIME_TOKEN and write a key
// that does not exist yet in the default branch's scope. Merged code ships in
// the release anyway, so what a poisoned cache buys is persistence: the code is
// reverted from source, and the cache keeps feeding release binaries.
//
// So in a workflow that publishes a release (it runs `gh release create`), a
// job whose output ships, because it uploads a build artifact or publishes the
// release, restores no cache. setup-go must say `cache: false` (its default
// is true), and every other action it uses must be one known to restore none.
// An action this lint does not know is a finding, not a pass: a new action in a
// release job is exactly the change it exists to make someone look at.

// noCacheActions restore no cache whatever their inputs. setup-go is checked
// separately, because it restores one unless told not to.
var noCacheActions = map[string]bool{
	"actions/checkout":          true,
	"actions/upload-artifact":   true,
	"actions/download-artifact": true,
	"actions/github-script":     true,
}

var (
	usesAction = regexp.MustCompile(`(?m)^\s*(?:-\s+)?uses:\s*([^@\s]+)`)
	cacheOff   = regexp.MustCompile(`(?m)^\s+cache:\s*false\s*$`)
	jobKey     = regexp.MustCompile(`^  ([\w-]+):\s*$`)
)

// releaseJob is one entry of a workflow's `jobs:` map.
type releaseJob struct {
	id   string
	text string // the job's lines, from its key to the next job's
}

// releaseJobs splits a workflow file into jobs: each runs from a two-space
// key under `jobs:` to the next one.
func releaseJobs(text string) []releaseJob {
	var (
		jobs   []releaseJob
		inJobs bool
	)
	for _, line := range strings.Split(text, "\n") {
		if !inJobs {
			inJobs = line == "jobs:"
			continue
		}
		if m := jobKey.FindStringSubmatch(line); m != nil {
			jobs = append(jobs, releaseJob{id: m[1]})
			continue
		}
		if len(jobs) > 0 {
			jobs[len(jobs)-1].text += line + "\n"
		}
	}
	return jobs
}

// shipsOutput reports whether a release workflow's job's output reaches users:
// it uploads a build artifact, or it publishes the release.
func shipsOutput(job releaseJob) bool {
	return strings.Contains(job.text, "actions/upload-artifact@") || strings.Contains(job.text, "gh release create")
}

type releaseCacheScan struct {
	shipping map[string]int // jobs whose output ships, per file
	setupGo  map[string]int // setup-go steps in those jobs, per file
	findings []string
}

func scanReleaseCaches(files map[string]string) releaseCacheScan {
	scan := releaseCacheScan{shipping: map[string]int{}, setupGo: map[string]int{}}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		// Other workflows upload logs and test binaries, which reach no user.
		if !strings.Contains(files[name], "gh release create") {
			continue
		}
		for _, job := range releaseJobs(files[name]) {
			if !shipsOutput(job) {
				continue
			}
			scan.shipping[name]++
			for _, step := range workflowSteps("jobs:\n" + job.text) {
				for _, m := range usesAction.FindAllStringSubmatch(step.body, -1) {
					action := m[1]
					switch {
					case action == "actions/setup-go":
						scan.setupGo[name]++
						if !cacheOff.MatchString(step.body) {
							scan.findings = append(scan.findings, fmt.Sprintf(
								"%s: job %s ships its output but %q restores a Go cache (setup-go caches unless `cache: false`)",
								name, job.id, step.title))
						}
					case !noCacheActions[action]:
						scan.findings = append(scan.findings, fmt.Sprintf(
							"%s: job %s ships its output and uses %s, which this lint does not know to restore no cache",
							name, job.id, action))
					}
				}
			}
		}
	}
	return scan
}

func TestReleaseCacheLintFixtures(t *testing.T) {
	const head = "on:\n  workflow_dispatch:\njobs:\n"
	const checkout = "    - uses: actions/checkout@v6\n"
	const upload = "    - uses: actions/upload-artifact@v7\n      with:\n        name: af\n"
	setupGo := func(with string) string {
		return "    - name: Set up Go\n      uses: actions/setup-go@v6\n      with:\n        go-version: '1.25'\n" + with
	}
	job := func(id string, steps ...string) string {
		return "  " + id + ":\n    runs-on: ubuntu-24.04\n    steps:\n" + strings.Join(steps, "")
	}
	t.Run("a workflow that publishes no release may cache what it uploads", func(t *testing.T) {
		scan := scanReleaseCaches(map[string]string{"ci.yml": head + job("test", checkout, setupGo(""), upload)})
		if len(scan.findings) != 0 || scan.shipping["ci.yml"] != 0 {
			t.Errorf("findings = %q, shipping = %d; want none", scan.findings, scan.shipping["ci.yml"])
		}
	})
	for _, tc := range []struct {
		name     string
		workflow string
		findings int
	}{
		{"a build that turns the cache off", job("build", checkout, setupGo("        cache: false\n"), upload), 0},
		{"a build that asks for the cache", job("build", checkout, setupGo("        cache: true\n"), upload), 1},
		{"a build that leaves setup-go at its default", job("build", checkout, setupGo(""), upload), 1},
		{"a build that restores with actions/cache", job("build", checkout, "    - uses: actions/cache@v4\n      with:\n        path: ~/go\n", setupGo("        cache: false\n"), upload), 1},
		{"a publish job with an action the lint does not know", job("publish", checkout, "    - uses: some/release-action@v1\n", "    - run: gh release create v1\n"), 1},
		{"a preflight that ships nothing may cache", job("check", checkout, setupGo("        cache: true\n"), "    - run: go test ./...\n"), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Every fixture is a release workflow: its last job publishes.
			workflow := head + tc.workflow + job("release", "    - run: gh release create v1\n")
			scan := scanReleaseCaches(map[string]string{"fixture.yml": workflow})
			if len(scan.findings) != tc.findings {
				t.Errorf("findings = %q, want %d", scan.findings, tc.findings)
			}
		})
	}
}

// TestReleaseBuildsRestoreNoCache is the lint over the real workflow files. It
// scans every workflow, so a new job that ships a binary is held to it too.
func TestReleaseBuildsRestoreNoCache(t *testing.T) {
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

	scan := scanReleaseCaches(files)

	// Anti-vacuity: each release workflow ships from a build job (which sets up
	// Go) and a publish job. A splitter that stopped seeing them would pass.
	for _, wf := range []string{"stable-release.yml", "auto-release.yml"} {
		name := filepath.Join(".github", "workflows", wf)
		if got := scan.shipping[name]; got != 2 {
			t.Errorf("%s: found %d jobs whose output ships, want 2 (build and publish) — the lint is not seeing what it guards", name, got)
		}
		if got := scan.setupGo[name]; got != 1 {
			t.Errorf("%s: found %d setup-go steps in those jobs, want 1 — the lint is not seeing what it guards", name, got)
		}
	}

	if len(scan.findings) > 0 {
		t.Fatalf("a release build can restore a cache that repository code wrote:\n  %s", strings.Join(scan.findings, "\n  "))
	}
}

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Run the real entry point so exit codes and command stream handling are covered.
func TestPromptDriftProcess(t *testing.T) {
	if os.Getenv("PROMPT_DRIFT_PROCESS") != "1" {
		return
	}
	os.Args = []string{"prompt-drift", "test", "prompt.md"}
	main()
	os.Exit(0)
}

func TestPromptDrift(t *testing.T) {
	for _, tc := range []struct {
		name, live, raw, stderr, failure string
		code                             int
		evidence                         string
	}{
		{name: "stale working file", live: "watch\n"},
		{name: "real drift", live: "changed\n", code: 1, evidence: "live prompt differs from origin/master:prompt.md"},
		{name: "whitespace drift", live: "watch", code: 1, evidence: "live prompt differs from origin/master:prompt.md"},
		{name: "fetch failure", failure: "fetch", code: 2, evidence: "does not appear to be a git repository"},
		{name: "unreadable ref file", failure: "show", code: 2, evidence: "not in 'origin/master'"},
		{name: "stderr error envelope", stderr: `{"data":null,"error":{"message":"failed to get task: task with id test not found"}}`, code: 2, evidence: "failed to get task: task with id test not found"},
		{name: "plain stderr", stderr: "daemon unavailable", code: 2, evidence: "daemon unavailable"},
		{name: "invalid JSON", raw: "invalid", code: 2, evidence: "decode task"},
		{name: "null data", raw: `{"data":null}`, code: 2, evidence: "did not return prompt"},
		{name: "wrong task", raw: `{"data":{"id":"other","prompt":"watch\n"}}`, code: 2, evidence: "did not return prompt"},
		{name: "missing prompt", raw: `{"data":{"id":"test"}}`, code: 2, evidence: "did not return prompt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote, repo, bin := t.TempDir(), t.TempDir(), t.TempDir()
			git := func(dir string, args ...string) {
				t.Helper()
				cmd := exec.Command("git", args...)
				cmd.Dir = dir
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v: %s", args, err, out)
				}
			}
			write := func(path, value string, mode os.FileMode) {
				t.Helper()
				if err := os.WriteFile(path, []byte(value), mode); err != nil {
					t.Fatal(err)
				}
			}
			git(remote, "init", "-b", "master")
			write(filepath.Join(remote, "prompt.md"), "watch\n", 0600)
			git(remote, "add", ".")
			git(remote, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "prompt")
			git(repo, "clone", remote, ".")
			// The local checkout is deliberately wrong in every case. Only master counts.
			write(filepath.Join(repo, "prompt.md"), "stale checkout\n", 0600)
			if tc.failure == "fetch" {
				git(repo, "remote", "set-url", "origin", filepath.Join(remote, "missing"))
			}
			if tc.failure == "show" {
				git(remote, "rm", "prompt.md")
				git(remote, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "remove prompt")
			}
			raw := tc.raw
			if raw == "" {
				data, err := json.Marshal(map[string]any{"data": map[string]string{"id": "test", "prompt": tc.live}})
				if err != nil {
					t.Fatal(err)
				}
				raw = string(data)
			}
			write(filepath.Join(bin, "af"), "#!/bin/sh\nif [ -n \"$AF_TEST_STDERR\" ]; then\n printf '%s' \"$AF_TEST_STDERR\" >&2\n exit 1\nfi\nprintf '%s' \"$AF_TEST_STDOUT\"\n", 0700)
			cmd := exec.Command(os.Args[0], "-test.run=^TestPromptDriftProcess$")
			cmd.Dir = repo
			cmd.Env = append(os.Environ(), "PROMPT_DRIFT_PROCESS=1", "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "AF_TEST_STDOUT="+raw, "AF_TEST_STDERR="+tc.stderr)
			out, err := cmd.Output()
			code := 0
			if err != nil {
				if ee, ok := err.(*exec.ExitError); ok {
					code = ee.ExitCode()
				} else {
					t.Fatal(err)
				}
			}
			if code != tc.code {
				t.Errorf("exit = %d, want %d; stdout: %s", code, tc.code, out)
			}
			if tc.code == 0 && len(out) != 0 {
				t.Errorf("success should be silent: %s", out)
			}
			if tc.code != 0 {
				prefix := "FINDING:"
				if tc.code == 2 {
					prefix = "TOOLING:"
				}
				if !strings.HasPrefix(string(out), prefix) || !strings.Contains(string(out), tc.evidence) {
					t.Errorf("want %s and %q; stdout: %s", prefix, tc.evidence, out)
				}
				if tc.code == 2 && (!strings.Contains(string(out), "could not establish master's version (") || strings.Contains(string(out), "FINDING:")) {
					t.Errorf("invalid tooling outcome: %s", out)
				}
			}
		})
	}
}

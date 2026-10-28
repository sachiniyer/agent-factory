package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests drive scripts/docs-deploy-changed.sh (#5659) against a throwaway
// git repo and a fake `gh` on PATH, so the "diff against the last deployed
// commit" decision is checked without network or a real Pages deployment.

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commitFile(t *testing.T, dir, path string) string {
	t.Helper()
	full := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	prev, _ := os.ReadFile(full)
	if err := os.WriteFile(full, append(prev, 'x'), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", path)
	return gitIn(t, dir, "rev-parse", "HEAD")
}

// fakeGH writes a gh that serves `deployments` rows ("id sha") and a status per
// id ("id=state"); anything else exits 1.
func fakeGH(t *testing.T, rows []string, states map[string]string) string {
	t.Helper()
	bin := t.TempDir()
	var b strings.Builder
	b.WriteString("#!/usr/bin/env bash\n")
	b.WriteString("case \"$2\" in\n")
	b.WriteString("  *deployments?environment=github-pages*) printf '%s\\n' " + shellQuote(strings.Join(rows, "\n")) + " ;;\n")
	for id, st := range states {
		b.WriteString("  */deployments/" + id + "/statuses*) printf '%s\\n' " + shellQuote(st) + " ;;\n")
	}
	b.WriteString("  *) exit 1 ;;\nesac\n")
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func runDocsScript(t *testing.T, repo, ghBin string, env ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", filepath.Join(repoRoot(t), "scripts", "docs-deploy-changed.sh"))
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "PATH="+ghBin+":"+os.Getenv("PATH"),
		"GITHUB_REPOSITORY=o/r", "EVENT_NAME=push", "DEPLOY_DOCS=", "VERIFY_SHA=", "GIT_CONFIG_GLOBAL=/dev/null")
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func TestDocsDeployChanged(t *testing.T) {
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", "master")
	c1 := commitFile(t, repo, "docs/a.md")
	c2 := commitFile(t, repo, "docs/b.md")   // docs change whose run was cancelled
	c3 := commitFile(t, repo, "daemon/x.go") // code-only push that superseded it

	push := func(before, after string) []string {
		return []string{"EVENT_BEFORE=" + before, "EVENT_SHA=" + after}
	}

	t.Run("a superseded docs run is picked up by the survivor", func(t *testing.T) {
		gh := fakeGH(t, []string{"2 " + c1}, map[string]string{"2": "success"})
		got, err := runDocsScript(t, repo, gh, push(c2, c3)...)
		if err != nil || got != "docs_changed=true" {
			t.Fatalf("got %q, %v; want docs_changed=true", got, err)
		}
	})
	t.Run("nothing pending once the tip is deployed", func(t *testing.T) {
		gh := fakeGH(t, []string{"2 " + c3}, map[string]string{"2": "success"})
		got, _ := runDocsScript(t, repo, gh, push(c2, c3)...)
		if got != "docs_changed=false" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("a failed newer deployment is skipped for the last success", func(t *testing.T) {
		gh := fakeGH(t, []string{"3 " + c3, "2 " + c1}, map[string]string{"3": "failure", "2": "success"})
		got, _ := runDocsScript(t, repo, gh, push(c2, c3)...)
		if got != "docs_changed=true" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("no deployment falls back to the push range", func(t *testing.T) {
		gh := fakeGH(t, nil, nil)
		got, _ := runDocsScript(t, repo, gh, push(c2, c3)...)
		if got != "docs_changed=false" {
			t.Fatalf("code-only range: got %q", got)
		}
		got, _ = runDocsScript(t, repo, gh, push(c1, c2)...)
		if got != "docs_changed=true" {
			t.Fatalf("docs range: got %q", got)
		}
	})
	t.Run("API failure falls back to the push range", func(t *testing.T) {
		bin := t.TempDir()
		_ = os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/usr/bin/env bash\nexit 1\n"), 0o755)
		got, err := runDocsScript(t, repo, bin, push(c2, c3)...)
		if err != nil || got != "docs_changed=false" {
			t.Fatalf("got %q, %v", got, err)
		}
	})
	t.Run("a base that is not an ancestor falls back", func(t *testing.T) {
		gitIn(t, repo, "checkout", "-q", "-b", "side", c1)
		other := commitFile(t, repo, "docs/side.md")
		gitIn(t, repo, "checkout", "-q", "master")
		gh := fakeGH(t, []string{"2 " + other}, map[string]string{"2": "success"})
		got, _ := runDocsScript(t, repo, gh, push(c2, c3)...)
		if got != "docs_changed=false" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("verify_sha is the commit under examination", func(t *testing.T) {
		gh := fakeGH(t, []string{"2 " + c1}, map[string]string{"2": "success"})
		got, _ := runDocsScript(t, repo, gh, "EVENT_NAME=workflow_dispatch", "DEPLOY_DOCS=auto", "VERIFY_SHA="+c2, "EVENT_SHA="+c3)
		if got != "docs_changed=true" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("an older, already-deployed verify_sha does not hide a newer pending docs commit (#5688)", func(t *testing.T) {
		// verify_sha=c1 is deployed; c2 (docs) is pending; the run builds c3.
		gh := fakeGH(t, []string{"2 " + c1}, map[string]string{"2": "success"})
		got, _ := runDocsScript(t, repo, gh, "EVENT_NAME=workflow_dispatch", "DEPLOY_DOCS=auto", "VERIFY_SHA="+c1, "EVENT_SHA="+c3)
		if got != "docs_changed=true" {
			t.Fatalf("got %q; want docs_changed=true", got)
		}
	})
	t.Run("forced dispatch values pass through", func(t *testing.T) {
		got, _ := runDocsScript(t, repo, fakeGH(t, nil, nil), "EVENT_NAME=workflow_dispatch", "DEPLOY_DOCS=true", "EVENT_SHA="+c3)
		if got != "docs_changed=true" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("malformed verify_sha is rejected", func(t *testing.T) {
		_, err := runDocsScript(t, repo, fakeGH(t, nil, nil), "VERIFY_SHA=$(id)", "EVENT_SHA="+c3)
		if err == nil {
			t.Fatal("expected failure")
		}
	})
}

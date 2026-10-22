package proposal

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func testGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Env = gitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func appendFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

func assertAbsent(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s exists: af followed agent-controlled state", p)
		}
	}
}

func TestAgentGitConfigCannotRunPrograms(t *testing.T) {
	s, _ := newTestStore(t)
	startAttached(t, s, "p", sessA)
	if err := s.Attach("p", sessB); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	mark := func(n string) string { return filepath.Join(out, n) }
	appendFile(t, filepath.Join(s.Dir(), ".git", "config"),
		"[core]\n\tfsmonitor = \"touch "+mark("fsmonitor")+"; false\"\n"+
			"[diff]\n\texternal = \"touch "+mark("external")+"; true\"\n"+
			"[diff \"x\"]\n\ttextconv = \"touch "+mark("textconv")+"; cat\"\n"+
			"[filter \"x\"]\n\tclean = \"touch "+mark("clean")+"; cat\"\n")
	appendFile(t, filepath.Join(s.Dir(), ".gitattributes"), "* filter=x diff=x\n")

	editOnDisk(t, s, "p", "goals.md", "external edit\n")
	if _, _, err := s.Write("p", "technical", "human edit\n", Committer{Author: AuthorHuman}); err != nil {
		t.Fatal(err)
	}
	c, err := s.Changes(sessB)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Edits) == 0 || c.Edits[len(c.Edits)-1].Patch == "" {
		t.Fatalf("Changes = %+v, want edits with patches", c)
	}
	assertAbsent(t, mark("fsmonitor"), mark("external"), mark("textconv"), mark("clean"))
}

func TestGitdirPointerCannotRedirectCommits(t *testing.T) {
	s, _ := newTestStore(t)
	startAttached(t, s, "p", sessA)
	other := t.TempDir()
	testGit(t, other, "init", "-q")
	testGit(t, other, "commit", "-q", "--allow-empty", "-m", "user work")
	before := testGit(t, other, "rev-parse", "HEAD")

	if err := os.RemoveAll(filepath.Join(s.Dir(), ".git")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir(), ".git"), []byte("gitdir: "+filepath.Join(other, ".git")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	editOnDisk(t, s, "p", "goals.md", "external edit\n")
	if _, _, err := s.Write("p", "technical", "human edit\n", Committer{Author: AuthorHuman}); err != nil {
		t.Fatal(err)
	}
	if after := testGit(t, other, "rev-parse", "HEAD"); after != before {
		t.Fatalf("af committed into %s (HEAD %s -> %s)", other, before, after)
	}
}

// oldLayoutStore builds a store the way earlier builds did: the git dir inside
// the docs folder.
func oldLayoutStore(t *testing.T, s *Store) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(s.Dir(), "p"), 0o755); err != nil {
		t.Fatal(err)
	}
	testGit(t, s.Dir(), "init", "-q", "--template=")
	if err := os.WriteFile(filepath.Join(s.Dir(), "p", "goals.md"), []byte("old goals\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, s.Dir(), "add", "-A")
	testGit(t, s.Dir(), "commit", "-q", "-m", "start p\n\nAf-Author: agent\nAf-Session: "+sessA+"\n")
	cmd := exec.Command("git", "-C", s.Dir(), "hash-object", "-w", "--stdin")
	cmd.Env = gitEnv()
	cmd.Stdin = strings.NewReader("p\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	testGit(t, s.Dir(), "update-ref", attachRefPrefix+sessA, strings.TrimSpace(string(out)))
	testGit(t, s.Dir(), "update-ref", cursorRefPrefix+sessA, "HEAD")
}

func TestMigratesInTreeGitDirWithoutItsConfig(t *testing.T) {
	s, _ := newTestStore(t)
	oldLayoutStore(t, s)
	mark := filepath.Join(t.TempDir(), "fsmonitor")
	appendFile(t, filepath.Join(s.Dir(), ".git", "config"), "[core]\n\tfsmonitor = \"touch "+mark+"; false\"\n")
	appendFile(t, filepath.Join(s.Dir(), ".git", "hooks", "pre-commit"), "#!/bin/sh\ntouch "+mark+"\n")

	if _, _, err := s.Write("p", "goals", "new goals\n", agent(sessA)); err != nil {
		t.Fatal(err)
	}
	assertAbsent(t, mark, filepath.Join(s.Dir(), ".git"))
	slug, err := s.Attached(sessA)
	if err != nil || slug != "p" {
		t.Fatalf("Attached = %q, %v; want p", slug, err)
	}
	log, err := s.git(nil, "log", "--format=%s")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log, "start p") || !strings.Contains(log, "write p/goals.md") {
		t.Fatalf("history not carried over:\n%s", log)
	}
}

func TestMigrationRefusesSymlinkInOldGitDir(t *testing.T) {
	s, _ := newTestStore(t)
	oldLayoutStore(t, s)
	outside := t.TempDir()
	objects := filepath.Join(s.Dir(), ".git", "objects", "ff")
	if err := os.RemoveAll(objects); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, objects); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Write("p", "goals", "new goals\n", agent(sessA)); err == nil {
		t.Fatal("Write succeeded on a store whose old git dir holds a symlink")
	}
	assertAbsent(t, filepath.Join(filepath.Dir(s.Dir()), "proposals.git"))
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Fatalf("migration wrote into %s: %v", outside, entries)
	}
}

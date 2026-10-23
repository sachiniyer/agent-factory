package proposal

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportedReplaceRefsAreIgnored(t *testing.T) {
	s, _ := newTestStore(t)
	oldLayoutStore(t, s)
	real := testGit(t, s.Dir(), "rev-parse", "HEAD:p/goals.md")
	cmd := exec.Command("git", "-C", s.Dir(), "hash-object", "-w", "--stdin")
	cmd.Env = gitEnv()
	cmd.Stdin = strings.NewReader("planted\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	testGit(t, s.Dir(), "update-ref", "refs/replace/"+real, strings.TrimSpace(string(out)))
	if err := snapshotAll(s); err != nil {
		t.Fatal(err)
	}
	got, err := s.git(nil, "cat-file", "blob", "HEAD:p/goals.md")
	if err != nil {
		t.Fatal(err)
	}
	if got != "old goals\n" {
		t.Fatalf("HEAD:p/goals.md = %q; a planted replace ref changed what af reads", got)
	}
}

func TestCreateRollbackRemovesNothingThroughALinkedRoot(t *testing.T) {
	s, _ := newTestStore(t)
	startAttached(t, s, "p", sessA)
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "p"), 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(outside, "p", "keep.md")
	if err := os.WriteFile(keep, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(s.Dir(), s.Dir()+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, s.Dir()); err != nil {
		t.Fatal(err)
	}
	if err := s.removeSlugDir("p"); err == nil {
		t.Fatal("removeSlugDir succeeded through a linked store root")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("the rollback deleted a folder outside the store: %v", err)
	}
}

func TestCreateRollbackRemovesTheNewFolder(t *testing.T) {
	s, _ := newTestStore(t)
	startAttached(t, s, "p", sessA)
	unlock := plantRefLock(t, s, "refs/heads/main")
	defer unlock()
	if err := s.Create("q", []string{"goals"}, agent(sessA)); err == nil {
		t.Fatal("Create succeeded with the branch ref locked")
	}
	if _, err := os.Lstat(s.SlugPath("q")); !os.IsNotExist(err) {
		t.Fatalf("a failed Create left its folder behind: %v", err)
	}
}

func TestSlugsRefusesASymlinkedStoreRoot(t *testing.T) {
	s, _ := newTestStore(t)
	startAttached(t, s, "p", sessA)
	moved := s.Dir() + ".real"
	if err := os.Rename(s.Dir(), moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(s.Dir()+".empty", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(s.Dir()+".empty", s.Dir()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Slugs(); err == nil {
		t.Fatal("Slugs listed a store root that is a symlink")
	}
}

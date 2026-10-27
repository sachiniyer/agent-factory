package proposal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func plantNestedRepo(t *testing.T, dir, mark string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	testGit(t, dir, "init", "-q")
	testGit(t, dir, "commit", "-q", "--allow-empty", "-m", "nested")
	appendFile(t, filepath.Join(dir, ".git", "config"), "[core]\n\tfsmonitor = \"touch "+mark+"; false\"\n")
}

func TestNestedRepoInProposalIsNeverRunOrCommitted(t *testing.T) {
	s, _ := newTestStore(t)
	startAttached(t, s, "p", sessA)
	if err := s.Attach("p", sessB); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	plantNestedRepo(t, filepath.Join(s.SlugPath("p"), "sub"), filepath.Join(out, "sub"))
	plantNestedRepo(t, filepath.Join(s.SlugPath("p"), "nested.md"), filepath.Join(out, "nested-md"))
	appendFile(t, filepath.Join(s.SlugPath("p"), ".git", "config"), "[core]\n\tfsmonitor = \"touch "+filepath.Join(out, "slug")+"; false\"\n")

	editOnDisk(t, s, "p", "goals.md", "external edit\n")
	if _, _, err := s.Write("p", "technical", "human edit\n", Committer{Author: AuthorHuman}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Changes(sessB); err != nil {
		t.Fatal(err)
	}
	assertAbsent(t, filepath.Join(out, "sub"), filepath.Join(out, "nested-md"), filepath.Join(out, "slug"))
	if got, err := s.git(nil, "cat-file", "blob", "HEAD:p/goals.md"); err != nil || got != "external edit\n" {
		t.Fatalf("p/goals.md at HEAD = %q, %v; want the external edit", got, err)
	}
	tree, err := s.git(nil, "ls-tree", "-r", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(tree, "160000") || strings.Contains(tree, "p/sub") || strings.Contains(tree, "nested.md") {
		t.Fatalf("a nested repo was committed:\n%s", tree)
	}
}

func TestIgnoreFileCannotHideDocs(t *testing.T) {
	s, _ := newTestStore(t)
	startAttached(t, s, "p", sessA)
	if err := os.WriteFile(filepath.Join(s.SlugPath("p"), ".gitignore"), []byte("*.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir(), ".gitignore"), []byte("*\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AddDoc("p", "notes", agent(sessA)); err != nil {
		t.Fatalf("AddDoc: %v", err)
	}
	editOnDisk(t, s, "p", "goals.md", "external edit\n")
	if _, _, err := s.Write("p", "technical", "human edit\n", Committer{Author: AuthorHuman}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"p/notes.md", "p/goals.md"} {
		got, err := s.git(nil, "show", "HEAD:"+f)
		if err != nil {
			t.Fatalf("%s not committed: %v", f, err)
		}
		if f == "p/goals.md" && got != "external edit\n" {
			t.Fatalf("goals.md at HEAD = %q", got)
		}
	}
}

func TestAttributesCannotHidePatches(t *testing.T) {
	s, _ := newTestStore(t)
	startAttached(t, s, "p", sessA)
	if err := s.Attach("p", sessB); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir(), ".gitattributes"), []byte("*.md -diff\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	editOnDisk(t, s, "p", "goals.md", "visible edit\n")
	c, err := s.Changes(sessB)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Edits) == 0 || !strings.Contains(c.Edits[len(c.Edits)-1].Patch, "+visible edit") {
		t.Fatalf("patch hides the edit: %+v", c.Edits)
	}
}

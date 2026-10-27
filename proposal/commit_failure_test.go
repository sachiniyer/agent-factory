package proposal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func docText(t *testing.T, s *Store, slug, file string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(s.SlugPath(slug), file))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestWriteRestoresTheDocWhenCommitFails(t *testing.T) {
	s, _ := newTestStore(t)
	startAttached(t, s, "p", sessA)
	before := docText(t, s, "p", "goals.md")
	unlock := plantRefLock(t, s, "refs/heads/main")
	if _, _, err := s.Write("p", "goals", "written\n", agent(sessA)); err == nil {
		t.Fatal("Write succeeded with the branch ref locked")
	}
	if got := docText(t, s, "p", "goals.md"); got != before {
		t.Fatalf("after a failed Write the doc is %q, want %q", got, before)
	}
	unlock()
	if _, commit, err := s.Write("p", "goals", "written\n", agent(sessA)); err != nil || commit == "" {
		t.Fatalf("retried Write = %q, %v; want an agent commit", commit, err)
	}
}

func TestUnreadableDocIsNotStagedAsDeleted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any file")
	}
	s, _ := newTestStore(t)
	startAttached(t, s, "p", sessA)
	doc := filepath.Join(s.SlugPath("p"), "goals.md")
	if err := os.Chmod(doc, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(doc, 0o644) })
	_, _, _ = s.Write("p", "technical", "edit\n", agent(sessA))
	if _, err := s.git(nil, "cat-file", "-e", "HEAD:p/goals.md"); err != nil {
		t.Fatalf("an unreadable doc was committed as deleted: %v", err)
	}
}

func TestLegacyImportLoadsTheIndex(t *testing.T) {
	s, _ := newTestStore(t)
	oldLayoutStore(t, s)
	if _, err := s.Checkpoint(sessA); err != nil {
		t.Fatal(err)
	}
	log, err := s.git(nil, "log", "--format=%s", "--name-status")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(log, "D\tp/goals.md") {
		t.Fatalf("the first checkpoint after import deleted docs:\n%s", log)
	}
}

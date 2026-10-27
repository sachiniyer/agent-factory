package proposal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func plantRefLock(t *testing.T, s *Store, ref string) func() {
	t.Helper()
	lock := filepath.Join(filepath.Dir(s.Dir()), "proposals.git", filepath.FromSlash(ref)+".lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return func() { _ = os.Remove(lock) }
}

func TestAttachMovesAttachmentAndCursorTogether(t *testing.T) {
	s, _ := newTestStore(t)
	startAttached(t, s, "a", sessA)
	if err := s.Create("b", []string{"goals"}, agent(sessA)); err != nil {
		t.Fatal(err)
	}
	unlock := plantRefLock(t, s, cursorRefPrefix+sessA)
	if err := s.Attach("b", sessA); err == nil {
		t.Fatal("Attach succeeded with the cursor ref locked")
	}
	unlock()
	if slug, err := s.Attached(sessA); err != nil || slug != "a" {
		t.Fatalf("Attached = %q, %v; want the old attachment a", slug, err)
	}
}

func TestAddDocRollsBackWhenCommitFails(t *testing.T) {
	s, _ := newTestStore(t)
	startAttached(t, s, "p", sessA)
	unlock := plantRefLock(t, s, "refs/heads/main")
	if _, err := s.AddDoc("p", "notes", agent(sessA)); err == nil {
		t.Fatal("AddDoc succeeded with the branch ref locked")
	}
	unlock()
	assertAbsent(t, filepath.Join(s.SlugPath("p"), "notes.md"))
	if _, err := s.AddDoc("p", "notes", agent(sessA)); err != nil {
		t.Fatalf("retry AddDoc: %v", err)
	}
	if out, err := s.git(nil, "log", "-1", "--format=%(trailers:key=Af-Author,valueonly)", "--", "p/notes.md"); err != nil || strings.TrimSpace(out) != "agent" {
		t.Fatalf("notes.md first commit author = %q, %v; want agent", out, err)
	}
}

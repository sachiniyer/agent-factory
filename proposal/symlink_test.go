package proposal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSymlinkedDocIsNotRead(t *testing.T) {
	s, _ := newTestStore(t)
	startAttached(t, s, "p", sessA)
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("top secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc := filepath.Join(s.SlugPath("p"), "goals.md")
	if err := os.Remove(doc); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, doc); err != nil {
		t.Fatal(err)
	}
	if text, _, err := s.Read("p", "goals"); err == nil || strings.Contains(text, "secret") {
		t.Fatalf("Read = %q, %v; want an error", text, err)
	}
	docs, err := s.Docs("p")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs {
		if d == "goals.md" {
			t.Fatalf("Docs lists the symlink: %v", docs)
		}
	}
	if _, _, err := s.Write("p", "goals", "x\n", agent(sessA)); err == nil {
		t.Fatal("Write succeeded on a symlinked doc")
	}
	if data, _ := os.ReadFile(secret); string(data) != "top secret\n" {
		t.Fatalf("secret changed to %q", data)
	}
}

func TestSymlinkedSlugIsNotWrittenThrough(t *testing.T) {
	s, _ := newTestStore(t)
	startAttached(t, s, "p", sessA)
	victim := t.TempDir()
	readme := filepath.Join(victim, "readme.md")
	if err := os.WriteFile(readme, []byte("orig\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, s.SlugPath("evil")); err != nil {
		t.Fatal(err)
	}
	if err := s.RequireSlug("evil"); err == nil {
		t.Fatal("RequireSlug accepted a symlinked slug")
	}
	if _, _, err := s.Read("evil", "readme"); err == nil {
		t.Fatal("Read succeeded through a symlinked slug")
	}
	if _, _, err := s.Write("evil", "readme", "pwned\n", agent(sessA)); err == nil {
		t.Fatal("Write succeeded through a symlinked slug")
	}
	if _, _, err := s.AddDoc("evil", "notes", agent(sessA)); err == nil {
		t.Fatal("AddDoc succeeded through a symlinked slug")
	}
	if data, _ := os.ReadFile(readme); string(data) != "orig\n" {
		t.Fatalf("readme.md changed to %q", data)
	}
	assertAbsent(t, filepath.Join(victim, "notes.md"))
}

func TestLockSymlinkIsNotFollowed(t *testing.T) {
	s, _ := newTestStore(t)
	startAttached(t, s, "p", sessA)
	target := filepath.Join(t.TempDir(), "created")
	lock := filepath.Join(s.Dir(), ".lock")
	_ = os.Remove(lock)
	if err := os.Symlink(target, lock); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Write("p", "goals", "x\n", agent(sessA)); err != nil {
		t.Fatal(err)
	}
	assertAbsent(t, target)
}

func TestRemoveDocRefusesASymlinkedDoc(t *testing.T) {
	s, _ := newTestStore(t)
	startAttached(t, s, "p", sessA)
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("top secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc := filepath.Join(s.SlugPath("p"), "goals.md")
	if err := os.Remove(doc); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, doc); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RemoveDoc("p", "goals", agent(sessA)); err == nil {
		t.Fatal("RemoveDoc succeeded on a symlinked doc")
	}
	if data, _ := os.ReadFile(secret); string(data) != "top secret\n" {
		t.Fatalf("secret changed to %q", data)
	}
}

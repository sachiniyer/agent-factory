package proposal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func symlinkDocToSecret(t *testing.T, s *Store, slug, file string) string {
	t.Helper()
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("top secret\n> [!question] leak\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc := filepath.Join(s.SlugPath(slug), file)
	if err := os.Remove(doc); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, doc); err != nil {
		t.Fatal(err)
	}
	return secret
}

func TestReadAtSaveAndDetailRefuseSymlinkedDoc(t *testing.T) {
	s, _ := newTestStore(t)
	startAttached(t, s, "p", sessA)
	_, _, base, _, err := s.ReadAt("p", "technical")
	if err != nil {
		t.Fatal(err)
	}
	secret := symlinkDocToSecret(t, s, "p", "goals.md")
	if text, _, _, _, err := s.ReadAt("p", "goals"); err == nil || strings.Contains(text, "secret") {
		t.Fatalf("ReadAt = %q, %v; want an error", text, err)
	}
	if res, err := s.Save("p", "goals", "mine\n", base); err == nil {
		t.Fatalf("Save = %+v; want an error", res)
	}
	d, err := s.Detail("p")
	if err != nil {
		t.Fatal(err)
	}
	if d.OpenQuestions != 0 {
		t.Fatalf("Detail counted %d questions from the symlink target", d.OpenQuestions)
	}
	if data, _ := os.ReadFile(secret); !strings.HasPrefix(string(data), "top secret") {
		t.Fatalf("secret changed to %q", data)
	}
}

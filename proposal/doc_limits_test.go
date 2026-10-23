package proposal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func snapshotAll(s *Store) error {
	return s.withLock(func() error {
		_, err := s.commitPaths("snapshot external edits", AuthorExternal, "", ".")
		return err
	})
}

func TestUnreadableSlugFolderIsNotStagedAsDeleted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any folder")
	}
	s, _ := newTestStore(t)
	startAttached(t, s, "p", sessA)
	dir := s.SlugPath("p")
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if err := snapshotAll(s); err == nil {
		t.Fatal("a snapshot succeeded although a proposal folder could not be listed")
	}
	if _, err := s.git(nil, "cat-file", "-e", "HEAD:p/goals.md"); err != nil {
		t.Fatalf("docs of an unlistable folder were committed as deleted: %v", err)
	}
}

func TestOversizedDocIsRefused(t *testing.T) {
	s, _ := newTestStore(t)
	startAttached(t, s, "p", sessA)
	if err := os.Truncate(filepath.Join(s.SlugPath("p"), "goals.md"), maxDocSize+1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Read("p", "goals"); err == nil {
		t.Fatal("Read returned a doc larger than the limit")
	}
	if err := snapshotAll(s); err == nil {
		t.Fatal("a snapshot staged a doc larger than the limit")
	}
	big := strings.Repeat("x", maxDocSize+1)
	if _, _, err := s.Write("p", "technical", big, agent(sessA)); err == nil {
		t.Fatal("Write stored a doc larger than the limit")
	} else if !strings.Contains(err.Error(), "MiB") {
		t.Fatalf("Write failed for another reason: %v", err)
	}
}

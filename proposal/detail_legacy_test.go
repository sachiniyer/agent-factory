package proposal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHeadImportsALegacyStore(t *testing.T) {
	s, _ := newTestStore(t)
	oldLayoutStore(t, s)
	head, err := s.Head()
	if err != nil || head == "" {
		t.Fatalf("Head() = %q, %v; want the imported HEAD", head, err)
	}
}

func TestDetailSurvivesAnOversizedDoc(t *testing.T) {
	s, _ := newTestStore(t)
	startAttached(t, s, "p", sessA)
	if err := os.Truncate(filepath.Join(s.SlugPath("p"), "goals.md"), maxDocSize+1); err != nil {
		t.Fatal(err)
	}
	d, err := s.Detail("p")
	if err != nil {
		t.Fatalf("Detail failed on an oversized doc: %v", err)
	}
	if d.Path != s.SlugPath("p") || len(d.Docs) == 0 {
		t.Fatalf("Detail = %+v; want the path and the docs", d)
	}
}

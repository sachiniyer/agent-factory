package proposal

import (
	"os"
	"path/filepath"
	"testing"
)

// AddDoc snapshots another doc's uncommitted disk edit into history (so it is
// kept, never overwritten) and must publish that snapshot like every sibling
// path does; otherwise a Read marks the edited doc as seen and the watcher never
// tells attached Docs tabs about it (#5594).
func TestAddDocPublishesTheSnapshotItTakes(t *testing.T) {
	svc, host, store := newTestService(t)
	if _, err := svc.Start(sessA, "p", []string{"goals"}); err != nil {
		t.Fatal(err)
	}
	in := WatchInput{RepoIDs: []string{testRepoID}}
	svc.Scan(in)
	host.takeEvents()

	editOnDisk(t, store, "p", "goals.md", "edited on disk\n")
	bumpMtime(t, filepath.Join(store.SlugPath("p"), "goals.md"))
	res, err := svc.AddDoc(byRepo(), "p", "technical", AuthorExternal)
	if err != nil {
		t.Fatal(err)
	}

	// The other doc's edit survives, on disk and in a commit before the add.
	got, err := os.ReadFile(filepath.Join(store.SlugPath("p"), "goals.md"))
	if err != nil || string(got) != "edited on disk\n" {
		t.Fatalf("goals.md after AddDoc = %q, %v; the other doc's edit must not be lost", got, err)
	}
	if text, err := store.gitTrim("show", res.Commit+"^:p/goals.md"); err != nil || text != "edited on disk" {
		t.Fatalf("the edit must be committed before the add-doc commit, got %q, %v", text, err)
	}

	var goals, added bool
	for _, e := range host.takeEvents() {
		if e.Commit == nil {
			continue
		}
		switch {
		case e.Doc == "goals.md" && e.Author == AuthorExternal:
			goals = true
		case e.Doc == "technical.md" && *e.Commit == res.Commit:
			added = true
		}
	}
	if !goals {
		t.Fatal("AddDoc snapshotted goals.md but never published it, so attached Docs tabs go stale")
	}
	if !added {
		t.Fatal("AddDoc must still publish the new doc")
	}
	svc.Scan(in)
	if ev := host.takeEvents(); len(ev) != 0 {
		t.Fatalf("the scan after AddDoc must publish nothing more, got %+v", ev)
	}
}

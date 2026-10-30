package proposal

import (
	"path/filepath"
	"testing"
)

// RemoveDoc snapshots another doc's uncommitted disk edit into history and must
// publish that snapshot, as AddDoc and Write do; otherwise the watcher never
// tells attached Docs tabs about it.
func TestRemoveDocPublishesTheSnapshotItTakes(t *testing.T) {
	svc, host, store := newTestService(t)
	if _, err := svc.Start(sessA, "p", []string{"goals", "technical"}); err != nil {
		t.Fatal(err)
	}
	in := WatchInput{RepoIDs: []string{testRepoID}}
	svc.Scan(in)
	host.takeEvents()

	editOnDisk(t, store, "p", "goals.md", "edited on disk\n")
	bumpMtime(t, filepath.Join(store.SlugPath("p"), "goals.md"))
	res, err := svc.RemoveDoc(byRepo(), "p", "technical", AuthorExternal)
	if err != nil {
		t.Fatal(err)
	}

	var goals, removed bool
	for _, e := range host.takeEvents() {
		if e.Commit == nil {
			continue
		}
		switch {
		case e.Doc == "goals.md" && e.Author == AuthorExternal:
			goals = true
		case e.Doc == "technical.md" && *e.Commit == res.Commit:
			removed = true
		}
	}
	if !goals {
		t.Fatal("RemoveDoc snapshotted goals.md but never published it, so attached Docs tabs go stale")
	}
	if !removed {
		t.Fatal("RemoveDoc must still publish the removal")
	}
	svc.Scan(in)
	if ev := host.takeEvents(); len(ev) != 0 {
		t.Fatalf("the scan after RemoveDoc must publish nothing more, got %+v", ev)
	}
}

package proposal

import (
	"path/filepath"
	"testing"
)

func TestNoOpWritePublishesTheSnapshotItTakes(t *testing.T) {
	svc, host, store := newTestService(t)
	if _, err := svc.Start(sessA, "p", []string{"goals"}); err != nil {
		t.Fatal(err)
	}
	in := WatchInput{RepoIDs: []string{testRepoID}}
	svc.Scan(in)
	host.takeEvents()

	editOnDisk(t, store, "p", "goals.md", "same text\n")
	bumpMtime(t, filepath.Join(store.SlugPath("p"), "goals.md"))
	res, err := svc.Write(byRepo(), "p", "goals", "same text\n", AuthorExternal)
	if err != nil || res.Commit != "" {
		t.Fatalf("write = %+v, %v; want a no-op", res, err)
	}
	head, _ := store.Head()
	ev := host.takeEvents()
	if len(ev) != 1 || ev[0].Doc != "goals.md" || ev[0].Commit == nil || *ev[0].Commit != head || ev[0].Author != AuthorExternal {
		t.Fatalf("a no-op write that snapshots a disk edit must publish it, got %+v", ev)
	}
	svc.Scan(in)
	if ev := host.takeEvents(); len(ev) != 0 {
		t.Fatalf("the scan after the write must publish nothing, got %+v", ev)
	}
}

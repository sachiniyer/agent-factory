package proposal

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRejectedWritePublishesTheSnapshotItTakes(t *testing.T) {
	svc, host, store := newTestService(t)
	if _, err := svc.Start(sessA, "p", []string{"goals", "technical"}); err != nil {
		t.Fatal(err)
	}
	svc.Scan(WatchInput{RepoIDs: []string{testRepoID}})
	host.takeEvents()

	editOnDisk(t, store, "p", "goals.md", "edited on disk\n")
	bumpMtime(t, filepath.Join(store.SlugPath("p"), "goals.md"))
	big := strings.Repeat("x", maxDocSize+1)
	if _, err := svc.Write(byRepo(), "p", "technical", big, AuthorExternal); err == nil {
		t.Fatal("an oversized write succeeded")
	}
	for _, e := range host.takeEvents() {
		if e.Doc == "goals.md" && e.Commit != nil {
			return
		}
	}
	t.Fatal("a rejected write that snapshots a disk edit must publish it")
}

func TestRejectedSavePublishesTheSnapshotItTakes(t *testing.T) {
	svc, host, store := newTestService(t)
	if _, err := svc.Start(sessA, "p", []string{"goals"}); err != nil {
		t.Fatal(err)
	}
	lines := "one\ntwo\nthree\nfour\nfive\nsix\n"
	w, err := svc.Write(byRepo(), "p", "goals", lines, AuthorExternal)
	if err != nil {
		t.Fatal(err)
	}
	svc.Scan(WatchInput{RepoIDs: []string{testRepoID}})
	host.takeEvents()

	editOnDisk(t, store, "p", "goals.md", strings.Replace(lines, "one", "ONE", 1))
	bumpMtime(t, filepath.Join(store.SlugPath("p"), "goals.md"))
	big := strings.Replace(lines, "six", strings.Repeat("x", maxDocSize), 1)
	if _, err := svc.Save(byRepo(), "p", "goals", big, w.Commit); err == nil {
		t.Fatal("an oversized save succeeded")
	}
	for _, e := range host.takeEvents() {
		if e.Doc == "goals.md" && e.Commit != nil {
			return
		}
	}
	t.Fatal("a rejected save that snapshots a disk edit must publish it")
}

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

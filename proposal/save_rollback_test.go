package proposal

import (
	"path/filepath"
	"testing"
)

func TestSaveRestoresTheDocWhenCommitFails(t *testing.T) {
	s, _ := newTestStore(t)
	startAttached(t, s, "p", sessA)
	before := docText(t, s, "p", "goals.md")
	_, _, base, _, err := s.ReadAt("p", "goals")
	if err != nil {
		t.Fatal(err)
	}
	unlock := plantRefLock(t, s, "refs/heads/main")
	if _, err := s.Save("p", "goals", "human text\n", base); err == nil {
		t.Fatal("Save succeeded with the branch ref locked")
	}
	if got := docText(t, s, "p", "goals.md"); got != before {
		t.Fatalf("after a failed Save the doc is %q, want %q", got, before)
	}
	unlock()
	res, err := s.Save("p", "goals", "human text\n", base)
	if err != nil || !res.saved {
		t.Fatalf("retried Save = %+v, %v; want a human commit", res, err)
	}
}

func TestChangesPublishesTheSnapshotItTakes(t *testing.T) {
	svc, host, store := newTestService(t)
	if _, err := svc.Start(sessA, "p", []string{"goals"}); err != nil {
		t.Fatal(err)
	}
	in := WatchInput{RepoIDs: []string{testRepoID}}
	svc.Scan(in)
	host.takeEvents()

	editOnDisk(t, store, "p", "goals.md", "disk edit\n")
	bumpMtime(t, filepath.Join(store.SlugPath("p"), "goals.md"))
	if _, err := svc.Changes(sessA); err != nil {
		t.Fatal(err)
	}
	head, _ := store.Head()
	ev := host.takeEvents()
	if len(ev) != 1 || ev[0].Doc != "goals.md" || ev[0].Commit == nil || *ev[0].Commit != head || ev[0].Author != AuthorExternal {
		t.Fatalf("a changes call that snapshots a disk edit must publish it, got %+v", ev)
	}
	svc.Scan(in)
	if ev := host.takeEvents(); len(ev) != 0 {
		t.Fatalf("the scan after changes must publish nothing, got %+v", ev)
	}
}

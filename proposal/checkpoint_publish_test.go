package proposal

import (
	"path/filepath"
	"testing"
)

func TestCheckpointPublishesTheAgentCommit(t *testing.T) {
	svc, host, store := newTestService(t)
	if _, err := svc.Start(sessA, "p", []string{"goals", "api"}); err != nil {
		t.Fatal(err)
	}
	in := WatchInput{RepoIDs: []string{testRepoID}}
	svc.Scan(in)
	host.takeEvents()

	editOnDisk(t, store, "p", "goals.md", "agent edit\n")
	bumpMtime(t, filepath.Join(store.SlugPath("p"), "goals.md"))
	cp, err := svc.Checkpoint(sessA)
	if err != nil {
		t.Fatal(err)
	}
	ev := host.takeEvents()
	if len(ev) != 1 || ev[0].Doc != "goals.md" || ev[0].Commit == nil || *ev[0].Commit != cp.Commit || ev[0].Author != AuthorAgent {
		t.Fatalf("a checkpoint commit must publish its doc, got %+v", ev)
	}
	svc.Scan(in)
	if ev := host.takeEvents(); len(ev) != 0 {
		t.Fatalf("the scan after a checkpoint must publish nothing, got %+v", ev)
	}
}

package proposal

import (
	"path/filepath"
	"testing"
)

func TestStartAttachAndCheckpointPublishTheirSnapshots(t *testing.T) {
	svc, host, store := newTestService(t)
	if _, err := svc.Start(sessA, "p", []string{"goals"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Create("q", []string{"goals"}, Committer{Author: AuthorExternal}); err != nil {
		t.Fatal(err)
	}
	in := WatchInput{RepoIDs: []string{testRepoID}, Tabs: map[string]map[string][]string{testRepoID: {"q": {sessB}}}}
	svc.Scan(in)
	host.takeEvents()

	edit := func(text string) {
		t.Helper()
		editOnDisk(t, store, "q", "goals.md", text)
		bumpMtime(t, filepath.Join(store.SlugPath("q"), "goals.md"))
	}
	expectQ := func(step string) {
		t.Helper()
		for _, e := range host.takeEvents() {
			if e.Slug == "q" && e.Doc == "goals.md" && e.Author == AuthorExternal && e.Commit != nil {
				return
			}
		}
		t.Fatalf("%s must publish the external snapshot of q/goals.md", step)
	}

	edit("before start\n")
	if _, err := svc.Start(sessA, "r", []string{"goals"}); err != nil {
		t.Fatal(err)
	}
	expectQ("start")

	edit("before attach\n")
	if _, err := svc.Attach(sessA, "p"); err != nil {
		t.Fatal(err)
	}
	expectQ("attach")

	edit("before checkpoint\n")
	if _, err := svc.Checkpoint(sessA); err != nil {
		t.Fatal(err)
	}
	expectQ("checkpoint")
}

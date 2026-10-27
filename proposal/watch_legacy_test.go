package proposal

import (
	"path/filepath"
	"testing"
)

func TestScanWatchesALegacyStore(t *testing.T) {
	svc, host, store := newTestService(t)
	oldLayoutStore(t, store)
	in := WatchInput{RepoIDs: []string{testRepoID}, Tabs: map[string]map[string][]string{testRepoID: {"p": {sessA}}}}
	svc.Scan(in)
	host.takeEvents()

	editOnDisk(t, store, "p", "goals.md", "edited after the upgrade\n")
	bumpMtime(t, filepath.Join(store.SlugPath("p"), "goals.md"))
	svc.Scan(in)
	for _, e := range host.takeEvents() {
		if e.Slug == "p" && e.Doc == "goals.md" {
			return
		}
	}
	t.Fatal("Scan published no change for an edit in a store with the old layout")
}

//go:build linux

package git

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

func TestHookProgressCreationPrunesOnlyCompletedOrphans(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	type journal struct{ path, dir string }
	create := func(id string, finished bool, age time.Duration) journal {
		t.Helper()
		tree := filepath.Join(t.TempDir(), id)
		p, err := newHookProgress(hookRun{worktreePath: tree, scopeSessionID: id}, []string{"true"}, "af-hook-"+id, "test")
		if err != nil {
			t.Fatal(err)
		}
		if finished {
			if err := os.Mkdir(p.receipt(0), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(p.receipt(0), "exit"), []byte("0\n"), 0600); err != nil {
				t.Fatal(err)
			}
			p.finish()
		}
		path, _ := hookProgressPath(tree)
		at := time.Now().Add(-age)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		if finished {
			for _, file := range []string{filepath.Join(p.Directory, "finished"), filepath.Join(p.receipt(0), "exit")} {
				if err := os.Chtimes(file, at, at); err != nil {
					t.Fatal(err)
				}
			}
		}
		return journal{path, p.Directory}
	}
	if err := config.SaveRepoInstances("retention-test", json.RawMessage(`[{"id":"owned"}]`)); err != nil {
		t.Fatal(err)
	}
	owned := create("owned", true, 30*24*time.Hour)
	inFlight := create("active", false, 30*24*time.Hour)
	installSurvivorSystemctl(t, "case \"$*\" in *list-units*) echo 'af-hook-scope-test-0.scope loaded active running Hook';; esac\nexit 0\n")
	scopeLive := create("scope", true, 30*24*time.Hour)
	incomplete := create("incomplete", false, 30*24*time.Hour)
	if err := os.WriteFile(filepath.Join(incomplete.dir, "finished"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	recent := create("recent", true, 0)
	expired := create("expired", true, 15*24*time.Hour)
	var kept []journal
	for i := 0; i < 23; i++ {
		kept = append(kept, create(fmt.Sprintf("orphan%d", i), true, time.Duration(23-i)*time.Minute))
	}
	create("trigger", false, 0)
	exists := func(j journal, want bool) {
		t.Helper()
		for _, path := range []string{j.path, j.dir} {
			_, err := os.Stat(path)
			if want && err != nil {
				t.Errorf("protected journal removed: %s", path)
			}
			if !want && !os.IsNotExist(err) {
				t.Errorf("expired/excess orphan retained: %s", path)
			}
		}
	}
	exists(owned, true)
	exists(inFlight, true)
	exists(scopeLive, true)
	exists(incomplete, true)
	exists(recent, true)
	exists(expired, false)
	for i, j := range kept {
		exists(j, i >= 3)
	}
}

package commands

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
)

// TestFactoryReset_PartialResetClearsTheReapedRootCarry pins how a partial
// reset treats the reaped root carry (#4400 review round 7). A corrupt repo
// forces the per-repo delete path, which removes instances.json and — before
// this — left the carry beside it, so the first root the daemon created after
// the reset restored a stale account pin, conversation, and pending swap. The
// carry goes with the records it belongs to; a corrupt repo, whose records the
// reset leaves intact, keeps its carry with them.
func TestFactoryReset_PartialResetClearsTheReapedRootCarry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Chdir(t.TempDir())

	readableID := config.RepoIDFromRoot(t.TempDir())
	if err := config.SaveRepoInstances(readableID, []byte("[]")); err != nil {
		t.Fatal(err)
	}
	readableCarry, err := config.RepoReapedRootCarryPath(readableID)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, readableCarry, `{"workspace":"/repos/readable","account":"work"}`)

	corruptID := config.RepoIDFromRoot(t.TempDir())
	corruptInstances, err := config.RepoInstancesPath(corruptID)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, corruptInstances, "{ not valid json")
	corruptCarry, err := config.RepoReapedRootCarryPath(corruptID)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, corruptCarry, `{"workspace":"/repos/corrupt","account":"work"}`)

	plan, err := planFactoryReset()
	if err != nil {
		t.Fatalf("planFactoryReset: %v", err)
	}
	if len(plan.corruptRepoIDs) != 1 || plan.corruptRepoIDs[0] != corruptID {
		t.Fatalf("corruptRepoIDs = %v, want [%s] — the per-repo path is what this test exercises", plan.corruptRepoIDs, corruptID)
	}
	if _, err := executeFactoryReset(plan); err != nil {
		t.Fatalf("executeFactoryReset: %v", err)
	}

	if _, err := os.Lstat(readableCarry); !os.IsNotExist(err) {
		t.Errorf("a partial reset left the reset repo's root agent carry behind (lstat err = %v)", err)
	}
	if _, err := os.Stat(corruptCarry); err != nil {
		t.Errorf("a corrupt repo's root agent carry must survive with its records: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(corruptCarry)); err != nil {
		t.Errorf("the corrupt repo's instance directory must survive: %v", err)
	}
}

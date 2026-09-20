package commands

import (
	"os"
	"path/filepath"
	"strings"
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

// TestFactoryReset_KeepsRecordsWhenTheCarryCannotBeRemoved pins the atomicity
// Codex asked for: a carry that outlives its record set is precisely what the
// next root create adopts — no record means the create consumes the parked
// carry — so a reset that cannot remove the carry must not delete the records
// either. The unremovable carry here is a non-empty directory at the carry
// path, which fails os.Remove while leaving the repo directory writable, so
// instances.json would otherwise have been deleted.
func TestFactoryReset_KeepsRecordsWhenTheCarryCannotBeRemoved(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Chdir(t.TempDir())

	stuckID := config.RepoIDFromRoot(t.TempDir())
	if err := config.SaveRepoInstances(stuckID, []byte("[]")); err != nil {
		t.Fatal(err)
	}
	stuckInstances, err := config.RepoInstancesPath(stuckID)
	if err != nil {
		t.Fatal(err)
	}
	stuckCarry, err := config.RepoReapedRootCarryPath(stuckID)
	if err != nil {
		t.Fatal(err)
	}
	// A non-empty directory where the carry file belongs: os.Remove refuses it.
	writeFile(t, filepath.Join(stuckCarry, "occupant"), "x")

	// A corrupt repo forces the per-repo path this guard lives on.
	corruptID := config.RepoIDFromRoot(t.TempDir())
	corruptInstances, err := config.RepoInstancesPath(corruptID)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, corruptInstances, "{ not valid json")

	plan, err := planFactoryReset()
	if err != nil {
		t.Fatalf("planFactoryReset: %v", err)
	}
	if _, err := executeFactoryReset(plan); err == nil {
		t.Fatal("a carry that could not be removed must be reported, not swallowed")
	} else if !strings.Contains(err.Error(), stuckID) {
		t.Errorf("error must name the repo whose carry survived: %v", err)
	}

	if _, statErr := os.Stat(stuckInstances); statErr != nil {
		t.Errorf("records were deleted while the carry survived — the next root create would adopt it: %v", statErr)
	}
	if _, statErr := os.Stat(stuckCarry); statErr != nil {
		t.Errorf("fixture: the carry path should still be occupied: %v", statErr)
	}
}

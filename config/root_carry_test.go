package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRepoReapedRootCarryPath pins where the reaped root carry lives — beside
// the repo's instances file, under the same repo ID validation — and that
// deleting it is idempotent (#4400 review round 7).
func TestRepoReapedRootCarryPath(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	const repoID = "deadbeefcafe"

	instances, err := RepoInstancesPath(repoID)
	if err != nil {
		t.Fatal(err)
	}
	carry, err := RepoReapedRootCarryPath(repoID)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(filepath.Dir(instances), ReapedRootCarryFileName); carry != want {
		t.Fatalf("carry path = %q, want %q", carry, want)
	}
	if _, err := RepoReapedRootCarryPath("../escape"); err == nil {
		t.Fatal("an invalid repo ID must be refused, as it is for the instances file")
	}

	if err := DeleteRepoReapedRootCarry(repoID); err != nil {
		t.Fatalf("deleting an absent carry: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(carry), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(carry, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := DeleteRepoReapedRootCarry(repoID); err != nil {
		t.Fatalf("deleting the carry: %v", err)
	}
	if _, err := os.Lstat(carry); !os.IsNotExist(err) {
		t.Fatalf("carry still present after delete: %v", err)
	}
}

// TestRepoIDsWithReapedRootCarry pins the enumeration the daemon reconciles
// against its candidate set: only directories that actually hold a carry, a
// missing instances tree is no carries rather than an error, and an unreadable
// one is an error rather than an empty answer.
func TestRepoIDsWithReapedRootCarry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)

	// No instances directory at all: the first-run case.
	ids, err := RepoIDsWithReapedRootCarry()
	if err != nil {
		t.Fatalf("missing instances dir must not be an error: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("ids = %v, want none", ids)
	}

	write := func(repoID, name string) {
		t.Helper()
		path, err := RepoInstancesPath(repoID)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(filepath.Dir(path), name), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("aaaaaaaaaaaa", ReapedRootCarryFileName)
	write("cccccccccccc", ReapedRootCarryFileName)
	write("bbbbbbbbbbbb", InstancesFileName) // records but no carry

	ids, err = RepoIDsWithReapedRootCarry()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"aaaaaaaaaaaa", "cccccccccccc"}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids = %v, want %v (sorted)", ids, want)
		}
	}
}

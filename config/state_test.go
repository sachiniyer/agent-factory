package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRepoInstancesPath_RejectsTraversal exercises the path-construction
// helper directly. Every malicious id must produce an error before the path
// is returned, so callers can never read or write outside the instances
// directory (#515).
func TestRepoInstancesPath_RejectsTraversal(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tempHome)

	malicious := []string{
		"",
		"..",
		"../../../etc/passwd",
		"foo/../bar",
		"/etc/passwd",
		"foo/bar",
		"foo\\bar",
		".",
		".hidden",
		"foo\x00bar",
	}
	for _, id := range malicious {
		t.Run(id, func(t *testing.T) {
			_, err := repoInstancesPath(id)
			assert.Error(t, err, "expected %q to be rejected", id)
		})
	}
}

// TestRepoInstancesPath_AcceptsLegitimate locks in the success path for the
// real production id shape plus existing test fixtures.
func TestRepoInstancesPath_AcceptsLegitimate(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tempHome)

	legitimate := []string{
		RepoIDFromRoot("/some/path"),
		"test-repo",
		"test-repo-id",
		"abc123def456",
	}
	for _, id := range legitimate {
		t.Run(id, func(t *testing.T) {
			path, err := repoInstancesPath(id)
			require.NoError(t, err)
			expectedParent, perr := instancesDirPath()
			require.NoError(t, perr)
			// The resolved path must live under the instances directory.
			assert.True(t, strings.HasPrefix(path, expectedParent+string(filepath.Separator)),
				"path %q did not stay under %q", path, expectedParent)
			assert.True(t, strings.HasSuffix(path, InstancesFileName))
		})
	}
}

// TestLoadSaveRepoInstances_RejectsTraversal covers the public API surface
// that the daemon RPC ultimately calls. Both Load and Save must surface an
// error and must not touch the filesystem outside the instances directory.
func TestLoadSaveRepoInstances_RejectsTraversal(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tempHome)

	// Plant a sentinel file outside the instances directory. A successful
	// traversal would either expose its bytes or clobber it; both are
	// observable by inspecting the file afterwards.
	sentinelDir := filepath.Join(tempHome, "sentinel")
	require.NoError(t, os.MkdirAll(sentinelDir, 0o755))
	sentinelPath := filepath.Join(sentinelDir, "secret.json")
	originalBytes := []byte(`{"do-not":"clobber"}`)
	require.NoError(t, os.WriteFile(sentinelPath, originalBytes, 0o644))

	malicious := []string{
		"..",
		"../../../etc/passwd",
		"foo/../bar",
		"/etc/passwd",
		"../sentinel",
		"",
	}
	for _, id := range malicious {
		t.Run("load_"+id, func(t *testing.T) {
			_, err := LoadRepoInstances(id)
			assert.Error(t, err, "LoadRepoInstances(%q) should fail", id)
		})
		t.Run("save_"+id, func(t *testing.T) {
			err := SaveRepoInstances(id, json.RawMessage(`[]`))
			assert.Error(t, err, "SaveRepoInstances(%q) should fail", id)
		})
		t.Run("update_"+id, func(t *testing.T) {
			err := UpdateRepoInstances(id, func(raw json.RawMessage) (json.RawMessage, error) {
				return raw, nil
			})
			assert.Error(t, err, "UpdateRepoInstances(%q) should fail", id)
		})
		t.Run("delete_"+id, func(t *testing.T) {
			err := DeleteRepoInstances(id)
			assert.Error(t, err, "DeleteRepoInstances(%q) should fail", id)
		})
	}

	// Sentinel must be untouched.
	after, err := os.ReadFile(sentinelPath)
	require.NoError(t, err)
	assert.Equal(t, originalBytes, after, "sentinel file should not have been modified")
}

// TestLoadRepoInstances_SurfacesReadError verifies that an existing-but-
// unreadable instances.json produces an error rather than a silent empty list.
// This is the load-side guarantee behind #766: callers must be able to tell
// "no sessions" apart from "couldn't read sessions" so read-modify-write paths
// don't clobber present-but-unreadable data. A missing file is a separate case
// (covered elsewhere) and must continue to yield "[]" with no error.
func TestLoadRepoInstances_SurfacesReadError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod-based permission denial is ineffective when running as root")
	}
	tempHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tempHome)

	repoID := RepoIDFromRoot("/path/to/repo")
	require.NoError(t, SaveRepoInstances(repoID, json.RawMessage(`[{"title":"keep-me"}]`)))

	path, err := repoInstancesPath(repoID)
	require.NoError(t, err)

	// Make the file unreadable, simulating a transient permission/I/O error.
	require.NoError(t, os.Chmod(path, 0o000))
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	_, err = LoadRepoInstances(repoID)
	require.Error(t, err, "an unreadable instances.json must surface an error, not an empty list")
	assert.False(t, os.IsNotExist(err), "error must be a read/permission failure, not a missing-file case")
}

// TestSaveRepoInstances_RoundTrip is a smoke test that the legitimate path
// still works end-to-end after the validation change.
func TestSaveRepoInstances_RoundTrip(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tempHome)

	repoID := RepoIDFromRoot("/path/to/repo")
	payload := json.RawMessage(`[{"title":"hello"}]`)

	require.NoError(t, SaveRepoInstances(repoID, payload))
	got, err := LoadRepoInstances(repoID)
	require.NoError(t, err)
	assert.JSONEq(t, string(payload), string(got))
}

// TestSaveStateWritesSchemaVersion pins the EXACT serialized shape of
// state.json, so a new field cannot land in a user's state file unnoticed. It is
// an approval gate, not a formality: adding onboarding_seen failed it, and that
// is the check working — the field is written to every user's state.json, so it
// is approved here deliberately.
//
// onboarding_seen carries no omitempty, matching help_screens_seen: both are
// written even at their zero value, so the file always states the answer rather
// than leaving a reader to infer it from absence.
func TestSaveStateWritesSchemaVersion(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tempHome)

	require.NoError(t, SaveState(&State{HelpScreensSeen: 7}))

	raw, err := os.ReadFile(filepath.Join(tempHome, StateFileName))
	require.NoError(t, err)
	assert.JSONEq(t, `{"schema_version":1,"help_screens_seen":7,"onboarding_seen":false}`, string(raw))

	loaded := LoadState()
	assert.Equal(t, StateSchemaVersion, loaded.SchemaVersion)
	assert.Equal(t, uint32(7), loaded.HelpScreensSeen)
	assert.False(t, loaded.OnboardingSeen)
}

// TestTrySaveStateNeverBlocks is the launch-path guarantee, and it is the whole
// reason TrySaveState exists.
//
// SaveState takes a blocking WithFileLock with no timeout. The onboarding marker
// is written during launch, and this repo has a documented bug class where work
// moved in front of the TUI turns a benign lock into a launch hang — af starting
// while another process holds state.json would simply never draw. Losing the race
// must cost one extra onboarding offer, not the terminal.
//
// The lock is held by a REAL concurrent holder here, not a stub: the point is
// that the syscall does not wait.
func TestTrySaveStateNeverBlocks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	statePath := filepath.Join(home, StateFileName)

	held := make(chan struct{})
	released := make(chan struct{})
	go func() {
		_ = WithFileLock(statePath, func() error {
			close(held)
			<-released // hold it until the assertion below has run
			return nil
		})
	}()
	<-held

	done := make(chan struct{})
	var acquired bool
	var err error
	go func() {
		acquired, err = TrySaveState(&State{OnboardingSeen: true})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		close(released)
		t.Fatal("TrySaveState BLOCKED on a held state lock — on the launch path that is a TUI that never draws")
	}
	close(released)

	if err != nil {
		t.Fatalf("a contended try-lock is not an error, it is a skip: %v", err)
	}
	if acquired {
		t.Fatal("TrySaveState reported it acquired a lock another process was holding")
	}
}

// TestTrySaveStateWritesWhenUncontended pins the normal path: with nothing
// holding the lock, the marker is written and reads back.
func TestTrySaveStateWritesWhenUncontended(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	acquired, err := TrySaveState(&State{OnboardingSeen: true})
	if err != nil {
		t.Fatalf("try save: %v", err)
	}
	if !acquired {
		t.Fatal("an uncontended try-lock must acquire")
	}

	got := LoadState()
	if !got.OnboardingSeen {
		t.Error("the onboarding marker must survive a save/load round trip — otherwise onboarding re-offers forever")
	}
}

// TestOnboardingMarkerDefaultsFalse pins that a fresh home has not seen
// onboarding, and that an existing state.json without the field (written by an
// older af) reads as not-seen rather than failing to parse — the field is
// additive, so no schema bump is involved.
func TestOnboardingMarkerDefaultsFalse(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)

	fresh := LoadState()
	if fresh.OnboardingSeen {
		t.Error("a fresh home must not claim onboarding was already seen")
	}

	// A state.json from a version that predates the field.
	old := []byte(`{"schema_version":` + strconv.Itoa(StateSchemaVersion) + `,"help_screens_seen":3}`)
	if err := os.WriteFile(filepath.Join(home, StateFileName), old, 0644); err != nil {
		t.Fatal(err)
	}
	loaded := LoadState()
	if loaded.OnboardingSeen {
		t.Error("a state.json with no onboarding field must read as not-seen")
	}
	if loaded.HelpScreensSeen != 3 {
		t.Errorf("the additive field must not disturb existing state, got help_screens_seen=%d", loaded.HelpScreensSeen)
	}
}

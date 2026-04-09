package session

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// e2eState holds the state file path so readStateFile can find it.
var e2eStateFile string

// setupE2ERepo creates a real git repo, writes stateful hook scripts, and
// configures remote_hooks in the repo config. Returns the repo path and a
// cleanup function.
func setupE2ERepo(t *testing.T) string {
	t.Helper()

	// Use a custom AGENT_FACTORY_HOME so we don't pollute the real config.
	afHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", afHome)

	// Create a real git repo.
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "config", "--local", "user.email", "test@e2e.com")
	runGit(t, repoDir, "config", "--local", "user.name", "E2E Test")
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("e2e"), 0644))
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "init")

	// State file that the scripts use to track sessions.
	stateFile := filepath.Join(t.TempDir(), "sessions.json")
	require.NoError(t, os.WriteFile(stateFile, []byte("[]"), 0644))
	e2eStateFile = stateFile

	// Create hook scripts that use the state file.
	scriptDir := t.TempDir()

	launchCmd := writeE2EScript(t, scriptDir, "launch.sh", `
STATE_FILE="`+stateFile+`"
NAME=""
PROMPT=""
while [ $# -gt 0 ]; do
  case "$1" in
    --name) NAME="$2"; shift 2;;
    --prompt) PROMPT="$2"; shift 2;;
    *) shift;;
  esac
done

if [ -z "$NAME" ]; then echo "error: --name required" >&2; exit 1; fi

# Add session to state file
ENTRY="{\"name\": \"$NAME\", \"status\": \"running\", \"prompt\": \"$PROMPT\"}"
# Read existing, append, write back (simple jq-free approach)
python3 -c "
import json, sys
with open('$STATE_FILE') as f: sessions = json.load(f)
sessions.append(json.loads('$ENTRY'))
with open('$STATE_FILE', 'w') as f: json.dump(sessions, f)
"
echo "$ENTRY"
`)

	listCmd := writeE2EScript(t, scriptDir, "list.sh", `
STATE_FILE="`+stateFile+`"
cat "$STATE_FILE"
`)

	attachCmd := writeE2EScript(t, scriptDir, "attach.sh", `
NAME="$1"
echo "=== Remote Session: $NAME ==="
echo "Session is running on remote host."
echo "Output from remote agent..."
sleep 0.2
`)

	deleteCmd := writeE2EScript(t, scriptDir, "delete.sh", `
STATE_FILE="`+stateFile+`"
NAME=""
while [ $# -gt 0 ]; do
  case "$1" in
    --name) NAME="$2"; shift 2;;
    *) shift;;
  esac
done

if [ -z "$NAME" ]; then echo "error: --name required" >&2; exit 1; fi

# Remove session from state file
python3 -c "
import json
with open('$STATE_FILE') as f: sessions = json.load(f)
sessions = [s for s in sessions if s.get('name') != '$NAME']
with open('$STATE_FILE', 'w') as f: json.dump(sessions, f)
"
echo "{\"name\": \"$NAME\", \"deleted\": true}"
`)

	// Save the repo config with remote_hooks.
	repo, err := config.RepoFromPath(repoDir)
	require.NoError(t, err)

	cfg := &config.RepoConfig{
		RemoteHooks: &config.RemoteHooks{
			LaunchCmd: launchCmd,
			ListCmd:   listCmd,
			AttachCmd: attachCmd,
			DeleteCmd: deleteCmd,
		},
	}
	require.NoError(t, config.SaveRepoConfig(repo.ID, cfg))

	return repoDir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v failed: %s", args, string(out))
}

func writeE2EScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte("#!/usr/bin/env bash\nset -euo pipefail\n"+body), 0755))
	return path
}

// TestE2ERemoteHooksFullLifecycle exercises the complete lifecycle of a remote
// session against a real git repo with stateful hook scripts:
//
//	NewInstance → Start → Preview → IsAlive → Serialize → Kill → verify cleanup
func TestE2ERemoteHooksFullLifecycle(t *testing.T) {
	// Check python3 is available (scripts use it for JSON state manipulation)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available, skipping e2e test")
	}

	repoDir := setupE2ERepo(t)

	// --- Step 1: NewInstance with ForceRemote should pick the remote backend ---
	instance, err := NewInstance(InstanceOptions{
		Title:       "e2e-fix-auth",
		Path:        repoDir,
		Program:     "claude",
		ForceRemote: true,
	})
	require.NoError(t, err)
	assert.True(t, instance.IsRemote(), "NewInstance with ForceRemote should pick HookBackend")
	assert.Equal(t, "remote", instance.GetBackend().Type())
	assert.False(t, instance.Started())

	// --- Step 2: Start (first-time) should call launch_cmd ---
	err = instance.Start(true)
	require.NoError(t, err)
	assert.True(t, instance.Started())
	expectedSlug := slugify("e2e-fix-auth")
	assert.Equal(t, expectedSlug, instance.Branch, "Branch should be set from launch response")

	// Verify the remote state file was updated
	stateData := readStateFile(t, repoDir)
	require.Len(t, stateData, 1, "state file should have 1 session after launch")
	assert.Equal(t, expectedSlug, stateData[0]["name"])
	assert.Equal(t, "running", stateData[0]["status"])

	// --- Step 3: Preview should capture output from attach_cmd PTY ---
	time.Sleep(500 * time.Millisecond) // Let PTY reader capture output
	preview, err := instance.Preview()
	require.NoError(t, err)
	assert.Contains(t, preview, "Remote Session: "+expectedSlug, "Preview should contain output from attach.sh")

	// --- Step 4: PreviewFullHistory should also work ---
	fullHistory, err := instance.PreviewFullHistory()
	require.NoError(t, err)
	assert.Contains(t, fullHistory, "Remote Session: "+expectedSlug)

	// --- Step 5: IsAlive should return true (session is in state file with status=running) ---
	assert.True(t, instance.TmuxAlive(), "TmuxAlive (delegates to IsAlive) should return true")

	// --- Step 6: HasUpdated should always return (false, false) for remote ---
	updated, hasPrompt := instance.HasUpdated()
	assert.False(t, updated)
	assert.False(t, hasPrompt)

	// --- Step 7: SendPrompt/SendPromptCommand should return errors ---
	assert.Error(t, instance.SendPrompt("hello"))
	assert.Error(t, instance.SendPromptCommand("hello"))

	// --- Step 8: SetPreviewSize should be a no-op (no error) ---
	assert.NoError(t, instance.SetPreviewSize(120, 40))

	// --- Step 9: Remote-specific checks ---
	assert.Equal(t, "", instance.GetWorktreePath(), "remote instances have no worktree")
	_, err = instance.RepoName()
	assert.Error(t, err, "RepoName should error for remote instances")

	// --- Step 10: ToInstanceData should serialize correctly ---
	data := instance.ToInstanceData()
	assert.Equal(t, "remote", data.BackendType)
	assert.Equal(t, "e2e-fix-auth", data.Title)
	assert.Equal(t, expectedSlug, data.Branch)
	assert.NotNil(t, data.RemoteMeta)
	assert.Equal(t, "running", data.RemoteMeta["status"])

	// Verify JSON round-trip
	jsonBytes, err := json.Marshal(data)
	require.NoError(t, err)
	var restored InstanceData
	require.NoError(t, json.Unmarshal(jsonBytes, &restored))
	assert.Equal(t, "remote", restored.BackendType)
	assert.Equal(t, expectedSlug, restored.RemoteMeta["name"])

	// --- Step 11: Kill should call delete_cmd and clean up ---
	err = instance.Kill()
	require.NoError(t, err)
	assert.False(t, instance.Started())

	// Verify the state file was updated
	stateData = readStateFile(t, repoDir)
	assert.Len(t, stateData, 0, "state file should be empty after delete")
}

// TestE2ERemoteHooksMultipleSessions tests managing multiple remote sessions.
func TestE2ERemoteHooksMultipleSessions(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available, skipping e2e test")
	}

	repoDir := setupE2ERepo(t)

	// Create and start two sessions.
	inst1, err := NewInstance(InstanceOptions{Title: "session-alpha", Path: repoDir, Program: "claude", ForceRemote: true})
	require.NoError(t, err)
	require.True(t, inst1.IsRemote())

	inst2, err := NewInstance(InstanceOptions{Title: "session-beta", Path: repoDir, Program: "claude", ForceRemote: true})
	require.NoError(t, err)
	require.True(t, inst2.IsRemote())

	require.NoError(t, inst1.Start(true))
	require.NoError(t, inst2.Start(true))

	// Both should be in the state file.
	stateData := readStateFile(t, repoDir)
	require.Len(t, stateData, 2)

	// Both should report as alive.
	assert.True(t, inst1.TmuxAlive())
	assert.True(t, inst2.TmuxAlive())

	// Kill session-alpha.
	require.NoError(t, inst1.Kill())

	// State file should have only session-beta.
	stateData = readStateFile(t, repoDir)
	require.Len(t, stateData, 1)
	assert.Equal(t, slugify("session-beta"), stateData[0]["name"])

	// session-alpha should no longer be alive, session-beta should be.
	assert.False(t, inst1.TmuxAlive())
	assert.True(t, inst2.TmuxAlive())

	// Clean up session-beta.
	require.NoError(t, inst2.Kill())
	stateData = readStateFile(t, repoDir)
	assert.Len(t, stateData, 0)
}

// TestE2ELocalBackendStillWorks verifies that repos without remote_hooks
// still default to LocalBackend.
func TestE2ELocalBackendStillWorks(t *testing.T) {
	afHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", afHome)

	// Create a git repo with NO remote_hooks config.
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "config", "--local", "user.email", "test@local.com")
	runGit(t, repoDir, "config", "--local", "user.name", "Local Test")
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "test.txt"), []byte("hi"), 0644))
	runGit(t, repoDir, "add", "test.txt")
	runGit(t, repoDir, "commit", "-m", "init")

	instance, err := NewInstance(InstanceOptions{
		Title:   "local-test",
		Path:    repoDir,
		Program: "bash",
	})
	require.NoError(t, err)
	assert.False(t, instance.IsRemote(), "should default to LocalBackend")
	assert.Equal(t, "local", instance.GetBackend().Type())
}

// TestE2EBackendResolutionWithConfig verifies that backendForPath reads
// the repo config and returns the correct backend type.
func TestE2EBackendResolutionWithConfig(t *testing.T) {
	afHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", afHome)

	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "config", "--local", "user.email", "test@res.com")
	runGit(t, repoDir, "config", "--local", "user.name", "Res Test")
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "f.txt"), []byte("x"), 0644))
	runGit(t, repoDir, "add", "f.txt")
	runGit(t, repoDir, "commit", "-m", "init")

	// Without config → local
	b, err := backendForPath(repoDir)
	require.NoError(t, err)
	assert.Equal(t, "local", b.Type())

	// Add remote_hooks config
	repo, err := config.RepoFromPath(repoDir)
	require.NoError(t, err)
	cfg := &config.RepoConfig{
		RemoteHooks: &config.RemoteHooks{
			LaunchCmd: "/bin/echo",
			ListCmd:   "/bin/echo",
			AttachCmd: "/bin/echo",
			DeleteCmd: "/bin/echo",
		},
	}
	require.NoError(t, config.SaveRepoConfig(repo.ID, cfg))

	// With config → remote
	b, err = backendForPath(repoDir)
	require.NoError(t, err)
	assert.Equal(t, "remote", b.Type())
}

// readStateFile reads and parses the sessions state file that the e2e hook
// scripts maintain.
func readStateFile(t *testing.T, _ string) []map[string]interface{} {
	t.Helper()

	raw, err := os.ReadFile(e2eStateFile)
	require.NoError(t, err)

	var sessions []map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &sessions))
	return sessions
}

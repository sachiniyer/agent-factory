package session

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
		AutoYes:     true,
		ForceRemote: true,
	})
	require.NoError(t, err)
	assert.True(t, instance.Capabilities().Workspace == WorkspaceRemote, "NewInstance with ForceRemote should pick HookBackend")
	assert.Equal(t, "remote", instance.GetBackend().Type())
	assert.False(t, instance.Started())
	assert.True(t, instance.AutoYes, "NewInstance should preserve AutoYes from options")

	// --- Step 2: Start (first-time) should call launch_cmd ---
	err = instance.Start(true)
	require.NoError(t, err)
	assert.True(t, instance.Started())
	expectedSlug := Slugify("e2e-fix-auth")
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

	// --- Step 6: Snapshot should always report (false, false) for remote ---
	obs, err := instance.AgentServer().Snapshot()
	assert.NoError(t, err)
	assert.False(t, obs.Updated)
	assert.False(t, obs.HasPrompt)

	// --- Step 7: prompt delivery should return errors ---
	// The active delivery seam is AgentServer.SendPrompt (which routes to the
	// reliable SendPromptCommand path); the remote runtime rejects it. The raw
	// PTY-write SendPrompt path was deleted as dead post-migration (#1626).
	assert.Error(t, instance.AgentServer().SendPrompt("hello"))
	assert.Error(t, instance.SendPromptCommand("hello"))

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
	assert.True(t, data.AutoYes, "ToInstanceData should persist AutoYes")

	// Verify JSON round-trip
	jsonBytes, err := json.Marshal(data)
	require.NoError(t, err)
	var restored InstanceData
	require.NoError(t, json.Unmarshal(jsonBytes, &restored))
	assert.Equal(t, "remote", restored.BackendType)
	assert.Equal(t, expectedSlug, restored.RemoteMeta["name"])
	assert.True(t, restored.AutoYes, "InstanceData JSON round-trip should preserve AutoYes")

	// Regression for #261: FromInstanceData must copy AutoYes back onto the
	// restored Instance. Before the fix, this field was silently dropped,
	// so sessions persisted with AutoYes=true would come back with
	// AutoYes=false after restart.
	rebuilt, err := FromInstanceData(restored)
	require.NoError(t, err)
	assert.True(t, rebuilt.AutoYes, "FromInstanceData must restore AutoYes from persisted data")
	// Close the rebuilt instance's preview PTY so it does not leak past the
	// test. The shared state file is cleaned up by Step 11's Kill() on the
	// original instance.
	if hb, ok := rebuilt.GetBackend().(*HookBackend); ok {
		hb.closePTY(rebuilt.Title)
	}

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
	require.True(t, inst1.Capabilities().Workspace == WorkspaceRemote)

	inst2, err := NewInstance(InstanceOptions{Title: "session-beta", Path: repoDir, Program: "claude", ForceRemote: true})
	require.NoError(t, err)
	require.True(t, inst2.Capabilities().Workspace == WorkspaceRemote)

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
	assert.Equal(t, Slugify("session-beta"), stateData[0]["name"])

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
	assert.False(t, instance.Capabilities().Workspace == WorkspaceRemote, "should default to LocalBackend")
	assert.Equal(t, "local", instance.GetBackend().Type())
}

// TestE2EBackendResolutionRejectsEmptyHookCommands is the resolution-layer
// regression test for #738: a remote_hooks config with an empty required
// command string must fail fast at backend resolution with an actionable error
// naming the offending field, rather than constructing a HookBackend that
// later dies with exec's cryptic "exec: no command" at operation time.
func TestE2EBackendResolutionRejectsEmptyHookCommands(t *testing.T) {
	afHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", afHome)

	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "config", "--local", "user.email", "test@empty.com")
	runGit(t, repoDir, "config", "--local", "user.name", "Empty Test")
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "f.txt"), []byte("x"), 0644))
	runGit(t, repoDir, "add", "f.txt")
	runGit(t, repoDir, "commit", "-m", "init")

	repo, err := config.RepoFromPath(repoDir)
	require.NoError(t, err)
	// list_cmd is intentionally left empty here too, to confirm it remains
	// optional: only launch_cmd should trip the guard.
	cfg := &config.RepoConfig{
		RemoteHooks: &config.RemoteHooks{
			LaunchCmd: "",
			AttachCmd: "/bin/echo",
			DeleteCmd: "/bin/echo",
		},
	}
	require.NoError(t, config.SaveRepoConfig(repo.ID, cfg))

	// loadHookBackendForPath must reject an empty launch_cmd.
	_, err = loadHookBackendForPath(repoDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "launch_cmd")
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

// setupE2ERelativeHooksRepo creates a real git repo whose in-repo config
// declares remote_hooks as repo-relative paths (#834), with the hook scripts
// checked into the repo under .agent-factory/hooks/. The scripts track
// sessions in a plain-text state file (one name per line) so the test needs
// no python3. Returns the repo dir and the state file path.
func setupE2ERelativeHooksRepo(t *testing.T) (string, string) {
	t.Helper()

	afHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", afHome)

	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "config", "--local", "user.email", "test@rel.com")
	runGit(t, repoDir, "config", "--local", "user.name", "Rel Test")

	stateFile := filepath.Join(t.TempDir(), "sessions.txt")
	require.NoError(t, os.WriteFile(stateFile, nil, 0644))

	hooksDir := filepath.Join(repoDir, config.InRepoConfigDirName, "hooks")
	require.NoError(t, os.MkdirAll(hooksDir, 0755))

	writeE2EScript(t, hooksDir, "launch.sh", `
STATE_FILE="`+stateFile+`"
NAME=""
while [ $# -gt 0 ]; do
  case "$1" in
    --name) NAME="$2"; shift 2;;
    *) shift;;
  esac
done
if [ -z "$NAME" ]; then echo "error: --name required" >&2; exit 1; fi
echo "$NAME" >> "$STATE_FILE"
echo "{\"name\": \"$NAME\", \"status\": \"running\"}"
`)

	writeE2EScript(t, hooksDir, "list.sh", `
STATE_FILE="`+stateFile+`"
OUT="["
SEP=""
while IFS= read -r n; do
  [ -z "$n" ] && continue
  OUT="$OUT$SEP{\"name\": \"$n\", \"status\": \"running\"}"
  SEP=","
done < "$STATE_FILE"
echo "$OUT]"
`)

	writeE2EScript(t, hooksDir, "attach.sh", `
NAME="$1"
echo "=== Remote Session: $NAME ==="
sleep 0.2
`)

	writeE2EScript(t, hooksDir, "terminal.sh", `
NAME="$1"
echo "=== Remote Shell: $NAME ==="
`)

	writeE2EScript(t, hooksDir, "delete.sh", `
STATE_FILE="`+stateFile+`"
NAME=""
while [ $# -gt 0 ]; do
  case "$1" in
    --name) NAME="$2"; shift 2;;
    *) shift;;
  esac
done
if [ -z "$NAME" ]; then echo "error: --name required" >&2; exit 1; fi
grep -v -x -F "$NAME" "$STATE_FILE" > "$STATE_FILE.tmp" || true
mv "$STATE_FILE.tmp" "$STATE_FILE"
echo "{\"name\": \"$NAME\", \"deleted\": true}"
`)

	// The whole point: the config carries repo-relative commands.
	require.NoError(t, os.WriteFile(config.InRepoConfigPath(repoDir), []byte(`{
  "remote_hooks": {
    "launch_cmd": "./.agent-factory/hooks/launch.sh",
    "list_cmd": "./.agent-factory/hooks/list.sh",
    "attach_cmd": "./.agent-factory/hooks/attach.sh",
    "delete_cmd": "./.agent-factory/hooks/delete.sh",
    "terminal_cmd": "./.agent-factory/hooks/terminal.sh"
  }
}`), 0644))

	runGit(t, repoDir, "add", "-A")
	runGit(t, repoDir, "commit", "-m", "init with relative remote_hooks")

	return repoDir, stateFile
}

// readLineStateFile reads the one-name-per-line session state file kept by
// the relative-hooks scripts.
func readLineStateFile(t *testing.T, stateFile string) []string {
	t.Helper()
	raw, err := os.ReadFile(stateFile)
	require.NoError(t, err)
	var names []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line != "" {
			names = append(names, line)
		}
	}
	return names
}

// TestE2ERemoteHooksRelativePaths is the acceptance test for #834: an in-repo
// remote_hooks config written with repo-relative paths must work even though
// the process cwd is not the repo root (the daemon's cwd is unrelated to the
// repo). Before the fix, exec resolved "./..." against the cwd and every hook
// failed with "no such file or directory".
func TestE2ERemoteHooksRelativePaths(t *testing.T) {
	repoDir, stateFile := setupE2ERelativeHooksRepo(t)

	repo, err := config.RepoFromPath(repoDir)
	require.NoError(t, err)
	cwd, err := os.Getwd()
	require.NoError(t, err)
	require.NotEqual(t, repo.Root, cwd, "test must run with cwd outside the repo for the relative paths to be meaningful")

	// Backend resolution rewrites the commands to absolute paths under the
	// repo root before they reach any exec site.
	hook, err := loadHookBackendForPath(repoDir)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(repo.Root, ".agent-factory/hooks/launch.sh"), hook.Hooks.LaunchCmd)
	assert.Equal(t, filepath.Join(repo.Root, ".agent-factory/hooks/list.sh"), hook.Hooks.ListCmd)
	assert.Equal(t, filepath.Join(repo.Root, ".agent-factory/hooks/attach.sh"), hook.Hooks.AttachCmd)
	assert.Equal(t, filepath.Join(repo.Root, ".agent-factory/hooks/delete.sh"), hook.Hooks.DeleteCmd)
	assert.Equal(t, filepath.Join(repo.Root, ".agent-factory/hooks/terminal.sh"), hook.Hooks.TerminalCmd)

	// Full lifecycle: launch_cmd, list_cmd (liveness), attach_cmd (preview),
	// and delete_cmd all execute from a cwd outside the repo.
	instance, err := NewInstance(InstanceOptions{
		Title:       "rel-hooks",
		Path:        repoDir,
		Program:     "claude",
		ForceRemote: true,
	})
	require.NoError(t, err)
	require.True(t, instance.Capabilities().Workspace == WorkspaceRemote)

	require.NoError(t, instance.Start(true))
	assert.Equal(t, []string{Slugify("rel-hooks")}, readLineStateFile(t, stateFile), "launch_cmd must have run")
	assert.True(t, instance.TmuxAlive(), "list_cmd must report the session as running")

	// Startup import goes through the same resolved config.
	imported, err := ListRemoteHookInstanceData(repo.Root, hook.Hooks, time.Now())
	require.NoError(t, err)
	require.Len(t, imported, 1)
	assert.Equal(t, Slugify("rel-hooks"), imported[0].Branch)

	require.NoError(t, instance.Kill())
	assert.Empty(t, readLineStateFile(t, stateFile), "delete_cmd must have run")
}

// TestE2ERemoteHooksRelativePathsLinkedWorktree pins the linked-worktree rule
// from #834: relative hook paths resolve against the main repository root —
// the root whose .agent-factory/config.json was loaded — never against the
// linked worktree's own path.
func TestE2ERemoteHooksRelativePathsLinkedWorktree(t *testing.T) {
	repoDir, stateFile := setupE2ERelativeHooksRepo(t)

	worktreeDir := filepath.Join(t.TempDir(), "linked-wt")
	runGit(t, repoDir, "worktree", "add", worktreeDir, "-b", "wt-branch")

	repo, err := config.RepoFromPath(worktreeDir)
	require.NoError(t, err)
	mainRepo, err := config.RepoFromPath(repoDir)
	require.NoError(t, err)
	require.Equal(t, mainRepo.Root, repo.Root, "linked worktree must resolve to the main repo root")

	hook, err := loadHookBackendForPath(worktreeDir)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(mainRepo.Root, ".agent-factory/hooks/launch.sh"), hook.Hooks.LaunchCmd,
		"hooks must resolve against the main repo root, not the worktree")
	assert.NotContains(t, hook.Hooks.LaunchCmd, worktreeDir)

	// And they execute: launch + delete through an instance rooted at the
	// linked worktree.
	instance, err := NewInstance(InstanceOptions{
		Title:       "wt-session",
		Path:        worktreeDir,
		Program:     "claude",
		ForceRemote: true,
	})
	require.NoError(t, err)
	require.True(t, instance.Capabilities().Workspace == WorkspaceRemote)

	require.NoError(t, instance.Start(true))
	assert.Equal(t, []string{Slugify("wt-session")}, readLineStateFile(t, stateFile))
	require.NoError(t, instance.Kill())
	assert.Empty(t, readLineStateFile(t, stateFile))
}

// TestE2EBackendResolutionWithInRepoConfig verifies that loadHookBackendForPath
// reads the in-repo .agent-factory/config.json (#800) and that it shadows the
// legacy per-repo location.
func TestE2EBackendResolutionWithInRepoConfig(t *testing.T) {
	afHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", afHome)

	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "config", "--local", "user.email", "test@inrepo.com")
	runGit(t, repoDir, "config", "--local", "user.name", "InRepo Test")
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "f.txt"), []byte("x"), 0644))
	runGit(t, repoDir, "add", "f.txt")
	runGit(t, repoDir, "commit", "-m", "init")

	// In-repo remote_hooks alone resolve the remote backend.
	require.NoError(t, os.MkdirAll(filepath.Join(repoDir, config.InRepoConfigDirName), 0755))
	require.NoError(t, os.WriteFile(config.InRepoConfigPath(repoDir),
		[]byte(`{"remote_hooks": {"launch_cmd": "/bin/echo in-repo", "list_cmd": "/bin/echo", "attach_cmd": "/bin/echo", "delete_cmd": "/bin/echo"}}`), 0644))

	// A conflicting legacy config is shadowed by the in-repo file.
	repo, err := config.RepoFromPath(repoDir)
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoConfig(repo.ID, &config.RepoConfig{
		RemoteHooks: &config.RemoteHooks{
			LaunchCmd: "/bin/echo legacy",
			ListCmd:   "/bin/echo",
			AttachCmd: "/bin/echo",
			DeleteCmd: "/bin/echo",
		},
	}))

	hook, err := loadHookBackendForPath(repoDir)
	require.NoError(t, err)
	assert.Equal(t, "/bin/echo in-repo", hook.Hooks.LaunchCmd)
}

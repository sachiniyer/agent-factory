package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// operatorHome lays out the operator's real agent homes on the daemon box.
func operatorHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	write := func(rel, body string) {
		p := filepath.Join(home, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	}
	write(".claude/settings.json", `{"theme":"dark"}`)
	write(".claude/CLAUDE.md", "be terse")
	write(".claude/.credentials.json", "OPERATORS-OWN-LOGIN") // must never travel
	write(".claude.json", `{"mcpServers":{"x":{}}}`)
	write(".codex/config.toml", "model = \"o3\"\ncli_auth_credentials_store = \"keyring\"\n[profiles.a]\nx = 1\n")
	write(".codex/auth.json", "OPERATORS-OWN-CODEX-LOGIN")
}

func configWorkspace(t *testing.T, agent, host string, entries ...string) *sandboxWorkspace {
	t.Helper()
	w := workspaceOn(t, writeAccount(t, agent, "session-account-secret"), host)
	w.spec.AgentConfigFiles = entries
	return w
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}

// Only the files the operator declared are placed — and only in the session's own
// account scope. Nothing from the operator's home that was not declared moves,
// and in particular never their own login.
func TestAgentConfigPlacesOnlyTheDeclaredFilesInTheSessionScope(t *testing.T) {
	operatorHome(t)
	w := configWorkspace(t, "claude", "box1", "claude:settings.json", "claude:.claude.json")
	require.NoError(t, w.syncAgentConfigIn(time.Minute))

	scope := w.AccountDir()
	assert.Equal(t, `{"theme":"dark"}`, read(t, filepath.Join(scope, "settings.json")))
	assert.Equal(t, `{"mcpServers":{"x":{}}}`, read(t, filepath.Join(scope, ".claude.json")), "the MCP config, from ~/.claude.json")
	assert.NoFileExists(t, filepath.Join(scope, "CLAUDE.md"), "an undeclared file must not travel")
	assert.NoFileExists(t, filepath.Join(scope, ".credentials.json"), "the operator's own login must never be carried as 'config'")
	entries, _ := os.ReadDir(scope)
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(scope, e.Name()))
		assert.NotContains(t, string(b), "OPERATORS-OWN-LOGIN")
	}
}

// The licence is the one that gates credentials: no listed host, nothing sent;
// no account scope to point the agent at, nothing sent.
func TestAgentConfigNeedsAListedHostAndAnAccountScope(t *testing.T) {
	operatorHome(t)
	sh := &recordingShell{}

	unlisted := configWorkspace(t, "claude", "", "claude:settings.json")
	unlisted.shell = sh
	require.NoError(t, unlisted.syncAgentConfigIn(time.Second))
	assert.Empty(t, sh.scripts, "an unlisted target receives no config")
	assert.Empty(t, sh.stdin)

	noAccount := &sandboxWorkspace{shell: sh, SessionDir: "/s",
		spec: ProvisionSpec{AccountSyncHost: "box1", AgentConfigFiles: []string{"claude:settings.json"}}}
	require.NoError(t, noAccount.syncAgentConfigIn(time.Second))
	assert.Empty(t, sh.scripts, "without an account scope there is nowhere to put it")
}

func TestAgentConfigIgnoresEntriesForAnotherAgent(t *testing.T) {
	operatorHome(t)
	sh := &recordingShell{}
	w := configWorkspace(t, "claude", "box1", "codex:config.toml")
	w.shell = sh
	require.NoError(t, w.syncAgentConfigIn(time.Second))
	assert.Empty(t, sh.scripts)
}

// A missing declared file must not fail the create.
func TestAgentConfigSkipsAMissingFile(t *testing.T) {
	operatorHome(t)
	require.NoError(t, os.Remove(filepath.Join(os.Getenv("HOME"), ".claude", "CLAUDE.md")))
	w := configWorkspace(t, "claude", "box1", "claude:CLAUDE.md", "claude:settings.json")
	require.NoError(t, w.syncAgentConfigIn(time.Minute))
	assert.NoFileExists(t, filepath.Join(w.AccountDir(), "CLAUDE.md"))
	assert.FileExists(t, filepath.Join(w.AccountDir(), "settings.json"))
}

// A copied codex config.toml must not override the session's account: the
// keyring credential store would make codex use the machine-wide identity.
func TestAgentConfigForcesTheCodexFileCredentialStore(t *testing.T) {
	operatorHome(t)
	w := configWorkspace(t, "codex", "box1", "codex:config.toml")
	require.NoError(t, w.syncAgentConfigIn(time.Minute))
	got := read(t, filepath.Join(w.AccountDir(), "config.toml"))
	assert.NotContains(t, got, "keyring")
	assert.Contains(t, got, `cli_auth_credentials_store = "file"`)
	assert.Contains(t, got, `model = "o3"`, "the rest of the operator's config is kept")
	assert.Less(t, indexOf(got, "cli_auth_credentials_store"), indexOf(got, "[profiles.a]"),
		"the bare key must sit above every table to stay top-level")
	assert.NotContains(t, got, "OPERATORS-OWN-CODEX-LOGIN")
	assert.NoFileExists(t, filepath.Join(w.AccountDir(), "auth.json"))
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// Session A's placement never lands in B's scope.
func TestAgentConfigNeverLandsInAnotherSessionsScope(t *testing.T) {
	operatorHome(t)
	a := configWorkspace(t, "claude", "box1", "claude:settings.json")
	b := configWorkspace(t, "claude", "box1", "claude:settings.json")
	require.NoError(t, a.syncAgentConfigIn(time.Minute))
	assert.FileExists(t, filepath.Join(a.AccountDir(), "settings.json"))
	assert.NoDirExists(t, b.AccountDir())
}

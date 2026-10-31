package session

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	toml "github.com/pelletier/go-toml/v2"
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
	_, err := w.syncAgentConfigIn(time.Minute)
	require.NoError(t, err)

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
	_, err := unlisted.syncAgentConfigIn(time.Second)
	require.NoError(t, err)
	assert.Empty(t, sh.scripts, "an unlisted target receives no config")
	assert.Empty(t, sh.stdin)

	noAccount := &sandboxWorkspace{shell: sh, SessionDir: "/s",
		spec: ProvisionSpec{AccountSyncHost: "box1", AgentConfigFiles: []string{"claude:settings.json"}}}
	_, err = noAccount.syncAgentConfigIn(time.Second)
	require.NoError(t, err)
	assert.Empty(t, sh.scripts, "without an account scope there is nowhere to put it")
}

func TestAgentConfigIgnoresEntriesForAnotherAgent(t *testing.T) {
	operatorHome(t)
	sh := &recordingShell{}
	w := configWorkspace(t, "claude", "box1", "codex:config.toml")
	w.shell = sh
	_, err := w.syncAgentConfigIn(time.Second)
	require.NoError(t, err)
	assert.Empty(t, sh.scripts)
}

// A missing declared file must not fail the create.
func TestAgentConfigSkipsAMissingFile(t *testing.T) {
	operatorHome(t)
	require.NoError(t, os.Remove(filepath.Join(os.Getenv("HOME"), ".claude", "CLAUDE.md")))
	w := configWorkspace(t, "claude", "box1", "claude:CLAUDE.md", "claude:settings.json")
	_, err := w.syncAgentConfigIn(time.Minute)
	require.NoError(t, err)
	assert.NoFileExists(t, filepath.Join(w.AccountDir(), "CLAUDE.md"))
	assert.FileExists(t, filepath.Join(w.AccountDir(), "settings.json"))
}

// A .claude.json over the old 1 MiB credential bound but under the larger
// agent-config bound is now PLACED: the feature's headline file (the MCP server
// config) no longer silently no-ops for an operator whose per-project history
// grew it past a megabyte (#5571 W7.7).
func TestAgentConfigPlacesAClaudeJsonOverTheOldCredentialBound(t *testing.T) {
	operatorHome(t)
	big := bytes.Repeat([]byte(" "), 2<<20) // 2 MiB: over the 1 MiB credential cap, under the 64 MiB agent-config cap
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("HOME"), ".claude.json"), big, 0o600))
	w := configWorkspace(t, "claude", "box1", "claude:.claude.json")
	_, err := w.syncAgentConfigIn(time.Minute)
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(w.AccountDir(), ".claude.json"))
	got, err := os.ReadFile(filepath.Join(w.AccountDir(), ".claude.json"))
	require.NoError(t, err)
	assert.Len(t, got, len(big), "the whole file travels, byte for byte")
}

// The bound is the archive's total, not only each file's: the whole selection is
// staged into one retained buffer, so two files each under the per-file cap but
// over it together place the first and skip the second — the closed claude set
// near the cap cannot hold ~192 MiB in the daemon's memory at once.
func TestAgentConfigSkipsAFileThatWouldOverflowTheArchiveBound(t *testing.T) {
	operatorHome(t)
	big := bytes.Repeat([]byte(" "), agentConfigMaxFileBytes/2+1) // each under the cap; two together exceed it
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("HOME"), ".claude", "settings.json"), big, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("HOME"), ".claude.json"), big, 0o600))
	w := configWorkspace(t, "claude", "box1", "claude:settings.json", "claude:.claude.json")
	_, err := w.syncAgentConfigIn(time.Minute)
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(w.AccountDir(), "settings.json"))
	assert.NoFileExists(t, filepath.Join(w.AccountDir(), ".claude.json"), "over the aggregate bound it is skipped, not placed")
}

// A copied codex config.toml must not override the session's account: the
// keyring credential store would make codex use the machine-wide identity.
func TestAgentConfigForcesTheCodexFileCredentialStore(t *testing.T) {
	operatorHome(t)
	w := configWorkspace(t, "codex", "box1", "codex:config.toml")
	_, err := w.syncAgentConfigIn(time.Minute)
	require.NoError(t, err)
	got := read(t, filepath.Join(w.AccountDir(), "config.toml"))
	assert.NotContains(t, got, "keyring")
	assert.Equal(t, "file", decodeCodexConfig(t, got)[codexCredStoreKey])
	assert.Equal(t, "o3", decodeCodexConfig(t, got)["model"], "the rest of the operator's config is kept")
	assert.NotContains(t, got, "OPERATORS-OWN-CODEX-LOGIN")
	assert.NoFileExists(t, filepath.Join(w.AccountDir(), "auth.json"))
}

func decodeCodexConfig(t *testing.T, body string) map[string]any {
	t.Helper()
	var tree map[string]any
	require.NoError(t, toml.Unmarshal([]byte(body), &tree), body)
	return tree
}

// countCredStoreKeys counts the key at any depth of a decoded config.
func countCredStoreKeys(v any) int {
	n := 0
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if k == codexCredStoreKey {
				n++
			}
			n += countCredStoreKeys(child)
		}
	case []any:
		for _, child := range t {
			n += countCredStoreKeys(child)
		}
	}
	return n
}

// However TOML spells the credential-store key, the copy ends with exactly one,
// at the top level, set to "file": never the operator's value.
func TestForceCodexFileCredentialStoreCoversEverySpelling(t *testing.T) {
	spellings := map[string]string{
		"bare":                "cli_auth_credentials_store = \"keyring\"\n",
		"basic quoted":        "\"cli_auth_credentials_store\" = \"keyring\"\n",
		"literal quoted":      "'cli_auth_credentials_store' = 'keyring'\n",
		"escaped quoted":      "\"cli_auth_\\u0063redentials_store\" = \"keyring\"\n",
		"spaced around equal": "  cli_auth_credentials_store   =   \"keyring\"   # operator\n",
		"dotted":              "profiles.x.cli_auth_credentials_store = \"keyring\"\n",
		"dotted quoted":       "profiles.x.\"cli_auth_credentials_store\" = \"keyring\"\n",
		"in a table":          "[profiles.x]\ncli_auth_credentials_store = \"keyring\"\n",
		"in a quoted table":   "[profiles.x]\n\"cli_auth_credentials_store\" = \"keyring\"\n",
		"inline table":        "profiles = { x = { cli_auth_credentials_store = \"keyring\" } }\n",
		"array of tables":     "[[hooks]]\ncli_auth_credentials_store = \"keyring\"\n",
		"quoted top, table":   "\"cli_auth_credentials_store\" = \"keyring\"\n[profiles.x]\ncli_auth_credentials_store = \"auto\"\n",
	}
	for name, body := range spellings {
		t.Run(name, func(t *testing.T) {
			out, err := forceCodexFileCredentialStore([]byte("model = \"o3\"\n" + body))
			require.NoError(t, err)
			tree := decodeCodexConfig(t, string(out))
			assert.Equal(t, "file", tree[codexCredStoreKey], string(out))
			assert.Equal(t, 1, countCredStoreKeys(tree), "no other spelling survives: %s", out)
			assert.NotContains(t, string(out), "keyring")
			assert.Equal(t, "o3", tree["model"])
		})
	}
	t.Run("absent key is added", func(t *testing.T) {
		out, err := forceCodexFileCredentialStore([]byte("model = \"o3\"\n"))
		require.NoError(t, err)
		assert.Equal(t, "file", decodeCodexConfig(t, string(out))[codexCredStoreKey])
	})
	t.Run("empty file", func(t *testing.T) {
		out, err := forceCodexFileCredentialStore(nil)
		require.NoError(t, err)
		assert.Equal(t, "file", decodeCodexConfig(t, string(out))[codexCredStoreKey])
	})
}

// A config that does not parse is refused: it is not copied, so the keyring setting
// it may hide never reaches the sandbox.
func TestAgentConfigRefusesAnUnparsableCodexConfig(t *testing.T) {
	operatorHome(t)
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("HOME"), ".codex", "config.toml"),
		[]byte("\"cli_auth_credentials_store\" = \"keyring\"\nthis is = = not toml [\n"), 0o600))
	_, err := forceCodexFileCredentialStore([]byte("a = [\n"))
	require.Error(t, err)
	w := configWorkspace(t, "codex", "box1", "codex:config.toml")
	_, err = w.syncAgentConfigIn(time.Minute)
	require.NoError(t, err)
	assert.NoFileExists(t, filepath.Join(w.AccountDir(), "config.toml"))
}

// Session A's placement never lands in B's scope.
func TestAgentConfigNeverLandsInAnotherSessionsScope(t *testing.T) {
	operatorHome(t)
	a := configWorkspace(t, "claude", "box1", "claude:settings.json")
	b := configWorkspace(t, "claude", "box1", "claude:settings.json")
	_, err := a.syncAgentConfigIn(time.Minute)
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(a.AccountDir(), "settings.json"))
	assert.NoDirExists(t, b.AccountDir())
}

// The operator's selected agent config survives an account swap exactly as it
// survives a restart: the rotation replaces the whole live scope with the staged
// one, so the incoming scope must carry it before the agent restarts.
func TestAgentConfigSurvivesAnOffBoxAccountSwap(t *testing.T) {
	f := newSwapFixture(t)
	opHome := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(opHome, "settings.json"), []byte(`{"model":"opus"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(opHome, "CLAUDE.md"), []byte("be terse"), 0o600))
	// This test process is both the daemon box and the sandbox: the operator's home
	// is CLAUDE_CONFIG_DIR while af stages, the host's scope while the (in-process)
	// agent-server rotates.
	hostScope := os.Getenv("CLAUDE_CONFIG_DIR")
	t.Setenv("CLAUDE_CONFIG_DIR", opHome)
	f.w.spec.AgentConfigFiles = []string{"claude:settings.json", "claude:CLAUDE.md"}
	f.inst.agentSrv = hostEnvServer{f.srv, t, hostScope}

	_, err := f.inst.SwapOffBoxAccount("alt", false)
	require.NoError(t, err)

	assert.Equal(t, secretAlt, f.hostCred(t, "account"), "the new account is live")
	assert.Equal(t, `{"model":"opus"}`, readOrEmpty(filepath.Join(f.dir, "account", "settings.json")), "the model settings came along")
	assert.Equal(t, "be terse", readOrEmpty(filepath.Join(f.dir, "account", "CLAUDE.md")), "and the instructions")
}

// A session with no declared config gets none, as before.
func TestNoAgentConfigIsStagedWhenNoneIsDeclared(t *testing.T) {
	f := newSwapFixture(t)
	_, err := f.inst.SwapOffBoxAccount("alt", false)
	require.NoError(t, err)
	assert.NoFileExists(t, filepath.Join(f.dir, "account", "settings.json"))
}

// hostEnvServer runs the fake agent-server's swap with the host's environment.
type hostEnvServer struct {
	*acctSwapServer
	t     *testing.T
	scope string
}

func (s hostEnvServer) SwapAccountInPlace() (string, error) {
	s.t.Setenv("CLAUDE_CONFIG_DIR", s.scope)
	return s.acctSwapServer.SwapAccountInPlace()
}

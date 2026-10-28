package session

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

const testSecretA = "refresh-token-AAAA"

func writeAccount(t *testing.T, agent, secret string) sessionenv.Account {
	t.Helper()
	dir := t.TempDir()
	rel := accountCredentialRel[agent][0]
	require.NoError(t, os.MkdirAll(filepath.Join(dir, filepath.Dir(rel)), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, rel), []byte(secret), 0o600))
	// Non-credential state must stay home.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "history.jsonl"), []byte("private history"), 0o600))
	return sessionenv.Account{Agent: agent, Name: "work-" + agent, Dir: dir}
}

// workspaceOn is a sandboxWorkspace whose "host" is a local temp dir, so the real
// tar/sh scripts run against a real filesystem.
func workspaceOn(t *testing.T, acct sessionenv.Account, host string) *sandboxWorkspace {
	t.Helper()
	return &sandboxWorkspace{
		shell:      localShell{},
		SessionDir: t.TempDir(),
		spec:       ProvisionSpec{Account: acct, AccountSyncHost: host},
	}
}

func TestAccountSyncOnlyToAListedHost(t *testing.T) {
	acct := writeAccount(t, "claude", testSecretA)
	sh := &recordingShell{}
	w := &sandboxWorkspace{shell: sh, SessionDir: "/s", spec: ProvisionSpec{Account: acct}} // no AccountSyncHost
	err := w.syncAccountIn(time.Second)
	require.ErrorContains(t, err, "not in the global host list")
	assert.Empty(t, sh.scripts, "an unlisted target must be sent nothing — not even a mkdir")
	assert.Empty(t, sh.stdin)

	_, err = w.pullAccountBack(time.Second)
	require.Error(t, err)
	assert.Empty(t, sh.scripts)

	// No account: nothing to do, nothing sent, no error.
	none := &sandboxWorkspace{shell: sh, SessionDir: "/s"}
	require.NoError(t, none.syncAccountIn(time.Second))
	assert.Empty(t, sh.scripts)
}

func TestAccountSyncLandsInTheSessionScopeOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	acctA := writeAccount(t, "claude", testSecretA)
	acctB := writeAccount(t, "claude", "refresh-token-BBBB")
	a := workspaceOn(t, acctA, "box1")
	b := workspaceOn(t, acctB, "box1")

	require.NoError(t, a.syncAccountIn(time.Minute))

	got, err := os.ReadFile(filepath.Join(a.AccountDir(), ".credentials.json"))
	require.NoError(t, err)
	assert.Equal(t, testSecretA, string(got))
	info, err := os.Stat(filepath.Join(a.AccountDir(), ".credentials.json"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	assert.NoFileExists(t, filepath.Join(a.AccountDir(), "history.jsonl"), "only credential files travel")

	// A's sync is invisible to B's scope, and to the remote user's own home.
	assert.NoDirExists(t, b.AccountDir(), "session A's sync must never land in session B's scope")
	entries, _ := os.ReadDir(home)
	assert.Empty(t, entries, "the remote user's global home must never be written")
	assert.True(t, strings.HasPrefix(a.AccountDir(), a.SessionDir+"/"))

	// And B syncing its own account does not disturb A's.
	require.NoError(t, b.syncAccountIn(time.Minute))
	again, _ := os.ReadFile(filepath.Join(a.AccountDir(), ".credentials.json"))
	assert.Equal(t, testSecretA, string(again))
	bGot, _ := os.ReadFile(filepath.Join(b.AccountDir(), ".credentials.json"))
	assert.Equal(t, "refresh-token-BBBB", string(bGot))
}

func TestAccountRefreshOnTheHostFlowsBack(t *testing.T) {
	acct := writeAccount(t, "claude", testSecretA)
	w := workspaceOn(t, acct, "box1")
	require.NoError(t, w.syncAccountIn(time.Minute))

	n, err := w.pullAccountBack(time.Minute)
	require.NoError(t, err)
	assert.Zero(t, n, "an unchanged credential must not be rewritten")

	// The agent on the host rotates its token.
	require.NoError(t, os.WriteFile(filepath.Join(w.AccountDir(), ".credentials.json"), []byte("rotated-token"), 0o600))
	n, err = w.pullAccountBack(time.Minute)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	got, _ := os.ReadFile(filepath.Join(acct.Dir, ".credentials.json"))
	assert.Equal(t, "rotated-token", string(got), "the refresh must reach the registry copy")
	info, _ := os.Stat(filepath.Join(acct.Dir, ".credentials.json"))
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	hist, _ := os.ReadFile(filepath.Join(acct.Dir, "history.jsonl"))
	assert.Equal(t, "private history", string(hist), "write-back touches credential files only")
}

func TestAccountTeardownWritesBackThenRemoves(t *testing.T) {
	acct := writeAccount(t, "codex", "codex-token-1")
	w := workspaceOn(t, acct, "box1")
	require.NoError(t, w.syncAccountIn(time.Minute))
	require.DirExists(t, w.AccountDir())
	require.NoError(t, os.WriteFile(filepath.Join(w.AccountDir(), "auth.json"), []byte("codex-token-2"), 0o600))

	require.NoError(t, w.teardownAccount(time.Minute))
	assert.NoDirExists(t, w.AccountDir(), "teardown must remove the synced credentials from the host")
	got, _ := os.ReadFile(filepath.Join(acct.Dir, "auth.json"))
	assert.Equal(t, "codex-token-2", string(got), "the last refresh is written back before removal")

	require.NoError(t, w.teardownAccount(time.Minute), "teardown is idempotent")
}

// The archive comes from the HOST and is untrusted: a name off the allowlist, a
// traversal or a link must refuse the whole write-back and write nothing.
func TestAccountWriteBackRefusesAHostileArchive(t *testing.T) {
	acct := writeAccount(t, "claude", testSecretA)
	for name, hdr := range map[string]*tar.Header{
		"traversal": {Name: "../../evil", Mode: 0o600, Size: 1, Typeflag: tar.TypeReg},
		"unlisted":  {Name: "settings.json", Mode: 0o600, Size: 1, Typeflag: tar.TypeReg},
		"symlink":   {Name: ".credentials.json", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			require.NoError(t, tw.WriteHeader(hdr))
			if hdr.Size > 0 {
				_, _ = tw.Write([]byte("x"))
			}
			require.NoError(t, tw.Close())
			w := &sandboxWorkspace{shell: fixedOutputShell{out: buf.Bytes()}, SessionDir: "/s",
				spec: ProvisionSpec{Account: acct, AccountSyncHost: "box1"}}
			_, err := w.pullAccountBack(time.Second)
			require.Error(t, err)
			got, _ := os.ReadFile(filepath.Join(acct.Dir, ".credentials.json"))
			assert.Equal(t, testSecretA, string(got))
		})
	}
}

type fixedOutputShell struct{ out []byte }

func (f fixedOutputShell) Run(time.Duration, string, io.Reader, bool) ([]byte, error) {
	return f.out, nil
}

// The agent is pointed at the scoped dir with its own variable; the value rides
// the 0600 identity file, and no credential content is ever in a command.
func TestAccountEnvPointsTheAgentAtTheScopedDir(t *testing.T) {
	acct := writeAccount(t, "claude", testSecretA)
	w := &sandboxWorkspace{shell: &recordingShell{}, SessionDir: "/s", program: "claude",
		spec: ProvisionSpec{Title: "t", Account: acct, AccountSyncHost: "box1"}}
	name, value, ok := w.accountEnv()
	require.True(t, ok)
	assert.Equal(t, "CLAUDE_CONFIG_DIR", name)
	assert.Equal(t, "/s/account", value)

	cmd, err := w.agentServerCommand()
	require.NoError(t, err)
	assert.Contains(t, cmd, "CLAUDE_CONFIG_DIR")
	assert.NotContains(t, cmd, testSecretA)

	plain := &sandboxWorkspace{shell: &recordingShell{}, SessionDir: "/s", program: "claude", spec: ProvisionSpec{Title: "t"}}
	_, _, ok = plain.accountEnv()
	assert.False(t, ok)
	pcmd, err := plain.agentServerCommand()
	require.NoError(t, err)
	assert.NotContains(t, pcmd, "CLAUDE_CONFIG_DIR", "no account, no account variable")
}

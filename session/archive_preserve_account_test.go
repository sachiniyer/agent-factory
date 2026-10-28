package session

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// preservedSessionWithAccount is an ssh session whose "host" is a local temp
// dir: a workspace with a file in it, and an account synced into <dir>/account.
func preservedSessionWithAccount(t *testing.T, run func(time.Duration, string, io.Reader, bool) ([]byte, error)) (inst *Instance, dir, registryCred string) {
	t.Helper()
	acct := writeAccount(t, "claude", testSecretA)
	dir = t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "workspace"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workspace", "work.txt"), []byte("unpushed work"), 0o644))

	p := &sandboxProvisioner{sessionDir: dir}
	if run == nil {
		run = func(d time.Duration, script string, stdin io.Reader, combined bool) ([]byte, error) {
			return localShell{}.Run(d, script, stdin, combined)
		}
	}
	p.runCommandFn = run
	w := &sandboxWorkspace{shell: p, SessionDir: dir, spec: ProvisionSpec{Account: acct, AccountSyncHost: "box1"}}
	require.NoError(t, w.syncAccountIn(time.Minute))
	p.account = w

	b := &sshBackend{provisioner: p, cleanup: &SSHRuntimeCleanupData{SessionDir: dir}}
	inst = &Instance{Title: "t", backend: b, started: true, Host: "box1", Tabs: []*Tab{newRemoteAgentTab()}}
	attachStoppableAgent(t, inst, false)
	return inst, dir, filepath.Join(acct.Dir, ".credentials.json")
}

// W8.5 x W7.3: a preserving archive keeps the workspace on its host, but not the
// synced credentials — and a refresh the agent wrote there is written back first.
func TestArchivePreserveRemovesSyncedAccountAfterWritingItBack(t *testing.T) {
	inst, dir, registryCred := preservedSessionWithAccount(t, nil)
	require.DirExists(t, filepath.Join(dir, "account"), "premise: the account is synced")
	// The agent rotated its token on the host before the archive.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "account", ".credentials.json"), []byte("rotated-before-archive"), 0o600))

	loc, err := inst.ArchiveSandboxPreserving()
	require.NoError(t, err)
	assert.Equal(t, dir, loc.Dir)

	assert.FileExists(t, filepath.Join(dir, "workspace", "work.txt"), "the workspace is preserved intact")
	assert.NoDirExists(t, filepath.Join(dir, "account"), "an archive that keeps the workspace must not keep the credentials")
	got, _ := os.ReadFile(registryCred)
	assert.Equal(t, "rotated-before-archive", string(got), "the refresh is written back before the credentials are removed")
	assert.NotNil(t, inst.ArchivedWorkspace(), "the row records where the workspace lives")
}

// Best-effort, like the reap: a host that cannot be reached for the write-back
// never blocks the archive.
func TestArchivePreserveSucceedsWhenTheAccountTeardownFails(t *testing.T) {
	inst, _, _ := preservedSessionWithAccount(t, nil)
	b := inst.currentBackend().(*sshBackend)
	b.provisioner.runCommandFn = func(time.Duration, string, io.Reader, bool) ([]byte, error) {
		return nil, errors.New("ssh: connection refused")
	}
	loc, err := inst.ArchiveSandboxPreserving()
	require.NoError(t, err, "a failed account teardown must not block the archive")
	assert.NotEmpty(t, loc.Dir)
	assert.NotNil(t, inst.ArchivedWorkspace())
}

// A session with no synced account is archived exactly as before.
func TestArchivePreserveWithoutAnAccountTouchesNoAccountState(t *testing.T) {
	dir := t.TempDir()
	p := &sandboxProvisioner{sessionDir: dir, runCommandFn: func(time.Duration, string, io.Reader, bool) ([]byte, error) {
		t.Fatal("no command may run for a session with no account")
		return nil, nil
	}}
	b := &sshBackend{provisioner: p, cleanup: &SSHRuntimeCleanupData{SessionDir: dir}}
	inst := &Instance{Title: "t", backend: b, started: true, Tabs: []*Tab{newRemoteAgentTab()}}
	attachStoppableAgent(t, inst, false)
	_, err := inst.ArchiveSandboxPreserving()
	require.NoError(t, err)
}

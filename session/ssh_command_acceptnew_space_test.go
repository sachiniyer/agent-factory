package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// The accept-new persistence guarantee (#2556) against a SPACED store path —
// the report's exploit scenario, reproduced as an in-package regression test.
//
// Drives the EXACT production functions (sshCommandPinnedTo +
// prepareSSHHostKeyStore + runOverTransport) against a real loopback sshd, with
// AGENT_FACTORY_HOME set to a directory whose path contains a space and NO
// ssh.known_hosts set, so the accept-new store resolves to the af-owned file
// under the spaced AF home (acceptNewKnownHostsPathFor).
//
// Pre-fix: the spaced af-owned store was silently bypassed — ssh re-split the
// UserKnownHostsFile value on whitespace, never read the af-owned file, treated
// the host as unknown on EVERY connection, and wrote nothing persistable. So:
//   - the af-owned store stayed EMPTY forever; and
//   - a host key that changed between connections was re-TOFU-accepted each
//     time (the persistence guarantee the #2556 af-owned-store decision exists
//     to provide — and the report's exploit scenario).
//
// With the fix: the first connection auto-accepts AND persists the key to the
// exact spaced af-owned file; rotating the sshd's host key makes the SECOND
// connection REFUSE the changed key, proving the swap is detected.

func TestSSHAcceptNewPersistsLearnedKeyToSpacedAFHomeAndDetectsChange(t *testing.T) {
	lab := startLoopbackSSHD(t)

	// A spaced AGENT_FACTORY_HOME — the accept-new store resolves to
	// <spaced AF home>/<sshKnownHostsFileName> because ssh.known_hosts is unset.
	spacedAFHome := filepath.Join(t.TempDir(), "af home with space")
	require.NoError(t, os.MkdirAll(spacedAFHome, 0o700))
	t.Setenv("AGENT_FACTORY_HOME", spacedAFHome)
	// Isolate from the operator's ~/.ssh too.
	t.Setenv("HOME", t.TempDir())

	cfg := config.SSHConfig{
		Host:         "127.0.0.1",
		Port:         lab.port,
		User:         lab.user,
		IdentityFile: lab.keyPath,
	}
	afStore := filepath.Join(spacedAFHome, sshKnownHostsFileName)

	// --- Step 3: provision end-to-end under accept-new ----------------------
	require.NoError(t, prepareSSHHostKeyStore(cfg, config.SSHHostKeyAcceptNew),
		"prepareSSHHostKeyStore must create the spaced af-owned store")
	cmd, err := sshCommandPinnedTo(cfg, config.SSHHostKeyAcceptNew, "", 0)
	require.NoError(t, err)
	out, err := runOverTransport(cmd, "echo ACCEPT_NEW_FIRST")
	require.NoError(t, err,
		"the first accept-new connection must succeed against the spaced af-owned store.\nssh said:\n%s", out)
	assert.Contains(t, out, "ACCEPT_NEW_FIRST")

	// --- Step 4: assert the spaced af-owned store now CONTAINS the key ------
	// THIS is the line that was empty pre-fix (the store was bypassed; ssh wrote
	// nothing persistable to it).
	body, rerr := os.ReadFile(afStore)
	require.NoError(t, rerr, "the af-owned store must exist and be readable")
	assert.NotEmpty(t, strings.TrimSpace(string(body)),
		"the learned key MUST be persisted to the spaced af-owned store — pre-fix this file stayed empty "+
			"because ssh re-split the UserKnownHostsFile value and never read or wrote the af-owned file")
	assert.Contains(t, string(body), fmt.Sprintf("[127.0.0.1]:%d", lab.port),
		"the persisted entry must be keyed for the host af connected to")

	// --- Step 5: rotate the sshd's host key ---------------------------------
	// A fresh host key the daemon has never seen. Pre-fix: the store was bypassed
	// every time, so ssh treated the host as unknown on every connection and
	// re-TOFU-accepted whatever key was presented — a swap was never detected.
	// With the fix: the FIRST key is persisted, so the SECOND (different) key is
	// a CHANGE, which accept-new refuses.
	rotateErr := rotateLoopbackSSHDHostKey(t, lab)
	require.NoError(t, rotateErr, "must rotate the sshd's host key to test change-detection")

	// --- Step 6: second connection must REFUSE the changed key -------------
	cmd2, err := sshCommandPinnedTo(cfg, config.SSHHostKeyAcceptNew, "", 0)
	require.NoError(t, err)
	out2, err2 := runOverTransport(cmd2, "echo SHOULD_BE_REFUSED")
	require.Error(t, err2,
		"a CHANGED host key must be REFUSED under accept-new once the original key is persisted — "+
			"pre-fix the store was bypassed and the new key was re-TOFU-accepted every time.\nssh said:\n%s", out2)
	assert.Contains(t, out2, "Host key verification failed",
		"the refusal must be for the recorded reason — accept-new refuses a CHANGED key")
}

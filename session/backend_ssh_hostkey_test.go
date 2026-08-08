package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// The host-key guarantees of `backend = "ssh"`, restated against the composed
// ssh command after the transport converged onto the sandbox one (#3052).
//
// These previously asserted x/crypto callbacks directly. That mechanism is gone,
// so they now assert the OPTIONS that carry the same meaning — which is the only
// honest translation: the posture is no longer af's code to execute, it is af's
// instruction to OpenSSH. What must not change is which posture each setting
// produces and, just as importantly, WHICH FILE it reads and writes (#2556).

func sshCmdFor(t *testing.T, cfg config.SSHConfig, posture string) string {
	t.Helper()
	cmd, err := sshCommandForConfig(cfg, posture, "")
	require.NoError(t, err)
	return cmd
}

// strict is the default posture and verifies against ssh.known_hosts, else the
// user's ~/.ssh/known_hosts — unchanged from before #2556.
func TestSSHStrictPostureVerifiesAgainstTheConfiguredFile(t *testing.T) {
	kh := filepath.Join(t.TempDir(), "my_known_hosts")
	cmd := sshCmdFor(t, config.SSHConfig{Host: "h.example.com", KnownHosts: kh}, config.SSHHostKeyStrict)

	assert.Contains(t, cmd, "StrictHostKeyChecking=yes", "strict must refuse an unknown or changed key")
	assert.Contains(t, cmd, "UserKnownHostsFile='"+kh+"'")
	assert.Contains(t, cmd, "GlobalKnownHostsFile=/dev/null",
		"the old client read ONE file; a system-wide entry must not satisfy verification now")
	assert.Contains(t, cmd, "KnownHostsCommand=none",
		"nor may an ssh_config helper program supply a key af never vouched for")
}

func TestSSHStrictPostureFallsBackToTheUserKnownHosts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cmd := sshCmdFor(t, config.SSHConfig{Host: "h.example.com"}, config.SSHHostKeyStrict)
	assert.Contains(t, cmd, "UserKnownHostsFile='"+filepath.Join(home, ".ssh", "known_hosts")+"'")
}

// An unexpected posture value must still fail SAFE to strict, exactly as the old
// default branch did.
func TestSSHUnknownPostureFailsSafeToStrict(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cmd := sshCmdFor(t, config.SSHConfig{Host: "h.example.com"}, "not-a-real-posture")
	assert.Contains(t, cmd, "StrictHostKeyChecking=yes")
}

// #2556's destination guarantee, and the one most worth preserving: accept-new
// records learned keys in the af-owned store, NEVER the user's shared
// ~/.ssh/known_hosts.
func TestSSHAcceptNewWritesToAFHomeNotUserKnownHosts(t *testing.T) {
	afHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", afHome)
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)

	cmd := sshCmdFor(t, config.SSHConfig{Host: "h.example.com"}, config.SSHHostKeyAcceptNew)

	assert.Contains(t, cmd, "StrictHostKeyChecking=accept-new",
		"accept-new trusts an unknown key but must still refuse a CHANGED one")
	assert.Contains(t, cmd, "UserKnownHostsFile='"+filepath.Join(afHome, sshKnownHostsFileName)+"'")
	assert.NotContains(t, cmd, filepath.Join(userHome, ".ssh", "known_hosts"),
		"accept-new must never be pointed at the user's shared known_hosts (#2556)")

	// The store must exist before ssh runs, or the first connection fails — but
	// creating it is NOT composition's job, because composition also runs while
	// persisted cleanup handles are being loaded. prepareSSHHostKeyStore is the
	// step that does it, immediately before the command runs.
	_, statErr := os.Stat(filepath.Join(afHome, sshKnownHostsFileName))
	assert.True(t, os.IsNotExist(statErr), "composing the command must not create the store")
	require.NoError(t, prepareSSHHostKeyStore(config.SSHConfig{Host: "h.example.com"}, config.SSHHostKeyAcceptNew))
	_, statErr = os.Stat(filepath.Join(afHome, sshKnownHostsFileName))
	assert.NoError(t, statErr, "the af-owned store must exist by the time ssh runs")
}

func TestSSHAcceptNewUsesConfiguredKnownHosts(t *testing.T) {
	afHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", afHome)
	kh := filepath.Join(t.TempDir(), "my_known_hosts")

	cmd := sshCmdFor(t, config.SSHConfig{Host: "h2.example.com", KnownHosts: kh}, config.SSHHostKeyAcceptNew)
	assert.Contains(t, cmd, "UserKnownHostsFile='"+kh+"'")
	assert.NotContains(t, cmd, sshKnownHostsFileName, "with ssh.known_hosts set, the af store must not be used")
}

// insecure performs no verification and records nothing — the equivalent of the
// old InsecureIgnoreHostKey.
func TestSSHInsecurePostureVerifiesNothing(t *testing.T) {
	cmd := sshCmdFor(t, config.SSHConfig{Host: "whatever"}, config.SSHHostKeyInsecure)
	assert.Contains(t, cmd, "StrictHostKeyChecking=no")
	assert.Contains(t, cmd, "UserKnownHostsFile='"+os.DevNull+"'",
		"insecure must not record anything either — /dev/null is the equivalent of ignoring the key")
}

// The login user is PINNED, so an ssh_config `User` directive cannot silently
// change who af connects as. That is the behaviour the old client had by virtue
// of never reading ssh_config at all.
func TestSSHLoginUserIsPinnedAgainstSSHConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cmd := sshCmdFor(t, config.SSHConfig{Host: "h.example.com", User: "deploy"}, config.SSHHostKeyStrict)
	assert.Contains(t, cmd, "User='deploy'")

	// With no ssh.user, the daemon's own account is pinned rather than left to
	// ssh_config — again matching the old loginUser().
	bare := sshCmdFor(t, config.SSHConfig{Host: "h.example.com"}, config.SSHHostKeyStrict)
	assert.Contains(t, bare, "-o User='")
}

// An identity file is offered, and the AGENT is still available — the old
// authMethods offered both, so IdentitiesOnly must not appear.
func TestSSHIdentityFileDoesNotDisableTheAgent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	key := filepath.Join(t.TempDir(), "id_ed25519")
	cmd := sshCmdFor(t, config.SSHConfig{Host: "h.example.com", IdentityFile: key}, config.SSHHostKeyStrict)

	assert.Contains(t, cmd, "-i '"+key+"'")
	assert.NotContains(t, cmd, "IdentitiesOnly",
		"the old client offered identity-file keys AND agent keys; disabling the agent would break setups")
}

// A provision must never block on a prompt: there is no human attached.
func TestSSHCommandNeverPrompts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	assert.Contains(t, sshCmdFor(t, config.SSHConfig{Host: "h"}, config.SSHHostKeyStrict), "BatchMode=yes")
}

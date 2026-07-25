package session

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/sachiniyer/agent-factory/config"
)

func testSSHPublicKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	sshPub, err := ssh.NewPublicKey(pub)
	require.NoError(t, err)
	return sshPub
}

// TestSSHAcceptNewWritesToAFHomeNotUserKnownHosts is the #2556 destination
// guarantee: with no ssh.known_hosts configured, accept-new records the learned
// key in the af-owned store under AF_HOME and NEVER touches the user's shared
// ~/.ssh/known_hosts. This is the "a test must pin WHERE the key lands" contract.
func TestSSHAcceptNewWritesToAFHomeNotUserKnownHosts(t *testing.T) {
	afHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", afHome)
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)

	p := &sshProvisioner{
		cfg:                 config.SSHConfig{Host: "ephemeral.example.com", Port: 22},
		hostKeyVerification: config.SSHHostKeyAcceptNew,
	}
	cb, err := p.hostKeyCallback()
	require.NoError(t, err)

	require.NoError(t,
		cb("ephemeral.example.com:22", &net.TCPAddr{IP: net.IPv4(203, 0, 113, 7), Port: 22}, testSSHPublicKey(t)),
		"accept-new must trust an unknown host on first connect")

	data, err := os.ReadFile(filepath.Join(afHome, "ssh_known_hosts"))
	require.NoError(t, err, "accept-new must record the key in the af-owned store")
	assert.Contains(t, string(data), "ephemeral.example.com")

	_, statErr := os.Stat(filepath.Join(userHome, ".ssh", "known_hosts"))
	assert.True(t, os.IsNotExist(statErr), "accept-new must NOT write to ~/.ssh/known_hosts")
}

// TestSSHAcceptNewRefusesChangedKey: TOFU still protects against MITM — once a
// host's key is learned, a DIFFERENT key for that host is refused.
func TestSSHAcceptNewRefusesChangedKey(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	p := &sshProvisioner{
		cfg:                 config.SSHConfig{Host: "h.example.com", Port: 22},
		hostKeyVerification: config.SSHHostKeyAcceptNew,
	}
	addr := &net.TCPAddr{IP: net.IPv4(198, 51, 100, 3), Port: 22}

	cb1, err := p.hostKeyCallback()
	require.NoError(t, err)
	require.NoError(t, cb1("h.example.com:22", addr, testSSHPublicKey(t)))

	// A fresh callback reloads the learned key; a changed key must be refused.
	cb2, err := p.hostKeyCallback()
	require.NoError(t, err)
	assert.Error(t, cb2("h.example.com:22", addr, testSSHPublicKey(t)),
		"accept-new must refuse a CHANGED host key")
}

// TestSSHAcceptNewUsesConfiguredKnownHosts: when the operator set ssh.known_hosts,
// accept-new writes THERE, and the af store is not created.
func TestSSHAcceptNewUsesConfiguredKnownHosts(t *testing.T) {
	afHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", afHome)
	t.Setenv("HOME", t.TempDir())
	kh := filepath.Join(t.TempDir(), "my_known_hosts")

	p := &sshProvisioner{
		cfg:                 config.SSHConfig{Host: "h2.example.com", Port: 22, KnownHosts: kh},
		hostKeyVerification: config.SSHHostKeyAcceptNew,
	}
	cb, err := p.hostKeyCallback()
	require.NoError(t, err)
	require.NoError(t, cb("h2.example.com:22", &net.TCPAddr{IP: net.IPv4(192, 0, 2, 9), Port: 22}, testSSHPublicKey(t)))

	data, err := os.ReadFile(kh)
	require.NoError(t, err)
	assert.Contains(t, string(data), "h2.example.com")

	_, statErr := os.Stat(filepath.Join(afHome, "ssh_known_hosts"))
	assert.True(t, os.IsNotExist(statErr), "with ssh.known_hosts set, the af store must not be used")
}

// TestSSHInsecureAcceptsAnyKey: insecure mode performs no verification.
func TestSSHInsecureAcceptsAnyKey(t *testing.T) {
	p := &sshProvisioner{
		cfg:                 config.SSHConfig{Host: "whatever"},
		hostKeyVerification: config.SSHHostKeyInsecure,
	}
	cb, err := p.hostKeyCallback()
	require.NoError(t, err)
	assert.NoError(t, cb("whatever:22", &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 22}, testSSHPublicKey(t)))
}

// TestSSHStrictDefaultRequiresKnownHost: strict (the default posture) refuses an
// unknown host — the existing behavior, unchanged. Points at an empty known_hosts
// so the callback construction succeeds but verification finds nothing.
func TestSSHStrictDefaultRequiresKnownHost(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	kh := filepath.Join(t.TempDir(), "known_hosts")
	require.NoError(t, os.WriteFile(kh, []byte{}, 0o600))

	p := &sshProvisioner{
		cfg:                 config.SSHConfig{Host: "s.example.com", Port: 22, KnownHosts: kh},
		hostKeyVerification: config.SSHHostKeyStrict,
	}
	cb, err := p.hostKeyCallback()
	require.NoError(t, err)
	assert.Error(t,
		cb("s.example.com:22", &net.TCPAddr{IP: net.IPv4(203, 0, 113, 1), Port: 22}, testSSHPublicKey(t)),
		"strict must refuse an unknown host (no accept-new)")
}

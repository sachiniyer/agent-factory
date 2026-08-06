package session

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// End-to-end proof that the #2847 contract CONNECTS, not merely that it
// validates.
//
// Every other test here checks a composed string or a parsed record. Those would
// all keep passing if the pinned known_hosts were subtly wrong — the wrong host
// spelling, a bad port form, an option that does not do what its name suggests —
// because none of them ever hands the result to OpenSSH. This one does: a real
// sshd, a real key exchange, over the exact command hookProvisionSSHCommand
// builds from a real provision record.
//
// It runs entirely in user space on a high port with its own host key and its own
// authorized_keys, so it touches no system sshd and needs no container.

// throwawaySSHD starts an sshd the calling user owns, and returns its port and
// the PUBLIC host key a provision_cmd would have vouched for.
func throwawaySSHD(t *testing.T) (port int, hostPubKey string) {
	t.Helper()
	sshdPath := "/usr/sbin/sshd"
	if _, err := os.Stat(sshdPath); err != nil {
		if p, lookErr := exec.LookPath("sshd"); lookErr == nil {
			sshdPath = p
		} else {
			t.Skipf("no sshd available, so the connect path cannot be exercised here: %v", err)
		}
	}
	dir := t.TempDir()
	// sshd refuses a world-readable directory chain for authorized_keys.
	require.NoError(t, os.Chmod(dir, 0o700))

	hostKey := filepath.Join(dir, "host_ed25519")
	require.NoError(t, exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", hostKey).Run())
	userKey := filepath.Join(dir, "id_ed25519")
	require.NoError(t, exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", userKey).Run())

	pub, err := os.ReadFile(hostKey + ".pub")
	require.NoError(t, err)
	// known_hosts wants "<type> <base64>" without the trailing comment field.
	fields := strings.Fields(string(pub))
	require.GreaterOrEqual(t, len(fields), 2)
	hostPubKey = fields[0] + " " + fields[1]

	authorized := filepath.Join(dir, "authorized_keys")
	userPub, err := os.ReadFile(userKey + ".pub")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(authorized, userPub, 0o600))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port = ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())

	cfg := filepath.Join(dir, "sshd_config")
	require.NoError(t, os.WriteFile(cfg, []byte(fmt.Sprintf(`
Port %d
ListenAddress 127.0.0.1
HostKey %s
AuthorizedKeysFile %s
PidFile %s
UsePAM no
PasswordAuthentication no
KbdInteractiveAuthentication no
StrictModes no
PermitUserEnvironment no
LogLevel QUIET
`, port, hostKey, authorized, filepath.Join(dir, "sshd.pid"))), 0o600))

	cmd := exec.Command(sshdPath, "-D", "-f", cfg)
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a user-space sshd here: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, dialErr := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Skipf("the throwaway sshd never came up on 127.0.0.1:%d", port)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// The identity the composed command must use. hookProvisionSSHCommand does not
	// choose a key — the operator's ssh does — so point ssh at ours for the test.
	t.Setenv("AF_TEST_SSH_IDENTITY", userKey)
	return port, hostPubKey
}

// withIdentity appends the test identity to a composed command, standing in for
// the operator's own ssh_config/agent in production.
func withIdentity(cmd string) string {
	if key := os.Getenv("AF_TEST_SSH_IDENTITY"); key != "" {
		return strings.Replace(cmd, "ssh ", "ssh -i "+key+" -o IdentitiesOnly=yes ", 1)
	}
	return cmd
}

// THE end-to-end claim: a record from provision_cmd produces a command that
// authenticates the host against the pinned key and runs a command on it.
func TestProvisionedHostActuallyConnects(t *testing.T) {
	port, hostPubKey := throwawaySSHD(t)

	record := &hookProvisionRecord{Host: "127.0.0.1", Port: port, HostKey: hostPubKey}
	host, p := hookProvisionHostPort(record)
	knownHosts, err := hookProvisionKnownHosts(t.TempDir(), host, p, record.HostKey)
	require.NoError(t, err)

	sp := &sandboxProvisioner{sshCmd: withIdentity(hookProvisionSSHCommand(knownHosts, record))}
	out, err := sp.Run(30*time.Second, "echo af-provisioned-and-connected", nil, true)
	require.NoError(t, err, "the pinned command must actually connect; output: %s", out)
	assert.Contains(t, string(out), "af-provisioned-and-connected",
		"a command must really run on the provisioned host, not merely validate")
}

// The pin must be the AUTHORITY, not decoration: a record vouching for the wrong
// key must be refused by OpenSSH even though the host is reachable and the
// credentials are valid.
func TestProvisionedHostRefusesAWrongPinnedKey(t *testing.T) {
	port, _ := throwawaySSHD(t)

	// A syntactically valid key for a different machine.
	other := filepath.Join(t.TempDir(), "other")
	require.NoError(t, exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", other).Run())
	pub, err := os.ReadFile(other + ".pub")
	require.NoError(t, err)
	fields := strings.Fields(string(pub))
	wrong := fields[0] + " " + fields[1]

	record := &hookProvisionRecord{Host: "127.0.0.1", Port: port, HostKey: wrong}
	host, p := hookProvisionHostPort(record)
	knownHosts, err := hookProvisionKnownHosts(t.TempDir(), host, p, record.HostKey)
	require.NoError(t, err)

	sp := &sandboxProvisioner{sshCmd: withIdentity(hookProvisionSSHCommand(knownHosts, record))}
	out, err := sp.Run(30*time.Second, "echo should-not-run", nil, true)
	require.Error(t, err, "a key the script did not vouch for must be refused; output: %s", out)
	assert.NotContains(t, string(out), "should-not-run",
		"nothing may execute on a host whose key does not match the pin")
}

// And it must not hang waiting for a human when the key is unknown — BatchMode
// is what turns an unanswerable prompt into a prompt failure.
func TestProvisionedHostFailsFastRatherThanPrompting(t *testing.T) {
	port, _ := throwawaySSHD(t)

	empty := filepath.Join(t.TempDir(), "known_hosts")
	require.NoError(t, os.WriteFile(empty, nil, 0o600))
	sp := &sandboxProvisioner{sshCmd: withIdentity(hookProvisionSSHCommand(empty,
		&hookProvisionRecord{Host: "127.0.0.1", Port: port, HostKey: "unused"}))}

	started := time.Now()
	_, err := sp.Run(20*time.Second, "echo nope", nil, true)
	require.Error(t, err, "an unknown host must be refused, not trusted")
	assert.Less(t, time.Since(started), 15*time.Second,
		"it must fail fast rather than block on a prompt no unattended provision can answer")
}

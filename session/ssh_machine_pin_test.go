package session

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// #3122: pinning an ADDRESS is not pinning a MACHINE.
//
// The container-level proof lives in integration/backend_ssh_vip_test.go, which
// puts two sshds behind a real round-robin balancer. These cover the decision
// logic without one, so a regression is caught in seconds rather than minutes.

func TestParsesTheServerSideOfSSHConnection(t *testing.T) {
	// sshd's format is "client_ip client_port server_ip server_port"; the SERVER
	// fields are the address that machine accepted the connection on, which behind
	// a balancer is the backend's own address rather than the VIP.
	addr, port, ok := parseSSHConnectionServerAddr("172.17.0.1 49266 172.17.0.4 22")
	require.True(t, ok)
	assert.Equal(t, "172.17.0.4", addr)
	assert.Equal(t, 22, port)

	// Trailing newline is what a shell actually delivers.
	addr, port, ok = parseSSHConnectionServerAddr("10.0.0.2 5 10.0.0.9 2222\n")
	require.True(t, ok)
	assert.Equal(t, "10.0.0.9", addr)
	assert.Equal(t, 2222, port)

	// IPv6 round-trips through net.ParseIP's canonical spelling.
	addr, _, ok = parseSSHConnectionServerAddr("::1 5 2001:DB8::1 22")
	require.True(t, ok)
	assert.Equal(t, "2001:db8::1", addr)

	// Everything else is refused rather than guessed at: this value becomes a dial
	// target AND a token in a composed shell command.
	for name, raw := range map[string]string{
		"empty":             "",
		"too few fields":    "1.2.3.4 22 5.6.7.8",
		"too many fields":   "1.2.3.4 22 5.6.7.8 22 extra",
		"server not an IP":  "1.2.3.4 22 not-an-ip 22",
		"port not a number": "1.2.3.4 22 5.6.7.8 ssh",
		"port out of range": "1.2.3.4 22 5.6.7.8 70000",
		"port zero":         "1.2.3.4 22 5.6.7.8 0",
	} {
		t.Run(name, func(t *testing.T) {
			_, _, ok := parseSSHConnectionServerAddr(raw)
			assert.False(t, ok, "%q must be refused", raw)
		})
	}
}

// The decision, driven through the real transport seam with a stubbed remote.
func TestLearnPinnedMachineOnlyRepinsWhenItHelpsAndCanBeReached(t *testing.T) {
	// A listener standing in for the backend's own address:port, so "reachable" is
	// a fact rather than an assumption.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			_ = c.Close()
		}
	}()
	reachablePort := ln.Addr().(*net.TCPAddr).Port

	// stubTransport makes the provisioner's `sh -c '<sshCmd> "$@"'` print a canned
	// SSH_CONNECTION and ignore the script, so this exercises the real Run path
	// without an sshd.
	stubTransport := func(sshConnection string) string {
		return "printf '%s' " + shellQuoteSandbox(sshConnection) + " #"
	}

	t.Run("a different, reachable machine is pinned", func(t *testing.T) {
		addr, port := learnPinnedMachine(
			stubTransport(fmt.Sprintf("10.0.0.1 5 127.0.0.1 %d", reachablePort)), "198.51.100.7", 22)
		assert.Equal(t, "127.0.0.1", addr)
		assert.Equal(t, reachablePort, port)
	})

	t.Run("the same address and port is no gain", func(t *testing.T) {
		// An ordinary host, and also what an IP-preserving (DSR) balancer reports:
		// re-pinning to it would change nothing.
		addr, port := learnPinnedMachine(
			stubTransport(fmt.Sprintf("10.0.0.1 5 127.0.0.1 %d", reachablePort)), "127.0.0.1", reachablePort)
		assert.Empty(t, addr)
		assert.Zero(t, port)
	})

	t.Run("an unreachable machine falls back", func(t *testing.T) {
		// Shrunk so this costs milliseconds rather than the shipped 20s. The bound
		// itself is covered by TestTheDialProbeIsBounded, which asserts the shipped
		// value separately — this only needs the reachability DECISION.
		prev := sshDialProbeTimeout
		sshDialProbeTimeout = 300 * time.Millisecond
		defer func() { sshDialProbeTimeout = prev }()

		// TEST-NET-2, routed nowhere: a balanced backend on a network the daemon
		// cannot reach. Pinning it would turn a working backend into one where every
		// step fails, which is worse than the split it would prevent.
		addr, port := learnPinnedMachine(stubTransport("10.0.0.1 5 198.51.100.9 22"), "203.0.113.5", 22)
		assert.Empty(t, addr)
		assert.Zero(t, port)
	})

	t.Run("an unusable answer falls back", func(t *testing.T) {
		addr, port := learnPinnedMachine(stubTransport("not what sshd exports"), "203.0.113.5", 22)
		assert.Empty(t, addr)
		assert.Zero(t, port)
	})

	t.Run("a transport that fails falls back", func(t *testing.T) {
		addr, port := learnPinnedMachine("exit 1 #", "203.0.113.5", 22)
		assert.Empty(t, addr)
		assert.Zero(t, port)
	})
}

// The pinned machine's PORT has to travel with its address: behind a balancer the
// backend listens on a different port than the VIP is reached on, so pinning the
// machine without its port dials the wrong socket.
func TestPinnedPortOverridesTheConfiguredOne(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	defer SetSSHRelayBinaryForTest("/opt/af/af")()

	cmd, err := sshCommandPinnedTo(
		config.SSHConfig{Host: "vip.example", Port: 2200}, config.SSHHostKeyStrict, "10.0.0.9", 22)
	require.NoError(t, err)
	assert.Contains(t, proxyCommandValue(t, cmd), "ssh-relay '10.0.0.9' 22",
		"the relay must dial the MACHINE's port, not the VIP's")
	assert.Contains(t, cmd, "-p 2200",
		"while ssh still speaks to the NAME on its configured port, which is what keeps known_hosts and "+
			"certificate principals resolving as unpinned")
	fields := strings.Fields(cmd)
	assert.Equal(t, "'vip.example'", fields[len(fields)-1])

	// Zero keeps the configured port, which is every pin written before #3122 —
	// including a persisted handle from #3118.
	cmd, err = sshCommandPinnedTo(
		config.SSHConfig{Host: "vip.example", Port: 2200}, config.SSHHostKeyStrict, "10.0.0.9", 0)
	require.NoError(t, err)
	assert.Contains(t, proxyCommandValue(t, cmd), "ssh-relay '10.0.0.9' 2200")
}

// A cleanup handle must carry the machine across a daemon restart, which is the
// constraint that ruled out a ControlMaster multiplex: a live socket cannot be
// adopted by a fresh process, a recorded address can.
func TestRestoredHandleReapsTheMachineItRecorded(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	defer SetSSHRelayBinaryForTest("/opt/af/af")()

	raw, err := json.Marshal(&RuntimeCleanupData{SSH: &SSHRuntimeCleanupData{
		Config:              config.SSHConfig{Host: "vip.example", Port: 2200},
		SessionDir:          "/root/.af-sessions/x.AbCdEf",
		DialAddress:         "10.0.0.9",
		DialPort:            22,
		HostKeyVerification: config.SSHHostKeyStrict,
	}})
	require.NoError(t, err)
	var back RuntimeCleanupData
	require.NoError(t, json.Unmarshal(raw, &back))
	require.Equal(t, 22, back.SSH.DialPort, "the port must survive the round trip, or the reap dials the VIP")

	backend, _, err := restoreRuntimeCleanup("vip", "ssh", &back)
	require.NoError(t, err)
	sb, ok := backend.(*sshBackend)
	require.True(t, ok)
	assert.Contains(t, proxyCommandValue(t, sb.provisioner.sshCmd), "ssh-relay '10.0.0.9' 22",
		"a reap composed in a fresh daemon must reach the machine the workspace is on")

	// A handle from before #3122 has no port and must replay exactly as it did.
	older, _, err := restoreRuntimeCleanup("older", "ssh", &RuntimeCleanupData{SSH: &SSHRuntimeCleanupData{
		Config:              config.SSHConfig{Host: "h.example", Port: 2200},
		SessionDir:          "/root/.af-sessions/y.AbCdEf",
		DialAddress:         "198.51.100.8",
		HostKeyVerification: config.SSHHostKeyStrict,
	}})
	require.NoError(t, err)
	ob, ok := older.(*sshBackend)
	require.True(t, ok)
	assert.Contains(t, proxyCommandValue(t, ob.provisioner.sshCmd),
		"ssh-relay '198.51.100.8' "+strconv.Itoa(2200),
		"an older handle carries no port, so it keeps using the configured one")
}

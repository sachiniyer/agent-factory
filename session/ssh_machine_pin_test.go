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
	ip, port, ok := parseSSHConnectionServerAddr("172.17.0.1 49266 172.17.0.4 22")
	require.True(t, ok)
	assert.Equal(t, "172.17.0.4", ip.String())
	assert.Equal(t, 22, port)

	// Trailing newline is what a shell actually delivers.
	ip, port, ok = parseSSHConnectionServerAddr("10.0.0.2 5 10.0.0.9 2222\n")
	require.True(t, ok)
	assert.Equal(t, "10.0.0.9", ip.String())
	assert.Equal(t, 2222, port)

	// IPv6 round-trips through net.ParseIP's canonical spelling.
	ip, _, ok = parseSSHConnectionServerAddr("::1 5 2001:DB8::1 22")
	require.True(t, ok)
	assert.Equal(t, "2001:db8::1", ip.String())

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
	// A listener on a GLOBAL UNICAST address, because loopback is now refused as a
	// machine identity — see the loopback subtest below for why. This binds a real
	// interface address so "reachable" stays a fact rather than an assumption.
	host := globalUnicastLocalAddr(t)
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
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
		// The listener below is necessarily on a LOCAL address, which the guard
		// correctly refuses — so this one case pretends the address is somewhere else.
		// Everything the guard actually decides is asserted against the real
		// implementation in the subtest below.
		prev := daemonHoldsAddressFn
		daemonHoldsAddressFn = func(net.IP) bool { return false }
		defer func() { daemonHoldsAddressFn = prev }()

		addr, port := learnPinnedMachine(
			stubTransport(fmt.Sprintf("10.0.0.1 5 %s %d", host, reachablePort)), "198.51.100.7", 22)
		assert.Equal(t, host, addr)
		assert.Equal(t, reachablePort, port)
	})

	t.Run("the same address and port is no gain", func(t *testing.T) {
		// An ordinary host, and also what an IP-preserving (DSR) balancer reports:
		// re-pinning to it would change nothing.
		addr, port := learnPinnedMachine(
			stubTransport(fmt.Sprintf("10.0.0.1 5 %s %d", host, reachablePort)), host, reachablePort)
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

	t.Run("an address that means \"me\" is refused", func(t *testing.T) {
		// SSH_CONNECTION describes the socket the backend ACCEPTED, so a balancer that
		// reaches its backends over loopback makes every one of them report 127.0.0.1.
		// Evaluated from the daemon that address is the DAEMON'S OWN HOST, and the
		// reachability probe would cheerfully connect to its sshd — after which every
		// step, including the reap, would target the wrong machine entirely. A real
		// listener is bound here so the probe WOULD have succeeded, which is what makes
		// this test about the classification rather than about reachability.
		self, selfErr := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, selfErr)
		defer func() { _ = self.Close() }()
		go func() {
			for {
				c, acceptErr := self.Accept()
				if acceptErr != nil {
					return
				}
				_ = c.Close()
			}
		}()
		selfPort := self.Addr().(*net.TCPAddr).Port

		reported := map[string]string{
			"loopback":      fmt.Sprintf("10.0.0.1 5 127.0.0.1 %d", selfPort),
			"unspecified":   "10.0.0.1 5 0.0.0.0 22",
			"link-local":    "10.0.0.1 5 169.254.1.1 22",
			"IPv6 loopback": "10.0.0.1 5 ::1 22",
		}
		// A GLOBALLY UNICAST address this daemon itself holds is still "me".
		// IsGlobalUnicast accepts private fleet addressing, which is what makes it the
		// right predicate — and also means it accepts a 10.x/172.16.x address that may
		// be the daemon's own, where the reachability probe would succeed against the
		// wrong machine and shared keys would make it silent.
		reported["an address this daemon holds"] = fmt.Sprintf("10.0.0.1 5 %s 22", globalUnicastLocalAddr(t))
		for name, reported := range reported {
			t.Run(name, func(t *testing.T) {
				addr, port := learnPinnedMachine(stubTransport(reported), "203.0.113.5", 22)
				assert.Empty(t, addr, "%s is not a portable machine identity", name)
				assert.Zero(t, port)
			})
		}
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
		DialAddress:         "10.0.0.9:22",
		HostKeyVerification: config.SSHHostKeyStrict,
	}})
	require.NoError(t, err)
	var back RuntimeCleanupData
	require.NoError(t, json.Unmarshal(raw, &back))
	require.Equal(t, "10.0.0.9:22", back.SSH.DialAddress,
		"the port must survive the round trip, or the reap dials the VIP")

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

// globalUnicastLocalAddr returns an address of this host that is a portable
// machine identity, or skips. CI runners have one; a machine with only loopback
// cannot exercise the reachable case at all.
func globalUnicastLocalAddr(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	require.NoError(t, err)
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok || !ipNet.IP.IsGlobalUnicast() || ipNet.IP.To4() == nil {
			continue
		}
		return ipNet.IP.String()
	}
	t.Skip("this host has no globally-unicast IPv4 address, so the reachable case cannot be built")
	return ""
}

// A pinned machine must survive a daemon ROLLBACK — both the reading of it and
// the record itself.
//
// A release without machine-pinning DROPS any field it does not know on its next
// checkpoint, so a pin kept in a new field would be gone for good after one
// old-daemon restart, leaving no handle at all. The pin therefore lives in
// DialAddress, which that release round-trips intact, in a spelling it cannot
// misuse.
func TestPinnedMachineSurvivesADaemonRollback(t *testing.T) {
	t.Run("a differing port is spelled so an older release cannot misuse it", func(t *testing.T) {
		var c SSHRuntimeCleanupData
		sshRecordPinnedMachine(&c, "10.0.0.9", 22, 2200)
		assert.Equal(t, "10.0.0.9:22", c.DialAddress,
			"an older release appends the CONFIGURED port itself, so this becomes an unresolvable "+
				"[10.0.0.9:22]:2200, the relay cannot dial, ssh exits 255 and the record is RETAINED — "+
				"rather than reaching some other backend and retiring the tombstone")

		// It is the ONLY field, so nothing is lost when a release that does not
		// understand it re-serialises the record.
		round := roundTripThroughOlderDaemon(t, c)
		assert.Equal(t, "10.0.0.9:22", round.DialAddress, "the machine must survive an old daemon's checkpoint")

		addr, port := sshPinnedCleanupTarget(&round)
		assert.Equal(t, "10.0.0.9", addr)
		assert.Equal(t, 22, port)
	})

	t.Run("a matching port keeps the spelling an older release reads correctly", func(t *testing.T) {
		var c SSHRuntimeCleanupData
		sshRecordPinnedMachine(&c, "10.0.0.9", 2200, 2200)
		assert.Equal(t, "10.0.0.9", c.DialAddress,
			"here it appends the same port and reaches the same machine, so it keeps a usable pin")
		addr, port := sshPinnedCleanupTarget(&c)
		assert.Equal(t, "10.0.0.9", addr)
		assert.Zero(t, port, "zero means the configured port")
	})

	t.Run("an address pin from before #3122 is unchanged", func(t *testing.T) {
		var c SSHRuntimeCleanupData
		sshRecordPinnedMachine(&c, "198.51.100.8", 0, 2200)
		assert.Equal(t, "198.51.100.8", c.DialAddress)
		addr, port := sshPinnedCleanupTarget(&c)
		assert.Equal(t, "198.51.100.8", addr)
		assert.Zero(t, port)
	})

	t.Run("legacy bare addresses of BOTH families read as address pins", func(t *testing.T) {
		// SplitHostPort refuses both spellings, which is what makes the two forms
		// distinguishable without a version marker.
		for _, bare := range []string{"198.51.100.8", "2001:db8::1"} {
			addr, port := sshPinnedCleanupTarget(&SSHRuntimeCleanupData{DialAddress: bare})
			assert.Equal(t, bare, addr)
			assert.Zero(t, port)
		}
	})

	t.Run("an unusable pin still PINS rather than unpinning", func(t *testing.T) {
		// Falling back to a name-based command behind a multi-backend VIP is the very
		// leak this fixes: it would reap whichever backend answered and retire the
		// tombstone. Keeping the pin makes the dial fail and the record retry.
		for _, broken := range []string{"10.0.0.9:not-a-port", "10.0.0.9:0", ":22"} {
			addr, _ := sshPinnedCleanupTarget(&SSHRuntimeCleanupData{DialAddress: broken})
			assert.NotEmpty(t, addr, "%q must still produce a pin, so the reap fails closed", broken)
		}
	})
}

// roundTripThroughOlderDaemon models what a release without machine-pinning does
// to a record: it decodes into a struct lacking any newer field and re-encodes,
// silently dropping what it did not know.
func roundTripThroughOlderDaemon(t *testing.T, c SSHRuntimeCleanupData) SSHRuntimeCleanupData {
	t.Helper()
	type olderRecord struct {
		Config              config.SSHConfig `json:"config"`
		SessionDir          string           `json:"session_dir"`
		RemotePID           string           `json:"remote_pid,omitempty"`
		DialAddress         string           `json:"dial_address,omitempty"`
		HostKeyVerification string           `json:"host_key_verification,omitempty"`
	}
	raw, err := json.Marshal(&c)
	require.NoError(t, err)
	var older olderRecord
	require.NoError(t, json.Unmarshal(raw, &older))
	reserialised, err := json.Marshal(&older)
	require.NoError(t, err)
	var back SSHRuntimeCleanupData
	require.NoError(t, json.Unmarshal(reserialised, &back))
	return back
}

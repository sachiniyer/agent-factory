package session

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/sshrelay"
)

// #3086: every ssh transport step must reach ONE machine.
//
// The property under test is not "the string contains ProxyCommand" — it is that
// a target which GENUINELY resolves to two reachable addresses puts every step on
// one of them, and that its reap targets that same one after a daemon restart.
// So the fixture below is a real round-robin name over a real resolver, with a
// CONTROL that proves the fixture can actually split. A test that asserted only
// the composed string would pass just as happily against a pin that resolves to
// nothing.

// TestPinnedSSHDialAddressKeepsEveryStepOnOneMachine is the issue's acceptance
// criterion, driven through the mechanism.
//
// Two listeners on two loopback addresses stand in for two machines behind one
// name, and the name's DNS answers ROTATE — which is what a round-robin record,
// a load balancer, or a multi-A-record host does, and is why af cannot simply
// trust that consecutive resolutions agree.
func TestPinnedSSHDialAddressKeepsEveryStepOnOneMachine(t *testing.T) {
	machineA, machineB, port := twoMachinesBehindOneName(t)
	const name = "pinned-multi.test"
	useRotatingResolver(t, "127.0.0.1", "127.0.0.2")

	// THE CONTROL, and it is not decoration: it proves the fixture reproduces the
	// bug. Without it, "every pinned dial landed on one machine" would be equally
	// true of a fixture where only one machine was ever reachable, and the test
	// would assert nothing at all.
	hosts, err := net.LookupHost(name)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"127.0.0.1", "127.0.0.2"}, hosts,
		"the target must GENUINELY resolve to two addresses — that is the condition #3086 is about")
	for i := 0; i < 6; i++ {
		// The GREETING is read, not just the connection opened, and that is what
		// makes the counters below trustworthy rather than racy. net.Dial returns as
		// soon as the handshake completes, which can be BEFORE the listener's Accept
		// has run and incremented its counter — so a control dial's increment could
		// land after the reset() below and be miscounted as a pinned one. The
		// greeting is written after the increment, so receiving it orders the two.
		// Measured: without this the test failed 7 times in 25.
		require.NoError(t, greetingFrom(net.JoinHostPort(name, strconv.Itoa(port))),
			"both stand-in machines must be reachable by name")
	}
	require.NotZero(t, machineA.count(), "fixture is vacuous: machine A never saw a name-based dial")
	require.NotZero(t, machineB.count(), "fixture is vacuous: machine B never saw a name-based dial — "+
		"the name must resolve to two REACHABLE addresses and unpinned dials must be able to reach either, "+
		"or this test proves nothing")

	// Resolve ONCE, the way provisioning does.
	pinned := resolvePinnedSSHDialAddress(name, port)
	require.Contains(t, []string{"127.0.0.1", "127.0.0.2"}, pinned,
		"the pin must be one of the addresses the name actually answers with")
	chosen, other := machineA, machineB
	if pinned == "127.0.0.2" {
		chosen, other = machineB, machineA
	}

	// FLUSH BOTH ACCEPT LOOPS BEFORE ZEROING THE COUNTERS. The probe above opens a
	// real connection and closes it without reading, so unlike the control dials
	// there is nothing to order its listener's increment against — and an increment
	// that lands after reset() is counted as a pinned dial, which is precisely the
	// off-by-one that made this test fail 6 times in 30 with the counter reading 7.
	// Each listener accepts sequentially, so a connection whose greeting has been
	// received was accepted after everything already queued: receiving one from each
	// machine proves both loops are past the probe.
	for _, m := range []*standInMachine{machineA, machineB} {
		require.NoError(t, greetingFrom(net.JoinHostPort(m.addr, strconv.Itoa(port))))
	}
	machineA.reset()
	machineB.reset()

	// EVERY step of a session — each provision command, the `ssh -L` tunnel, and
	// the reap — is a separate ssh invocation that dials through this relay. Run it
	// the number of times a session would and prove they all land together.
	for i := 0; i < 6; i++ {
		require.NoError(t, relayOnce(pinned, port), "relay dial %d", i)
	}
	assert.Equal(t, 6, chosen.count(), "every pinned dial must reach the machine the session was pinned to")
	assert.Zero(t, other.count(),
		"a pinned session reached a SECOND machine — that is #3086 exactly: the workspace lands on one host "+
			"while the clone, the tunnel or the reap lands on another, and the reap then removes the wrong "+
			"directory and reports success while the real workspace leaks")

	// And the reap composed from the PERSISTED handle, in what is effectively a
	// fresh daemon, must target that same machine. This is the constraint that
	// ruled out a ControlMaster multiplex: a live master cannot survive a restart,
	// a recorded address can.
	backend, _, err := restoreRuntimeCleanup("pinned", "ssh", &RuntimeCleanupData{SSH: &SSHRuntimeCleanupData{
		Config:              config.SSHConfig{Host: name, Port: port},
		SessionDir:          "/home/af/.af-sessions/x.AbCdEf",
		DialAddress:         pinned,
		HostKeyVerification: config.SSHHostKeyInsecure,
	}})
	require.NoError(t, err)
	sb, ok := backend.(*sshBackend)
	require.True(t, ok)
	assert.Contains(t, proxyCommandValue(t, sb.provisioner.sshCmd),
		sshrelay.Subcommand+" '"+pinned+"' "+strconv.Itoa(port),
		"the restored teardown must dial the machine the session actually ran on")
}

// "Every step" is a structural claim, so it is pinned structurally.
//
// The leak in #3086 is not that one command dialled the wrong host — it is that
// the provision commands, the `ssh -L` tunnel and the reap are SEPARATE ssh
// invocations that could each land somewhere different. They cannot, for exactly
// one reason: all three are built from the single sshCmd string the pin lives in.
// A refactor that gave the tunnel or the reap its own composition would reopen the
// bug while every string assertion above still passed.
func TestEveryTransportStepRidesTheOnePinnedCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	defer SetSSHRelayBinaryForTest("/opt/af/af")()

	pinnedCmd, err := sshCommandPinnedTo(
		config.SSHConfig{Host: "many.example.com", Port: 2222}, config.SSHHostKeyInsecure, "198.51.100.8")
	require.NoError(t, err)
	p := newSSHSandboxProvisioner(ProvisionSpec{Title: "pinned"}, pinnedCmd, "", "")

	// Every provision step AND the reap run through Run → buildRunCommand.
	built := p.buildRunCommand(context.Background(), "echo probe", nil)
	assert.Contains(t, strings.Join(built.Args, " "), sshrelay.Subcommand,
		"the provision steps and the reap must run over the pinned command, not a freshly composed one")

	// The tunnel is the third invocation, and it is composed inside startTunnel —
	// which dials, so it cannot be called here. Asserted against the SOURCE, the way
	// the ExitOnForwardFailure guarantee next door is: what matters is that it
	// extends p.sshCmd rather than building its own.
	src, err := os.ReadFile("backend_sandbox.go")
	require.NoError(t, err)
	assert.Contains(t, string(src), `p.sshCmd+`+"`"+` -o ExitOnForwardFailure=yes -N -L "$1"`+"`",
		"startTunnel must extend the ONE pinned command; a tunnel that composed its own ssh invocation would "+
			"re-resolve the name and could forward to a different machine than the workspace is on (#3086)")
}

// The destination stays the NAME whether or not a session is pinned — that is the
// whole reason this approach survives where #3090 did not. ssh computes the
// known_hosts key and the certificate principal from the destination, and only
// ssh, given the name, computes both correctly.
func TestPinnedCommandKeepsTheNameAsSSHsDestination(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	defer SetSSHRelayBinaryForTest("/opt/af/af")()

	cmd, err := sshCommandPinnedTo(
		config.SSHConfig{Host: "many.example.com", Port: 2222}, config.SSHHostKeyStrict, "198.51.100.8")
	require.NoError(t, err)

	fields := strings.Fields(cmd)
	assert.Equal(t, "'many.example.com'", fields[len(fields)-1],
		"ssh's destination must remain the configured NAME: it is what keeps a host certificate valid, and "+
			"pinning the address instead is what #3090 had to revert")
	assert.NotContains(t, cmd, "HostKeyAlias",
		"no alias is needed when the destination is already the name — and no alias VALUE is correct, since a "+
			"plain known_hosts entry on a non-default port wants [name]:port while a certificate principal "+
			"wants the bare name")
	assert.Contains(t, cmd, "-o ProxyCommand='"+"'\\''/opt/af/af'\\''"+" "+sshrelay.Subcommand+" '\\''198.51.100.8'\\'' 2222'",
		"the pin belongs in the ProxyCommand, which decides only where the socket goes")
}

// An unpinned command must be exactly what it always was. A session af could not
// resolve, and a handle written before any of this, both take this path.
func TestUnpinnedCommandCarriesNoProxyCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cmd, err := sshCommandPinnedTo(config.SSHConfig{Host: "h.example.com", Port: 2222}, config.SSHHostKeyStrict, "")
	require.NoError(t, err)
	// "-o ProxyCommand", not the bare word, for the reason
	// TestSSHCommandOmitsThePost85HostKeyHelperOption records: t.TempDir() embeds
	// the TEST NAME, that path is interpolated into UserKnownHostsFile, and a test
	// with the option in its name therefore matches its own directory and fails
	// against correct code. Measured — this assertion was red before it was
	// narrowed.
	assert.NotContains(t, cmd, "-o ProxyCommand",
		"with no address to pin, af must compose the ordinary name-based command rather than a relay to nowhere")
	assert.Equal(t, "'h.example.com'", strings.Fields(cmd)[len(strings.Fields(cmd))-1])
}

// The ProxyCommand crosses TWO shells and one percent expansion, and every one of
// them can corrupt it silently.
func TestPinnedProxyCommandSurvivesQuotingAndPercentExpansion(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// A relay path with BOTH hazards: a space (which the shell splits) and a `%`
	// (which ssh percent-expands). Measured on OpenSSH_9.6p1: an unescaped `%d`
	// aborts the connection with `vdollar_percent_expand: unknown key %d`.
	defer SetSSHRelayBinaryForTest("/opt/af home/pct%dir/af")()
	cmd, err := sshCommandPinnedTo(config.SSHConfig{Host: "h.example.com"}, config.SSHHostKeyStrict, "198.51.100.8")
	require.NoError(t, err)

	assert.Contains(t, cmd, "pct%%dir",
		"a literal %% in the relay path must be doubled, or ssh reads it as a token and refuses to connect")
	assert.NotContains(t, cmd, "%h", "the address is fixed; %%h/%%p would expand to what ssh is dialling, "+
		"which is the name — the exact thing the pin exists to bypass")

	// The value af emits, unwrapped one shell at a time, must still name the relay
	// as a single argument. This is the assertion that fails if either quoting
	// layer is dropped.
	value := proxyCommandValue(t, cmd)
	assert.Equal(t, `'/opt/af home/pct%%dir/af' `+sshrelay.Subcommand+` '198.51.100.8' 22`, value)
	assert.Equal(t, `'/opt/af home/pct%dir/af' `+sshrelay.Subcommand+` '198.51.100.8' 22`,
		strings.ReplaceAll(value, "%%", "%"),
		"after ssh's percent expansion the relay path must still be one quoted argument")
}

// A zoned IPv6 link-local address is two hazards at once: the zone is part of the
// address's identity, and its separator is the character ssh expands.
func TestPinnedProxyCommandCarriesAZonedIPv6Address(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	defer SetSSHRelayBinaryForTest("/opt/af/af")()

	cmd, err := sshCommandPinnedTo(config.SSHConfig{Host: "h.example.com"}, config.SSHHostKeyStrict, "fe80::1%eth0")
	require.NoError(t, err)
	assert.Contains(t, proxyCommandValue(t, cmd), "'fe80::1%%eth0'",
		"fe80::1 on eth0 is not fe80::1 on eth1, so the zone travels with the address — escaped, because "+
			"ssh would otherwise expand %%e")

	// And the relay brackets it back into a dialable address rather than leaving
	// the caller to remember.
	assert.Equal(t, "[fe80::1%eth0]:22", net.JoinHostPort("fe80::1%eth0", "22"))
}

// A pin af cannot compose must not quietly become a name-based command. Reaching
// that code means the session IS on a known machine; dialling the name instead
// could reap a different one, report success and retire the only tombstone.
func TestUncomposablePinRetainsTheRecordRatherThanDiallingTheName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	prev := sshRelayBinary
	sshRelayBinary = func() (string, error) { return "", fmt.Errorf("no /proc/self/exe") }
	defer func() { sshRelayBinary = prev }()

	_, _, err := restoreRuntimeCleanup("pinned", "ssh", &RuntimeCleanupData{SSH: &SSHRuntimeCleanupData{
		Config:              config.SSHConfig{Host: "many.example.com", Port: 2222},
		SessionDir:          "/home/af/.af-sessions/x.AbCdEf",
		DialAddress:         "198.51.100.8",
		HostKeyVerification: config.SSHHostKeyStrict,
	}})
	require.Error(t, err, "restoreRuntimeCleanup must report the handle as unavailable, which RETAINS and "+
		"retries it — silently reaping by name could remove a different machine's directory and retire the "+
		"tombstone for good")

	// The same failure must NOT block a handle with nothing to pin: that one has
	// always dialled the name, and refusing it would leak the workspace it exists
	// to remove.
	_, _, err = restoreRuntimeCleanup("ordinary", "ssh", &RuntimeCleanupData{SSH: &SSHRuntimeCleanupData{
		Config:              config.SSHConfig{Host: "h.example.com", Port: 2222},
		SessionDir:          "/home/af/.af-sessions/x.AbCdEf",
		HostKeyVerification: config.SSHHostKeyStrict,
	}})
	require.NoError(t, err)
}

// An address af could not settle on degrades to today's behaviour rather than to
// a dead backend — #3092's lesson, with a new cause.
func TestUnresolvableHostFallsBackToDiallingTheName(t *testing.T) {
	assert.Empty(t, resolvePinnedSSHDialAddress("no-such-host.invalid", 22),
		"a probe that cannot answer must report no pin, so the composer emits the name-based command af has "+
			"always emitted")
}

// A pin af cannot RUN must cost the pin, not the session.
//
// This is the failure #3118 hit in CI before it shipped, and it is worth stating
// precisely because it is the exact shape of #3092. A ProxyCommand that cannot
// exec does not degrade — ssh gets no transport at all, so every step dies on its
// 30s deadline with a timeout naming neither the relay nor the cause. An unpinned
// session in the same situation would simply have worked. So the create path
// checks first and declines to pin.
//
// The reverse asymmetry is asserted by
// TestUncomposablePinRetainsTheRecordRatherThanDiallingTheName: on TEARDOWN the
// same condition must fail LOUD, because a record already knows which machine
// holds its workspace and reaping a different one retires the tombstone for good.
func TestAnUnusableRelayBinaryFallsBackToDiallingTheName(t *testing.T) {
	// A listener that certainly answers, so the ONLY thing under test is the relay
	// check — not whether the address resolved.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	// A usable relay pins.
	usable := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.WriteFile(usable, []byte("#!/bin/sh\n"), 0o755))
	restore := SetSSHRelayBinaryForTest(usable)
	assert.Equal(t, "127.0.0.1", pinForProvision("127.0.0.1", port),
		"an executable relay must pin, or this test proves nothing about the cases below")
	restore()

	dir := t.TempDir()
	notThere := filepath.Join(dir, "deleted-af")
	notExecutable := filepath.Join(dir, "not-executable")
	require.NoError(t, os.WriteFile(notExecutable, []byte("#!/bin/sh\n"), 0o644))

	for name, path := range map[string]string{
		// os.Executable trims Linux's " (deleted)" suffix, so a daemon whose binary
		// was replaced in place still names a real file. One deleted outright does not.
		"a binary that is gone":   notThere,
		"a binary that is not +x": notExecutable,
		"a directory, not a file": dir,
	} {
		t.Run(name, func(t *testing.T) {
			defer SetSSHRelayBinaryForTest(path)()
			assert.Empty(t, pinForProvision("127.0.0.1", port),
				"%s must cost the PIN, not the whole session: a ProxyCommand that cannot exec leaves ssh with "+
					"no transport, so every step times out where an unpinned session would have connected", name)
		})
	}

	// And a relay af cannot even name at all.
	prev := sshRelayBinary
	sshRelayBinary = func() (string, error) { return "", fmt.Errorf("no /proc/self/exe") }
	defer func() { sshRelayBinary = prev }()
	assert.Empty(t, pinForProvision("127.0.0.1", port))
}

// The probe is BOUNDED, because it is the one part of this that runs in-process.
//
// sshRuntime.Provision carries no context, so an unbounded dial to an address that
// silently drops packets would hold a session create for the kernel's SYN-retry
// window — well past the 30s deadline the first ssh step would have failed at.
// 198.51.100.0/24 is TEST-NET-2 (RFC 5737): reserved for documentation, routed
// nowhere, so a connect to it hangs rather than being refused, which is exactly
// the shape this bounds.
func TestTheDialProbeIsBounded(t *testing.T) {
	// The SHIPPED value, asserted separately from the behaviour, so shrinking the
	// timeout below cannot quietly become the only thing this test checks.
	assert.Equal(t, sshDialTimeout, sshDialProbeTimeout,
		"the probe must ship with a real deadline, not a test's")

	// Now prove the deadline is APPLIED. A probe with no timeout at all would
	// ignore this and hang, so the short value tests the mechanism rather than
	// standing in for it — and it costs milliseconds instead of the full 20s.
	prev := sshDialProbeTimeout
	sshDialProbeTimeout = 300 * time.Millisecond
	defer func() { sshDialProbeTimeout = prev }()

	// NEITHER CASE MAY BE A REFUSED CONNECTION. ECONNREFUSED returns instantly
	// whether or not a deadline exists, so a test built on one passes against an
	// unbounded probe — it would assert nothing. Both cases below STALL.

	// A stalled RESOLVER, which is deterministic and entirely in-process: the DNS
	// server reads the query and never answers. Go's Dialer deadline covers name
	// resolution as well as the connect, so this is the half a black-holed address
	// cannot reach. Without a deadline Go's own resolver still gives up eventually
	// (~2 attempts x 5s), which is why the assertion is "promptly", not "at all".
	t.Run("a stalled resolver", func(t *testing.T) {
		useStallingResolver(t)
		start := time.Now()
		assert.Empty(t, mustReturnWithin(t, 60*time.Second, func() string {
			return resolvePinnedSSHDialAddress("stalled.test", 22)
		}))
		assert.Less(t, time.Since(start), 3*time.Second,
			"the probe must give up on ITS OWN deadline; without one it waits out the resolver's retries")
	})

	// A black-holed ADDRESS: TEST-NET-2 (RFC 5737) is reserved for documentation and
	// routed nowhere, so a SYN to it goes unanswered. Not every network agrees —
	// some return an ICMP unreachable, which arrives instantly — so the precondition
	// is checked and the case SKIPS rather than passing vacuously.
	t.Run("a black-holed address", func(t *testing.T) {
		const blackhole = "198.51.100.9:22"
		probe, err := net.DialTimeout("tcp", blackhole, 750*time.Millisecond)
		if probe != nil {
			_ = probe.Close()
		}
		if err == nil || !errors.Is(err, os.ErrDeadlineExceeded) {
			var netErr net.Error
			if !errors.As(err, &netErr) || !netErr.Timeout() {
				t.Skipf("%s is not black-holed on this network (%v); a refused address returns instantly "+
					"and would make this assertion vacuous", blackhole, err)
			}
		}
		start := time.Now()
		assert.Empty(t, mustReturnWithin(t, 60*time.Second, func() string {
			return resolvePinnedSSHDialAddress("198.51.100.9", 22)
		}))
		assert.Less(t, time.Since(start), 20*time.Second,
			"the probe must give up on ITS OWN deadline rather than the kernel's SYN-retry window")
	})
}

// mustReturnWithin fails loudly instead of hanging the suite, because "blocks a
// session create forever" is the defect under test and a timed-out `go test` would
// report it as an unrelated package timeout.
func mustReturnWithin(t *testing.T, limit time.Duration, fn func() string) string {
	t.Helper()
	done := make(chan string, 1)
	go func() { done <- fn() }()
	select {
	case v := <-done:
		return v
	case <-time.After(limit):
		t.Fatal("resolvePinnedSSHDialAddress ignored its deadline: an unbounded probe blocks a session create, " +
			"where an unpinned create fails at the first ssh step's own 30s deadline. It runs IN-PROCESS, so " +
			"unlike the relay there is no process group for a caller to kill")
		return ""
	}
}

// useStallingResolver points the process resolver at a DNS server that reads every
// query and answers none — the "stalled resolver" half of the probe's deadline,
// which no choice of address can exercise.
func useStallingResolver(t *testing.T) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() {
		buf := make([]byte, 512)
		for {
			if _, _, readErr := pc.ReadFrom(buf); readErr != nil {
				return
			}
			// Deliberately no reply.
		}
	}()

	prevResolver := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "udp", pc.LocalAddr().String())
		},
	}
	t.Cleanup(func() {
		net.DefaultResolver = prevResolver
		_ = pc.Close()
	})
}

// proxyCommandValue extracts the ProxyCommand value from a composed command as
// the outer `sh -c` would hand it to ssh: unwrapped one layer, still carrying the
// inner quoting and the doubled percents ssh has yet to expand.
func proxyCommandValue(t *testing.T, cmd string) string {
	t.Helper()
	const marker = "-o ProxyCommand='"
	i := strings.Index(cmd, marker)
	require.GreaterOrEqual(t, i, 0, "no ProxyCommand in %q", cmd)
	rest := cmd[i+len(marker):]
	// shellQuoteSandbox spells an embedded quote '\'' — undo that, then stop at the
	// closing quote.
	rest = strings.ReplaceAll(rest, `'\''`, "\x00")
	end := strings.Index(rest, "'")
	require.GreaterOrEqual(t, end, 0, "unterminated ProxyCommand in %q", cmd)
	return strings.ReplaceAll(rest[:end], "\x00", "'")
}

// relayOnce drives the production relay for one connection and reads the far
// side's greeting back, so a dial that connects but relays nothing still fails —
// and so the listener's counter is settled before the caller looks at it.
func relayOnce(host string, port int) error {
	var out strings.Builder
	if err := sshrelay.Run(host, strconv.Itoa(port), strings.NewReader(""), &out); err != nil {
		return err
	}
	if out.String() == "" {
		return fmt.Errorf("relay connected but returned no bytes")
	}
	return nil
}

// greetingFrom opens a plain connection and waits for the stand-in machine to
// announce itself, which is the point at which that machine has certainly counted
// the connection.
func greetingFrom(addr string) error {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	buf := make([]byte, 64)
	if _, err := conn.Read(buf); err != nil {
		return err
	}
	return nil
}

// standInMachine is one of the two hosts behind the round-robin name. It greets
// every connection so a relayed dial can be told apart from a bare TCP connect.
type standInMachine struct {
	addr string
	ln   net.Listener
	mu   sync.Mutex
	n    int
}

func (m *standInMachine) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.n
}

func (m *standInMachine) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.n = 0
}

func (m *standInMachine) serve() {
	for {
		conn, err := m.ln.Accept()
		if err != nil {
			return
		}
		m.mu.Lock()
		m.n++
		m.mu.Unlock()
		go func() {
			_, _ = conn.Write([]byte("on " + m.addr + "\n"))
			_ = conn.Close()
		}()
	}
}

// twoMachinesBehindOneName binds two loopback addresses to the SAME port, which
// is what makes them addressable as one name. 127.0.0.0/8 is entirely local on
// Linux, so no privileges and no external interface are involved.
func twoMachinesBehindOneName(t *testing.T) (*standInMachine, *standInMachine, int) {
	t.Helper()
	for attempt := 0; attempt < 8; attempt++ {
		lnB, err := net.Listen("tcp", "127.0.0.2:0")
		if err != nil {
			t.Skipf("this platform cannot bind a second loopback address (%v); the multi-address property "+
				"needs two REACHABLE addresses and asserting it against one would prove nothing", err)
		}
		port := lnB.Addr().(*net.TCPAddr).Port
		lnA, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			// The port is free on .2 but taken on .1; try another.
			_ = lnB.Close()
			continue
		}
		a := &standInMachine{addr: "127.0.0.1", ln: lnA}
		b := &standInMachine{addr: "127.0.0.2", ln: lnB}
		t.Cleanup(func() { _ = lnA.Close(); _ = lnB.Close() })
		go a.serve()
		go b.serve()
		return a, b, port
	}
	t.Skip("could not find a port free on both loopback addresses")
	return nil, nil, 0
}

// useRotatingResolver points the process resolver at an in-process DNS server
// that answers with the given addresses in a DIFFERENT ORDER each time.
//
// Rotation is the point, not a flourish: a name whose answers are stable would
// make consecutive unpinned dials agree by luck, and the control above would stop
// distinguishing a working pin from no pin at all. Round-robin DNS and load
// balancers genuinely rotate, which is exactly why af cannot assume two
// resolutions of one name reach the same machine.
//
// `localhost` is deliberately NOT used for this: on Debian and Ubuntu it maps to
// 127.0.0.1 alone (::1 is ip6-localhost), so a localhost-based fixture would be
// single-address and this test would pass while proving nothing.
func useRotatingResolver(t *testing.T, addrs ...string) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)

	var turn int
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, readErr := pc.ReadFrom(buf)
			if readErr != nil {
				return
			}
			rotated := make([]string, 0, len(addrs))
			for i := range addrs {
				rotated = append(rotated, addrs[(i+turn)%len(addrs)])
			}
			reply, answeredA := dnsReply(buf[:n], rotated)
			// ROTATE PER LOOKUP, NOT PER QUERY. Go resolves a name by sending the A
			// and AAAA queries CONCURRENTLY, so a counter advanced on every packet
			// depends on which of the two happens to arrive first — measured: the
			// order came back constant for four straight lookups, then a dial landed
			// on the other machine out of nowhere. Advancing only on the query whose
			// answer this fixture actually varies makes each lookup see the next
			// rotation, deterministically.
			if answeredA {
				turn++
			}
			if reply != nil {
				_, _ = pc.WriteTo(reply, from)
			}
		}
	}()

	prev := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "udp", pc.LocalAddr().String())
		},
	}
	t.Cleanup(func() {
		net.DefaultResolver = prev
		_ = pc.Close()
	})
}

// dnsMinHeader is the fixed-size prefix of a DNS message: id, flags, and the four
// section counts.
const dnsMinHeader = 12

// dnsReply answers an A query with the given addresses and an AAAA query with an
// empty NOERROR, which is what stops Go's parallel A/AAAA lookup from failing.
//
// It answers whatever name is asked. The resolver is redirected here for one test
// only, and matching on the name would make the fixture depend on resolv.conf's
// ndots and search list — settings that vary per machine and per container, and
// that decide only whether Go asks for `n.test` or `n.test.some.search.domain`
// first.
func dnsReply(query []byte, addrs []string) (reply []byte, answeredA bool) {
	if len(query) < dnsMinHeader {
		return nil, false
	}
	// Walk the QNAME labels to find where the question ends.
	i := dnsMinHeader
	for i < len(query) && query[i] != 0 {
		i += int(query[i]) + 1
	}
	if i+5 > len(query) {
		return nil, false
	}
	qEnd := i + 5 // the zero label byte, then QTYPE and QCLASS
	qType := binary.BigEndian.Uint16(query[i+1 : i+3])

	reply = make([]byte, qEnd)
	copy(reply, query[:qEnd])
	binary.BigEndian.PutUint16(reply[2:4], 0x8180) // response, recursion available
	binary.BigEndian.PutUint16(reply[4:6], 1)      // one question, echoed back

	// Anything that is not an A query gets NOERROR with no answers. For AAAA that
	// is what settles the other half of Go's parallel lookup instead of leaving it
	// to time out.
	const typeA = 1
	if qType != typeA {
		return reply, false
	}
	answers := 0
	for _, a := range addrs {
		ip := net.ParseIP(a).To4()
		if ip == nil {
			continue
		}
		rr := make([]byte, 0, 16)
		rr = append(rr, 0xC0, 0x0C) // a pointer to the question's name
		rr = binary.BigEndian.AppendUint16(rr, typeA)
		rr = binary.BigEndian.AppendUint16(rr, 1) // class IN
		rr = binary.BigEndian.AppendUint32(rr, 0) // TTL 0: never cached anywhere
		rr = binary.BigEndian.AppendUint16(rr, 4)
		rr = append(rr, ip...)
		reply = append(reply, rr...)
		answers++
	}
	binary.BigEndian.PutUint16(reply[6:8], uint16(answers))
	return reply, true
}

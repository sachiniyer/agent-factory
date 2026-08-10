package session

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// #3044: the ssh runtime and hook provisioning resolved the same "embedded port
// vs separate port" input in OPPOSITE directions — ssh let the embedded port
// win, hook let the field win — so one record reached different ports depending
// on which backend read it. In a feature whose justification was that af owns
// transport ONE way, that is the defect the abstraction existed to prevent.
//
// This asserts they AGREE by construction, by driving both real entry points
// with the same input. A "mirrors the ssh backend" comment would not have caught
// it and would not catch the next one (#2097).
func TestSSHAndHookResolveTheSameAddressIdentically(t *testing.T) {
	cases := []struct {
		name     string
		address  string
		port     int
		wantHost string
		wantPort int // 0 = unspecified; each backend then applies its own default
	}{
		{"embedded port only", "10.0.0.7:2222", 0, "10.0.0.7", 2222},
		{"separate port only", "10.0.0.7", 3333, "10.0.0.7", 3333},
		{"both, agreeing", "10.0.0.7:2222", 2222, "10.0.0.7", 2222},
		{"neither", "10.0.0.7", 0, "10.0.0.7", 0},
		{"bracketed ipv6 with port", "[::1]:2222", 0, "::1", 2222},
		{"bare ipv6 is not host:port", "::1", 0, "::1", 0},
		// A SERVICE NAME is what ssh.Dial already accepted via /etc/services, so
		// both backends must keep accepting it — refusing would break working
		// configs and, worse, strand their cleanup handles.
		{"service-name port", "10.0.0.7:ssh", 0, "10.0.0.7", 22},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The hook entry point.
			hookHost, hookPort, hookErr := hookProvisionHostPort(
				&hookProvisionRecord{Host: tc.address, Port: tc.port})
			require.NoError(t, hookErr)

			// The ssh entry point. Since #3052 that backend composes an ssh(1)
			// command rather than dialling in-process, so the resolved address is
			// asserted where it now lives — which is itself the convergence: there is
			// no second resolver left to disagree.
			t.Setenv("HOME", t.TempDir())
			sshCmd, sshErr := sshCommandPinnedTo(config.SSHConfig{Host: tc.address, Port: tc.port}, config.SSHHostKeyInsecure, "", 0)
			require.NoError(t, sshErr)
			sshHost := tc.wantHost

			assert.Equal(t, tc.wantHost, hookHost)
			assert.Equal(t, tc.wantPort, hookPort)
			assert.Equal(t, tc.wantHost, sshHost,
				"both backends must resolve the same address to the same host")

			// ssh substitutes its default for an unspecified port; that is the only
			// legitimate difference, and it is a DEFAULT, not a different rule.
			wantSSHPort := tc.wantPort
			if wantSSHPort == 0 {
				wantSSHPort = sshDefaultPort
			}
			assert.Contains(t, sshCmd, "-p "+strconv.Itoa(wantSSHPort),
				"the ssh backend must reach the same port the hook path resolved")
			assert.Contains(t, sshCmd, "'"+sshHost+"'", "and the same host")
		})
	}
}

// A genuine conflict is REFUSED by both, rather than silently ranked. Two
// different ports in one configuration means one of them is a mistake, and
// picking either sends the session somewhere nobody asked for.
func TestConflictingPortsAreRefusedByBothBackends(t *testing.T) {
	const address, port = "10.0.0.7:2222", 3333

	_, _, hookErr := hookProvisionHostPort(&hookProvisionRecord{Host: address, Port: port})
	require.Error(t, hookErr, "hook provisioning must refuse a conflicting address")
	assert.Contains(t, hookErr.Error(), "2222")
	assert.Contains(t, hookErr.Error(), "3333", "the error must name BOTH values so it is obvious which to delete")

	t.Setenv("HOME", t.TempDir())
	_, sshErr := sshCommandPinnedTo(config.SSHConfig{Host: address, Port: port}, config.SSHHostKeyInsecure, "", 0)
	require.Error(t, sshErr, "the ssh backend must refuse the same input, not silently prefer one")
	assert.Contains(t, sshErr.Error(), "2222")
	assert.Contains(t, sshErr.Error(), "3333")
}

// A mistyped port is named as a port problem rather than passed through to fail
// later as an address problem.
func TestMalformedEmbeddedPortIsRejected(t *testing.T) {
	for name, address := range map[string]string{
		"unknown service name": "10.0.0.7:definitelynotaservice",
		"out of range":         "10.0.0.7:99999",
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := resolveSSHHostPort(address, 0)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "port", "the error must name the port, not the address generally")
		})
	}
}

// Codex 3734417507 (P1): a cleanup handle persisted BEFORE conflicts were
// refused may embed one port and carry another. That handle is how af reaps a
// workspace it already provisioned, so refusing there protects nothing and leaks
// everything — the reap would fail before reaching the host on every retry, the
// tombstone would be retained forever, and the remote process and workspace
// would survive.
func TestLegacyConflictingCleanupHandleCanStillReap(t *testing.T) {
	data := &RuntimeCleanupData{SSH: &SSHRuntimeCleanupData{
		Config:     config.SSHConfig{Host: "10.0.0.7:2222", Port: 3333}, // written before #3044
		SessionDir: "/remote/.af-sessions/xyz",
		RemotePID:  "4242",
	}}

	backend, teardown, err := restoreRuntimeCleanup("legacy session", "ssh", data)
	require.NoError(t, err, "a teardown handle must never be refused for an ambiguous address — that leaks the workspace")
	require.NotNil(t, teardown)

	sb, ok := backend.(*sshBackend)
	require.True(t, ok)
	assert.Contains(t, sb.provisioner.sshCmd, "'10.0.0.7'",
		"the restored transport must resolve cleanly, not carry the conflict forward")
	assert.Contains(t, sb.provisioner.sshCmd, "-p 2222",
		"normalized with the precedence those configs were written against")
}

// Codex 3734417514, restated after #3052: the address is a pure configuration
// fact, so it must be diagnosed before anything local. Composing the command is
// now where that happens, and it fails on a conflict without touching the
// filesystem or the network.
func TestSSHAddressConflictIsDiagnosedBeforeAnythingLocal(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no keys, no known_hosts: a local check would fail too
	_, err := sshCommandPinnedTo(config.SSHConfig{Host: "10.0.0.7:2222", Port: 3333}, config.SSHHostKeyStrict, "", 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "2222", "the ADDRESS conflict must win over any local prerequisite failure")
	assert.Contains(t, err.Error(), "3333")
}

// Codex 3734417518: a backend whose address cannot resolve must not be offered.
func TestUnresolvableSSHAddressMakesTheBackendUnavailable(t *testing.T) {
	for name, sshCfg := range map[string]config.SSHConfig{
		"conflicting ports":    {Host: "10.0.0.7:2222", Port: 3333},
		"unknown service name": {Host: "10.0.0.7:definitelynotaservice"},
	} {
		t.Run(name, func(t *testing.T) {
			cfgCopy := sshCfg
			err := BackendConfigError(BackendSSH, &config.ResolvedConfig{SSH: &cfgCopy})
			require.Error(t, err, "the picker must not offer a backend whose every create fails in the resolver")
			assert.Contains(t, err.Error(), "backend=ssh")
		})
	}
}

// Codex 3734483402 (P1): a handle persisted before #3044 may spell its port as a
// SERVICE NAME ("server:ssh"). ssh.Dial resolved that through /etc/services, so
// such configs worked — and refusing them now would strand the cleanup handle,
// leaking the remote process and workspace on every retry.
//
// Broader than the reap path: those configs also worked at CREATE time, so
// rejecting a service name would have been a plain regression.
func TestLegacyServiceNamePortStillReaps(t *testing.T) {
	data := &RuntimeCleanupData{SSH: &SSHRuntimeCleanupData{
		Config:     config.SSHConfig{Host: "server.invalid:ssh"},
		SessionDir: "/remote/.af-sessions/xyz",
	}}

	backend, teardown, err := restoreRuntimeCleanup("legacy service name", "ssh", data)
	require.NoError(t, err, "a service-name port must not strand a teardown handle")
	require.NotNil(t, teardown)

	sb, ok := backend.(*sshBackend)
	require.True(t, ok)
	assert.Contains(t, sb.provisioner.sshCmd, "'server.invalid'")
	assert.Contains(t, sb.provisioner.sshCmd, "-p 22", "resolved through /etc/services, exactly as ssh.Dial did")
}

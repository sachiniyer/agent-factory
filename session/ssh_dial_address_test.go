package session

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// One resolved address for a session's whole lifetime (#3086).
//
// The bug is silent, so the tests have to assert the MECHANISM rather than an
// observable failure: there is no error to catch, no exception, no bad exit
// status. A hostname with two A records simply lets each step's own `ssh`
// invocation pick independently, and the session ends up spread over two
// machines that each behaved correctly. So what is pinned here is that the
// literal address reaches the composed command, that it is recorded in the
// cleanup handle, and that a failure to resolve is an ERROR rather than a quiet
// fallback to the name.

func fixedResolver(t *testing.T, addrs ...string) {
	t.Helper()
	out := make([]net.IPAddr, 0, len(addrs))
	for _, a := range addrs {
		ip := net.ParseIP(a)
		require.NotNil(t, ip, "fixture %q is not an IP", a)
		out = append(out, net.IPAddr{IP: ip})
	}
	restore := SetSSHAddressResolverForTest(func(context.Context, string) ([]net.IPAddr, error) {
		return out, nil
	})
	t.Cleanup(restore)
}

// THE CORE PIN. Two addresses come back; the composed command must carry ONE
// literal, because every step runs that command and each would otherwise resolve
// for itself.
func TestSSHCommandDialsTheResolvedLiteralAddress(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fixedResolver(t, "198.51.100.7", "198.51.100.8")

	addr, err := resolveSSHDialAddress("many.example.com")
	require.NoError(t, err)
	assert.Equal(t, "198.51.100.7", addr)

	cmd, err := sshCommandForConfig(config.SSHConfig{Host: "many.example.com"}, config.SSHHostKeyStrict, addr)
	require.NoError(t, err)

	// The DESTINATION is the last token, and it is the only place the difference
	// matters: the name still appears — correctly — as the HostKeyAlias.
	fields := strings.Fields(cmd)
	assert.Equal(t, "'198.51.100.7'", fields[len(fields)-1],
		"the destination must be the pinned literal, got %q", cmd)
	assert.False(t, strings.HasSuffix(cmd, "'many.example.com'"),
		"the NAME must not be the destination — that is what lets each step resolve differently")
}

// The host-key guarantee has to survive the pin. Dialling an address would
// otherwise key known_hosts by ADDRESS, invalidating every existing entry and,
// under accept-new, writing a fresh one per address.
//
// The alias must be the exact string OpenSSH would have computed on its own.
// Measured against OpenSSH_9.6p1: a plain connection on port 2201 records
// `[127.0.0.1]:2201`, while the same connection under `HostKeyAlias=real.example`
// records `real.example` — the port is NOT appended to an alias. So a bare-name
// alias would silently change the lookup key for every non-default-port user.
func TestSSHCommandKeepsTheHostKeyKeyedByName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	def, err := sshCommandForConfig(config.SSHConfig{Host: "h.example.com"}, config.SSHHostKeyStrict, "198.51.100.7")
	require.NoError(t, err)
	assert.Contains(t, def, "HostKeyAlias='h.example.com'",
		"on the default port known_hosts is keyed by the bare name")

	ported, err := sshCommandForConfig(
		config.SSHConfig{Host: "h.example.com", Port: 2222}, config.SSHHostKeyStrict, "198.51.100.7")
	require.NoError(t, err)
	assert.Contains(t, ported, "HostKeyAlias='[h.example.com]:2222'",
		"on a non-default port it is keyed by the BRACKETED form, and HostKeyAlias is used verbatim — "+
			"a bare-name alias here would silently orphan every existing entry")
}

// A resolution failure must be explicit. Falling back to the name would restore
// the exact behaviour this exists to remove, at the moment DNS is least
// trustworthy, and would do it invisibly.
func TestSSHProvisionRefusesWhenTheHostDoesNotResolve(t *testing.T) {
	restore := SetSSHAddressResolverForTest(func(context.Context, string) ([]net.IPAddr, error) {
		return nil, errors.New("no such host")
	})
	defer restore()

	_, err := resolveSSHDialAddress("gone.example.com")

	require.Error(t, err, "a name that does not resolve must fail, never fall back to itself")
	assert.Contains(t, err.Error(), "gone.example.com")
	assert.Contains(t, err.Error(), "backend=ssh")
}

// An empty answer is a failure too, and the one a naive implementation returns
// as a valid empty string.
func TestSSHResolutionRefusesAnEmptyAnswer(t *testing.T) {
	restore := SetSSHAddressResolverForTest(func(context.Context, string) ([]net.IPAddr, error) {
		return nil, nil
	})
	defer restore()

	_, err := resolveSSHDialAddress("empty.example.com")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no addresses")
}

// A literal host is returned unchanged and never looked up: there is nothing to
// pin, and resolving a name that is not one is how a working config breaks.
func TestSSHLiteralHostIsNotResolved(t *testing.T) {
	restore := SetSSHAddressResolverForTest(func(context.Context, string) ([]net.IPAddr, error) {
		t.Fatal("a literal address must not be sent to the resolver")
		return nil, nil
	})
	defer restore()

	for _, literal := range []string{"198.51.100.7", "::1", "2001:db8::1"} {
		got, err := resolveSSHDialAddress(literal)
		require.NoError(t, err)
		assert.Equal(t, literal, got)
	}
}

// The teardown must reach the machine the session is ON. Re-resolving at reap
// time can answer differently, and removing the wrong machine's directory
// reports success while leaking the real workspace forever.
func TestRestoredSSHCleanupDialsTheRecordedAddress(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	restore := SetSSHAddressResolverForTest(func(context.Context, string) ([]net.IPAddr, error) {
		t.Fatal("a handle that records its address must not consult the resolver again")
		return nil, nil
	})
	defer restore()

	data := &RuntimeCleanupData{SSH: &SSHRuntimeCleanupData{
		Config:              config.SSHConfig{Host: "many.example.com"},
		SessionDir:          "/home/af/.af-sessions/x.AbCdEf",
		DialAddress:         "198.51.100.8",
		HostKeyVerification: config.SSHHostKeyStrict,
	}}
	backend, _, err := restoreRuntimeCleanup("pinned", "ssh", data)
	require.NoError(t, err)

	sb, ok := backend.(*sshBackend)
	require.True(t, ok)
	assert.Contains(t, sb.provisioner.sshCmd, "'198.51.100.8'",
		"the reap must dial the address the session was provisioned on")
	assert.Contains(t, sb.provisioner.sshCmd, "HostKeyAlias='many.example.com'",
		"and still verify the host key under the configured name")
}

// A record written before #3086 carries no address. It resolves ONCE at restore —
// still one address for the handle's whole life, rather than a fresh answer per
// step — and the address it picks is the one every later step uses.
func TestLegacySSHCleanupResolvesOnceAtRestore(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	calls := 0
	restore := SetSSHAddressResolverForTest(func(context.Context, string) ([]net.IPAddr, error) {
		calls++
		return []net.IPAddr{{IP: net.ParseIP("203.0.113.5")}}, nil
	})
	defer restore()

	data := &RuntimeCleanupData{SSH: &SSHRuntimeCleanupData{
		Config:              config.SSHConfig{Host: "legacy.example.com"},
		SessionDir:          "/home/af/.af-sessions/x.AbCdEf",
		HostKeyVerification: config.SSHHostKeyStrict,
	}}
	backend, _, err := restoreRuntimeCleanup("legacy-pin", "ssh", data)
	require.NoError(t, err)

	sb, ok := backend.(*sshBackend)
	require.True(t, ok)
	assert.Equal(t, 1, calls, "exactly one lookup for the handle, not one per step")
	assert.Contains(t, sb.provisioner.sshCmd, "'203.0.113.5'")
}

// …and if a legacy record's host does not resolve at all, the teardown still
// composes. This is the ONE place a name fallback is right: there is no session
// being created that could land on the wrong host, and refusing a teardown leaks
// the workspace it exists to remove (the #3044 lesson). Provision refuses.
func TestLegacySSHCleanupStillComposesWhenResolutionFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	restore := SetSSHAddressResolverForTest(func(context.Context, string) ([]net.IPAddr, error) {
		return nil, errors.New("no such host")
	})
	defer restore()

	data := &RuntimeCleanupData{SSH: &SSHRuntimeCleanupData{
		Config:              config.SSHConfig{Host: "legacy.example.com"},
		SessionDir:          "/home/af/.af-sessions/x.AbCdEf",
		HostKeyVerification: config.SSHHostKeyStrict,
	}}
	backend, teardown, err := restoreRuntimeCleanup("legacy-unresolvable", "ssh", data)
	require.NoError(t, err, "a teardown must never be refused for an unresolvable name — that leaks the workspace")
	require.NotNil(t, teardown)

	sb, ok := backend.(*sshBackend)
	require.True(t, ok)
	assert.Contains(t, sb.provisioner.sshCmd, "'legacy.example.com'",
		"with no address available the name is the only thing left to dial")
}

// The recorded address must survive a storage round-trip, or a daemon restart
// puts the teardown right back to re-resolving — which is the failure this whole
// change exists to remove, reintroduced by a missing struct tag.
func TestSSHCleanupHandleCarriesTheDialAddressThroughStorage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	src := &RuntimeCleanupData{SSH: &SSHRuntimeCleanupData{
		Config:              config.SSHConfig{Host: "many.example.com"},
		SessionDir:          "/home/af/.af-sessions/x.AbCdEf",
		DialAddress:         "198.51.100.9",
		HostKeyVerification: config.SSHHostKeyStrict,
	}}

	assert.Equal(t, "198.51.100.9", src.clone().SSH.DialAddress, "clone must carry it")

	// Through the real serialization, which is what a daemon restart replays.
	raw, err := json.Marshal(src)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"dial_address":"198.51.100.9"`,
		"the address must be a PERSISTED field; without the tag a restart silently re-resolves")

	var back RuntimeCleanupData
	require.NoError(t, json.Unmarshal(raw, &back))

	restore := SetSSHAddressResolverForTest(func(context.Context, string) ([]net.IPAddr, error) {
		t.Fatal("a replayed handle carrying its address must not consult the resolver")
		return nil, nil
	})
	defer restore()

	backend, _, err := restoreRuntimeCleanup("replayed", "ssh", &back)
	require.NoError(t, err)
	sb, ok := backend.(*sshBackend)
	require.True(t, ok)
	assert.Contains(t, sb.provisioner.sshCmd, "'198.51.100.9'",
		"after a restart the teardown must still reach the machine the session is on")
}

// ssh.host may carry its port inline, and a resolver handed the whole
// "127.0.0.1:32770" string looks up a NAME that does not exist. This is the
// fixture that was missing: every unit test above used a bare host, so they all
// passed while TestSSHBackendRoundTrip and TestSSHBackendArchiveRestore — whose
// sshd containers are addressed exactly this way — failed against a real sshd.
//
// A bare-host fixture asserts the claim while checking something weaker. This one
// fails against the defect.
func TestSSHDialAddressSplitsAnEmbeddedPortBeforeResolving(t *testing.T) {
	asked := ""
	restore := SetSSHAddressResolverForTest(func(_ context.Context, host string) ([]net.IPAddr, error) {
		asked = host
		return []net.IPAddr{{IP: net.ParseIP("203.0.113.11")}}, nil
	})
	defer restore()

	got, err := resolveSSHDialAddressFor(config.SSHConfig{Host: "many.example.com:32770"})
	require.NoError(t, err)
	assert.Equal(t, "many.example.com", asked, "the PORT must be split off before the lookup")
	assert.Equal(t, "203.0.113.11", got)

	// The separate port field is equally fine, and a literal host with an inline
	// port must not be resolved at all — the round-trip tests' exact shape.
	restore2 := SetSSHAddressResolverForTest(func(context.Context, string) ([]net.IPAddr, error) {
		t.Fatal("a literal address must not be sent to the resolver, inline port or not")
		return nil, nil
	})
	defer restore2()
	literal, err := resolveSSHDialAddressFor(config.SSHConfig{Host: "127.0.0.1:32770"})
	require.NoError(t, err, "this is exactly how TestSSHBackendRoundTrip addresses its sshd container")
	assert.Equal(t, "127.0.0.1", literal)
}

package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const provisionKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIProvisionedHostKey"

// The record is one JSON object and nothing else — the #2862 contract, adopted
// from this contract's first line rather than dragged onto it later.
func TestParseHookProvisionRecordAcceptsOnlyTheRecord(t *testing.T) {
	good := `{"host":"10.0.0.7","user":"af","port":2222,"host_key":"` + provisionKey + `"}`
	record, _, violation := parseHookProvisionRecord(good + "\n")
	require.NotNil(t, record)
	require.Nil(t, violation)
	assert.Equal(t, "10.0.0.7", record.Host)
	assert.Equal(t, "af", record.User)
	assert.Equal(t, 2222, record.Port)

	for name, stdout := range map[string]string{
		"prose before":    "provisioning…\n" + good + "\n",
		"prose after":     good + "\nprovisioned\n",
		"a second record": good + "\n" + good + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			r, _, v := parseHookProvisionRecord(stdout)
			assert.Nil(t, r, "stdout carrying more than the record must yield none")
			require.NotNil(t, v, "and must report the violation so the error can quote it")
		})
	}
}

// host_key is REQUIRED and the parse is what makes that fail closed: a record
// without one never reaches the pinning code at all.
func TestParseHookProvisionRecordRequiresHostAndKey(t *testing.T) {
	for name, body := range map[string]string{
		"no host_key":    `{"host":"10.0.0.7"}`,
		"empty host_key": `{"host":"10.0.0.7","host_key":"  "}`,
		"no host":        `{"host_key":"` + provisionKey + `"}`,
		"unknown field":  `{"host":"10.0.0.7","host_key":"` + provisionKey + `","url":"http://x"}`,
	} {
		t.Run(name, func(t *testing.T) {
			record, sawJSON, violation := parseHookProvisionRecord(body + "\n")
			assert.Nil(t, record)
			assert.True(t, sawJSON, "it printed JSON, so the error must be about SHAPE not absence")
			assert.Nil(t, violation, "a wrong-shaped record is not a stdout violation")
		})
	}
}

// THE host-key answer. A machine created seconds ago has no known_hosts entry,
// and none of af's three postures can help: strict refuses, accept-new is TOFU
// where every session is a first contact, insecure invites the MITM that would
// see the bearer token. The script vouches for the key out of band, af pins it
// for this session, and verification becomes real rather than trust-on-sight.
func TestHookProvisionPinsExactlyTheKeyTheScriptVouchedFor(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hook-hosts", "sess")

	path, err := hookProvisionKnownHosts(dir, "10.0.0.7", 0, provisionKey)
	require.NoError(t, err)
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.7 "+provisionKey+"\n", string(body),
		"exactly one key, for exactly this host — that is what makes StrictHostKeyChecking a verification")

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

// A non-default port is part of the known_hosts identity and OpenSSH spells it
// "[host]:port". Getting it wrong fails as "unknown host", which sends the
// operator hunting for the wrong problem.
func TestHookProvisionKnownHostsUsesOpenSSHPortSyntax(t *testing.T) {
	dir := t.TempDir()
	path, err := hookProvisionKnownHosts(dir, "10.0.0.7", 2222, provisionKey)
	require.NoError(t, err)
	body, _ := os.ReadFile(path)
	assert.True(t, strings.HasPrefix(string(body), "[10.0.0.7]:2222 "), "got %q", string(body))

	// Port 22 is the default and must NOT be bracketed, or it would never match.
	plain, err := hookProvisionKnownHosts(t.TempDir(), "10.0.0.7", 22, provisionKey)
	require.NoError(t, err)
	body22, _ := os.ReadFile(plain)
	assert.True(t, strings.HasPrefix(string(body22), "10.0.0.7 "), "got %q", string(body22))
}

// The composed invocation is what makes the pin load-bearing. Each option is
// here for a reason and a regression in any of them silently weakens the check.
func TestHookProvisionSSHCommandPinsVerification(t *testing.T) {
	cmd := hookProvisionSSHCommand("/af/known_hosts", &hookProvisionRecord{
		Host: "10.0.0.7", User: "af", Port: 2222, HostKey: provisionKey,
	})

	assert.Contains(t, cmd, "UserKnownHostsFile='/af/known_hosts'", "verification must use the pinned file")
	assert.Contains(t, cmd, "StrictHostKeyChecking=yes", "with strict checking, or the pin decides nothing")
	assert.Contains(t, cmd, "GlobalKnownHostsFile=/dev/null",
		"or a system-wide entry for a recycled address could satisfy the check instead of our key")
	assert.Contains(t, cmd, "BatchMode=yes", "an unattended provision must fail rather than hang on a prompt")
	assert.Contains(t, cmd, "-p 2222")
	assert.Contains(t, cmd, "'af@10.0.0.7'")
}

// No user and no port is the common case and must not emit empty flags.
func TestHookProvisionSSHCommandMinimalRecord(t *testing.T) {
	cmd := hookProvisionSSHCommand("/af/kh", &hookProvisionRecord{Host: "h.invalid", HostKey: provisionKey})
	assert.NotContains(t, cmd, "-p ")
	assert.Contains(t, cmd, "'h.invalid'")
	assert.NotContains(t, cmd, "@")
}

// A record may spell the port either way; both must reach the same pin.
func TestHookProvisionHostPortAcceptsEitherSpelling(t *testing.T) {
	h, p := hookProvisionHostPort(&hookProvisionRecord{Host: "10.0.0.7:2222"})
	assert.Equal(t, "10.0.0.7", h)
	assert.Equal(t, 2222, p)

	h, p = hookProvisionHostPort(&hookProvisionRecord{Host: "10.0.0.7", Port: 2222})
	assert.Equal(t, "10.0.0.7", h)
	assert.Equal(t, 2222, p)

	h, p = hookProvisionHostPort(&hookProvisionRecord{Host: "10.0.0.7"})
	assert.Equal(t, "10.0.0.7", h)
	assert.Zero(t, p)
}

// The two contracts are alternatives. Setting both is refused rather than
// silently preferring one, because the other's absence would then look like a
// working config.
func TestHookProvisionAndLaunchAreMutuallyExclusive(t *testing.T) {
	h := newHookState(t, "exit 0", "")
	p := newHookProvisioner(h, "both contracts")
	p.hooks.ProvisionCmd = "./provision.sh"
	require.Error(t, p.hooks.Validate())
	assert.Contains(t, p.hooks.Validate().Error(), "alternatives, not layers")
}

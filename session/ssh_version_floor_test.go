package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// The three regressions #3061 shipped, each pinned by the property that would have
// caught it (#3092).
//
// The connecting theme is that a composed command is a COMPATIBILITY SURFACE. Every
// option costs a minimum OpenSSH version, and every option that changes how a host
// is identified costs a class of host-key configuration. Neither cost is visible at
// the call site, which is why they are asserted here instead.

// EVERY option the command emits, so a new one cannot be added without a reviewer
// pricing its version cost. Deliberately an ALLOWLIST rather than a ban on the one
// option that broke: `KnownHostsCommand=none` was itself defensive, and the next
// too-new option will not be that one.
//
// The floor these keep is OpenSSH 7.6 (2017) — `StrictHostKeyChecking=accept-new`,
// and only under that posture. Ubuntu 20.04 ships 8.2 and Debian 11 ships 8.4, both
// comfortably inside; `KnownHostsCommand` (8.5, March 2021) put them outside.
func TestSSHCommandEmitsOnlyLongEstablishedOptions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	// Options af may emit, each with the OpenSSH release that introduced it.
	allowed := map[string]string{
		"User":                  "1.2",
		"StrictHostKeyChecking": "1.2 (accept-new: 7.6)",
		"UserKnownHostsFile":    "1.2",
		"GlobalKnownHostsFile":  "1.2",
		"BatchMode":             "1.2",
		"ExitOnForwardFailure":  "4.4",
	}

	for _, posture := range []string{
		config.SSHHostKeyStrict, config.SSHHostKeyAcceptNew, config.SSHHostKeyInsecure, "unknown-future",
	} {
		cmd := sshCmdFor(t, config.SSHConfig{Host: "h.example.com", Port: 2222}, posture)
		fields := strings.Fields(cmd)
		for i, f := range fields {
			if f != "-o" || i+1 >= len(fields) {
				continue
			}
			name, _, _ := strings.Cut(strings.Trim(fields[i+1], "'"), "=")
			_, ok := allowed[name]
			assert.True(t, ok,
				"posture %q emits -o %s=…, which is not in the priced allowlist. Every option costs a MINIMUM "+
					"OpenSSH VERSION: `KnownHostsCommand` (8.5) made backend=ssh unusable on Ubuntu 20.04 and "+
					"Debian 11 because an unrecognised -o aborts option parsing rather than warning (#3092). "+
					"Add it here with the release that introduced it, or drop it.", posture, name)
		}
	}
}

// The option that actually broke it, named so the regression cannot come back
// under the allowlist's more general wording.
func TestSSHCommandOmitsThePost85HostKeyHelperOption(t *testing.T) {
	// NOT named after the option, and asserted as "-o <name>" rather than the bare
	// word: t.TempDir() embeds the TEST NAME in its path, and that path is
	// interpolated into UserKnownHostsFile — so a test called …KnownHostsCommand
	// matches its own directory and fails against correct code. Measured.
	t.Setenv("HOME", t.TempDir())
	cmd := sshCmdFor(t, config.SSHConfig{Host: "h.example.com"}, config.SSHHostKeyStrict)
	assert.NotContains(t, cmd, "-o KnownHostsCommand",
		"OpenSSH 8.5+, and redundant: -F none means no config file is read, so nothing can install "+
			"such a helper. It cost every <8.5 user the whole backend (#3092)")
}

// The connection is NAME-based. Dialling a literal forces a HostKeyAlias, and no
// alias value is correct: measured against a real sshd whose host key is certified
// for the principal `real.example`, `HostKeyAlias=[real.example]:2202` is REJECTED
// (the alias is the principal) while a PLAIN known_hosts entry on a non-default
// port requires exactly that bracketed form. One string cannot be both (#3092).
func TestSSHCommandDialsTheConfiguredNameNotAnAddress(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	for _, cfg := range []config.SSHConfig{
		{Host: "h.example.com"},
		{Host: "h.example.com", Port: 2222},
	} {
		cmd := sshCmdFor(t, cfg, config.SSHHostKeyStrict)
		fields := strings.Fields(cmd)
		assert.Equal(t, "'h.example.com'", fields[len(fields)-1],
			"ssh must resolve the NAME — that is what keeps host certificates valid and keeps the "+
				"dialer's try-each-address fallback (#3092)")
		assert.NotContains(t, cmd, "HostKeyAlias",
			"an alias is only needed when dialling an address, and no alias value satisfies both a "+
				"plain [host]:port entry and a certificate principal")
	}
}

// A typo in ssh.identity_file must REFUSE, not silently authenticate as somebody
// else. ssh treats `-i /missing` as a warning and falls through to agent/default
// keys, and IdentitiesOnly is deliberately off here, so the fallback is silent.
func TestSSHIdentityFileMustBeReadable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	missing := filepath.Join(t.TempDir(), "typo_id_ed25519")

	_, err := sshCommandForConfig(
		config.SSHConfig{Host: "h.example.com", IdentityFile: missing}, config.SSHHostKeyStrict)

	require.Error(t, err, "an unreadable identity file must fail the command, not warn inside ssh")
	assert.Contains(t, err.Error(), missing, "the operator needs to see WHICH path")
	assert.Contains(t, err.Error(), "backend=ssh")

	// …and a readable one still composes, so the check is the only thing this moved.
	present := filepath.Join(t.TempDir(), "id_ed25519")
	require.NoError(t, os.WriteFile(present, []byte("key"), 0o600))
	cmd, err := sshCommandForConfig(
		config.SSHConfig{Host: "h.example.com", IdentityFile: present}, config.SSHHostKeyStrict)
	require.NoError(t, err)
	assert.Contains(t, cmd, "-i '"+present+"'")
}

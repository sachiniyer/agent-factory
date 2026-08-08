package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

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
// else. ssh treats an unusable `-i` as a warning and falls through to
// agent/default keys, and IdentitiesOnly is deliberately off here.
//
// Asserted against verifySSHIdentityFile, NOT the composer: composition has to
// stay filesystem-pure because restoreRuntimeCleanup calls it while persisted
// handles are loading, and a check there captures a permanently dead cleanup the
// moment a key is briefly unavailable — the defect this file already fixed once
// for the accept-new store.
func TestSSHIdentityFileMustBeReadable(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "typo_id_ed25519")

	err := verifySSHIdentityFile(config.SSHConfig{Host: "h.example.com", IdentityFile: missing})
	require.Error(t, err, "an unreadable identity file must be refused")
	assert.Contains(t, err.Error(), missing, "the operator needs to see WHICH path")
	assert.Contains(t, err.Error(), "backend=ssh")

	// os.Stat SUCCEEDS for all three of these, and ssh can load none of them as a
	// key — so an existence check would pass them straight through to the silent
	// agent fallback this exists to prevent.
	asDir := filepath.Join(dir, "a-directory")
	require.NoError(t, os.Mkdir(asDir, 0o700))
	assert.Error(t, verifySSHIdentityFile(config.SSHConfig{IdentityFile: asDir}),
		"a directory is not a key, and os.Stat is happy with it")

	if os.Geteuid() != 0 {
		unreadable := filepath.Join(dir, "mode000")
		require.NoError(t, os.WriteFile(unreadable, []byte("k"), 0o000))
		assert.Error(t, verifySSHIdentityFile(config.SSHConfig{IdentityFile: unreadable}),
			"an existing but unreadable file passes os.Stat and still cannot be loaded")
	}

	// A NAMED PIPE is the one that decides HOW the check is written, not just what
	// it returns. os.Stat is happy with it and ssh cannot load it — but opening it
	// to find that out BLOCKS until some process opens the write end, and nothing
	// ever will. So this case fails an open-first implementation by HANGING rather
	// than by returning the wrong answer, and a bounded subtest is what turns that
	// into a red instead of a suite that never finishes.
	fifo := filepath.Join(dir, "a-fifo")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))
	done := make(chan error, 1)
	go func() { done <- verifySSHIdentityFile(config.SSHConfig{IdentityFile: fifo}) }()
	select {
	case err := <-done:
		assert.Error(t, err, "a named pipe is not a key, and os.Stat is happy with it")
	case <-time.After(10 * time.Second):
		t.Fatal("verifySSHIdentityFile BLOCKED on a named pipe: it must settle the file KIND with a " +
			"stat before opening, because opening a FIFO waits for a writer that never comes — a hung " +
			"provision is worse than the silent fallback this check exists to prevent")
	}

	// A real, readable, regular file passes — so the check moved nothing else.
	present := filepath.Join(dir, "id_ed25519")
	require.NoError(t, os.WriteFile(present, []byte("key"), 0o600))
	require.NoError(t, verifySSHIdentityFile(config.SSHConfig{IdentityFile: present}))

	// And an unset identity file is not an error: the agent is the intended path.
	require.NoError(t, verifySSHIdentityFile(config.SSHConfig{Host: "h.example.com"}))
}

// Composition must not touch the filesystem, whatever the identity file says.
func TestSSHCommandCompositionIgnoresAnUnreadableIdentity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	missing := filepath.Join(t.TempDir(), "gone_id_ed25519")

	cmd, err := sshCommandForConfig(
		config.SSHConfig{Host: "h.example.com", IdentityFile: missing}, config.SSHHostKeyStrict)

	require.NoError(t, err,
		"restoreRuntimeCleanup composes while loading persisted handles; refusing here would capture a "+
			"permanently dead teardown the moment a key is briefly unavailable")
	assert.Contains(t, cmd, "-i '"+missing+"'")
}

// A cleanup record written by the short-lived #3090 pinning knows the exact
// machine its workspace is on. Dropping that would let a multi-address name
// re-resolve to a DIFFERENT machine, remove nothing, report success and retire
// the only tombstone — a permanent leak. So it is still decoded and honoured.
func TestPinnedCleanupRecordStillDialsItsRecordedAddress(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	raw, err := json.Marshal(&RuntimeCleanupData{SSH: &SSHRuntimeCleanupData{
		Config:              config.SSHConfig{Host: "many.example.com", Port: 2222},
		SessionDir:          "/home/af/.af-sessions/x.AbCdEf",
		DialAddress:         "198.51.100.8",
		HostKeyVerification: config.SSHHostKeyStrict,
	}})
	require.NoError(t, err)
	var back RuntimeCleanupData
	require.NoError(t, json.Unmarshal(raw, &back))
	require.Equal(t, "198.51.100.8", back.SSH.DialAddress, "the field must still DECODE")

	backend, _, err := restoreRuntimeCleanup("pinned-legacy", "ssh", &back)
	require.NoError(t, err)
	sb, ok := backend.(*sshBackend)
	require.True(t, ok)

	fields := strings.Fields(sb.provisioner.sshCmd)
	assert.Equal(t, "'198.51.100.8'", fields[len(fields)-1],
		"the teardown must reach the machine the session actually ran on")
	assert.Contains(t, sb.provisioner.sshCmd, "HostKeyAlias='[many.example.com]:2222'",
		"and keep known_hosts keyed by name, which is what dialling an address requires")
}

// Every other record — before #3090 and after #3092 — dials the name, with no
// alias, because that is what keeps certificates and dialer fallback working.
func TestUnpinnedCleanupRecordDialsTheName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	backend, _, err := restoreRuntimeCleanup("ordinary", "ssh", &RuntimeCleanupData{SSH: &SSHRuntimeCleanupData{
		Config:              config.SSHConfig{Host: "h.example.com", Port: 2222},
		SessionDir:          "/home/af/.af-sessions/x.AbCdEf",
		HostKeyVerification: config.SSHHostKeyStrict,
	}})
	require.NoError(t, err)
	sb, ok := backend.(*sshBackend)
	require.True(t, ok)

	fields := strings.Fields(sb.provisioner.sshCmd)
	assert.Equal(t, "'h.example.com'", fields[len(fields)-1])
	assert.NotContains(t, sb.provisioner.sshCmd, "HostKeyAlias")
}

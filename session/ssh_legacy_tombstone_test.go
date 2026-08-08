package session

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// The retire path for a pre-#2704 ssh tombstone, rebuilt on the OpenSSH toolchain
// after #3052 deleted the x/crypto client that used to classify it.
//
// Every case below goes through restoreRuntimeCleanup rather than calling the
// wrapper directly, because "which records reach this at all" IS the guarantee:
// the branch must be unreachable for a handle that recorded a posture, and a
// unit test of the wrapper alone would pass while the wiring was wrong.
//
// The store fixture is a real ed25519 host key in a real known_hosts file, and
// each fixture asserts `ssh-keygen -F` finds it before the test relies on the
// tool's answer. A stub would let a lookup that never runs read as "absent",
// which is exactly the fabricated negative this file exists to prevent.

// requireSSHKeygen skips honestly rather than asserting something weaker on a
// machine without the toolchain the retire path is built on.
func requireSSHKeygen(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen is not installed; the legacy-tombstone retire path has nothing to ask")
	}
}

// writeKnownHostsFixture writes a known_hosts file containing one real host key
// recorded under entryName, and proves ssh-keygen can find it there.
func writeKnownHostsFixture(t *testing.T, entryName string) string {
	t.Helper()
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "hostkey")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", keyPath).CombinedOutput(); err != nil {
		t.Fatalf("generating a host key failed: %v: %s", err, out)
	}
	pub, err := os.ReadFile(keyPath + ".pub")
	require.NoError(t, err)
	fields := strings.Fields(string(pub))
	require.GreaterOrEqual(t, len(fields), 2, "public key %q has no type/base64 pair", pub)

	khPath := filepath.Join(dir, "known_hosts")
	require.NoError(t, os.WriteFile(khPath, []byte(entryName+" "+fields[0]+" "+fields[1]+"\n"), 0o600))

	// The fixture is only meaningful if the REAL tool reads it the way the retire
	// path will. Assert that here so a malformed line cannot masquerade as "absent".
	require.NoError(t, exec.Command("ssh-keygen", "-F", entryName, "-f", khPath).Run(),
		"fixture: ssh-keygen -F %q must find the entry it was just given", entryName)
	return khPath
}

// emptyKnownHostsFixture is a real, readable, EMPTY store — the honest "the host
// is not here" case. It is deliberately not a missing file: a missing file makes
// ssh-keygen exit 255, which is inconclusive, not absent.
func emptyKnownHostsFixture(t *testing.T) string {
	t.Helper()
	khPath := filepath.Join(t.TempDir(), "known_hosts")
	require.NoError(t, os.WriteFile(khPath, nil, 0o600))
	return khPath
}

// restoreSSHTombstone replays a persisted ssh cleanup handle through the real
// restore path and returns its teardown, with the remote command stubbed to the
// failure a legacy record actually hits: the host cannot be reached.
func restoreSSHTombstone(t *testing.T, posture, host string, port int, knownHosts string) func() error {
	t.Helper()
	data := &RuntimeCleanupData{SSH: &SSHRuntimeCleanupData{
		Config:              config.SSHConfig{Host: host, Port: port, KnownHosts: knownHosts},
		SessionDir:          "/home/af/.af-sessions/legacy.AbCdEf",
		HostKeyVerification: posture,
	}}
	backend, teardown, err := restoreRuntimeCleanup("legacy-tombstone", "ssh", data)
	require.NoError(t, err)
	require.NotNil(t, teardown)

	sb, ok := backend.(*sshBackend)
	require.True(t, ok, "restored backend is %T, want *sshBackend", backend)
	// Set the seam AFTER restore on purpose: Run reads it per call, and doing it
	// here proves the teardown closure the daemon holds is the one being driven.
	sb.provisioner.runCommandFn = func(time.Duration, string, io.Reader, bool) ([]byte, error) {
		return []byte("ssh: connect to host " + host + ": Connection refused"),
			errors.New("exit status 255")
	}
	return teardown
}

// A pre-#2704 record whose host is absent from the strict store can never be
// completed by retrying, so it must be retired rather than backed off forever.
func TestLegacySSHTombstoneRetiresWhenHostAbsentFromStrictStore(t *testing.T) {
	requireSSHKeygen(t)

	err := restoreSSHTombstone(t, "", "legacy.invalid", 0, emptyKnownHostsFixture(t))()

	require.Error(t, err)
	assert.True(t, CleanupHandleUnusable(err),
		"a legacy handle whose host is absent from the strict store must retire, got %v", err)
	assert.Contains(t, err.Error(), "legacy.invalid", "the operator needs to know WHICH host to add")
	assert.Contains(t, err.Error(), "#2704", "the message must name the change that made this record legacy")
}

// The same record, but the host IS in the store: the failure is something else
// (the host is down, the network is out), and that can heal. Keep retrying.
func TestLegacySSHTombstoneRetainsWhenHostIsInStrictStore(t *testing.T) {
	requireSSHKeygen(t)

	kh := writeKnownHostsFixture(t, "legacy.invalid")
	err := restoreSSHTombstone(t, "", "legacy.invalid", 0, kh)()

	require.Error(t, err)
	assert.False(t, CleanupHandleUnusable(err),
		"a host present in the strict store may still come back; retrying must continue, got %v", err)
}

// THE GUARD THAT KEEPS THIS LEGACY-ONLY. Any session created since #2704 records
// a posture — config defaults it to strict at parse time — so a handle that has
// one must never take this branch, however its store lookup would answer. This is
// the assertion that current `ssh.host` users are unaffected.
func TestSSHTombstoneWithRecordedPostureNeverRetires(t *testing.T) {
	requireSSHKeygen(t)

	for _, posture := range []string{config.SSHHostKeyStrict, config.SSHHostKeyAcceptNew, config.SSHHostKeyInsecure} {
		t.Run(posture, func(t *testing.T) {
			err := restoreSSHTombstone(t, posture, "current.invalid", 0, emptyKnownHostsFixture(t))()

			require.Error(t, err)
			assert.False(t, CleanupHandleUnusable(err),
				"posture %q was recorded, so the record is not legacy and must keep retrying, got %v", posture, err)
		})
	}
}

// A FAILED LOOKUP IS NOT AN ABSENCE. With no ssh-keygen on PATH there is no
// answer at all, and retiring on that would abandon a real cleanup obligation on
// a fabricated negative.
func TestLegacySSHTombstoneRetainsWhenSSHKeygenIsUnavailable(t *testing.T) {
	kh := emptyKnownHostsFixture(t)
	t.Setenv("PATH", t.TempDir()) // empty dir: exec.Command cannot resolve ssh-keygen

	err := restoreSSHTombstone(t, "", "legacy.invalid", 0, kh)()

	require.Error(t, err)
	assert.False(t, CleanupHandleUnusable(err),
		"no lookup ran, so nothing was proven absent; the record must be retained, got %v", err)
}

// ssh-keygen exits 255 — not 1 — when it cannot read the store at all. That is
// "could not tell", and it must be treated as such rather than as "not found".
func TestLegacySSHTombstoneRetainsWhenStoreIsUnreadable(t *testing.T) {
	requireSSHKeygen(t)

	missing := filepath.Join(t.TempDir(), "no-such-dir", "known_hosts")
	// Confirm the premise: this is the non-0/1 exit, not the absent exit.
	statusErr := exec.Command("ssh-keygen", "-F", "legacy.invalid", "-f", missing).Run()
	var exitErr *exec.ExitError
	require.True(t, errors.As(statusErr, &exitErr), "expected ssh-keygen to exit non-zero, got %v", statusErr)
	require.NotEqual(t, 1, exitErr.ExitCode(), "premise: an unreadable store must NOT look like a clean not-found")

	err := restoreSSHTombstone(t, "", "legacy.invalid", 0, missing)()

	require.Error(t, err)
	assert.False(t, CleanupHandleUnusable(err),
		"an unreadable store proves nothing about the host; the record must be retained, got %v", err)
}

// known_hosts keys a non-default port under [host]:port, so the probe must ask
// about that name — otherwise a perfectly present host reads as absent and a
// live cleanup obligation is retired.
func TestLegacySSHTombstoneUsesThePortedKnownHostsName(t *testing.T) {
	requireSSHKeygen(t)

	kh := writeKnownHostsFixture(t, "[legacy.invalid]:2222")
	err := restoreSSHTombstone(t, "", "legacy.invalid", 2222, kh)()

	require.Error(t, err)
	assert.False(t, CleanupHandleUnusable(err),
		"the host is recorded under its bracketed ported name and must be found there, got %v", err)
}

func TestKnownHostsLookupName(t *testing.T) {
	assert.Equal(t, "h.example", knownHostsLookupName("h.example", 0), "unset port means the default")
	assert.Equal(t, "h.example", knownHostsLookupName("h.example", sshDefaultPort), "22 is never bracketed")
	assert.Equal(t, "[h.example]:2222", knownHostsLookupName("h.example", 2222))
}

func TestLookupKnownHostClassifiesEachOutcome(t *testing.T) {
	requireSSHKeygen(t)

	present := writeKnownHostsFixture(t, "found.invalid")
	assert.Equal(t, knownHostsPresent, lookupKnownHost(present, "found.invalid"))
	assert.Equal(t, knownHostsAbsent, lookupKnownHost(present, "other.invalid"))
	assert.Equal(t, knownHostsInconclusive, lookupKnownHost(filepath.Join(t.TempDir(), "gone", "kh"), "found.invalid"))
}

package session

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// The #3086 pin against a REAL sshd, because the thing that killed the previous
// attempt is not expressible as a string assertion.
//
// #3090 pinned by making the resolved address ssh's destination and restoring the
// name with `-o HostKeyAlias`. That is correct-looking and wrong: OpenSSH uses the
// alias as BOTH the known_hosts lookup key AND the certificate principal, and on a
// non-default port those need different values — `[name]:port` for a plain entry,
// the bare `name` for a certificate. Only a live sshd holding a real host
// certificate can tell the two apart, which is why these tests start one.
//
// EVERY TEST HERE USES A `.invalid` HOST NAME, and that is load-bearing rather
// than cosmetic. RFC 6761 guarantees `.invalid` never resolves, so a connection
// that succeeds PROVES the ProxyCommand carried it — there is no address behind
// the name for ssh to fall back to. A resolvable fixture could pass with the pin
// doing nothing at all.

// TestPinnedSessionValidatesAHostCertificateOnANonDefaultPort is the #3090
// regression, with its old mechanism reproduced beside its fix.
func TestPinnedSessionValidatesAHostCertificateOnANonDefaultPort(t *testing.T) {
	lab := startSSHLab(t)
	defer SetSSHRelayBinaryForTest(afRelayBinary(t))()

	// known_hosts holds ONLY the CA line, so nothing but certificate validation can
	// satisfy this connection.
	knownHosts := lab.writeKnownHosts(t, "@cert-authority pinned.invalid "+lab.caPub)
	cfg := lab.sshConfig(knownHosts)

	pinnedCmd, err := sshCommandPinnedTo(cfg, config.SSHHostKeyStrict, "127.0.0.1")
	require.NoError(t, err)
	out, err := runOverTransport(pinnedCmd, "echo CERT_ACCEPTED")
	require.NoError(t, err, "a pinned session must still validate a host certificate.\nssh said:\n%s", out)
	assert.Contains(t, out, "CERT_ACCEPTED")

	// THE CONTROL: the mechanism #3090 shipped, reproduced verbatim against the same
	// sshd and the same certificate. It must FAIL — if it did not, this test would
	// not be pinning anything, because the fix and the bug would be
	// indistinguishable here.
	legacyCmd := hostKeyAliasPinnedCommand(t, cfg, config.SSHHostKeyStrict, "127.0.0.1")
	legacyOut, legacyErr := runOverTransport(legacyCmd, "echo SHOULD_NOT_HAPPEN")
	require.Error(t, legacyErr,
		"the HostKeyAlias mechanism #3090 shipped must still fail against a certified host on a non-default "+
			"port — that is the regression this approach exists to avoid, and a control that passes means the "+
			"fixture is not exercising certificates at all.\nssh said:\n%s", legacyOut)
	assert.Contains(t, legacyOut, "Host key verification failed",
		"and it must fail for the RECORDED reason (the alias is used as the certificate principal), not for "+
			"some unrelated setup problem")
}

// The other half of the bind #3090 could not satisfy: a PLAIN known_hosts entry on
// a non-default port, which OpenSSH keys as `[host]:port`. One mechanism now
// serves both shapes, because ssh computes the key from its own destination.
func TestPinnedSessionMatchesAPlainKnownHostsEntryOnANonDefaultPort(t *testing.T) {
	lab := startSSHLab(t)
	defer SetSSHRelayBinaryForTest(afRelayBinary(t))()

	entry := fmt.Sprintf("[pinned.invalid]:%d %s", lab.port, lab.hostPub)
	cfg := lab.sshConfig(lab.writeKnownHosts(t, entry))

	pinnedCmd, err := sshCommandPinnedTo(cfg, config.SSHHostKeyStrict, "127.0.0.1")
	require.NoError(t, err)
	out, err := runOverTransport(pinnedCmd, "echo PLAIN_ENTRY_OK")
	require.NoError(t, err, "ssh said:\n%s", out)
	assert.Contains(t, out, "PLAIN_ENTRY_OK")

	// And the key is looked up under the NAME, not the pinned address: an entry for
	// a different name must not satisfy it, however the socket got there.
	wrong := lab.sshConfig(lab.writeKnownHosts(t, fmt.Sprintf("[other.invalid]:%d %s", lab.port, lab.hostPub)))
	wrongCmd, err := sshCommandPinnedTo(wrong, config.SSHHostKeyStrict, "127.0.0.1")
	require.NoError(t, err)
	wrongOut, wrongErr := runOverTransport(wrongCmd, "echo SHOULD_NOT_HAPPEN")
	require.Error(t, wrongErr, "ssh said:\n%s", wrongOut)
	assert.Contains(t, wrongOut, "pinned.invalid",
		"the host must still be IDENTIFIED by the configured name — that is the constraint that ruled out "+
			"every address-based pin")
}

// The provision path streams af's own binary over ssh's stdin, and the remote
// `cat` finishes only on EOF. A relay that closed the whole connection there would
// lose the reply, and one that never closed the write half would hang forever.
func TestPinnedSessionStreamsStdinAndSeesEOF(t *testing.T) {
	lab := startSSHLab(t)
	defer SetSSHRelayBinaryForTest(afRelayBinary(t))()

	cfg := lab.sshConfig(lab.writeKnownHosts(t,
		fmt.Sprintf("[pinned.invalid]:%d %s", lab.port, lab.hostPub)))
	pinnedCmd, err := sshCommandPinnedTo(cfg, config.SSHHostKeyStrict, "127.0.0.1")
	require.NoError(t, err)

	// Big enough to cross many relay buffers, so a half-close bug shows up as a
	// short file rather than passing by luck on a payload that fits in one write.
	payload := strings.Repeat("af-binary-stand-in-0123456789\n", 40000)
	dst := filepath.Join(t.TempDir(), "streamed")
	p := newSSHSandboxProvisioner(ProvisionSpec{Title: "stream"}, pinnedCmd, "", "")
	out, err := p.Run(60*time.Second, "cat > "+shellQuoteSandbox(dst)+" && wc -c < "+shellQuoteSandbox(dst),
		strings.NewReader(payload), false)
	require.NoError(t, err, "ssh said:\n%s", out)
	assert.Equal(t, strconv.Itoa(len(payload)), strings.TrimSpace(string(out)),
		"the whole stream must reach the far side and the remote command must then see EOF")
}

// The pin's escape hatch, against REAL ssh output rather than an invented string.
//
// af resolves with Go's pure resolver and ssh resolves with getaddrinfo, so an
// nsswitch source Go does not implement (LDAP, sssd, mDNS) can make both succeed
// with DIFFERENT addresses — and af would have pinned somewhere ssh would never
// have gone. That fails CLOSED on host-key verification, so it is an availability
// bug rather than a security one: nothing wrong is trusted, but the backend stops
// working for exactly those users. The recovery is one unpinned retry, and this is
// the detector it turns on.
//
// Both branches are driven through the real transport, because the whole point is
// that the marker is OpenSSH's wording and not af's guess about it.
func TestHostKeyFailureIsDetectedFromRealSSHOutput(t *testing.T) {
	lab := startSSHLab(t)
	defer SetSSHRelayBinaryForTest(afRelayBinary(t))()

	t.Run("an unknown host key retries", func(t *testing.T) {
		// known_hosts holds an entry for a DIFFERENT name, so this host is unknown.
		cfg := lab.sshConfig(lab.writeKnownHosts(t,
			fmt.Sprintf("[other.invalid]:%d %s", lab.port, lab.hostPub)))
		cmd, err := sshCommandPinnedTo(cfg, config.SSHHostKeyStrict, "127.0.0.1")
		require.NoError(t, err)

		p := newSSHSandboxProvisioner(ProvisionSpec{Title: "unknown"}, cmd, "", "")
		// combined=false, which is what the FIRST provision step uses — so ssh's
		// diagnostic lands in exec.ExitError.Stderr rather than in the error text,
		// and a detector reading only err.Error() would miss it.
		_, runErr := p.Run(30*time.Second, "echo SHOULD_NOT_HAPPEN", nil, false)
		require.Error(t, runErr)
		assert.True(t, sshHostKeyVerificationFailed(runErr),
			"OpenSSH's own `Host key verification failed.` must be recognised through the real error chain; "+
				"got %v", runErr)
		assert.True(t, shouldRetryProvisionUnpinned("127.0.0.1", "", runErr),
			"a pinned attempt that never created a session dir must be retryable unpinned")
	})

	t.Run("a CHANGED host key does not retry", func(t *testing.T) {
		// A different key under the RIGHT name: ssh reports the identification as
		// changed, which is a security signal that must surface immediately rather
		// than be delayed by a retry that would fail the same way.
		other := filepath.Join(t.TempDir(), "impostor")
		out, genErr := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "impostor",
			"-f", other).CombinedOutput()
		require.NoError(t, genErr, "%s", out)
		impostor, readErr := os.ReadFile(other + ".pub")
		require.NoError(t, readErr)

		cfg := lab.sshConfig(lab.writeKnownHosts(t,
			fmt.Sprintf("[pinned.invalid]:%d %s", lab.port, strings.TrimSpace(string(impostor)))))
		cmd, err := sshCommandPinnedTo(cfg, config.SSHHostKeyStrict, "127.0.0.1")
		require.NoError(t, err)

		p := newSSHSandboxProvisioner(ProvisionSpec{Title: "changed"}, cmd, "", "")
		_, runErr := p.Run(30*time.Second, "echo SHOULD_NOT_HAPPEN", nil, false)
		require.Error(t, runErr)
		assert.False(t, sshHostKeyVerificationFailed(runErr),
			"a CHANGED identification must NOT be treated as the retryable case: it is a security signal, and "+
				"the unpinned attempt verifies the same name against the same store and fails identically")
		assert.False(t, shouldRetryProvisionUnpinned("127.0.0.1", "", runErr))
	})
}

// hostKeyAliasPinnedCommand reproduces the mechanism #3090 shipped and #3100
// reverted: dial the resolved address as ssh's DESTINATION, and restore the name
// as the host-key lookup key with an alias.
//
// It lives in the test rather than in production because it is the CONTROL, not a
// code path — the fix's whole claim is that this shape is unnecessary, and a claim
// with no failing counterpart is untested.
func hostKeyAliasPinnedCommand(t *testing.T, cfg config.SSHConfig, posture, dialAddr string) string {
	t.Helper()
	base, err := sshCommandPinnedTo(cfg, posture, "")
	require.NoError(t, err)
	host, port, err := resolveSSHHostPort(cfg.Host, cfg.Port)
	require.NoError(t, err)
	if port == 0 {
		port = sshDefaultPort
	}
	suffix := " " + shellQuoteSandbox(host)
	require.True(t, strings.HasSuffix(base, suffix))
	return strings.TrimSuffix(base, suffix) +
		" -o HostKeyAlias=" + shellQuoteSandbox(knownHostsLookupName(host, port)) +
		" " + shellQuoteSandbox(dialAddr)
}

// runOverTransport runs one command through the production transport — the same
// `sh -c '<sshCmd> "$@"'` composition every provision step and the reap use, so
// the two shells and the percent expansion are all exercised for real.
func runOverTransport(sshCmd, script string) (string, error) {
	p := newSSHSandboxProvisioner(ProvisionSpec{Title: "probe"}, sshCmd, "", "")
	out, err := p.Run(45*time.Second, script, nil, true)
	return string(out), err
}

// sshLab is a throwaway sshd on a high loopback port, with its own host key, its
// own CA, and its own client key. It never touches the system sshd, the operator's
// ~/.ssh, or any port but the one it bound.
type sshLab struct {
	dir     string
	port    int
	hostPub string
	caPub   string
	keyPath string
	user    string
}

func (l *sshLab) sshConfig(knownHosts string) config.SSHConfig {
	return config.SSHConfig{
		Host:         "pinned.invalid",
		Port:         l.port,
		User:         l.user,
		IdentityFile: l.keyPath,
		KnownHosts:   knownHosts,
	}
}

func (l *sshLab) writeKnownHosts(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "known_hosts")
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
	return path
}

func startSSHLab(t *testing.T) *sshLab {
	t.Helper()
	sshdBin := lookSSHD(t)
	for _, tool := range []string{"ssh", "ssh-keygen"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed; the certificate bind this test exists for cannot be checked without "+
				"a real OpenSSH client and server", tool)
		}
	}
	me, err := user.Current()
	require.NoError(t, err)

	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700))
	keygen := func(name, comment string, args ...string) {
		full := append([]string{"-q", "-t", "ed25519", "-N", "", "-C", comment, "-f", filepath.Join(dir, name)}, args...)
		out, genErr := exec.Command("ssh-keygen", full...).CombinedOutput()
		require.NoError(t, genErr, "ssh-keygen %v: %s", full, out)
	}
	keygen("host_key", "host")
	keygen("client_key", "client")
	keygen("ca_key", "ca")

	// A host CERTIFICATE for the bare principal `pinned.invalid` — the value ssh
	// derives from its own destination, and the one `HostKeyAlias=[name]:port`
	// cannot produce.
	signOut, err := exec.Command("ssh-keygen", "-q", "-s", filepath.Join(dir, "ca_key"),
		"-I", "af-test-hostcert", "-h", "-n", "pinned.invalid", "-V", "-5m:+52w",
		filepath.Join(dir, "host_key.pub")).CombinedOutput()
	require.NoError(t, err, "signing the host certificate: %s", signOut)

	read := func(name string) string {
		b, readErr := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, readErr)
		return strings.TrimSpace(string(b))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "authorized_keys"),
		[]byte(read("client_key.pub")+"\n"), 0o600))

	port := freeLoopbackPort(t)
	sshdConfig := filepath.Join(dir, "sshd_config")
	require.NoError(t, os.WriteFile(sshdConfig, []byte(strings.Join([]string{
		"Port " + strconv.Itoa(port),
		// LOOPBACK ONLY. This sshd accepts the invoking user's key, so it must not be
		// reachable from anywhere but this machine.
		"ListenAddress 127.0.0.1",
		"HostKey " + filepath.Join(dir, "host_key"),
		"HostCertificate " + filepath.Join(dir, "host_key-cert.pub"),
		"AuthorizedKeysFile " + filepath.Join(dir, "authorized_keys"),
		"PidFile " + filepath.Join(dir, "sshd.pid"),
		// t.TempDir() is under a world-searchable /tmp, which StrictModes rejects.
		"StrictModes no",
		"UsePAM no",
		"PasswordAuthentication no",
		"KbdInteractiveAuthentication no",
		"PermitRootLogin no",
	}, "\n")+"\n"), 0o600))

	logPath := filepath.Join(dir, "sshd.log")
	// No -D: sshd daemonizes and writes its pid file, which is what the cleanup
	// below kills. -e would send logs to a stderr that goes away with the fork.
	start := exec.Command(sshdBin, "-f", sshdConfig, "-E", logPath)
	if out, startErr := start.CombinedOutput(); startErr != nil {
		logs, _ := os.ReadFile(logPath)
		t.Skipf("cannot start a private sshd (%v): %s\n%s", startErr, out, logs)
	}
	t.Cleanup(func() {
		pid, readErr := os.ReadFile(filepath.Join(dir, "sshd.pid"))
		if readErr != nil {
			return
		}
		// Signal by the pid IT recorded, so nothing else on this box is touched.
		if n, convErr := strconv.Atoi(strings.TrimSpace(string(pid))); convErr == nil {
			if proc, findErr := os.FindProcess(n); findErr == nil {
				_ = proc.Kill()
				_, _ = proc.Wait()
			}
		}
	})
	waitForListener(t, port)

	return &sshLab{
		dir:     dir,
		port:    port,
		hostPub: read("host_key.pub"),
		caPub:   read("ca_key.pub"),
		keyPath: filepath.Join(dir, "client_key"),
		user:    me.Username,
	}
}

func lookSSHD(t *testing.T) string {
	t.Helper()
	if path, err := exec.LookPath("sshd"); err == nil {
		return path
	}
	// sshd usually lives in an sbin a non-root PATH omits.
	for _, candidate := range []string{"/usr/sbin/sshd", "/usr/local/sbin/sshd", "/sbin/sshd", "/opt/homebrew/sbin/sshd"} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	t.Skip("no sshd on this host; the certificate bind #3090 tripped over cannot be checked without a real server")
	return ""
}

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	return port
}

func waitForListener(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port))); err == nil {
			_ = conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Skip("the private sshd never started listening; skipping rather than reporting a false failure")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// afRelayBinary builds the real `af` once per test binary and returns its path.
//
// THE REAL BINARY, not a stand-in script: the ProxyCommand is af invoking itself,
// and the property that matters most — that nothing but relayed bytes reaches
// stdout — is a property of the whole program's startup, not of sshrelay.Run in
// isolation. A shell stand-in would test the composition and none of that.
var (
	afRelayOnce sync.Once
	afRelayPath string
	afRelayErr  error
)

func afRelayBinary(t *testing.T) string {
	t.Helper()
	afRelayOnce.Do(func() {
		if _, err := exec.LookPath("go"); err != nil {
			afRelayErr = fmt.Errorf("no go toolchain to build af with: %w", err)
			return
		}
		dir, err := os.MkdirTemp("", "af-relay-bin")
		if err != nil {
			afRelayErr = err
			return
		}
		out := filepath.Join(dir, "af")
		build := exec.Command("go", "build", "-o", out, "github.com/sachiniyer/agent-factory")
		build.Dir = ".."
		if combined, buildErr := build.CombinedOutput(); buildErr != nil {
			afRelayErr = fmt.Errorf("building af: %w: %s", buildErr, combined)
			return
		}
		afRelayPath = out
	})
	if afRelayErr != nil {
		t.Skipf("cannot build the af binary the ProxyCommand runs (%v)", afRelayErr)
	}
	return afRelayPath
}

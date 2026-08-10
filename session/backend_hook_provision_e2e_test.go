package session

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// End-to-end proof that the #2847 contract CONNECTS, not merely that it
// validates.
//
// Every other test here checks a composed string or a parsed record. Those would
// all keep passing if the pinned known_hosts were subtly wrong — the wrong host
// spelling, a bad port form, an option that does not do what its name suggests —
// because none of them ever hands the result to OpenSSH. This one does: a real
// sshd, a real key exchange, over the exact command hookProvisionSSHCommand
// builds from a real provision record.
//
// It runs entirely in user space on a high port with its own host key and its own
// authorized_keys, so it touches no system sshd and needs no container.

// throwawaySSHD starts an sshd the calling user owns, and returns its port and
// the PUBLIC host key a provision_cmd would have vouched for.
func throwawaySSHD(t *testing.T, extraConfig ...string) (port int, hostPubKey string) {
	t.Helper()
	// These start real daemons and do real handshakes, which is exactly what the
	// non-native matrix cells run `-short` to avoid repeating.
	if testing.Short() {
		t.Skip("skipping the real-sshd provision E2E in short mode")
	}
	// ssh-keyscan proves socket ownership below. Without it every readiness probe
	// would read as "not our listener" and five tests would blame a healthy sshd
	// for failing to start — a misdiagnosis worse than an honest skip.
	if _, err := exec.LookPath("ssh-keyscan"); err != nil {
		t.Skipf("ssh-keyscan is absent, so sshd socket ownership cannot be established: %v", err)
	}
	sshdPath := "/usr/sbin/sshd"
	if _, err := os.Stat(sshdPath); err != nil {
		p, lookErr := exec.LookPath("sshd")
		if lookErr != nil {
			// The ONLY skip. Anything else — a bad directive, permissions, a lost
			// port race — must FAIL, or this whole file can go quietly inert while
			// the required suites stay green.
			t.Skipf("no sshd on this machine, so the connect path cannot be exercised: %v", lookErr)
		}
		sshdPath = p
	}

	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700)) // sshd refuses a loose chain for authorized_keys

	hostKey := filepath.Join(dir, "host_ed25519")
	require.NoError(t, exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", hostKey).Run())
	userKey := filepath.Join(dir, "id_ed25519")
	require.NoError(t, exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", userKey).Run())

	pub, err := os.ReadFile(hostKey + ".pub")
	require.NoError(t, err)
	fields := strings.Fields(string(pub))
	require.GreaterOrEqual(t, len(fields), 2)
	hostPubKey = fields[0] + " " + fields[1]

	authorized := filepath.Join(dir, "authorized_keys")
	userPub, err := os.ReadFile(userKey + ".pub")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(authorized, userPub, 0o600))

	// Selecting a port by binding :0 and closing it leaves a window in which
	// another process can take it. Losing that race must not read as "no sshd" or
	// as a broken assertion against a stranger's listener, so a lost bind RETRIES
	// with a fresh port and anything else fails loudly.
	for attempt := 1; attempt <= 5; attempt++ {
		ln, lerr := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, lerr)
		port = ln.Addr().(*net.TCPAddr).Port
		require.NoError(t, ln.Close())

		cfg := filepath.Join(dir, fmt.Sprintf("sshd_config_%d", attempt))
		// sshd_config is FIRST-VALUE-WINS, so a caller's overrides must precede the
		// defaults — appending them silently does nothing, which is how an earlier
		// draft offered only publickey while claiming to offer password auth.
		body := strings.Join(extraConfig, "\n") + fmt.Sprintf(`
Port %d
ListenAddress 127.0.0.1
HostKey %q
AuthorizedKeysFile %q
PidFile %q
UsePAM no
PasswordAuthentication no
KbdInteractiveAuthentication no
StrictModes no
PermitUserEnvironment no
LogLevel ERROR
`, port, hostKey, authorized, filepath.Join(dir, "sshd.pid"))
		require.NoError(t, os.WriteFile(cfg, []byte(body), 0o600))

		cmd := exec.Command(sshdPath, "-D", "-e", "-f", cfg)
		var diag strings.Builder
		cmd.Stderr = &diag
		require.NoError(t, cmd.Start(), "sshd is installed but could not be started")

		// ONE owner of Wait, for the process's whole life: a second concurrent Wait
		// (here and in cleanup) deadlocks.
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()

		ready, exited := waitForSSHD(done, port, hostPubKey)
		if ready {
			t.Cleanup(func() {
				_ = cmd.Process.Kill()
				<-done
			})
			t.Setenv("AF_TEST_SSH_IDENTITY", userKey)
			// A correct key is not enough: sshd refuses a LOCKED account (common for
			// root and service users in containers) before it ever consults
			// authorized_keys, and PermitRootLogin does not unlock one. Probe once
			// with the known-good identity so that state is reported as an honest
			// skip rather than as five failing provision assertions.
			if !fixtureLoginWorks(t, port, hostPubKey, userKey) {
				t.Skip("this account cannot log in over ssh (locked or otherwise unusable), " +
					"so the provision transport cannot be exercised here")
			}
			return port, hostPubKey
		}
		_ = cmd.Process.Kill()
		<-done
		if exited && strings.Contains(strings.ToLower(diag.String()), "address already in use") {
			continue // lost the bind race; try another port
		}
		t.Fatalf("the throwaway sshd did not come up on 127.0.0.1:%d (attempt %d): %s",
			port, attempt, strings.TrimSpace(diag.String()))
	}
	t.Fatal("could not obtain a free port for the throwaway sshd after 5 attempts")
	return 0, ""
}

// waitForSSHD reports whether OUR sshd is listening. It watches the process as
// well as the port, so a daemon that died is distinguished from one still
// starting — and a port answered by an unrelated listener never counts, because
// the process must still be alive for readiness to be declared.
func waitForSSHD(done <-chan struct{}, port int, wantHostKey string) (ready, exited bool) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			return false, true
		default:
		}
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			// A live process plus an answering port is NOT proof the port is ours:
			// a stranger may hold it while our sshd is still starting and about to
			// fail its bind. Only the host key settles ownership — nothing else has
			// the key we just generated.
			if sshdOwnsPort(port, wantHostKey) {
				select {
				case <-done:
					return false, true // ours died after answering: not usable
				default:
					return true, false
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false, false
}

// sshdOwnsPort reports whether the listener on port presents the host key we
// generated — the only evidence that the socket belongs to the sshd this harness
// started, rather than to a process that won the bind race.
func sshdOwnsPort(port int, wantHostKey string) bool {
	out, err := exec.Command("ssh-keyscan", "-T", "2", "-p", strconv.Itoa(port), "127.0.0.1").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && f[1]+" "+f[2] == wantHostKey {
			return true
		}
	}
	return false
}

// withIdentity appends the test identity to a composed command, standing in for
// the operator's own ssh_config/agent in production.
func withIdentity(cmd string) string {
	if key := os.Getenv("AF_TEST_SSH_IDENTITY"); key != "" {
		return strings.Replace(cmd, "ssh ", "ssh "+fixtureSSHOptions(key), 1)
	}
	return cmd
}

// fixtureSSHOptions is the isolation every fixture connection needs.
//
// `-F none` is the load-bearing one: ssh reads the user's ~/.ssh/config unless
// told otherwise, so a runner with `Host *` or `Host 127.0.0.1` setting User,
// HostName, ProxyCommand or ProxyJump could redirect or invalidate these
// connections — making positive tests fail and negative tests "pass" for reasons
// that have nothing to do with host-key pinning.
//
// Generated paths are shell-quoted because they come from t.TempDir(), which
// honours TMPDIR: whitespace or a metacharacter there would otherwise split the
// argument and hand ssh something other than one identity file.
func fixtureSSHOptions(identity string) string {
	return "-F none -i " + shellQuoteSandbox(identity) + " -o IdentitiesOnly=yes "
}

// fixtureLoginWorks reports whether the calling account can authenticate to the
// fixture daemon at all.
//
// It deliberately uses an INDEPENDENT ssh invocation rather than
// hookProvisionKnownHosts/hookProvisionSSHCommand. Probing with the code under
// test would map any regression in those helpers — a dropped -p, a wrong
// non-default-port host entry — to "the environment cannot log in", and every
// E2E test would SKIP instead of exposing the defect it exists to catch. A probe
// must not be able to disarm the suite.
func fixtureLoginWorks(t *testing.T, port int, hostPubKey, userKey string) bool {
	t.Helper()
	kh := filepath.Join(t.TempDir(), "probe_known_hosts")
	require.NoError(t, os.WriteFile(kh,
		[]byte(fmt.Sprintf("[127.0.0.1]:%d %s\n", port, hostPubKey)), 0o600))
	probe := exec.Command("ssh",
		"-F", "none",
		"-i", userKey,
		"-o", "IdentitiesOnly=yes",
		"-o", "UserKnownHostsFile="+kh,
		"-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "BatchMode=yes",
		"-p", strconv.Itoa(port), "127.0.0.1", "true")
	return probe.Run() == nil
}

// THE end-to-end claim: a record from provision_cmd produces a command that
// authenticates the host against the pinned key and runs a command on it.
func TestProvisionedHostActuallyConnects(t *testing.T) {
	port, hostPubKey := throwawaySSHD(t)

	record := &hookProvisionRecord{Host: "127.0.0.1", Port: port, HostKey: hostPubKey}
	host, p, hpErr := hookProvisionHostPort(record)
	require.NoError(t, hpErr)
	knownHosts, err := hookProvisionKnownHosts(t.TempDir(), host, p, record.HostKey)
	require.NoError(t, err)

	sp := &sandboxProvisioner{sshCmd: withIdentity(hookProvisionSSHCommand(knownHosts, record))}
	out, err := sp.Run(30*time.Second, "echo af-provisioned-and-connected", nil, true)
	require.NoError(t, err, "the pinned command must actually connect; output: %s", out)
	assert.Contains(t, string(out), "af-provisioned-and-connected",
		"a command must really run on the provisioned host, not merely validate")
}

// The pin must be the AUTHORITY, not decoration: a record vouching for the wrong
// key must be refused by OpenSSH even though the host is reachable and the
// credentials are valid.
func TestProvisionedHostRefusesAWrongPinnedKey(t *testing.T) {
	port, _ := throwawaySSHD(t)

	// A syntactically valid key for a different machine.
	other := filepath.Join(t.TempDir(), "other")
	require.NoError(t, exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", other).Run())
	pub, err := os.ReadFile(other + ".pub")
	require.NoError(t, err)
	fields := strings.Fields(string(pub))
	wrong := fields[0] + " " + fields[1]

	record := &hookProvisionRecord{Host: "127.0.0.1", Port: port, HostKey: wrong}
	host, p, hpErr := hookProvisionHostPort(record)
	require.NoError(t, hpErr)
	knownHosts, err := hookProvisionKnownHosts(t.TempDir(), host, p, record.HostKey)
	require.NoError(t, err)

	sp := &sandboxProvisioner{sshCmd: withIdentity(hookProvisionSSHCommand(knownHosts, record))}
	out, err := sp.Run(30*time.Second, "echo should-not-run", nil, true)
	require.Error(t, err, "a key the script did not vouch for must be refused; output: %s", out)
	assert.NotContains(t, string(out), "should-not-run",
		"nothing may execute on a host whose key does not match the pin")
}

// And it must not hang waiting for a human when the key is unknown — BatchMode
// is what turns an unanswerable prompt into a prompt failure.
func TestProvisionedHostFailsFastRatherThanPrompting(t *testing.T) {
	port, _ := throwawaySSHD(t)

	empty := filepath.Join(t.TempDir(), "known_hosts")
	require.NoError(t, os.WriteFile(empty, nil, 0o600))
	sp := &sandboxProvisioner{sshCmd: withIdentity(hookProvisionSSHCommand(empty,
		&hookProvisionRecord{Host: "127.0.0.1", Port: port, HostKey: "unused"}))}

	started := time.Now()
	_, err := sp.Run(20*time.Second, "echo nope", nil, true)
	require.Error(t, err, "an unknown host must be refused, not trusted")
	assert.Less(t, time.Since(started), 15*time.Second,
		"it must fail fast rather than block on a prompt no unattended provision can answer")
}

// Codex 3727303059: `provision_cmd` may spell the port inside `host`
// ("127.0.0.1:2222" with Port zero), and provisionHost NORMALIZES that into the
// pinned record before composing. Every other connect test supplies the separate
// Port field, so that handoff was production-only and untested — a regression
// would pass the colon-bearing value to ssh as the DESTINATION while this suite
// stayed green.
//
// This composes exactly as provisionHost does, then actually connects.
func TestProvisionedHostConnectsWithAnEmbeddedPort(t *testing.T) {
	port, hostPubKey := throwawaySSHD(t)
	hostStore := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", hostStore)
	preservedKnownHosts := filepath.Join(t.TempDir(), "known_hosts")

	// Drives the REAL provisionHost, so the assertion cannot be satisfied by
	// restating its normalization: the seam captures whatever command
	// provisionHost actually composed, and the connection below uses THAT.
	var composed string
	prev := newHookSandboxProvisioner
	newHookSandboxProvisioner = func(spec ProvisionSpec, sshCmd, afBin, program string) *sandboxProvisioner {
		// provisionHost is deliberately failed below, so its per-session pin is
		// removed before this test makes the later connection. Preserve a test-owned
		// copy while the provisioning seam still has access to it; relying on the
		// production failure path to leak the file is not part of this E2E contract.
		actualKnownHosts := filepath.Join(hostStore, "hook-hosts", Slugify("embedded port"), "known_hosts")
		body, err := os.ReadFile(actualKnownHosts)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(preservedKnownHosts, body, 0o600))
		composed = strings.Replace(sshCmd, actualKnownHosts, preservedKnownHosts, 1)
		require.NotEqual(t, sshCmd, composed, "the captured command must use the test-owned pin")
		sp := prev(spec, sshCmd, afBin, program)
		// ABORT before any remote step runs. The provisioning pipeline's second
		// step is configureGit, which executes `git config --global` — and the
		// "remote" here is THIS machine, so letting it proceed would rewrite the
		// developer's real user.name/user.email and add safe.directory "*" during
		// an ordinary `go test ./...`. Only the composed command is wanted, and it
		// is already captured above.
		sp.runCommandFn = func(time.Duration, string, io.Reader, bool) ([]byte, error) {
			return nil, errors.New("e2e: captured the composed command; aborting before any remote step")
		}
		return sp
	}
	t.Cleanup(func() { newHookSandboxProvisioner = prev })

	// A provision_cmd returning the port INSIDE host, the form whose
	// normalization is under test.
	record := fmt.Sprintf(`{"host":"127.0.0.1:%d","host_key":%q}`, port, hostPubKey)
	h := newHookState(t, "printf '%s' '"+record+"'\nexit 0\n", "")
	p := newHookProvisioner(h, "embedded port")
	p.hooks.ProvisionCmd = h.launch
	p.spec.CloneURL = "https://example.invalid/repo.git"

	// It will fail later (no real workspace to clone), which is fine: the
	// composed command is what this test is about, and it is captured before then.
	_, _ = p.provisionHost()
	require.NotEmpty(t, composed, "provisionHost must have composed an ssh command")

	assert.NotContains(t, composed, fmt.Sprintf("127.0.0.1:%d", port),
		"the colon-bearing value must never reach ssh as the destination — provisionHost must normalize it")
	assert.Contains(t, composed, fmt.Sprintf("-p %d", port), "the port must be passed with -p")

	// And the command provisionHost built must actually connect.
	sp := &sandboxProvisioner{sshCmd: withIdentity(composed)}
	out, err := sp.Run(30*time.Second, "echo embedded-port-connected", nil, true)
	require.NoError(t, err, "the command provisionHost composed must connect; output: %s", out)
	assert.Contains(t, string(out), "embedded-port-connected")
}

// Codex 3727303056: the earlier fail-fast test proved nothing about BatchMode.
// With an EMPTY known_hosts, StrictHostKeyChecking=yes already refuses the
// unknown key without prompting, so removing BatchMode left it green.
//
// BatchMode's real job is refusing AUTHENTICATION interaction. So: pin the host
// key CORRECTLY (host verification passes, and is no longer the thing failing),
// offer password auth, and give ssh an askpass it would have to invoke to
// prompt. With BatchMode the askpass must never run.
func TestProvisionedHostBatchModeRefusesAuthPrompts(t *testing.T) {
	// PermitRootLogin yes matters: OpenSSH defaults to prohibit-password, which
	// disables password and keyboard-interactive FOR ROOT. Under a root CI runner
	// the server would end authentication without ever reaching askpass, and this
	// test would pass with BatchMode deleted — vacuous exactly where it must bite.
	port, hostPubKey := throwawaySSHD(t,
		"PermitRootLogin yes", "PasswordAuthentication yes", "KbdInteractiveAuthentication yes")
	record := &hookProvisionRecord{Host: "127.0.0.1", Port: port, HostKey: hostPubKey}

	// THE COUNTERFACTUAL, and the reason this can no longer lie. Reasoning that
	// "the setup would prompt" is what failed twice here already, so the
	// no-BatchMode run must ACTUALLY produce a prompt. If it cannot, the
	// environment is unable to demonstrate the property and this fails rather
	// than reporting a guarantee it never exercised.
	require.True(t, runPinnedSSHAndReportPrompt(t, record, false),
		"without BatchMode this setup must reach an interactive prompt — otherwise the assertion "+
			"below proves nothing, which is exactly how this test passed vacuously twice")

	assert.False(t, runPinnedSSHAndReportPrompt(t, record, true),
		"BatchMode=yes must stop ssh ever asking for a credential — the askpass helper ran, so it did not")
}

// runPinnedSSHAndReportPrompt connects with the pinned host key and no usable
// identity, reporting whether ssh reached for a credential. SSH_ASKPASS_REQUIRE
// =force makes ssh use the helper even without a tty, so a prompt is observable
// as a marker file.
func runPinnedSSHAndReportPrompt(t *testing.T, record *hookProvisionRecord, batchMode bool) bool {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "askpass-was-invoked")
	askpass := filepath.Join(dir, "askpass.sh")
	require.NoError(t, os.WriteFile(askpass,
		[]byte("#!/usr/bin/env bash\ntouch "+shellQuoteSandbox(marker)+"\necho wrong-password\n"), 0o755))

	knownHosts, err := hookProvisionKnownHosts(dir, record.Host, record.Port, record.HostKey)
	require.NoError(t, err)
	noKey := filepath.Join(dir, "unauthorized")
	require.NoError(t, exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", noKey).Run())

	t.Setenv("SSH_ASKPASS", askpass)
	t.Setenv("SSH_ASKPASS_REQUIRE", "force")
	t.Setenv("DISPLAY", ":0")

	cmd := hookProvisionSSHCommand(knownHosts, record)
	if !batchMode {
		cmd = strings.Replace(cmd, "-o BatchMode=yes ", "", 1)
	}
	// Identity options BEFORE the destination — ssh treats anything after it as
	// the remote command, so appending them silently does nothing.
	cmd = strings.Replace(cmd, "ssh ", "ssh "+fixtureSSHOptions(noKey), 1)

	sp := &sandboxProvisioner{sshCmd: cmd}
	_, runErr := sp.Run(20*time.Second, "echo should-not-run", nil, true)
	require.Error(t, runErr, "with no usable key the connection must fail either way")

	_, statErr := os.Stat(marker)
	return statErr == nil
}

// Codex 3732268431: readiness must establish SOCKET OWNERSHIP, not merely that
// our process is momentarily alive. A stranger holding the port while our sshd
// is still starting would otherwise be accepted, and the connection tests would
// target it. Only the host key settles it.
func TestSSHDOwnershipCheckRejectsAStranger(t *testing.T) {
	if _, err := exec.LookPath("ssh-keyscan"); err != nil {
		t.Skipf("ssh-keyscan absent, so ownership cannot be established here: %v", err)
	}
	// A plain TCP listener: reachable, answers a dial, and is emphatically not
	// our sshd.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, aErr := ln.Accept()
			if aErr != nil {
				return
			}
			_ = c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	assert.False(t, sshdOwnsPort(port, "ssh-ed25519 AAAAsomekeywedidnotgenerate"),
		"a listener that is not our sshd must never be accepted as ready — that is the bind race")

	// And the positive direction: a real harness sshd IS recognised.
	realPort, realKey := throwawaySSHD(t)
	assert.True(t, sshdOwnsPort(realPort, realKey), "our own sshd must be recognised by its host key")
}

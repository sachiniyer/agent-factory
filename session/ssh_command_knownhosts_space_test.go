package session

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// The single-layer quoting bug UserKnownHostsFile carried since the transport
// convergence (#3061): shellQuote quoted the value only for the outer `sh -c
// '<sshCmd> "$@"'` wrapper, whose shell consumed those single quotes before ssh
// saw the argument. ssh's own config parser then re-split the multi-value
// UserKnownHostsFile value on whitespace, so a known_hosts path containing a
// space was never the file af prepared — under strict checking every provision,
// tunnel and reap failed host-key verification; under accept-new the af-owned
// TOFU store was silently bypassed and learned keys were never persisted.
//
// The fix restores the INNER layer of the two-stage quoting discipline
// sshPinnedProxyCommand already applied to its path: sshConfigQuote wraps the
// value in double quotes (which survive the outer shell and group the value
// inside ssh's config parser), and shellQuote still wraps the whole
// `keyword=<value>` token for the outer shell. These tests pin both layers and
// the end-to-end behaviour the report measured.

// sshConfigQuote groups a value as one ssh_config token and escapes the
// characters ssh's parser would otherwise interpret. The common case (a path
// with whitespace but no quote or backslash) reduces to a plain double-quoted
// token; both edge characters round-trip exactly because ssh_config honours
// `\"` and `\\` inside a double-quoted value (verified against OpenSSH 10.0p2).
func TestSSHConfigQuoteGroupsWithWhitespaceAndEscapesMetacharacters(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "/af/known_hosts", `"/af/known_hosts"`},
		{"whitespace", "/dir with space/kh", `"/dir with space/kh"`},
		{"tab", "/dir\twith\ttabs/kh", `"/dir` + "\t" + `with` + "\t" + `tabs/kh"`},
		{"double quote needs escaping", `/a"b/kh`, `"/a\"b/kh"`},
		{"backslash needs escaping", `/a\b/kh`, `"/a\\b/kh"`},
		{"backslash before quote is stable", `/a\"b/kh`, `"/a\\\"b/kh"`},
		{"empty", "", `""`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, sshConfigQuote(c.in))
		})
	}
}

// TestSSHCommandHandsAKnownHostsPathWithSpacesToSSHsArgvIntact is the report's
// "split proof": a stub ssh first in PATH prints exactly the argv the production
// transport hands it, so the claim is not just about the composed string but
// about what ssh receives after the outer `sh -c` consumes its quoting.
//
// Pre-fix the stub saw ONE argv element `UserKnownHostsFile=/dir with space/kh`
// — the outer shell had eaten shellQuote's single quotes — and ssh's config
// parser then tokenized that bare value into the three paths `/dir`, `with`,
// `space/kh`. The fix keeps the inner double quotes, so the stub sees
// `UserKnownHostsFile="/dir with space/kh"`: one value at ssh's layer too.
func TestSSHCommandHandsAKnownHostsPathWithSpacesToSSHsArgvIntact(t *testing.T) {
	stubDir := t.TempDir()
	stubPath := filepath.Join(stubDir, "ssh")
	// NUL-separated argv records: robust against the spaces and the remote script.
	stub := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\0' \"$a\"; done\n"
	require.NoError(t, os.WriteFile(stubPath, []byte(stub), 0o755))
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	spacedKh := filepath.Join(t.TempDir(), "dir with space", "known_hosts")
	cmd := sshCmdFor(t, config.SSHConfig{Host: "h.invalid", Port: 2222, KnownHosts: spacedKh},
		config.SSHHostKeyStrict)

	p := newSSHSandboxProvisioner(ProvisionSpec{Title: "argv-probe"}, cmd, "", "")
	out, err := p.Run(10*time.Second, "echo remote", nil, true)
	require.NoError(t, err,
		"the stub ssh exits 0; an error here means the transport wrapper failed to run it")

	argv := strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
	var ukh, usr string
	for _, a := range argv {
		switch {
		case strings.HasPrefix(a, "UserKnownHostsFile="):
			ukh = a
		case strings.HasPrefix(a, "User="):
			usr = a
		}
	}
	require.NotEmpty(t, ukh, "ssh must receive a UserKnownHostsFile option; full argv:\n%v", argv)
	assert.Equal(t, `UserKnownHostsFile="`+spacedKh+`"`, ukh,
		"the spaced path reaches ssh as ONE double-quoted value; pre-fix this was `UserKnownHostsFile=%s` "+
			"(bare — the outer shell had consumed the quotes), which ssh re-split on whitespace", spacedKh)
	// And the same discipline carries the User= pin: the value is double-quoted at
	// the config layer, not bare.
	assert.NotEmpty(t, usr, "ssh must receive a User= option; full argv:\n%v", argv)
	assert.Contains(t, usr, `="`, "the User= value is double-quoted at the ssh config layer")
}

// loopbackSSHD is a throwaway sshd on a loopback port that accepts the invoking
// account's public key. It is loopback-only, uses a throwaway host key and
// authorized_keys, and permits root login so the e2e test runs whether the
// daemon account is root or not — the existing startSSHLab forbids root, which
// would make this test skip on a root-owned box rather than exercise the fix.
type loopbackSSHD struct {
	dir     string
	port    int
	hostPub string
	keyPath string
	user    string
}

// rotateLoopbackSSHDHostKey regenerates the sshd's host key in place, restarts
// the daemon on the same port, and updates lab.hostPub. Used to prove that a
// CHANGED host key is detected (refused under accept-new) once the original key
// has been persisted — the persistence guarantee the report's exploit scenario
// is about.
func rotateLoopbackSSHDHostKey(t *testing.T, lab *loopbackSSHD) error {
	t.Helper()
	// Kill the running sshd so the port is free and the new key takes effect.
	pidPath := filepath.Join(lab.dir, "sshd.pid")
	if pidBytes, err := os.ReadFile(pidPath); err == nil {
		if n, cerr := strconv.Atoi(strings.TrimSpace(string(pidBytes))); cerr == nil {
			if proc, ferr := os.FindProcess(n); ferr == nil {
				_ = proc.Kill()
				_, _ = proc.Wait()
			}
		}
	}
	hostKey := filepath.Join(lab.dir, "host_key")
	_ = os.Remove(hostKey)
	_ = os.Remove(hostKey + ".pub")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "",
		"-C", "af-space-test-rotated", "-f", hostKey).CombinedOutput(); err != nil {
		return fmt.Errorf("rotating host key: %s: %w", out, err)
	}
	// Start a fresh sshd with the same config (same port, same dir, new host key).
	logPath := filepath.Join(lab.dir, "sshd.log")
	start := exec.Command(lookSSHD(t), "-f", filepath.Join(lab.dir, "sshd_config"), "-E", logPath)
	if out, err := start.CombinedOutput(); err != nil {
		return fmt.Errorf("restarting rotated sshd: %s: %w", out, err)
	}
	waitForListener(t, lab.port)
	pub, err := os.ReadFile(hostKey + ".pub")
	if err != nil {
		return fmt.Errorf("reading rotated host pub: %w", err)
	}
	lab.hostPub = strings.TrimSpace(string(pub))
	return nil
}

func startLoopbackSSHD(t *testing.T) *loopbackSSHD {
	t.Helper()
	sshdBin := lookSSHD(t)
	for _, tool := range []string{"ssh", "ssh-keygen"} {
		_, err := exec.LookPath(tool)
		require.NoError(t, err, "%s is not installed", tool)
	}
	me, err := user.Current()
	require.NoError(t, err)

	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700))
	keygen := func(name string) {
		out, genErr := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "",
			"-C", "af-space-test", "-f", filepath.Join(dir, name)).CombinedOutput()
		require.NoError(t, genErr, "ssh-keygen: %s", out)
	}
	keygen("host_key")
	keygen("client_key")
	read := func(name string) string {
		b, rErr := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, rErr)
		return strings.TrimSpace(string(b))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "authorized_keys"),
		[]byte(read("client_key.pub")+"\n"), 0o600))

	port := freeLoopbackPort(t)
	sshdConfig := filepath.Join(dir, "sshd_config")
	require.NoError(t, os.WriteFile(sshdConfig, []byte(strings.Join([]string{
		"Port " + strconv.Itoa(port),
		"ListenAddress 127.0.0.1",
		"HostKey " + filepath.Join(dir, "host_key"),
		"AuthorizedKeysFile " + filepath.Join(dir, "authorized_keys"),
		"PidFile " + filepath.Join(dir, "sshd.pid"),
		// t.TempDir() is under a world-searchable /tmp, which StrictModes rejects.
		"StrictModes no",
		"UsePAM no",
		"PasswordAuthentication no",
		"KbdInteractiveAuthentication no",
		// Permit the invoking account whether or not it is root: this sshd is
		// loopback-only, throwaway, and accepts only the test's client key.
		"PermitRootLogin yes",
		"PubkeyAuthentication yes",
	}, "\n")+"\n"), 0o600))

	logPath := filepath.Join(dir, "sshd.log")
	start := exec.Command(sshdBin, "-f", sshdConfig, "-E", logPath)
	if out, startErr := start.CombinedOutput(); startErr != nil {
		logs, _ := os.ReadFile(logPath)
		t.Skipf("cannot start a private sshd (%v): %s\n%s", startErr, out, logs)
	}
	t.Cleanup(func() {
		pid, rErr := os.ReadFile(filepath.Join(dir, "sshd.pid"))
		if rErr != nil {
			return
		}
		if n, cErr := strconv.Atoi(strings.TrimSpace(string(pid))); cErr == nil {
			if proc, fErr := os.FindProcess(n); fErr == nil {
				_ = proc.Kill()
				_, _ = proc.Wait()
			}
		}
	})
	waitForListener(t, port)
	return &loopbackSSHD{dir: dir, port: port, hostPub: read("host_key.pub"),
		keyPath: filepath.Join(dir, "client_key"), user: me.Username}
}

// TestSSHBackendHonoursAKnownHostsPathContainingSpaces is the end-to-end proof
// the report measured: against a real sshd, byte-identical known_hosts content
// succeeds when the path has no spaces and (with the fix) succeeds when it does.
// Pre-fix the spaced path was re-split by ssh's config parser, so strict
// verification failed with "Host key verification failed" even though the entry
// was present in the exact file af prepared.
func TestSSHBackendHonoursAKnownHostsPathContainingSpaces(t *testing.T) {
	lab := startLoopbackSSHD(t)

	// A directory whose name contains a space, holding the known_hosts file af
	// would prepare. The host key entry is byte-identical to a space-free
	// control; only the directory name differs.
	spacedDir := filepath.Join(t.TempDir(), "dir with space")
	require.NoError(t, os.MkdirAll(spacedDir, 0o700))
	spacedKh := filepath.Join(spacedDir, "known_hosts")
	entry := fmt.Sprintf("[127.0.0.1]:%d %s\n", lab.port, lab.hostPub)
	require.NoError(t, os.WriteFile(spacedKh, []byte(entry), 0o600))

	cfg := config.SSHConfig{
		Host:         "127.0.0.1",
		Port:         lab.port,
		User:         lab.user,
		IdentityFile: lab.keyPath,
		KnownHosts:   spacedKh,
	}
	cmd, err := sshCommandPinnedTo(cfg, config.SSHHostKeyStrict, "", 0)
	require.NoError(t, err)

	out, err := runOverTransport(cmd, "echo SPACED_OK")
	require.NoError(t, err,
		"ssh must consult the spaced known_hosts path af prepared (pre-fix it re-split the path on "+
			"whitespace and failed host-key verification).\nssh said:\n%s", out)
	assert.Contains(t, out, "SPACED_OK")
}

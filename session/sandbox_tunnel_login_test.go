package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tunnel never rides a ControlMaster (-S none, #7121), so it has to log in
// on its own. An operator whose login needs a prompt (MFA, a key passphrase
// with no agent) and who reuses a master for everything else now gets a tunnel
// that never comes up, while their sandbox.ssh still works by hand, through
// the master. The error has to name that cause, for each backend whose ssh
// reads the operator's ssh_config. backend=ssh runs ssh with -F none, where no
// ControlMaster can apply, so its error must not send anyone looking for one.
func TestTunnelNeverListenedNamesItsOwnLogin(t *testing.T) {
	for _, label := range []string{string(BackendSandbox), string(BackendHook)} {
		err := waitForSandboxTunnelWithin(label, freeLocalAddrForTest(t), 100*time.Millisecond, 20*time.Millisecond)
		require.Error(t, err)
		msg := err.Error()
		assert.Contains(t, msg, "never started listening", label)
		assert.Contains(t, msg, sandboxTransportConfigKey(label), "%s: the error still names what the operator can test", label)
		assert.Contains(t, msg, "-S none", "%s: the error must say af keeps the tunnel off any master", label)
		assert.Contains(t, msg, "ControlMaster", label)
		assert.Contains(t, msg, "without a prompt", "%s: the error must say what that requires of the login", label)
	}

	err := waitForSandboxTunnelWithin(string(BackendSSH), freeLocalAddrForTest(t), 100*time.Millisecond, 20*time.Millisecond)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "ControlMaster",
		"backend=ssh passes -F none, so no ControlMaster can apply and none must be suggested")
}

// The tunnel's own words are what an operator needs and cannot otherwise see:
// both spawn paths used to drop its stderr. When the forward never comes up,
// the error quotes the last line the tunnel's ssh wrote, on the lifeline path
// and the direct one alike.
func TestTunnelNeverListenedQuotesTheTunnelsLastWords(t *testing.T) {
	for _, path := range []struct {
		name     string
		lifeline string
	}{
		{"direct", ""},
		{"lifeline", os.Args[0]},
	} {
		t.Run(path.name, func(t *testing.T) {
			forceTunnelLifelineExe(t, path.lifeline)
			dir := t.TempDir()
			ssh := filepath.Join(dir, "ssh")
			require.NoError(t, os.WriteFile(ssh, []byte("#!/bin/sh\n"+
				"echo 'debug1: an earlier line' >&2\n"+
				"echo 'user@sandbox.invalid: Permission denied (keyboard-interactive).' >&2\n"+
				"exit 255\n"), 0o755))
			prevProbe := waitForSandboxTunnelFn
			waitForSandboxTunnelFn = func(label, addr string, _, poll time.Duration) error {
				return waitForSandboxTunnelWithin(label, addr, 500*time.Millisecond, poll)
			}
			t.Cleanup(func() { waitForSandboxTunnelFn = prevProbe })

			p := &sandboxProvisioner{sshCmd: ssh}
			t.Cleanup(func() {
				p.reapMu.Lock()
				p.stopTunnel()
				p.reapMu.Unlock()
			})
			_, err := p.openTunnel("127.0.0.1:9")
			require.Error(t, err)
			p.reapMu.Lock()
			child := p.tunnel
			p.reapMu.Unlock()
			require.NotNil(t, child, "openTunnel leaves the failed generation for its caller's reap")
			require.Equal(t, path.lifeline != "", child.ll != nil, "the generation must have run on the %s path", path.name)
			assert.Contains(t, err.Error(), "never started listening")
			assert.Contains(t, err.Error(), "Permission denied (keyboard-interactive).",
				"the error must quote the last line the tunnel's ssh wrote: %v", err)
			assert.NotContains(t, err.Error(), "an earlier line", "only the last line, not the whole stream")
		})
	}
}

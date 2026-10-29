package session

import (
	"fmt"
	"net"
	"time"
)

// sandboxTransportConfigKey names the setting that holds the ssh invocation for
// this backend, so a transport failure points the operator at something they can
// run by hand. The two backends reach the same transport from different config
// surfaces, and telling an `ssh.host` user to check `sandbox_ssh` is worse than
// saying nothing.
func sandboxTransportConfigKey(label string) string {
	if label == string(BackendSSH) {
		return "ssh.host"
	}
	if label == string(BackendHook) {
		return "remote_hooks.provision_cmd"
	}
	return "sandbox.ssh"
}

// waitForSandboxTunnelFn is the readiness probe both startTunnel and the
// transport supervisor run after a spawn — a var so a supervisor test can give
// its fake tunnel a working forward without standing up a real listener.
var waitForSandboxTunnelFn = waitForSandboxTunnelWithin

// waitForSandboxTunnel probes the local end until it answers. Without this the
// provision would return an endpoint whose first request races the forward.
func waitForSandboxTunnelWithin(label, localAddr string, timeout, poll time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("tcp", localAddr, poll)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("backend=%s: the ssh tunnel to %s never started listening within %s — "+
				"check that the transport af runs for %s works by hand and that the remote permits port forwarding",
				label, localAddr, timeout, sandboxTransportConfigKey(label))
		}
		time.Sleep(poll)
	}
}

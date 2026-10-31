package session

import (
	"fmt"
	"net"
	"time"
)

// startTunnel runs `<sandbox_ssh> -N -L <local>:<remote>` as a long-lived child.
//
// The local port is chosen by binding :0 and closing it, rather than asking ssh
// for one: `ssh -L` has no "pick a port and tell me" form, so af must name the
// port it will dial. The listener is closed before ssh binds it, which is a
// small race — the readiness probe below is what actually establishes that the
// forward came up, so a lost race fails the provision rather than handing back a
// dead endpoint.
//
// The child is bound to this process's lifetime by the machinery in
// sandbox_tunnel.go (#5564): spawned under the watch lifeline when this binary
// can serve as one, registered for the daemon-exit stop, and env-stamped for
// the next daemon's ownership-proven sweep.
func (p *sandboxProvisioner) startTunnel(remoteAddr string) (string, error) {
	if p.startTunnelFn != nil {
		return p.startTunnelFn(remoteAddr)
	}
	localAddr, err := p.openTunnel(remoteAddr)
	if err != nil {
		return "", err
	}
	// The supervisor starts only after the FIRST generation proves ready: an
	// ssh that cannot establish at all is a provision failure (reaped as one),
	// not an outage to retry — the re-dial loop exists for transports that die
	// AFTER they once worked.
	go p.superviseTunnel()
	return localAddr, nil
}

// openTunnel spawns the first -L generation and waits for its forward to answer,
// WITHOUT starting the supervisor. Reattach (#5571 W1.5) probes the agent-server
// through this generation before deciding whether to adopt it: supervision —
// and with it the latched stop a failed attempt would need — begins only once
// the probe has accepted the runtime.
func (p *sandboxProvisioner) openTunnel(remoteAddr string) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("backend=%s: opening a local port for the tunnel failed: %w", p.label(), err)
	}
	localAddr := ln.Addr().String()
	_ = ln.Close()

	forward := fmt.Sprintf("%s:%s", localAddr, remoteAddr)
	// Registered before the spawn so a daemon exit racing this provision —
	// StopAllSandboxTunnels — has the entry it needs the moment the child is
	// born; the entry is dropped by stopTunnel, which reap reaches on every
	// failure path below.
	registerSandboxTunnel(p)
	// The transport contract the supervisor re-establishes on every re-dial is
	// pinned here, once: every generation binds the same localAddr so the
	// published endpoint is never stale, and W3.4's -R slot rides p.reverseFwds.
	p.reapMu.Lock()
	p.forward, p.localAddr = forward, localAddr
	p.reapMu.Unlock()
	// Spawn+publish happen atomically under reapMu (provisionTunnelChild), so
	// a StopAllSandboxTunnels stop can never interleave between a born child
	// and its publication — and a child born after that stop already ran is
	// killed by its own spawner rather than left registered to nothing.
	if _, err := p.provisionTunnelChild(); err != nil {
		return "", fmt.Errorf("backend=%s: starting the ssh tunnel failed: %w", p.label(), err)
	}

	if err := waitForSandboxTunnelFn(p.label(), localAddr, sandboxTunnelReadyTimeout, sandboxTunnelReadyPoll); err != nil {
		return "", err
	}
	return localAddr, nil
}

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

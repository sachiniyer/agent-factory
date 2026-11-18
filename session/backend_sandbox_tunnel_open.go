package session

import (
	"errors"
	"fmt"
	"net"
	"os"
	"time"
)

// startTunnel runs `<sandbox_ssh> -N -L <socket>:<remote>` as a long-lived
// child and returns the endpoint its forward serves.
//
// The local endpoint is a unix socket inside this daemon's private
// generation dir (sandbox_tunnel_endpoint.go, #7495) — never a TCP port: a
// port can only be reserved by binding :0 and closing it, a gap another
// local process could win, and af's first request through the tunnel
// carries the session's bearer credential. A unix path inside a
// uid-verified 0700 directory cannot be taken by another account, and
// StreamLocalBindUnlink makes even a same-uid squatter impossible to keep —
// while every dial still proves the acceptor is, or descends from, this
// tunnel's own spawn before a byte is written (verifyTunnelPeer).
//
// The child is bound to this process's lifetime by the machinery in
// sandbox_tunnel.go (#5564): spawned under the watch lifeline when this binary
// can serve as one, registered for the daemon-exit stop, and env-stamped for
// the next daemon's ownership-proven sweep.
func (p *sandboxProvisioner) startTunnel(remoteAddr string) (*AgentServerEndpoint, error) {
	if p.startTunnelFn != nil {
		return p.startTunnelFn(remoteAddr)
	}
	if _, err := p.openTunnel(remoteAddr); err != nil {
		return nil, err
	}
	// The supervisor starts only after the FIRST generation proves ready: an
	// ssh that cannot establish at all is a provision failure (reaped as one),
	// not an outage to retry — the re-dial loop exists for transports that die
	// AFTER they once worked.
	go p.superviseTunnel()
	return p.tunnelEndpoint(""), nil
}

// tunnelEndpoint builds the client handle for the socket the CURRENT tunnel
// generation serves (#7495). URL carries an unroutable placeholder authority —
// every dial goes to SocketPath through the unix dialer, so a code path that
// bypassed it would fail name resolution before a byte is written rather than
// reach a listener af did not provision. VerifyPeer is the per-connection
// proof that the acceptor is — or descends from — this tunnel's own spawn:
// nothing is written — the session's bearer included — to an endpoint that is
// not provably af's own ssh child.
//
// The token is filled by the caller: the credential's provenance is the
// provisioner's own derivation, and the endpoint that carries it must never
// adopt a value the remote merely reported.
func (p *sandboxProvisioner) tunnelEndpoint(token string) *AgentServerEndpoint {
	p.reapMu.Lock()
	sockPath := p.socketPath
	p.reapMu.Unlock()
	return &AgentServerEndpoint{
		URL:          tunnelEndpointURL,
		SocketPath:   sockPath,
		Token:        token,
		VerifyPeer:   p.verifyTunnelPeer,
		EndpointGone: p.noteEndpointGone,
	}
}

// noteEndpointGone is the endpoint's re-dial trigger for a socket that
// vanished while its tunnel generation lives (#7511): the supervisor re-dials
// only on tunnel death, so a dial that found no file at the socket path is
// what must recycle the transport — kill the current generation and the
// supervisor re-dials it into the re-established dir. The socket is
// re-checked first: a dial that raced a respawn whose file already landed
// must not kill the newborn.
func (p *sandboxProvisioner) noteEndpointGone() {
	p.reapMu.Lock()
	defer p.reapMu.Unlock()
	if p.tunnel == nil || p.socketPath == "" {
		return // already re-dialing, or no endpoint was ever minted
	}
	if _, err := os.Lstat(p.socketPath); err == nil {
		return // the path exists again — leave the fresh generation alone
	}
	killTunnel(p.tunnel) // child.done fires; the supervisor re-dials
}

// tunnelEndpointURL is the placeholder authority a tunnel endpoint's URL
// field carries. It deliberately cannot resolve: every dial for the endpoint
// goes to SocketPath, and an accidental fallback to TCP would fail DNS before
// writing — the misdial is a loud error, never a credential sent elsewhere.
const tunnelEndpointURL = "http://af-tunnel.invalid"

// openTunnel spawns the first -L generation and waits for its forward to answer,
// WITHOUT starting the supervisor. Reattach (#5571 W1.5) probes the agent-server
// through this generation before deciding whether to adopt it: supervision —
// and with it the latched stop a failed attempt would need — begins only once
// the probe has accepted the runtime.
func (p *sandboxProvisioner) openTunnel(remoteAddr string) (string, error) {
	// The socket path is minted inside this daemon's own generation dir —
	// the only place StreamLocalBindUnlink can ever unlink (#7495).
	sockPath, err := p.tunnelSocketPath()
	if err != nil {
		return "", fmt.Errorf("backend=%s: %w", p.label(), err)
	}

	// `ssh -L [<unix-socket>]:<remote-host>:<remote-port>`: the local end is
	// the socket file, bracket-quoted so a ':' anywhere in the AF home still
	// parses — OpenSSH's parse_fwd_field has read the escape since before
	// af's 7.6 floor (#7495 R5). The remote end is still the agent-server's
	// loopback port.
	forward := "[" + sockPath + "]:" + remoteAddr
	// Registered before the spawn so a daemon exit racing this provision —
	// StopAllSandboxTunnels — has the entry it needs the moment the child is
	// born; the entry is dropped by stopTunnel, which reap reaches on every
	// failure path below.
	registerSandboxTunnel(p)
	// The transport contract the supervisor re-establishes on every re-dial is
	// pinned here, once: every generation binds the same socketPath so the
	// published endpoint is never stale, and W3.4's -R slot rides p.reverseFwds.
	p.reapMu.Lock()
	p.forward, p.socketPath = forward, sockPath
	p.reapMu.Unlock()
	// Spawn+publish happen atomically under reapMu (provisionTunnelChild), so
	// a StopAllSandboxTunnels stop can never interleave between a born child
	// and its publication — and a child born after that stop already ran is
	// killed by its own spawner rather than left registered to nothing.
	child, err := p.provisionTunnelChild()
	if err != nil {
		return "", fmt.Errorf("backend=%s: starting the ssh tunnel failed: %w", p.label(), err)
	}

	if err := waitForSandboxTunnelFn(p, sandboxTunnelReadyTimeout, sandboxTunnelReadyPoll); err != nil {
		return "", withTunnelStderr(err, child)
	}
	return sockPath, nil
}

// tunnelOwnLoginNote is what the readiness error adds for a backend whose ssh
// reads the operator's ssh_config (#7121). There the operator may reuse a
// ControlMaster for everything, including logins that need a prompt, and the
// tunnel is the one connection that never rides it (spawnTunnel's -S none).
// Their sandbox.ssh then works by hand while the tunnel cannot log in, so the
// error has to say what af changed and what that requires. backend=ssh runs
// ssh with -F none, where no master can apply, so it gets no such note.
func tunnelOwnLoginNote(label string) string {
	if label == string(BackendSSH) {
		return ""
	}
	return ". The tunnel is a connection of its own: af runs its ssh with -o ControlMaster=no -S none, " +
		"so it never rides a ControlMaster and has to log in without a prompt (no MFA push or code, " +
		"no key passphrase outside an agent), even where your other ssh sessions to that host reuse a master. " +
		"Adding -S none -o BatchMode=yes to the command you test by hand shows what the tunnel sees"
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

// waitForSandboxTunnelWithin probes the local socket until it answers AND its
// acceptor proves to descend from this tunnel's own spawn (#7495). Without this
// the provision would return an endpoint whose first request races the
// forward — and without the peer check, "something answered" could not be
// told apart from "af's ssh bound the socket".
func waitForSandboxTunnelWithin(p *sandboxProvisioner, timeout, poll time.Duration) error {
	p.reapMu.Lock()
	sockPath := p.socketPath
	p.reapMu.Unlock()
	deadline := time.Now().Add(timeout)
	// peerErr tracks the last "answered but not af's own ssh child" refusal so
	// the deadline can say THAT — a foreign listener holding the endpoint is a
	// different failure than a forward that never bound, and the report has to
	// name it rather than blame the transport (#7495).
	var peerErr error
	for {
		conn, err := net.DialTimeout("unix", sockPath, poll)
		if err == nil {
			peerErr = p.verifyTunnelPeer(conn.(*net.UnixConn))
			_ = conn.Close()
			if peerErr == nil {
				// The peer says af's own ssh answered — but the proof asks the
				// FILESYSTEM, not the option af passed: an ssh_config
				// StreamLocalBindMask can still have stripped the bound
				// socket's mode (#7495 R3).
				fileErr := verifyTunnelSocketFile(sockPath)
				if fileErr == nil {
					return nil
				}
				if !errors.Is(fileErr, os.ErrNotExist) {
					// A wrong-shaped bound socket never self-heals — fail
					// now with the remedy rather than burn the deadline.
					return fmt.Errorf("backend=%s: %v", p.label(), fileErr)
				}
				// ENOENT: a re-dial is mid-rebind — keep polling; the peer
				// check reruns on the next pass.
			}
		} else {
			peerErr = nil
		}
		if time.Now().After(deadline) {
			if peerErr != nil {
				return fmt.Errorf("backend=%s: the ssh tunnel endpoint %s answers but is not provably af's own "+
					"ssh child — %v. af refuses to use an endpoint it cannot prove is its own",
					p.label(), sockPath, peerErr)
			}
			return fmt.Errorf("backend=%s: the ssh tunnel to %s never started listening within %s — "+
				"af needs the transport's ssh to bind a UNIX-socket -L forward (OpenSSH 6.7+); testing "+
				"it by hand with a TCP -L proves nothing. Check that the transport af runs for %s accepts "+
				"a socket forward and that the remote permits port forwarding%s",
				p.label(), sockPath, timeout, sandboxTransportConfigKey(p.label()), tunnelOwnLoginNote(p.label()))
		}
		time.Sleep(poll)
	}
}

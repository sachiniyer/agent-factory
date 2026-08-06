package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// The sandbox runtime (#2476 PR2) — the same provision-and-expose model as the
// ssh runtime, reached through the operator's OWN `ssh` invocation instead of
// af's Go client.
//
// WHY IT EXISTS. `backend = "ssh"` connects with golang.org/x/crypto/ssh and
// deliberately never reads ~/.ssh/config, so it cannot express a target that
// needs a jump host, a ProxyCommand, a bastion, or any other flag the operator
// already relies on. `sandbox_ssh` is a free-form command line — whatever
// already works in their terminal — and af runs the provision steps over it.
// Everything above the transport is shared: sandboxWorkspace (#2557) does the
// mktemp/clone/binary-stream/start-agent-server sequence against any
// sandboxShell.
//
// WHAT THIS RUNTIME OWNS, and the ssh runtime owns separately: the connection
// (there is none to hold — each step is a fresh `ssh` invocation), the tunnel (a
// long-lived `ssh -L` child rather than an in-process forward), and the reap.
//
// HOST KEYS ARE THE SSH BINARY'S PROBLEM HERE, on purpose. The operator's
// ssh_config, known_hosts and ProxyCommand are the authority, exactly as they
// are in their terminal — af adds no posture of its own, unlike `backend=ssh`
// which enforces ssh_host_key_verification because it bypasses all of that.

const (
	// sandboxTunnelReadyTimeout bounds the wait for the `ssh -L` child to make
	// the local port answer. A forward that never comes up must fail the
	// provision rather than hand back an endpoint nothing is listening on.
	sandboxTunnelReadyTimeout = 20 * time.Second
	sandboxTunnelReadyPoll    = 100 * time.Millisecond
	// sandboxTunnelWaitDelay bounds how long a killed tunnel child may take to
	// die before its pipes are force-closed. Safe HERE and nowhere else in this
	// file — see runCommand.
	sandboxTunnelWaitDelay = 2 * time.Second
)

// sshBinaryExitAmbiguous is the exit status ssh(1) uses for its OWN failures —
// "ssh could not connect", "host key changed", "ProxyCommand failed". A remote
// command that happens to exit 255 is indistinguishable from it, which is why
// a reap that sees it must report UNDETERMINED rather than a confident answer.
const sshBinaryExitAmbiguous = 255

type sandboxRuntime struct{}

func (sandboxRuntime) Provision(spec ProvisionSpec) (ProvisionResult, error) {
	cfg, err := resolveRepoConfig(spec.RepoRoot)
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("backend=sandbox: cannot resolve repo config for %q: %w", spec.RepoRoot, err)
	}
	if err := BackendConfigError(BackendSandbox, cfg); err != nil {
		return ProvisionResult{}, err
	}
	if spec.CloneURL == "" {
		return ProvisionResult{}, missingOriginError(BackendSandbox, spec.RepoRoot)
	}
	afBin, err := sshSelfBinary()
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("backend=sandbox: cannot locate the af binary to stream onto the sandbox: %w", err)
	}

	p := &sandboxProvisioner{
		spec:    spec,
		sshCmd:  strings.TrimSpace(cfg.SandboxSSH),
		afBin:   afBin,
		program: config.ResolveProgram(&cfg.Config, spec.Program),
	}
	res, err := p.provision()
	if err != nil {
		return ProvisionResult{}, p.reapProvisionFailure(err)
	}
	return res, nil
}

// sandboxProvisioner holds one provisioning's state. Unlike sshProvisioner there
// is no persistent client: every step is its own `ssh` invocation, so the only
// long-lived child is the tunnel.
type sandboxProvisioner struct {
	spec    ProvisionSpec
	sshCmd  string
	afBin   string
	program string

	sessionDir string
	remotePID  string

	tunnel   *exec.Cmd
	tunnelLn net.Listener

	reapMu  sync.Mutex
	reaped  bool
	reapErr error

	// runCommandFn is a per-instance seam so a test can drive the traps below
	// without a real sshd. Production leaves it nil.
	runCommandFn func(timeout time.Duration, script string, stdin io.Reader, combined bool) ([]byte, error)
}

func (p *sandboxProvisioner) provision() (ProvisionResult, error) {
	w := &sandboxWorkspace{shell: p, spec: p.spec, program: p.program}
	if err := w.makeSessionDir(sshShortStepTimeout); err != nil {
		return ProvisionResult{}, p.sandboxErr(err)
	}
	p.sessionDir = w.SessionDir
	if err := w.configureGit(sshShortStepTimeout); err != nil {
		return ProvisionResult{}, p.sandboxErr(err)
	}
	if err := w.cloneWorkspace(sshProvisionStepTimeout, sshShortStepTimeout); err != nil {
		return ProvisionResult{}, p.sandboxErr(err)
	}
	binary, err := os.Open(p.afBin)
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("backend=sandbox: opening the af binary %q to stream to the sandbox failed: %w", p.afBin, err)
	}
	copyErr := w.copyAfBinary(sshProvisionStepTimeout, binary)
	_ = binary.Close()
	if copyErr != nil {
		return ProvisionResult{}, p.sandboxErr(copyErr)
	}
	if err := p.verifyCopiedBinary(); err != nil {
		return ProvisionResult{}, err
	}
	if err := w.startAgentServer(sshShortStepTimeout); err != nil {
		return ProvisionResult{}, p.sandboxErr(err)
	}
	p.remotePID = w.RemotePID
	banner, err := w.readBanner(sshBannerPollTimeout, sshBannerPollInterval, sshShortStepTimeout)
	if err != nil {
		return ProvisionResult{}, p.sandboxErr(err)
	}
	localAddr, err := p.startTunnel(banner.Addr)
	if err != nil {
		return ProvisionResult{}, err
	}

	endpoint := &AgentServerEndpoint{URL: "http://" + localAddr, Token: banner.Token}
	teardown := p.reap
	log.InfoLog.Printf("sandbox runtime: session %q running via %q (dir %s), agent-server tunneled at %s",
		p.spec.Title, p.sshCmd, p.sessionDir, endpoint.URL)
	return ProvisionResult{
		Backend:  &sandboxBackend{remoteAgentBackend: remoteAgentBackend{reap: teardown}, provisioner: p},
		Endpoint: endpoint,
		Teardown: teardown,
	}, nil
}

func (p *sandboxProvisioner) sandboxErr(err error) error {
	return fmt.Errorf("backend=sandbox: %w", err)
}

// Run satisfies sandboxShell: one bounded remote command over the operator's ssh
// invocation.
//
// The command is composed as `sh -c '<sandbox_ssh> "$@"' af-sandbox <script>`
// rather than by string-concatenating the script onto the ssh command. That
// keeps the operator's own quoting intact (a ProxyCommand with embedded spaces,
// a quoted -o option) while the script reaches ssh as exactly ONE argv element,
// so nothing in a script — a repo path, a branch name — can break out and become
// another ssh flag.
func (p *sandboxProvisioner) Run(timeout time.Duration, script string, stdin io.Reader, combined bool) ([]byte, error) {
	if p.runCommandFn != nil {
		return p.runCommandFn(timeout, script, stdin, combined)
	}
	return p.runCommand(timeout, script, stdin, combined)
}

func (p *sandboxProvisioner) runCommand(timeout time.Duration, script string, stdin io.Reader, combined bool) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := p.buildRunCommand(ctx, script, stdin)

	var out []byte
	var err error
	if combined {
		out, err = cmd.CombinedOutput()
	} else {
		out, err = cmd.Output()
	}
	if ctx.Err() != nil {
		return out, fmt.Errorf("%q timed out after %s: %w", p.sshCmd, timeout, ctx.Err())
	}
	return out, err
}

// buildRunCommand composes the child. Split out so the WaitDelay rule below is
// exercised by a test against the real constructor rather than a restatement of
// it.
func (p *sandboxProvisioner) buildRunCommand(ctx context.Context, script string, stdin io.Reader) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "sh", "-c", p.sshCmd+` "$@"`, "af-sandbox", script)
	cmd.Stdin = stdin
	// TRAP 1 (#2476 handoff): WaitDelay force-closes the child's pipes after the
	// context fires. That is right for a child with no stdin payload, and WRONG
	// when stdin is non-nil: the binary stream would be cut mid-copy, ssh would
	// still exit 0 for the bytes it did send, and a TRUNCATED `af` would land on
	// the sandbox reporting success — failing later somewhere that points nowhere
	// near the copy. So it is set only when there is nothing to stream, and
	// verifyCopiedBinary checks the copy independently.
	if stdin == nil {
		cmd.WaitDelay = sandboxTunnelWaitDelay
	}
	return cmd
}

// verifyCopiedBinary re-checks the streamed `af` ON THE SANDBOX, because the
// copy's failure mode is silent. A short write, a cut stream, or a full disk all
// leave a binary that exists and is executable but is not the one we sent; the
// next step would then fail with "exec format error" or a version mismatch far
// from the cause. Comparing the byte count against the local file turns that
// into an error naming the copy.
func (p *sandboxProvisioner) verifyCopiedBinary() error {
	local, err := os.Stat(p.afBin)
	if err != nil {
		return fmt.Errorf("backend=sandbox: cannot stat the local af binary %q: %w", p.afBin, err)
	}
	remotePath := p.sessionDir + "/" + sshAfBinaryName
	out, err := p.Run(sshShortStepTimeout, "wc -c < "+shellQuoteSandbox(remotePath), nil, false)
	if err != nil {
		return fmt.Errorf("backend=sandbox: cannot verify the streamed af binary on the sandbox: %w", err)
	}
	got := strings.TrimSpace(string(out))
	want := fmt.Sprintf("%d", local.Size())
	if got != want {
		return fmt.Errorf("backend=sandbox: the af binary streamed to %s is %s bytes but the local one is %s — "+
			"the copy was truncated, so the sandbox holds a binary that would fail later with an unrelated error; "+
			"retry the session", remotePath, got, want)
	}
	return nil
}

// startTunnel runs `<sandbox_ssh> -N -L <local>:<remote>` as a long-lived child.
//
// The local port is chosen by binding :0 and closing it, rather than asking ssh
// for one: `ssh -L` has no "pick a port and tell me" form, so af must name the
// port it will dial. The listener is closed before ssh binds it, which is a
// small race — the readiness probe below is what actually establishes that the
// forward came up, so a lost race fails the provision rather than handing back a
// dead endpoint.
func (p *sandboxProvisioner) startTunnel(remoteAddr string) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("backend=sandbox: opening a local port for the tunnel failed: %w", err)
	}
	localAddr := ln.Addr().String()
	_ = ln.Close()

	forward := fmt.Sprintf("%s:%s", localAddr, remoteAddr)
	cmd := exec.Command("sh", "-c", p.sshCmd+` -N -L "$1"`, "af-sandbox", forward)
	// No stdin payload here, so WaitDelay is correct and wanted: a tunnel that
	// ignores its kill must not hold af's pipes open forever. This is the child
	// TRAP 1 says it belongs on.
	cmd.WaitDelay = sandboxTunnelWaitDelay
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("backend=sandbox: starting the ssh tunnel failed: %w", err)
	}
	p.tunnel = cmd

	if err := waitForSandboxTunnelWithin(localAddr, sandboxTunnelReadyTimeout, sandboxTunnelReadyPoll); err != nil {
		return "", err
	}
	return localAddr, nil
}

// waitForSandboxTunnel probes the local end until it answers. Without this the
// provision would return an endpoint whose first request races the forward.
func waitForSandboxTunnelWithin(localAddr string, timeout, poll time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("tcp", localAddr, poll)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("backend=sandbox: the ssh tunnel to %s never started listening within %s — "+
				"check that the sandbox_ssh command works by hand and that the sandbox permits port forwarding",
				localAddr, timeout)
		}
		time.Sleep(poll)
	}
}

func (p *sandboxProvisioner) reapProvisionFailure(provisionErr error) error {
	reapErr := p.reap()
	if reapErr == nil {
		return provisionErr
	}
	return fmt.Errorf("backend=sandbox: provisioning failed and cleanup of its partial workspace (dir %q) did not complete; inspect it before retrying: %w",
		p.sessionDir, errors.Join(provisionErr, reapErr))
}

// reap stops the tunnel child, kills the remote agent-server, and removes the
// session dir.
//
// TRAP 2 (#2476 handoff): ssh(1) exits 255 for its OWN failures, and a remote
// command that exits 255 is indistinguishable from it. So 255 is reported as
// UNDETERMINED (ErrWorkspaceStateUnknown) rather than as a completed reap — the
// daemon then RETAINS the record and retries, instead of deleting a row whose
// sandbox may still be running. Only a clean exit latches.
func (p *sandboxProvisioner) reap() error {
	p.reapMu.Lock()
	defer p.reapMu.Unlock()
	if p.reaped {
		return p.reapErr
	}
	p.stopTunnel()
	if p.sessionDir == "" {
		p.reaped = true
		return nil
	}

	script := "rm -rf " + shellQuoteSandbox(p.sessionDir)
	if p.remotePID != "" {
		script = "kill " + p.remotePID + " 2>/dev/null; " + script
	}
	out, err := p.Run(sshReapTimeout, script, nil, true)
	if err == nil {
		p.reaped = true
		p.reapErr = nil
		return nil
	}

	reapErr := fmt.Errorf("backend=sandbox: reaping %q failed: %s: %w",
		p.sessionDir, strings.TrimSpace(string(out)), err)
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == sshBinaryExitAmbiguous {
		// ssh's own failure, or a remote command that exited 255 — undecidable
		// from here. Do NOT latch: the next poll must actually retry.
		reapErr = fmt.Errorf("%w: %w", ErrWorkspaceStateUnknown, reapErr)
		log.ErrorLog.Printf("sandbox runtime: %v", reapErr)
		return reapErr
	}
	if errors.Is(err, context.DeadlineExceeded) {
		reapErr = fmt.Errorf("%w: %w", ErrWorkspaceStateUnknown, reapErr)
		log.ErrorLog.Printf("sandbox runtime: %v", reapErr)
		return reapErr
	}
	// The remote command ANSWERED with a definite non-255 status: the reap
	// completed and told us something, so it latches.
	p.reaped = true
	p.reapErr = reapErr
	log.ErrorLog.Printf("sandbox runtime: %v", reapErr)
	return reapErr
}

func (p *sandboxProvisioner) stopTunnel() {
	if p.tunnel == nil || p.tunnel.Process == nil {
		return
	}
	_ = p.tunnel.Process.Kill()
	_ = p.tunnel.Wait()
	p.tunnel = nil
	if p.tunnelLn != nil {
		_ = p.tunnelLn.Close()
		p.tunnelLn = nil
	}
}

// shellQuoteSandbox single-quotes a path for the remote shell.
func shellQuoteSandbox(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

type sandboxBackend struct {
	remoteAgentBackend
	provisioner *sandboxProvisioner
}

func (b *sandboxBackend) Type() string { return "sandbox" }

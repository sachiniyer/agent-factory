package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
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
		Backend: &sandboxBackend{
			remoteAgentBackend: remoteAgentBackend{reap: teardown},
			provisioner:        p,
			cleanup: &SandboxRuntimeCleanupData{
				SSHCommand: p.sshCmd,
				SessionDir: p.sessionDir,
				RemotePID:  p.remotePID,
			},
		},
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
	// Its own process group, so the deadline tears down the whole tree rather
	// than the `sh` alone. exec.CommandContext's default Cancel kills only the
	// direct child, and WaitDelay merely closes the capture pipes — neither
	// touches the ssh client, a ProxyCommand, or any helper it spawned. Those are
	// exactly what this backend exists to support, so without this a timed-out
	// clone or binary copy keeps running on the sandbox while provisioning starts
	// cleanup, racing remote work against its own deletion and orphaning local
	// transport processes. Same shape as boundedTmuxCommand and the hook runtime.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Negative pid targets the whole group. A group already gone (ESRCH) maps
		// to os.ErrProcessDone, which Wait ignores rather than reporting as a
		// command failure.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	// TRAP 1, with a correction the handoff's rule did not anticipate.
	//
	// The recorded guidance was "WaitDelay on the tunnel child, NEVER on the
	// binary-copy child", because force-closing the pipe mid-copy truncates the
	// stream while ssh still exits 0 — a short `af` that reports success and fails
	// much later somewhere unrelated. That reasoning holds ONLY while nothing else
	// catches a truncated copy.
	//
	// Two things make bounding the right call here instead. verifyCopiedBinary now
	// compares the byte count ON the sandbox, so a truncated copy fails AT the copy
	// rather than silently. And a zero WaitDelay is not neutral: os/exec documents
	// that it makes Wait block until every orphaned descriptor-holder closes, which
	// for this backend is precisely the ProxyCommand/wrapper it exists to support —
	// so the copy's advertised timeout would not bound provisioning at all.
	//
	// Once the deadline has passed the copy has already failed, so there is nothing
	// left to protect by waiting. Bound it, and let the size check reject whatever
	// partial result the kill leaves behind.
	cmd.WaitDelay = sandboxTunnelWaitDelay
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
	// -o ExitOnForwardFailure=yes is load-bearing, not hygiene. The local port is
	// reserved by binding :0 and closing it, so another process can win it in the
	// gap. OpenSSH defaults this to NO (`ssh -G` confirms), which would leave this
	// -N child alive after a FAILED bind — and the readiness probe below would then
	// connect to whatever DID win the port and report a healthy tunnel owned by an
	// unrelated process. With it, ssh exits on the failed forward and the probe
	// times out honestly.
	cmd := exec.Command("sh", "-c", p.sshCmd+` -o ExitOnForwardFailure=yes -N -L "$1"`, "af-sandbox", forward)
	// Its own group so stopTunnel can kill the ssh client and any ProxyCommand
	// helper together, not just the `sh` that launched them.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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

	nonce, nonceErr := sandboxReapNonce()
	if nonceErr != nil {
		return fmt.Errorf("backend=sandbox: cannot generate a reap challenge: %w", nonceErr)
	}
	script, expect := p.reapScript(nonce)
	out, err := p.Run(sshReapTimeout, script, nil, true)
	// Only the UPPERCASED response proves the far side executed: the script
	// carries the lowercase challenge, so a wrapper logging its argv cannot forge
	// this.
	answered := strings.Contains(string(out), expect)
	if err == nil && answered {
		p.reaped = true
		p.reapErr = nil
		return nil
	}

	reapErr := fmt.Errorf("backend=sandbox: reaping %q failed: %s: %w",
		p.sessionDir, strings.TrimSpace(string(out)), err)

	// A reap may latch ONLY when the remote side demonstrably ran. Three ways it
	// may not have, all of which must retain the record and retry:
	//
	//  1. ssh exit 255 — its own failure ("could not connect", "host key
	//     changed"), indistinguishable from a remote command that exits 255.
	//  2. NO SENTINEL — sandbox_ssh is a free-form shell command, so a wrapper or
	//     a missing prerequisite can fail LOCALLY (127 command-not-found, 126 not
	//     executable, or any status a wrapper picks) before ssh ever runs. Its
	//     status is then a statement about the daemon host, not the sandbox, and
	//     latching it would retire a record whose agent-server is still running.
	//     The sentinel is echoed by the remote shell as the script's last act, so
	//     seeing it is positive proof the far side executed.
	//  3. The deadline expired mid-reap.
	switch {
	case errors.Is(err, context.DeadlineExceeded):
	case isSandboxAmbiguousExit(err):
	case !answered:
	default:
		// The remote side ran and reported a definite status: that is an ANSWER,
		// so it latches and the record may be retired.
		p.reaped = true
		p.reapErr = reapErr
		log.ErrorLog.Printf("sandbox runtime: %v", reapErr)
		return reapErr
	}
	reapErr = fmt.Errorf("%w: %w", ErrWorkspaceStateUnknown, reapErr)
	log.ErrorLog.Printf("sandbox runtime: %v", reapErr)
	return reapErr
}

// The reap's proof-of-execution marker.
//
// A fixed string echoed verbatim does NOT work, and the first version of this
// was wrong for exactly that reason: the marker also appears in the SCRIPT handed
// to sandbox_ssh, so a supported wrapper that logs its argv (`set -x`, or any
// wrapper that traces what it runs) reproduces the marker in combined output
// while failing LOCALLY, before ssh ever runs. Matching it then latched a
// wrapper failure as a definite remote answer — the precise bug the sentinel was
// added to prevent.
//
// So the marker is CHALLENGE-RESPONSE, not a password. af generates a lowercase
// hex nonce per reap; the remote shell must return it UPPERCASED. The script text
// contains only the lowercase form, so a wrapper echoing its own argv can never
// produce the uppercase answer — only actually running `tr` on the far side can.
// `tr` is POSIX and present on every shell this backend can reach.
const sandboxReapMarkerPrefix = "af-sandbox-reaped-"

// sandboxReapChallenge returns the lowercase nonce to send and the uppercase
// response that only genuine remote execution can produce.
func sandboxReapChallenge(nonce string) (send, expect string) {
	return sandboxReapMarkerPrefix + strings.ToLower(nonce),
		strings.ToUpper(sandboxReapMarkerPrefix + nonce)
}

// reapScript kills the agent-server by IDENTITY, not by bare PID, then removes
// the session dir and prints the sentinel.
//
// The identity check is shared with the ssh runtime (remotePIDIdentityKillScript):
// a numeric PID can be recycled between provision and teardown, and a blind
// `kill` would then signal an unrelated process on the operator's own host. It
// verifies argv[0] is this session's unique af path before either signal.
//
// The kill's status is deliberately NOT discarded — an earlier draft wrote
// `kill …; rm …`, which let a FAILED signal still report a clean reap while the
// server kept running. The rm is chained so the directory is only removed once
// the process it belongs to is gone.
func (p *sandboxProvisioner) reapScript(nonce string) (script, expect string) {
	send, expect := sandboxReapChallenge(nonce)
	rm := "rm -rf " + shellQuoteSandbox(p.sessionDir)
	// The remote shell must TRANSFORM the challenge, so the answer never appears
	// in the text a logging wrapper would echo.
	respond := "printf '%s' " + shellQuoteSandbox(send) + " | tr 'a-z' 'A-Z'"
	if p.remotePID == "" {
		return rm + " && " + respond, expect
	}
	kill := remotePIDIdentityKillScript(p.remotePID, p.sessionDir+"/"+sshAfBinaryName)
	return "{ " + kill + "; } && " + rm + " && " + respond, expect
}

// sandboxReapNonce makes each reap's challenge unique, so a marker captured from
// an earlier run's log cannot satisfy a later one.
func sandboxReapNonce() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// isSandboxAmbiguousExit reports ssh(1)'s own 255, which cannot be told apart
// from a remote command exiting 255.
func isSandboxAmbiguousExit(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == sshBinaryExitAmbiguous
}

func (p *sandboxProvisioner) stopTunnel() {
	if p.tunnel == nil || p.tunnel.Process == nil {
		return
	}
	// Negative pid: the whole group, so a ProxyCommand helper does not outlive
	// the client it was opened for.
	if err := syscall.Kill(-p.tunnel.Process.Pid, syscall.SIGKILL); err != nil {
		_ = p.tunnel.Process.Kill()
	}
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
	// cleanup is the immutable teardown identity, persisted so a reap that could
	// not prove it ran is retryable after a daemon restart.
	cleanup *SandboxRuntimeCleanupData
}

func (b *sandboxBackend) Type() string { return "sandbox" }

package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/internal/runtimetoken"
	"github.com/sachiniyer/agent-factory/log"
)

// The sandbox runtime (#2476 PR2) — the same provision-and-expose model as the
// ssh runtime, reached through the operator's OWN `ssh` invocation.
//
// WHY IT EXISTS. `backend = "ssh"` reads no ssh configuration file — by design,
// then and now — so it cannot express a target that needs a jump host, a
// ProxyCommand, a bastion, or any other flag the operator already relies on.
// `sandbox_ssh` is a free-form command line — whatever already works in their
// terminal — and af runs the provision steps over it. That is still the ONLY way
// to reach such a target, and #3052 deliberately did not change it.
//
// SINCE #3052 BOTH BACKENDS RUN ON THIS TYPE. `backend = "ssh"` composes an
// ssh(1) command from its `ssh.*` settings and provisions through here, so there
// is one transport instead of two that can disagree about an address (#3044).
//
// WHAT SEPARATES THEM IS WHO DECIDES, not which ssh client runs:
//
//   - `sandbox_ssh` — the OPERATOR writes the command. Their ssh_config,
//     known_hosts and ProxyCommand are the whole authority, exactly as in their
//     terminal, and af adds no posture of its own.
//   - `backend = "ssh"` — AF writes the command from `ssh.*` settings, passes
//     `-F none` so no configuration file is read at all, and enforces
//     `ssh_host_key_verification` itself. An external file cannot relax a posture
//     af is responsible for; see ssh_command.go, which measures why pinning
//     values was not enough.
//
// Everything above the transport is shared either way: sandboxWorkspace (#2557)
// does the mktemp/clone/binary-stream/start-agent-server sequence against any
// sandboxShell, and this type owns the tunnel (a long-lived `ssh -L` child) and
// the reap for both.

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
	adopting := spec.Adopt != nil
	if adopting {
		// The archived row's own recorded transport reaches the host, whatever the
		// current sandbox.ssh says (#5571 W8.6); there is nothing to clone.
		if spec.Adopt.Transport == nil || strings.TrimSpace(spec.Adopt.Transport.SSHCmd) == "" {
			return ProvisionResult{}, errors.New("backend=sandbox: the archived row recorded no transport to reach its preserved workspace")
		}
	} else {
		if err := BackendConfigError(BackendSandbox, cfg); err != nil {
			return ProvisionResult{}, err
		}
		if spec.CloneURL == "" {
			return ProvisionResult{}, missingOriginError(BackendSandbox, spec.RepoRoot)
		}
	}
	afBin, err := sshSelfBinary()
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("backend=sandbox: cannot locate the af binary to stream onto the sandbox: %w", err)
	}

	p := &sandboxProvisioner{
		spec:   spec,
		sshCmd: adoptedTransportOr(spec, strings.TrimSpace(cfg.SandboxSSH)),
		afBin:  afBin,
		// The wire program is the configured name and flags — never af's
		// auto-detected daemon path, which the sandbox's filesystem does not
		// have. The agent-server resolves it on the host's login PATH (#5571).
		program: remoteProgramForHost(cfg, spec.Program),
		// sandbox.binary is the operator's daemon-local override for air-gapped
		// or unbuilt hosts (#5571 W2.2) — global-only config, so it can never be
		// repo-supplied.
		sandboxBinary: strings.TrimSpace(cfg.Config.SandboxBinary),
	}
	res, err := p.provision()
	if err != nil {
		return p.failedProvision(err)
	}
	return res, nil
}

// sandboxProvisioner holds one provisioning's state, for `backend = "sandbox"`
// and (since #3052) `backend = "ssh"` alike. There is no persistent client: every
// step is its own `ssh` invocation, so the only long-lived child is the tunnel.
type sandboxProvisioner struct {
	spec   ProvisionSpec
	sshCmd string
	// readinessKey is where this provisioner's prerequisite probe records its
	// verdict. Empty means the ssh command, the key an unnamed sandbox.ssh host
	// has always used; a session on a LISTED host sets HostReadinessKey(name) so
	// create, CheckHost and the supervisor refresh all write the one key the
	// catalog reads (#5840).
	readinessKey string
	afBin        string
	program      string
	// sandboxBinary is the operator's sandbox.binary override — a daemon-local
	// af build to stream instead of the daemon's own binary or a release
	// fetch (#5571 W2.2). Empty in production means "platform selection".
	sandboxBinary string
	// backendLabel is the name the OWNING backend puts on this transport's errors.
	// Empty means "sandbox". Since #3052 `backend = "ssh"` provisions through this
	// same type, and an auth, host-key, clone, tunnel or cleanup failure reported
	// as `backend=sandbox` sends the operator to a config key they never set.
	//
	// NOT named `backend`: it holds a LABEL, not a Backend, and the #2096 source
	// guard is deliberately syntactic — it asks who names that field, package-wide.
	// A second field sharing the name would make the guard's question ambiguous,
	// and the honest fix is the precise name rather than an allowlist entry.
	backendLabel string

	sessionDir string
	remotePID  string
	// hostProgram is the program the sandbox host actually resolved and bound,
	// reported back in the startup banner (#5571 W2.1). Empty until the banner
	// arrives; remoteAgentBackend falls back to the wire program without it.
	hostProgram string
	// launchAttempted records that provision() reached the startAgentServer
	// call. An empty remotePID after that point is not the "nothing to signal"
	// pre-launch case (a clone/binary-copy failure before startAgentServer, where
	// no nohup'd server exists): the launch ran, and a wrapper that suppressed
	// `echo $!` left no PID to reap by even though the agent-server may still be
	// detached and running. reap() uses this to fail closed on an empty post-launch
	// PID rather than `rm -rf`-ing the workspace of a possibly-running server.
	launchAttempted bool

	// The reattach half of the runtime identity (#5571 W1.1), recorded at
	// provision so the backend's cleanup handle — and therefore the persisted
	// row — carries everything reattach and reap need:
	//
	//	tokenRef           the opaque nonce the row stores; the daemon's
	//	                   runtime-identity key derives the agent-server's
	//	                   expected bearer token from it. Never the token.
	//	remoteAddr         the agent-server's loopback address on the sandbox
	//	                   (the banner's addr) — the -L forward's destination.
	//	agentServerVersion the version the remote binary reported in its banner.
	tokenRef string
	// launchScript is the exact command that started the agent-server, persisted
	// so a crashed one can be relaunched from the row alone (W1.4). It names the
	// session dir's env files and flags, never a secret.
	launchScript string
	adoptPending bool // a restarted server has not adopted its workspace yet (W1.4)
	// persistIdentity is the daemon-installed durable write of the row a W1.4
	// restart fires before it spawns, before it adopts and when adoption ends.
	// In memory only; the daemon reinstalls it per reattach attempt.
	persistIdentity    func(*Instance) error
	remoteAddr         string
	agentServerVersion string

	// tunnel is the currently published generation of the supervised ssh
	// transport (#5571 W1.2): nil between a death and its re-dial, and after
	// stopTunnel. forward/localAddr pin the -L contract — localAddr is the
	// endpoint every consumer dials, fixed at provision and re-bound verbatim
	// on every re-dial so a consumer's cached URL is never stale — and forward
	// embeds the remote agent-server's listen address it targets. reverseFwds
	// is W3.4's -R slot (the in-sandbox callback), set at provision when a
	// credential was granted; each element is composed into the spawn as
	// `-R "$N"` so the reverse forward gets the same ExitOnForwardFailure
	// fatality, group teardown, and supervision.
	tunnel      *tunnelChild
	forward     string
	localAddr   string
	reverseFwds []string
	// callbackRef is the reference the granted callback credential derives from
	// (W3.6). It persists with the runtime identity, together with the -R spec in
	// reverseFwds, so a reattach after a daemon restart re-registers the credential
	// and re-establishes the same forward. Empty when no callback was granted.
	callbackRef string
	// callbackSocket is the sandbox-side socket the -R slot creates, removed
	// before a re-dial so a socket file a killed sshd left behind cannot make the
	// bind fail forever (the client would otherwise retry a dead path).
	callbackSocket string

	// account is the workspace that carries this provision's synced account, for
	// the teardown's write-back and removal (W7.3). nil without an account.
	account *sandboxWorkspace

	reapMu  sync.Mutex
	reaped  bool
	reapErr error
	// reaping tells StopAllSandboxTunnels (#5564) that reap() owns this
	// provisioner's stopTunnel, so a daemon exit skips it rather than park
	// behind the remote teardown's timeouts. reap's own stopTunnel is its
	// first act under reapMu, so skipping loses nothing.
	reaping atomic.Bool
	// stopped latches in stopTunnel under reapMu (#5564): a provisioner whose
	// tunnel was stopped is spent. A tunnel spawn that completes after the
	// stop ran — a daemon exit that overtook the provision — must be killed
	// by its spawner rather than published to a provisioner no remaining stop
	// will reach. stoppedCh is closed under the same latch so the transport
	// supervisor can wake out of a re-dial backoff immediately rather than
	// sleep through a teardown (#5571 W1.2); it is built lazily by
	// stoppedChanLocked because reap-only provisioners never spawn one.
	stopped   bool
	stoppedCh chan struct{}

	// runCtx, when set, parents every command Run starts, so cancelling it kills
	// the ssh process group. nil means context.Background (a create, which must
	// not be torn down by anything but its own timeout). Set only by an advisory
	// probe (CheckHost), which a daemon shutdown has to be able to abandon.
	runCtx context.Context

	// runCommandFn is a per-instance seam so a test can drive the traps below
	// without a real sshd. Production leaves it nil.
	runCommandFn func(timeout time.Duration, script string, stdin io.Reader, combined bool) ([]byte, error)
	// startTunnelFn is the same kind of seam for the -L child: a provision()
	// test substitutes it rather than needing a real forward. nil ⇒ startTunnel.
	startTunnelFn func(remoteAddr string) (localAddr string, err error)
}

// hostReadinessKey is the readiness-cache key this provisioner's probe records
// under: the listed host's canonical key when it has one, else the ssh command.
func (p *sandboxProvisioner) hostReadinessKey() string {
	if p.readinessKey != "" {
		return p.readinessKey
	}
	return p.sshCmd
}

func (p *sandboxProvisioner) provision() (ProvisionResult, error) {
	w := &sandboxWorkspace{shell: p, spec: p.spec, program: p.program}
	// Before anything is created on the host: a missing tool refuses the create
	// with its fix instead of failing a later step half-provisioned (#5571 W2.3).
	if err := w.checkHostPrereqs(p.hostReadinessKey(), sshShortStepTimeout); err != nil {
		return ProvisionResult{}, p.sandboxErr(err)
	}
	if p.spec.Adopt != nil {
		if err := p.beginAdoption(w); err != nil {
			return ProvisionResult{}, err
		}
	} else {
		if err := w.makeSessionDir(sshShortStepTimeout); err != nil {
			return ProvisionResult{}, p.sandboxWriteErr(err)
		}
		w.resolveSessionDirSymlinks(sshShortStepTimeout)
		p.sessionDir = w.SessionDir
		// The identity is repo-local (#5571 W2.7), so it is written after the clone
		// that creates the repository.
		if err := w.cloneWorkspace(sshProvisionStepTimeout, sshShortStepTimeout); err != nil {
			return ProvisionResult{}, p.sandboxWriteErr(err)
		}
		if err := w.configureGit(sshShortStepTimeout); err != nil {
			return ProvisionResult{}, p.sandboxWriteErr(err)
		}
	}
	// The binary stage is platform-aware (#5571 W2.2): probe the host's uname,
	// pick the af build it can actually run (sandbox.binary override / the
	// daemon's own binary / a sha256-verified release artifact), install it
	// into the host's shared cache, and run `af version` on the host before
	// any agent-server launch is attempted.
	if err := p.stageRemoteBinary(w); err != nil {
		return ProvisionResult{}, p.sandboxWriteErr(err)
	}
	// Mint the session's runtime identity BEFORE the agent-server launches
	// (#5571 W1.1): the derived token reaches it only through the 0600 file
	// beside its binary, and the ref is what the row persists so a restart can
	// re-derive the credential. Minting needs the daemon's runtime-identity key
	// in AF_HOME — an unwritable home fails the provision HERE rather than
	// stranding an agent-server whose expected token nobody recorded.
	ref, err := runtimetoken.NewTokenRef()
	if err != nil {
		return ProvisionResult{}, p.sandboxErr(err)
	}
	key, err := runtimetoken.EnsureKey()
	if err != nil {
		return ProvisionResult{}, p.sandboxErr(err)
	}
	token, err := runtimetoken.DeriveAgentServerToken(key, ref)
	if err != nil {
		return ProvisionResult{}, p.sandboxErr(err)
	}
	p.tokenRef, w.tokenRef, w.agentServerToken = ref, ref, token
	if err := w.writeRuntimeIdentity(sshShortStepTimeout); err != nil {
		return ProvisionResult{}, p.sandboxWriteErr(err)
	}
	// Re-check the callback credential at the LAST step that can still keep a
	// dead grant out of the launched agent-server's environment (#5571 W3.1,
	// PR #5577 review — see revalidateCallbackBeforeEnv for why this point).
	callbackLost := w.revalidateCallbackBeforeEnv()
	// The callback's carrier is decided BEFORE its env is committed: a granted
	// credential with no -R to ride is as dead as a revoked one, so it degrades
	// the same way rather than leaving the agent a URL nothing answers (W3.4).
	reverse, carrierLost := w.callbackReverseForward()
	if callbackLost == "" {
		callbackLost = carrierLost
	}
	p.reapMu.Lock()
	p.reverseFwds, p.callbackRef = nil, ""
	p.callbackSocket = ""
	if reverse != "" {
		p.reverseFwds = []string{reverse}
		p.callbackRef = w.spec.CallbackRef
		p.callbackSocket = w.CallbackSocketPath()
	}
	p.reapMu.Unlock()
	p.clearStaleAdoptedCallbackSocket()
	if err := w.writePolicy(sshShortStepTimeout); err != nil {
		return ProvisionResult{}, p.sandboxWriteErr(err)
	}
	if err := w.writeCallbackEnv(sshShortStepTimeout); err != nil {
		return ProvisionResult{}, p.sandboxWriteErr(err)
	}
	// W7.3: the session-scoped account, before the agent-server can launch the
	// agent. Refuses (sending nothing) when the target is not a listed host.
	if err := w.syncAccountIn(sshProvisionStepTimeout); err != nil {
		return ProvisionResult{}, p.sandboxWriteErr(err)
	}
	agentConfigSkipped, err := w.syncAgentConfigIn(sshProvisionStepTimeout)
	if err != nil {
		return ProvisionResult{}, p.sandboxWriteErr(err)
	}
	p.account = w
	// Reached only past the pre-launch steps (clone, binary copy, callback env),
	// so a startAgentServer failure here is not the pre-launch empty-PID case: a
	// nohup'd agent-server may have started even when the captured PID is empty or
	// garbage. Set the flag before the call so the reap refuses on either.
	p.launchAttempted = true
	if err := w.startAgentServer(sshShortStepTimeout); err != nil {
		p.remotePID = w.RemotePID
		return ProvisionResult{}, p.sandboxWriteErr(err)
	}
	p.remotePID, p.launchScript = w.RemotePID, w.launchScript
	banner, err := w.readBanner(sshBannerPollTimeout, sshBannerPollInterval, sshShortStepTimeout)
	if err != nil {
		return ProvisionResult{}, p.sandboxErr(err)
	}
	// The banner must echo the credential we supplied. Any other value means the
	// remote agent-server did not read af-server-token — the streamed binary was
	// stale or foreign — and would gate on a different, shared ambient
	// credential, which is exactly the credential-confusion this exists to
	// remove. The message names the file, never either token.
	if banner.Token != token {
		return ProvisionResult{}, p.sandboxErr(fmt.Errorf(
			"the remote agent-server reported a different bearer token than the session-scoped one provisioned for it; the streamed binary did not read %s",
			runtimetoken.TokenFileName))
	}
	// The banner's addr is the agent-server's own loopback port on the sandbox —
	// the destination a reattach's -L forward re-dials — and its version is the
	// remote binary's self-report. Both persist with the cleanup handle.
	p.remoteAddr, p.agentServerVersion = banner.Addr, banner.Version
	// The banner's program is the command the HOST resolved and bound — the
	// true runtime evidence (#5108); the wire value is only its pre-resolution
	// form, kept as the fallback for a banner from an older agent-server.
	p.hostProgram = banner.Program
	if p.hostProgram == "" {
		p.hostProgram = p.program
	}
	localAddr, err := p.startTunnel(banner.Addr)
	if err != nil {
		return ProvisionResult{}, err
	}

	if err := p.awaitAdoptedServer(localAddr, token); err != nil {
		return ProvisionResult{}, err
	}

	// The endpoint presents the token WE minted — identical to the banner's per
	// the check above, but the provenance matters: the daemon must never adopt
	// a credential the remote happened to report.
	endpoint := &AgentServerEndpoint{URL: "http://" + localAddr, Token: token}
	teardown := p.reap
	log.InfoLog.Printf("sandbox runtime: session %q running via %q (dir %s), agent-server tunneled at %s",
		p.spec.Title, p.sshCmd, p.sessionDir, endpoint.URL)
	return ProvisionResult{
		// Only the branch this runtime actually handed the agent-server via
		// --branch is the one it ran on (#5971). A runtime that does not take
		// the daemon's name leaves this empty rather than guess.
		Branch: strings.TrimSpace(p.spec.Branch),
		Backend: &sandboxBackend{
			// The command the sandbox's agent-server was bound to launch
			// (#5108) — recorded as runtime evidence when the launch RPC
			// returns. The host resolved it (#5571 W2.1), so the banner's
			// value is the truth, not the wire program af sent.
			remoteAgentBackend: remoteAgentBackend{resolvedProgram: p.hostProgram},
			provisioner:        p,
			cleanup: &SandboxRuntimeCleanupData{
				SSHCommand:         p.sshCmd,
				SessionDir:         p.sessionDir,
				RemotePID:          p.remotePID,
				RemoteAddr:         p.remoteAddr,
				AgentServerVersion: p.agentServerVersion,
				TokenRef:           p.tokenRef,
				LaunchScript:       p.launchScript,
				CallbackRef:        p.callbackRef,
				CallbackReverse:    p.callbackReverse(),
				AccountSyncHost:    p.accountSyncHost(),
			},
		},
		Endpoint: endpoint,
		Teardown: teardown,
		// A credential that died mid-provision (the re-check above, before the
		// env commit) is reported here — the caller's own revalidation names the
		// residual window after launch, and the later write must not clobber an
		// earlier, more precise reason.
		CallbackUnavailable: callbackLost,
		// Every agent-config file the account-scope placement refused, so the
		// session row can warn rather than appear fully provisioned (#6337).
		AgentConfigSkipped: agentConfigSkipped,
	}, nil
}

// label is the backend name this provisioner's errors carry. Defaulting the zero
// value to "sandbox" keeps every existing construction site correct without a
// flag day, while the ssh backend sets it explicitly.
func (p *sandboxProvisioner) label() string {
	if p.backendLabel == "" {
		return string(BackendSandbox)
	}
	return p.backendLabel
}

func (p *sandboxProvisioner) sandboxErr(err error) error {
	return fmt.Errorf("backend=%s: %w", p.label(), err)
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
	parent := p.runCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	cmd := p.buildRunCommand(ctx, script, stdin)

	var out []byte
	var err error
	if combined {
		out, err = cmd.CombinedOutput()
	} else {
		out, err = cmd.Output()
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		// The child exited cleanly (a non-zero exit surfaces as an *exec.ExitError,
		// not ErrWaitDelay) and only a descendant held the capture pipe open past
		// WaitDelay. The output is already complete, so this is not a failure —
		// consistent with backend_docker.go and doctor/remote.go, which normalize
		// a bare ErrWaitDelay to nil before any deadline check. (Previously this
		// was after the deadline check and had no ErrWaitDelay carve-out, so an
		// orphaned pipe-holder made the unconditional ctx.Err() check rewrite a
		// completed command as context.DeadlineExceeded, misrouting reap's
		// sentinel-present case into ErrWorkspaceStateUnknown instead of latching.)
		err = nil
	}
	if err != nil && ctx.Err() != nil {
		return out, fmt.Errorf("%q timed out after %s: %w", p.sshCmd, timeout, ctx.Err())
	}
	return out, err
}

// buildRunCommand composes the child. Split out so the WaitDelay rule below is
// exercised by a test against the real constructor rather than a restatement of
// it.
// remoteShScript wraps a provisioning or reap script as an explicit POSIX
// `sh -c` invocation. ssh hands its command argument to the remote account's
// LOGIN shell, so a bare script is parsed by fish or csh when that is what the
// account uses — and every `&&`, `$(...)`, `$!` and `mktemp -d "$HOME/..."` in
// these scripts is POSIX. The wrapper is a single-quoted word, which fish and
// POSIX shells both read the same way (#5571 W2.9).
func remoteShScript(script string) string {
	return "sh -c " + shellQuote(script)
}

func (p *sandboxProvisioner) buildRunCommand(ctx context.Context, script string, stdin io.Reader) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "sh", "-c", p.sshCmd+` "$@"`, "af-sandbox", remoteShScript(script))
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
	// Two things make bounding the right call here instead. installRemoteBinary
	// now compares the sha256 ON the sandbox (#5571 W2.2), so a truncated copy
	// fails AT the copy rather than silently. And a zero WaitDelay is not neutral:
	// os/exec documents
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

func (p *sandboxProvisioner) reapProvisionFailure(provisionErr error) error {
	// Probe BEFORE the reap: removing the partial workspace frees space and
	// could mask the very condition being measured (#5382 item 1). Doing it here
	// — the one failure path sandbox, ssh and hook provisioning all share —
	// keeps the classification identical across them (#5686).
	if evidence := p.remoteCapacityEvidence(provisionErr); evidence != nil {
		provisionErr = errors.Join(provisionErr, evidence)
	}
	reapErr := p.reap()
	if reapErr == nil {
		return provisionErr
	}
	return fmt.Errorf("backend=%s: provisioning failed and cleanup of its partial workspace (dir %q) did not complete; inspect it before retrying: %w",
		p.label(), p.sessionDir, errors.Join(provisionErr, reapErr))
}

// reap stops the tunnel child, kills the remote agent-server, and removes the
// session dir.
//
// TRAP 2 (#2476 handoff): ssh(1) exits 255 for its OWN failures, and a remote
// command that exits 255 is indistinguishable from it. So 255 is reported as
// UNDETERMINED (ErrWorkspaceStateUnknown) rather than as a completed reap — the
// daemon then RETAINS the record and retries, instead of deleting a row whose
// sandbox may still be running. Only a clean exit latches.
//
// Every PRE-FLIGHT failure reports the same way, for the same reason: it says
// something about this host, not about the sandbox. A nonce that cannot be
// generated (#3449) is one of them — the script is never built, so ssh never
// runs and the sandbox is untouched.
func (p *sandboxProvisioner) reap() error {
	// Advertised before the lock: a daemon-exit StopAllSandboxTunnels reads
	// this to leave the tunnel stop to us rather than wait out the remote
	// teardown below, and our own stopTunnel is this body's first act under
	// reapMu, so the claim is never empty.
	p.reaping.Store(true)
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

	if p.remotePID != "" && !positivePID(p.remotePID) {
		reapErr := fmt.Errorf("%w: backend=%s: refusing to reap %q with invalid remote PID %q — the agent-server may still be running; inspect before retrying",
			ErrWorkspaceStateUnknown, p.label(), p.sessionDir, p.remotePID)
		log.ErrorLog.Printf("sandbox runtime: %v", reapErr)
		return reapErr
	}
	// A launch that returned no PID at all is not the pre-launch empty-PID path
	// (a clone or binary-copy failure, where no nohup'd server exists): the
	// nohup ran, and a wrapper that suppressed `echo $!` left the agent-server
	// detached with no PID to reap by. The empty-PID branch of reapScript would
	// `rm -rf` the session dir without a kill and latch a clean reap, leaking the
	// server. Refuse the same way an unverifiable non-empty PID does: no latch, no
	// removal, the record is retained for retry/manual inspection.
	if p.launchAttempted && p.remotePID == "" {
		reapErr := fmt.Errorf("%w: backend=%s: refusing to reap %q after a launch returned no remote PID — the agent-server may still be running; inspect before retrying",
			ErrWorkspaceStateUnknown, p.label(), p.sessionDir)
		log.ErrorLog.Printf("sandbox runtime: %v", reapErr)
		return reapErr
	}

	// W7.3: write the account back and remove it BEFORE the session dir goes, while
	// the agent-server is still reachable. Best-effort — the session-dir removal
	// below is the backstop — so a failure is logged, never a reason to leave the
	// sandbox in place.
	if p.account != nil {
		if aerr := p.account.teardownAccount(sshShortStepTimeout); aerr != nil {
			log.WarningLog.Printf("sandbox runtime: account teardown for %q: %v", p.sessionDir, aerr)
		}
	}

	nonce, nonceErr := sandboxReapNonce()
	if nonceErr != nil {
		// PRE-FLIGHT, so nothing remote ran. Without a nonce there is no script to
		// send, ssh is never invoked, and the sandbox is exactly as it was: the
		// agent-server still running, the session dir still there, and this record
		// their only handle. That is the same UNDETERMINED state as ssh's 255 and a
		// missing sentinel below, and it must be reported the same way — a plain
		// error here reads as a teardown that TOLD us something, and
		// deleteSessionRecord retires the row over a sandbox that is still billing
		// (#3449). It also must not latch, so the next poll re-runs the reap once
		// the entropy source recovers.
		reapErr := fmt.Errorf("%w: backend=%s: cannot generate a reap challenge for %q, so its sandbox was never contacted; retry the teardown, and remove the sandbox by hand if it keeps failing: %w",
			ErrWorkspaceStateUnknown, p.label(), p.sessionDir, nonceErr)
		log.ErrorLog.Printf("sandbox runtime: %v", reapErr)
		return reapErr
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

	// err can be nil here: the command exited 0 but never returned the challenge,
	// which means the far side did not run. Formatting a nil with %w renders
	// "%!w(<nil>)", so say what actually happened instead.
	reapErr := fmt.Errorf("backend=%s: reaping %q failed: %s: %w",
		p.label(), p.sessionDir, strings.TrimSpace(string(out)), err)
	if err == nil {
		reapErr = fmt.Errorf("backend=%s: reaping %q did not complete — the sandbox never confirmed it ran: %s",
			p.label(), p.sessionDir, strings.TrimSpace(string(out)))
	}

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
//
// THE PROOF IS NOT CHAINED TO THE RM, and that distinction is what makes the
// definite-failure latch reachable. `rm && respond` looks right and is not: the
// sentinel means "the far side executed", while rm's status means "and here is
// what happened". Chaining them collapses the two, so an ANSWERED failure — an rm
// that returns a real permission or filesystem status — arrives with no sentinel
// and is read as "we never reached the sandbox". reap then reports
// ErrWorkspaceStateUnknown and retries forever, for a cause that will answer the
// same way every time. The old x/crypto reaper latched exactly this case, so the
// convergence would have regressed it. Emitting the proof unconditionally after
// the kill succeeds, and re-exporting rm's status as the script's own, keeps both
// facts and lets reap's `default:` branch latch.
func (p *sandboxProvisioner) reapScript(nonce string) (script, expect string) {
	send, expect := sandboxReapChallenge(nonce)
	rm := "rm -rf " + shellQuote(p.sessionDir)
	// The remote shell must TRANSFORM the challenge, so the answer never appears
	// in the text a logging wrapper would echo.
	respond := "printf '%s' " + shellQuote(send) + " | tr 'a-z' 'A-Z'"
	// A subshell so `exit "$rc"` reports rm's status as this group's status
	// without terminating anything that might follow it.
	removeAndConfirm := "( " + rm + "; rc=$?; " + respond + `; exit "$rc" )`
	afPath := p.sessionDir + "/" + sshAfBinaryName
	// A restart records the PID it launched on the HOST before af learns it, so a
	// daemon that exited in between leaves a row naming the dead server while the
	// restarted one runs. Reaping removes the session directory under whatever
	// still runs there, so the host's record is consulted too and a live match is
	// stopped (or the reap refused) first.
	hostKill := "( " + remoteRecordedPIDKillScript(p.agentServerPIDPath(), p.remotePID, afPath, p.tokenRef) + " )"
	if p.remotePID == "" {
		return hostKill + " && " + removeAndConfirm, expect
	}
	kill := remotePIDIdentityKillScript(p.remotePID, afPath, p.tokenRef)
	// A SUBSHELL, not a { } group. The identity-kill script ends in `exit 0` when
	// the process is already gone — the common case after a clean shutdown — and a
	// group command runs in the CURRENT shell, so that exit terminated the whole
	// remote script before `rm -rf` or the confirmation could run. The reap then
	// looked like "exited 0, never confirmed": the directory survived and the
	// record was retained forever. A subshell contains the exit and yields it as
	// the group's status, which is what `&&` needs.
	return hostKill + " && ( " + kill + " ) && " + removeAndConfirm, expect
}

// sandboxReapNonce makes each reap's challenge unique, so a marker captured from
// an earlier run's log cannot satisfy a later one.
//
// It is a var so the pre-flight failure path has a test: crypto/rand cannot be
// made to fail from a test on any platform af runs on, and that path decides
// whether a still-running sandbox keeps its record (#3449).
var sandboxReapNonce = func() (string, error) {
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
	// Unregistered even when there was never a child: the entry exists from
	// before the spawn, and StopAllSandboxTunnels must not revisit this
	// provisioner while the kill below is still in flight. stopped latches
	// with it — and stoppedCh closes — so a spawn still in flight kills its
	// own newborn rather than publish to a provisioner the registry no longer
	// names, and the transport supervisor parked on a backoff wakes and exits
	// (#5571 W2 supervision). The latch is checked because a retried reap
	// runs this again and a second close would panic.
	unregisterSandboxTunnel(p)
	if !p.stopped {
		p.stopped = true
		close(p.stoppedChanLocked())
	}
	killTunnel(p.tunnel)
	p.tunnel = nil
}

type sandboxBackend struct {
	remoteAgentBackend
	provisioner *sandboxProvisioner
	// cleanup is the immutable teardown identity, persisted so a reap that could
	// not prove it ran is retryable after a daemon restart.
	cleanup *SandboxRuntimeCleanupData
}

func (b *sandboxBackend) Type() string { return "sandbox" }

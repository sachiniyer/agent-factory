package session

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

// The SSH remote-machine runtime (#1592 Phase 4 PR5) — the first-class remote
// backend, the built-in opinionated version of what a hook `launch_cmd` did by
// hand (ssh in, clone, start a session). A session's workspace + agent run on a
// configured remote host; the host runs an `af agent-server` (PR1) bound to
// loopback behind a bearer token (plain HTTP); the daemon reaches it through an
// SSH local-forward tunnel and drives it over the remoteAgentServer HTTP/WS
// client (PR2) exactly as
// it drives a local in-process session. Same provision-and-expose model as the
// docker runtime (PR4), a different sandbox.
//
// It used to provision with the Go `golang.org/x/crypto/ssh` client, shelling to
// nothing. It no longer does (#3052): it composes an `ssh(1)` invocation from the
// same `ssh.*` settings and hands it to the SAME sandboxProvisioner
// `backend = "sandbox"` uses, so there is ONE ssh transport in the tree rather
// than two that can disagree about an address (#3044). What stayed is everything
// above the transport, including the locked decisions this mirrors from docker
// (Q3/Q4): the daemon's OWN `af` binary is streamed onto the remote (always
// version-matched to the daemon), and GitHub is the durable workspace store (the
// remote clones repo@origin into a per-session dir, otherwise disposable).
//
// What this backend still owns, and what makes it more than a `sandbox` with a
// generated command: it OPINIONATES. `ssh.user`, `ssh.port`, `ssh.identity_file`
// and `ssh_host_key_verification` are real settings with af-enforced meanings,
// and the composed command reads NO ssh configuration file (`-F none`) so nothing
// outside af can change them — see ssh_command.go, which measures why pinning the
// values alone was not enough. An operator who needs ssh_config, a bastion or a
// ProxyCommand uses `backend = "sandbox"`, which exists for that.
//
// Lifecycle (sshRuntime.Provision, called from the backend factory during
// NewInstance) — every step below runs over the composed command, in
// sandboxProvisioner:
//
//	mktemp -d     — a fresh per-session dir under the remote home (~/.af-sessions)
//	git clone     — clone the repo's origin into <dir>/workspace on the remote
//	stream af     — copy the daemon's own `af` binary into <dir>/af over ssh stdin
//	af agent-server — start it headless bound to 127.0.0.1:0 on the remote; read
//	                its startup banner (addr/token) from a file
//	local-forward — an `ssh -L` child from a daemon-local loopback port to the
//	                remote agent-server's loopback addr → http://127.0.0.1:<localport>
//
// The result is an AgentServerEndpoint the daemon dials over the tunnel, plus a
// teardown that kills the remote agent-server, removes the session dir, and closes
// the tunnel. The in-sandbox agent-server itself runs the ordinary LOCAL runtime
// (tmux + git worktree) against the clone — so provision/launch/preview/prompt/
// stream all work on the remote exactly as on the daemon's own box, reached over
// the wire.

const (
	// sshDefaultPort is the ssh port used when neither ssh.port nor a port in
	// ssh.host is set.
	sshDefaultPort = 22
	// sshSessionRoot is the remote directory (relative to the login home) under
	// which each session's per-session dir is mktemp'd. Documented in
	// docs/backends.md so operators know where to sweep an orphan.
	sshSessionRoot = ".af-sessions"
	// sshWorkspaceSubdir is the clone destination inside the per-session dir; the
	// agent-server runs against it (--repo) and its LOCAL backend creates the
	// session's git worktree + branch off it, just like a local session.
	sshWorkspaceSubdir = "workspace"
	// sshAfBinaryName / sshBannerName / sshLogName are the files the
	// runtime writes inside the per-session dir: the streamed `af` binary, the
	// agent-server's stdout banner (one JSON line: addr/token), its
	// stderr log (pulled into the error on a start failure).
	sshAfBinaryName = "af"
	sshBannerName   = "agent-server.json"
	sshLogName      = "agent-server.log"
	// sshKnownHostsFileName is the af-owned host-key store under AF_HOME that
	// ssh_host_key_verification=accept-new reads and writes when the operator has
	// not set ssh.known_hosts — kept out of the user's shared ~/.ssh/known_hosts
	// (#2556).
	sshKnownHostsFileName = "ssh_known_hosts"
)

// ssh command/dial timeouts. Provisioning steps (clone, binary stream) get a
// generous budget because a large clone or a slow link can take a while; the dial
// and the short setup/reap steps are bounded tighter so a create or kill never
// hangs on an unreachable host.
const (
	sshDialTimeout          = 20 * time.Second
	sshProvisionStepTimeout = 5 * time.Minute
	sshShortStepTimeout     = 30 * time.Second
	sshReapTimeout          = 30 * time.Second
	sshBannerPollTimeout    = 45 * time.Second
	sshBannerPollInterval   = 400 * time.Millisecond
)

// sshSelfBinary resolves the `af` binary to stream onto the remote. In production
// it is the running daemon's own executable — the same binary provides `af
// agent-server`, so the remote is always version-matched to the daemon (mirrors
// the docker runtime's docker cp). The round-trip test overrides it with a freshly
// built static binary compatible with the sshd test image.
var sshSelfBinary = os.Executable

// SetSSHSelfBinaryForTest overrides the `af` binary the ssh runtime streams onto
// the remote and returns a restore function. The round-trip integration test uses
// it to point at a freshly built static binary (the test binary itself is not
// `af`).
func SetSSHSelfBinaryForTest(path string) func() {
	prev := sshSelfBinary
	sshSelfBinary = func() (string, error) { return path, nil }
	return func() { sshSelfBinary = prev }
}

// sshRuntime provisions a real remote-machine sandbox (#1592 Phase 4 PR5).
// Declared in runtime.go's registry; its Provision is here (the runtime.go stub is
// replaced by this).
type sshRuntime struct{}

func (sshRuntime) Provision(spec ProvisionSpec) (ProvisionResult, error) {
	cfg, err := resolveRepoConfig(spec.RepoRoot)
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("backend=ssh: cannot resolve repo config for %q: %w", spec.RepoRoot, err)
	}
	// Shared with the ListBackends RPC (#1933) — see BackendConfigError.
	if err := BackendConfigError(BackendSSH, cfg); err != nil {
		return ProvisionResult{}, err
	}
	// Safe to dereference: BackendConfigError rejects a missing SSH section, so it
	// is set here (locked by TestBackendConfigError_ReportsRepoConfigRequirements).
	sshCfg := *cfg.SSH
	if spec.CloneURL == "" {
		// Shared with BackendUnusableReason (#1933) — one wording, choose time and
		// create time.
		return ProvisionResult{}, missingOriginError(BackendSSH, spec.RepoRoot)
	}

	// The same precondition BackendUnusableReason checks at choose time, so a host
	// without OpenSSH fails here with the message the picker already gave rather
	// than deep inside a provision step.
	if _, err := lookPath("ssh"); err != nil {
		return ProvisionResult{}, sshCLIMissingError(err)
	}

	afBin, err := sshSelfBinary()
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("backend=ssh: cannot locate the af binary to stream onto the remote: %w", err)
	}

	// ONE ssh transport. This backend used to carry its own in-process x/crypto
	// client, and having two paths is what produced #3044 — the same address
	// resolving differently depending on which read it. The composed command is
	// handed to the same sandboxProvisioner `backend=sandbox` uses (#3052).
	// Create time is where an identity typo must be refused — see
	// verifySSHIdentityFile for why the teardown path deliberately does not.
	if err := verifySSHIdentityFile(sshCfg); err != nil {
		return ProvisionResult{}, err
	}
	// ONE resolution, here, feeding EVERY later step. Each provision command, the
	// `ssh -L` tunnel and the reap are separate ssh invocations that would each
	// resolve `ssh.host` independently, so a name with several addresses could put
	// the workspace on one machine and the agent-server, tunnel or reap on another —
	// and the reap would then remove the wrong directory, report success, and leave
	// the real workspace leaking with nothing pointing at it (#3086). Pinning it
	// into the composed command means all of them ride the same address, because
	// they all ride this one string.
	//
	// An empty result means af could not settle on an address and this session
	// behaves exactly as it did before the pin — see resolvePinnedSSHDialAddress.
	pinHost, pinPort, err := resolveSSHHostPort(sshCfg.Host, sshCfg.Port)
	if err != nil {
		return ProvisionResult{}, err
	}
	if pinPort == 0 {
		pinPort = sshDefaultPort
	}
	dialAddr := pinForProvision(sshCfg, pinHost, pinPort, cfg.SSHHostKeyVerification)
	dialPort := 0
	sshCmd, err := sshCommandPinnedTo(sshCfg, cfg.SSHHostKeyVerification, dialAddr, dialPort)
	if err != nil {
		return ProvisionResult{}, err
	}
	// Composition is pure; the accept-new store is created HERE, immediately
	// before the command that appends to it runs. See prepareSSHHostKeyStore.
	if err := prepareSSHHostKeyStore(sshCfg, cfg.SSHHostKeyVerification); err != nil {
		return ProvisionResult{}, err
	}
	// AND ONE ADDRESS IS NOT ONE MACHINE. Behind an L4 load balancer the pin above
	// is satisfied by every backend, so the steps can still split (#3122). Ask the
	// machine which address it is and re-pin to that, BEFORE anything creates remote
	// state — so the workspace, the tunnel and the reap all ride the same host.
	//
	// Only when af is already pinning: an empty dialAddr means the posture cannot
	// refuse a wrong pin (#3086), and that reasoning applies to a machine address
	// exactly as it does to a resolved one.
	if dialAddr != "" {
		if machineAddr, machinePort := learnPinnedMachine(sshCmd, dialAddr, pinPort); machineAddr != "" {
			dialAddr, dialPort = machineAddr, machinePort
			sshCmd, err = sshCommandPinnedTo(sshCfg, cfg.SSHHostKeyVerification, dialAddr, dialPort)
			if err != nil {
				return ProvisionResult{}, err
			}
		}
	}
	program := config.ResolveProgram(&cfg.Config, spec.Program)
	p := newSSHSandboxProvisioner(spec, sshCmd, afBin, program)
	res, err := p.provision()
	if err != nil {
		// Best-effort reap whatever the failed provision left behind. Preserve any
		// cleanup failure in the returned error: no Instance exists yet to own a
		// retry handle, so silently dropping it would hide an orphan.
		return ProvisionResult{}, p.reapProvisionFailure(err)
	}
	res.Backend = &sshBackend{
		provisioner: p,
		cleanup: &SSHRuntimeCleanupData{
			Config:     sshCfg,
			SessionDir: p.sessionDir,
			RemotePID:  p.remotePID,
			// The one fact no re-resolution can recover: which machine this
			// session's workspace is actually on. A reap after a daemon restart
			// composes its command from this, in a fresh process, which is the
			// constraint that ruled out a ControlMaster multiplex (#3086).
			HostKeyVerification: cfg.SSHHostKeyVerification,
		},
	}
	// The pin fields are written together, because which of them is safe to write
	// depends on whether a rolled-back daemon could read it correctly (#3122).
	sshRecordPinnedMachine(res.Backend.(*sshBackend).cleanup, dialAddr, dialPort, pinPort)
	return res, nil
}

// newSSHSandboxProvisioner is the shared constructor for this backend's
// transport, so the create path and the persisted-handle restore path build it
// the same way — a divergence between those two is how a legacy handle stops
// being reapable (#3044).
func newSSHSandboxProvisioner(spec ProvisionSpec, sshCmd, afBin, program string) *sandboxProvisioner {
	return &sandboxProvisioner{
		spec:    spec,
		sshCmd:  sshCmd,
		afBin:   afBin,
		program: program,
		// Errors from the shared transport must name THIS backend. An operator whose
		// ssh.host clone failed should not be sent to look up sandbox_ssh.
		backendLabel: string(BackendSSH),
	}
}

// ensureKnownHostsFile creates path (and its parent directory) if absent.
// accept-new needs a file it can append the learned key to, and ssh(1) refuses a
// UserKnownHostsFile whose directory does not exist rather than creating it.
func ensureKnownHostsFile(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// sshBackend is the in-process Backend for an ssh session. Like dockerBackend, its
// agent-facing operations delegate to the instance's remote AgentServer (the
// HTTP/WS client to the remote agent-server, reached over the tunnel) — so
// lifecycle, preview, prompt, and liveness all go over the wire to the remote host.
// Its ONE local responsibility is running the runtime's teardown (kill the remote
// agent-server, remove the session dir, close the tunnel), which it shares via the
// same idempotent closure with the AgentServer Kill path.
//
// Its shared remote-AgentServer behavior lives in remoteAgentBackend; this type
// retains only SSH-specific provisioning and the serialized backend discriminator.
type sshBackend struct {
	remoteAgentBackend
	// provisioner owns the concrete reaper. nil for an ordinary inert backend.
	provisioner *sandboxProvisioner
	// cleanup is the immutable ORIGINAL teardown identity. Keeping the original
	// PID is safe because every retry re-verifies the unique session argv before
	// signalling; immutability lets snapshots stage it without waiting on SSH I/O.
	cleanup *SSHRuntimeCleanupData
}

var _ Backend = (*sshBackend)(nil)

func (b *sshBackend) Type() string { return "ssh" }

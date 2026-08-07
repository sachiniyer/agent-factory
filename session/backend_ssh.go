package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/config"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
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
// It provisions with the Go `golang.org/x/crypto/ssh` client (NOT shelling to the
// `ssh` binary) so the runtime owns the connection + tunnel and does not depend on
// the host's ssh binary — reusing the user's keys/agent and known_hosts. Locked
// decisions this mirrors from docker (Q3/Q4): the daemon's OWN `af` binary is
// streamed onto the remote (always version-matched to the daemon), and GitHub is
// the durable workspace store (the remote clones repo@origin into a per-session
// dir, otherwise disposable).
//
// Lifecycle (sshRuntime.Provision, called from the backend factory during
// NewInstance):
//
//	dial          — ssh to ssh.host with key auth + host-key verification
//	mktemp -d     — a fresh per-session dir under the remote home (~/.af-sessions)
//	git clone     — clone the repo's origin into <dir>/workspace on the remote
//	stream af     — copy the daemon's own `af` binary into <dir>/af over the ssh
//	                connection (scp-equivalent; no external scp/sftp dependency)
//	af agent-server — start it headless bound to 127.0.0.1:0 on the remote; read
//	                its startup banner (addr/token) from a file
//	local-forward — open an ssh tunnel from a daemon-local loopback port to the
//	                remote agent-server's loopback addr → http://127.0.0.1:<localport>
//
// The result is an AgentServerEndpoint the daemon dials over the tunnel, plus a
// teardown that kills the remote agent-server, removes the session dir, and closes
// the tunnel + ssh connection. The in-sandbox agent-server itself runs the
// ordinary LOCAL runtime (tmux + git worktree) against the clone — so
// provision/launch/preview/prompt/stream all work on the remote exactly as on the
// daemon's own box, reached over the wire.

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

	afBin, err := sshSelfBinary()
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("backend=ssh: cannot locate the af binary to stream onto the remote: %w", err)
	}

	// ONE ssh transport. This backend used to carry its own in-process x/crypto
	// client, and having two paths is what produced #3044 — the same address
	// resolving differently depending on which read it. The composed command is
	// handed to the same sandboxProvisioner `backend=sandbox` uses (#3052).
	sshCmd, err := sshCommandForConfig(sshCfg, cfg.SSHHostKeyVerification)
	if err != nil {
		return ProvisionResult{}, err
	}
	p := newSSHSandboxProvisioner(spec, sshCmd, afBin, config.ResolveProgram(&cfg.Config, spec.Program))
	res, err := p.provision()
	if err != nil {
		// Best-effort reap whatever the failed provision left behind. Preserve any
		// cleanup failure in the returned error: no Instance exists yet to own a
		// retry handle, so silently dropping it would hide an orphan.
		return ProvisionResult{}, p.reapProvisionFailure(err)
	}
	res.Backend = &sshBackend{
		remoteAgentBackend: remoteAgentBackend{reap: res.Teardown},
		provisioner:        p,
		cleanup: &SSHRuntimeCleanupData{
			Config:              sshCfg,
			SessionDir:          p.sessionDir,
			RemotePID:           p.remotePID,
			HostKeyVerification: cfg.SSHHostKeyVerification,
		},
	}
	return res, nil
}

// newSSHSandboxProvisioner is the shared constructor for this backend's
// transport, so the create path and the persisted-handle restore path build it
// the same way — a divergence between those two is how a legacy handle stops
// being reapable (#3044).
func newSSHSandboxProvisioner(spec ProvisionSpec, sshCmd, afBin, program string) *sandboxProvisioner {
	return &sandboxProvisioner{spec: spec, sshCmd: sshCmd, afBin: afBin, program: program}
}

// sshProvisioner holds the state of one remote provisioning so its steps and its
// reap closure share the ssh connection, the remote session dir, the started PID,
// and the tunnel.
type sshProvisioner struct {
	spec    ProvisionSpec
	cfg     config.SSHConfig
	afBin   string
	program string
	// hostKeyVerification is the operator's global-only ssh_host_key_verification
	// posture (strict|accept-new|insecure), resolved from config.Config — NOT the
	// repo-settable ssh table — so a cloned repo cannot relax it (#2556).
	hostKeyVerification string

	client         *ssh.Client
	agentConn      io.Closer
	sessionDir     string
	remotePID      string
	tunnelLn       net.Listener
	tunnelAcceptWG sync.WaitGroup
	tunnelWG       sync.WaitGroup

	// Reap memoizes only an attempt that COMPLETED. A timeout leaves the remote
	// directory's state unknown, so the daemon must retain the row and actually
	// retry on its next poll; sync.Once cannot express that conditional latch.
	reapMu  sync.Mutex
	reaped  bool
	reapErr error

	// Narrow per-instance seams let the cleanup contract be exercised without a
	// real SSH server. Production instances leave these nil and use the methods
	// below; tests inject only the remote command/dial/client-close boundary.
	reapRunCombined func(time.Duration, string) ([]byte, error)
	// reapRunKill reports whether SSH accepted the exec request. Reap consumes the
	// live PID at that acceptance boundary; only pre-acceptance failures retry it.
	// A persisted pre-kill copy remains safe after a crash because the remote
	// command re-verifies the process identity before signalling.
	reapRunKill     func(time.Duration, string) (bool, error)
	reapOpenSession func() (sshCommandSession, error)
	reapDial        func() error
	reapCloseClient func()
}

// provision runs the full remote lifecycle and returns the wiring an ssh session
// needs. Each step wraps the remote command's output in the error so a failure is
// self-diagnosing.

// agentSigners returns the signers a running ssh-agent holds, or nil. It PROBES
// the agent up front (rather than registering a lazy PublicKeysCallback) so an
// empty/wedged agent socket contributes nothing to the auth attempt instead of
// aborting the handshake or burning MaxAuthTries on keys that do not exist.
//
// When it returns a non-empty slice, it ALSO returns the live agent connection as
// an io.Closer: the signers are agentKeyringSigner values that sign by calling
// back into the agent over conn during the ssh handshake (they are not
// self-contained key snapshots), so the caller must keep conn open until the dial
// completes and then close it — otherwise the Unix socket FD and the agent
// client's readLoop goroutine leak on every session creation (#1684). When there
// are no usable signers, conn is closed here and a nil closer is returned so an
// empty/wedged agent never leaks either.
func agentSigners() ([]ssh.Signer, io.Closer) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, nil
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, nil
	}
	signers, err := agent.NewClient(conn).Signers()
	if err != nil || len(signers) == 0 {
		_ = conn.Close()
		return nil, nil
	}
	return signers, conn
}

// isUnknownHostKeyError reports whether err is knownhosts' "host not present"
// signal (a *knownhosts.KeyError carrying no known keys) rather than a key
// MISMATCH (Want non-empty), which accept-new must still refuse.
func isUnknownHostKeyError(err error) bool {
	var keyErr *knownhosts.KeyError
	return errors.As(err, &keyErr) && len(keyErr.Want) == 0
}

// appendKnownHostKey appends a single known_hosts line binding hostname → key.
func appendKnownHostKey(path, hostname string, key ssh.PublicKey) error {
	line := knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(line + "\n")
	return err
}

// ensureKnownHostsFile creates path (and its parent directory) if absent, so
// knownhosts.New can read an as-yet-unused af store.
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

// --- remote command helpers -------------------------------------------------

type sshSessionResult struct {
	out []byte
	err error
}

// sshCommandSession is the narrow x/crypto session surface the kill delivery
// boundary needs. The interface lets tests make an accepted/rejected exec
// explicit without standing up a real SSH server.
type sshCommandSession interface {
	Start(string) error
	Wait() error
	Close() error
}

// runAcceptedSSHCommand bounds both exec acceptance and completion. The boolean
// answers only whether Session.Start returned nil; callers must still inspect the
// error before treating the command as complete.
func runAcceptedSSHCommand(timeout time.Duration, sess sshCommandSession, command string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	started := make(chan error, 1)
	go func() { started <- sess.Start(command) }()
	select {
	case err := <-started:
		if err != nil {
			return false, fmt.Errorf("starting remote command failed: %w", err)
		}
	case <-ctx.Done():
		_ = sess.Close()
		return false, fmt.Errorf("remote command acceptance timed out after %s: %w", timeout, ctx.Err())
	}

	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	select {
	case err := <-done:
		return true, err
	case <-ctx.Done():
		_ = sess.Close()
		return true, fmt.Errorf("accepted remote command timed out after %s: %w", timeout, ctx.Err())
	}
}

// runOpenedSSHSession owns ordinary command execution after NewSession has
// established the SSH channel. Reap uses runAcceptedSSHCommand instead because
// its identity-bearing kill must expose Start acceptance separately from Wait.
func runOpenedSSHSession(timeout time.Duration, sess *ssh.Session, script string, stdin io.Reader, combined bool) ([]byte, error) {
	if stdin != nil {
		sess.Stdin = stdin
	}

	cmd := "sh -c " + shellQuote(script)
	ch := make(chan sshSessionResult, 1)
	go func() {
		var out []byte
		var runErr error
		if combined {
			out, runErr = sess.CombinedOutput(cmd)
		} else {
			out, runErr = sess.Output(cmd)
		}
		ch <- sshSessionResult{out, runErr}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return awaitSSHSession(ctx, sess, ch, timeout, script)
}

func awaitSSHSession(ctx context.Context, sess io.Closer, ch <-chan sshSessionResult, timeout time.Duration, script string) ([]byte, error) {
	select {
	case r := <-ch:
		return r.out, r.err
	case <-ctx.Done():
		_ = sess.Close() // unblock the CombinedOutput/Output goroutine
		return nil, fmt.Errorf("remote command timed out after %s: %q: %w", timeout, script, ctx.Err())
	}
}

// --- remote path helpers ----------------------------------------------------

// afPath is the streamed binary's remote path; the reap's identity-kill checks
// argv[0] against it (backend_ssh_reap.go). The other per-session paths live on
// sandboxWorkspace, which owns the provision steps that use them.
func (p *sshProvisioner) afPath() string { return p.sessionDir + "/" + sshAfBinaryName }

// expandUserPath expands a leading ~ to the user's home dir, so ssh.identity_file
// / ssh.known_hosts accept the usual ~/.ssh/... form.
func expandUserPath(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if path == "~" {
				return home
			}
			return filepath.Join(home, path[2:])
		}
	}
	return path
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

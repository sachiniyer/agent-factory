package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"

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

	p := &sshProvisioner{
		spec:                spec,
		cfg:                 sshCfg,
		afBin:               afBin,
		program:             config.ResolveProgram(&cfg.Config, spec.Program),
		hostKeyVerification: cfg.SSHHostKeyVerification,
	}
	res, err := p.provision()
	if err != nil {
		// Best-effort reap whatever the failed provision left behind (a dialed
		// connection, a half-created remote dir, a started agent-server, an opened
		// tunnel). Preserve any cleanup failure in the returned error: no Instance
		// exists yet to own a retry handle, so silently dropping it would hide an
		// orphan.
		return ProvisionResult{}, p.reapProvisionFailure(err)
	}
	return res, nil
}

func (p *sshProvisioner) reapProvisionFailure(provisionErr error) error {
	reapErr := p.reap()
	if reapErr == nil {
		return provisionErr
	}
	return fmt.Errorf("backend=ssh: provisioning failed and cleanup of its partial workspace on %s (remote dir %q) did not complete; inspect it before retrying: %w",
		p.cfg.Host, p.sessionDir, errors.Join(provisionErr, reapErr))
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
func (p *sshProvisioner) provision() (ProvisionResult, error) {
	if err := p.dial(); err != nil {
		return ProvisionResult{}, err
	}
	// The provision steps are transport-agnostic (#2476 phase 1): they run over
	// this provisioner's Run (the x/crypto ssh session). sessionDir/remotePID are
	// mirrored back onto the provisioner the instant each is known, because the
	// reap (which stays x/crypto-specific) reaps by them even if a LATER step fails.
	w := &sandboxWorkspace{shell: p, spec: p.spec, program: p.program}
	if err := w.makeSessionDir(sshShortStepTimeout); err != nil {
		return ProvisionResult{}, p.sshErr(err)
	}
	p.sessionDir = w.SessionDir
	if err := w.configureGit(sshShortStepTimeout); err != nil {
		return ProvisionResult{}, p.sshErr(err)
	}
	if err := w.cloneWorkspace(sshProvisionStepTimeout, sshShortStepTimeout); err != nil {
		return ProvisionResult{}, p.sshErr(err)
	}
	binary, err := os.Open(p.afBin)
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("backend=ssh: opening the af binary %q to stream to the remote failed: %w", p.afBin, err)
	}
	copyErr := w.copyAfBinary(sshProvisionStepTimeout, binary)
	_ = binary.Close()
	if copyErr != nil {
		return ProvisionResult{}, p.sshErr(copyErr)
	}
	if err := w.startAgentServer(sshShortStepTimeout); err != nil {
		return ProvisionResult{}, p.sshErr(err)
	}
	p.remotePID = w.RemotePID
	banner, err := w.readBanner(sshBannerPollTimeout, sshBannerPollInterval, sshShortStepTimeout)
	if err != nil {
		return ProvisionResult{}, p.sshErr(err)
	}
	localAddr, err := p.startTunnel(banner.Addr)
	if err != nil {
		return ProvisionResult{}, err
	}

	endpoint := &AgentServerEndpoint{
		URL:   "http://" + localAddr,
		Token: banner.Token,
	}
	teardown := p.reap
	log.InfoLog.Printf("ssh runtime: session %q running on %s (remote dir %s), agent-server tunneled at %s", p.spec.Title, p.cfg.Host, p.sessionDir, endpoint.URL)
	return ProvisionResult{
		Backend: &sshBackend{
			remoteAgentBackend: remoteAgentBackend{reap: teardown},
			provisioner:        p,
			cleanup: &SSHRuntimeCleanupData{
				Config:     p.cfg,
				SessionDir: p.sessionDir,
				RemotePID:  p.remotePID,
			},
		},
		Endpoint: endpoint,
		Teardown: teardown,
	}, nil
}

// dial establishes the ssh connection: resolve auth (agent + identity keys) and a
// known_hosts host-key callback, then connect. Host-key verification is always on
// so a MITM cannot impersonate the remote and capture the bearer token.
func (p *sshProvisioner) dial() error {
	auth, err := p.authMethods()
	if err != nil {
		return err
	}
	hostKey, err := p.hostKeyCallback()
	if err != nil {
		return err
	}
	host, port := p.hostPort()
	clientCfg := &ssh.ClientConfig{
		User:            p.loginUser(),
		Auth:            auth,
		HostKeyCallback: hostKey,
		Timeout:         sshDialTimeout,
	}
	addr := net.JoinHostPort(host, port)
	client, err := ssh.Dial("tcp", addr, clientCfg)
	// The ssh-agent connection (opened in authMethods) is only needed for the
	// handshake ssh.Dial just ran — the agent signers sign the auth challenge
	// over it, but nothing uses it afterward. Close it now, on success or
	// failure, so the Unix socket FD and the agent client's readLoop goroutine
	// never outlive the dial (#1684). reap() also closes it, guarding the
	// authMethods-succeeded-but-dial-never-reached path; both closes are
	// idempotent via the nil-out here.
	if p.agentConn != nil {
		_ = p.agentConn.Close()
		p.agentConn = nil
	}
	if err != nil {
		return fmt.Errorf("backend=ssh: dialing %s@%s failed (check ssh.host/ssh.user, key auth, and ssh.known_hosts): %w", clientCfg.User, addr, err)
	}
	p.client = client
	return nil
}

// authMethods collects the ssh auth methods: the configured identity file (or,
// with no explicit identity, the user's default key files) and any keys held by a
// running ssh-agent. This reuses the user's own keys without depending on the ssh
// binary.
//
// Identity-file keys are offered BEFORE the agent, and the agent is probed and
// added only when it actually holds keys — an empty or wedged agent socket (e.g.
// a gpg-agent with no identities) must not consume the server's MaxAuthTries or
// abort the handshake before the good key is tried.
func (p *sshProvisioner) authMethods() ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod

	var keyFiles []string
	if f := strings.TrimSpace(p.cfg.IdentityFile); f != "" {
		keyFiles = []string{expandUserPath(f)}
	} else if home, err := os.UserHomeDir(); err == nil {
		for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
			keyFiles = append(keyFiles, filepath.Join(home, ".ssh", name))
		}
	}
	explicit := strings.TrimSpace(p.cfg.IdentityFile) != ""
	for _, f := range keyFiles {
		data, err := os.ReadFile(f)
		if err != nil {
			if explicit {
				return nil, fmt.Errorf("backend=ssh: cannot read ssh.identity_file %q: %w", f, err)
			}
			continue
		}
		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			if explicit {
				return nil, fmt.Errorf("backend=ssh: cannot parse ssh.identity_file %q (encrypted keys must be loaded via ssh-agent): %w", f, err)
			}
			continue
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}

	if signers, conn := agentSigners(); len(signers) > 0 {
		methods = append(methods, ssh.PublicKeys(signers...))
		// The signers are agentKeyringSigner values that sign by calling back
		// into the agent over conn during the handshake, so conn must stay open
		// until ssh.Dial completes. Own it on the provisioner; reap() closes it
		// alongside the ssh client. (#1684)
		p.agentConn = conn
	}

	if len(methods) == 0 {
		return nil, fmt.Errorf("backend=ssh: no usable ssh auth found; set ssh.identity_file, or load a key into ssh-agent (SSH_AUTH_SOCK), or place a default key in ~/.ssh")
	}
	return methods, nil
}

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

// hostKeyCallback returns the host-key verification callback for the operator's
// configured posture (ssh_host_key_verification, global-only #2556). The default
// (strict) is the original behavior, so an existing backend=ssh user sees no
// change; only an explicit operator opt-in relaxes it.
//
//   - strict: verify against a known_hosts file, refuse an unknown or changed key.
//   - accept-new: trust-on-first-use — record an UNKNOWN key and accept it, but
//     still refuse a CHANGED key. Learned keys go to an af-owned store (never the
//     user's shared ~/.ssh/known_hosts).
//   - insecure: no verification, with an honest warning that names the MITM risk.
func (p *sshProvisioner) hostKeyCallback() (ssh.HostKeyCallback, error) {
	switch p.hostKeyVerification {
	case config.SSHHostKeyInsecure:
		log.WarningLog.Printf("backend=ssh: host-key verification is DISABLED for %s (ssh_host_key_verification=insecure) — a man-in-the-middle on this connection can capture the bearer token that controls the agent session", p.cfg.Host)
		return ssh.InsecureIgnoreHostKey(), nil
	case config.SSHHostKeyAcceptNew:
		return p.acceptNewHostKeyCallback()
	default:
		// strict, and any unexpected value: fail safe to the strictest posture.
		return p.strictHostKeyCallback()
	}
}

// strictHostKeyCallback verifies against ssh.known_hosts (else ~/.ssh/known_hosts)
// and refuses an unknown or changed key. This is the default, unchanged from
// before #2556.
func (p *sshProvisioner) strictHostKeyCallback() (ssh.HostKeyCallback, error) {
	path, err := p.strictKnownHostsPath()
	if err != nil {
		return nil, err
	}
	cb, err := knownhosts.New(path)
	if err != nil {
		return nil, fmt.Errorf("backend=ssh: cannot load known_hosts %q for host-key verification (add the remote's key with `ssh-keyscan`, point ssh.known_hosts at a file that has it, or set ssh_host_key_verification=accept-new to trust it on first connect): %w", path, err)
	}
	return cb, nil
}

// strictKnownHostsPath is the file strict mode verifies against: ssh.known_hosts
// if set, else the user's ~/.ssh/known_hosts. Unchanged from before #2556.
func (p *sshProvisioner) strictKnownHostsPath() (string, error) {
	if path := strings.TrimSpace(p.cfg.KnownHosts); path != "" {
		return expandUserPath(path), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("backend=ssh: cannot locate ~/.ssh/known_hosts (set ssh.known_hosts): %w", err)
	}
	return filepath.Join(home, ".ssh", "known_hosts"), nil
}

// acceptNewHostKeyCallback implements trust-on-first-use: an UNKNOWN host's key
// is recorded and accepted; a CHANGED key is still refused (MITM protection is
// preserved). Reads and writes ssh.known_hosts if the operator set it, else an
// af-owned store under AF_HOME — deliberately never the user's shared
// ~/.ssh/known_hosts, and never a path resolved from ssh_config, so the
// destination is predictable and af never edits a file it does not own (#2556).
func (p *sshProvisioner) acceptNewHostKeyCallback() (ssh.HostKeyCallback, error) {
	path, err := p.acceptNewKnownHostsPath()
	if err != nil {
		return nil, err
	}
	// knownhosts.New requires the file to exist; a fresh af store does not yet.
	if err := ensureKnownHostsFile(path); err != nil {
		return nil, fmt.Errorf("backend=ssh: cannot prepare host-key store %q for accept-new: %w", path, err)
	}
	verify, err := knownhosts.New(path)
	if err != nil {
		return nil, fmt.Errorf("backend=ssh: cannot load host-key store %q for accept-new: %w", path, err)
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		switch verr := verify(hostname, remote, key); {
		case verr == nil:
			return nil // known and matches
		case isUnknownHostKeyError(verr):
			if aerr := appendKnownHostKey(path, hostname, key); aerr != nil {
				return fmt.Errorf("backend=ssh: accept-new could not record the host key for %s in %q: %w", hostname, path, aerr)
			}
			log.WarningLog.Printf("backend=ssh: accept-new trusted a new host key for %s and recorded it in %s (trust-on-first-use)", hostname, path)
			return nil
		default:
			// A changed/mismatched key (KeyError with a non-empty Want) or any
			// other verification failure: refuse.
			return verr
		}
	}, nil
}

// acceptNewKnownHostsPath is where accept-new reads and writes: ssh.known_hosts
// if the operator set it, else an af-owned file under AF_HOME. Never the user's
// ~/.ssh/known_hosts and never an ssh_config-resolved path (#2556).
func (p *sshProvisioner) acceptNewKnownHostsPath() (string, error) {
	if kh := strings.TrimSpace(p.cfg.KnownHosts); kh != "" {
		return expandUserPath(kh), nil
	}
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", fmt.Errorf("backend=ssh: cannot resolve AF home for the accept-new host-key store: %w", err)
	}
	return filepath.Join(dir, sshKnownHostsFileName), nil
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

// hostPort splits ssh.host into host + port. A port embedded in ssh.host wins;
// otherwise ssh.port, otherwise the default 22.
func (p *sshProvisioner) hostPort() (string, string) {
	host := strings.TrimSpace(p.cfg.Host)
	if h, port, err := net.SplitHostPort(host); err == nil && port != "" {
		return h, port
	}
	port := sshDefaultPort
	if p.cfg.Port > 0 {
		port = p.cfg.Port
	}
	return host, fmt.Sprintf("%d", port)
}

// loginUser resolves the ssh login user: ssh.user, else the current OS user.
func (p *sshProvisioner) loginUser() string {
	if u := strings.TrimSpace(p.cfg.User); u != "" {
		return u
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
}

// startTunnel opens an ssh local-forward: a daemon-local loopback listener whose
// every accepted connection is proxied over the ssh connection to remoteAddr (the
// agent-server's 127.0.0.1:<port> on the remote). Returns the local
// 127.0.0.1:<port> the daemon dials. The bearer token still applies end-to-end inside the
// tunnel (defense in depth), and the agent-server port is never exposed on the
// remote's public interface.
func (p *sshProvisioner) startTunnel(remoteAddr string) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("backend=ssh: opening the local tunnel listener failed: %w", err)
	}
	p.tunnelLn = ln
	p.tunnelAcceptWG.Add(1)
	go p.acceptLoop(remoteAddr)
	return ln.Addr().String(), nil
}

// acceptLoop accepts local tunnel connections until the listener is closed (by
// reap), forwarding each to the remote agent-server addr.
func (p *sshProvisioner) acceptLoop(remoteAddr string) {
	defer p.tunnelAcceptWG.Done()
	for {
		local, err := p.tunnelLn.Accept()
		if err != nil {
			return // listener closed by reap
		}
		p.tunnelWG.Add(1)
		go p.forward(local, remoteAddr)
	}
}

// forward proxies one accepted local connection to remoteAddr over the ssh
// connection, copying bytes both ways until either side closes.
func (p *sshProvisioner) forward(local net.Conn, remoteAddr string) {
	defer p.tunnelWG.Done()
	defer func() { _ = local.Close() }()
	remote, err := p.client.Dial("tcp", remoteAddr)
	if err != nil {
		log.WarningLog.Printf("ssh runtime: tunnel dial to %s failed: %v", remoteAddr, err)
		return
	}
	defer func() { _ = remote.Close() }()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(remote, local); done <- struct{}{} }()
	go func() { _, _ = io.Copy(local, remote); done <- struct{}{} }()
	<-done
}

// --- remote command helpers -------------------------------------------------

// runCombined runs script via `sh -c` on the remote and returns its combined
// stdout+stderr — used for setup steps where the error text matters.
func (p *sshProvisioner) runCombined(timeout time.Duration, script string) ([]byte, error) {
	return p.runSession(timeout, script, nil, true)
}

// Run is the sandboxShell primitive the shared provision steps drive (#2476
// phase 1): `sh -c <script>` on the remote with an optional stdin, bounded by
// timeout, returning combined stdout+stderr or stdout only.
func (p *sshProvisioner) Run(timeout time.Duration, script string, stdin io.Reader, combined bool) ([]byte, error) {
	return p.runSession(timeout, script, stdin, combined)
}

// sshErr prefixes a transport-agnostic provision-step error (the shared steps
// carry no backend name) with "backend=ssh:", keeping the message byte-identical
// to the pre-refactor wording.
func (p *sshProvisioner) sshErr(err error) error {
	return fmt.Errorf("backend=ssh: %w", err)
}

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

// runSession opens one ssh session, runs `sh -c <script>` with an optional stdin,
// and returns its output, bounding the whole thing with a timeout that closes the
// session so a wedged remote command cannot hang a create or kill. Each ssh session
// runs exactly one command (the ssh protocol), so callers get a fresh one per step.
func (p *sshProvisioner) runSession(timeout time.Duration, script string, stdin io.Reader, combined bool) ([]byte, error) {
	sess, err := p.client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("opening ssh session failed: %w", err)
	}
	defer func() { _ = sess.Close() }()
	return runOpenedSSHSession(timeout, sess, script, stdin, combined)
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
	provisioner *sshProvisioner
	// cleanup is the immutable ORIGINAL teardown identity. Keeping the original
	// PID is safe because every retry re-verifies the unique session argv before
	// signalling; immutability lets snapshots stage it without waiting on SSH I/O.
	cleanup *SSHRuntimeCleanupData
}

var _ Backend = (*sshBackend)(nil)

func (b *sshBackend) Type() string { return "ssh" }

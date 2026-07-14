package session

import (
	"context"
	"encoding/json"
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
	// sshAfBinaryName / sshBannerName / sshLogName / sshPidName are the files the
	// runtime writes inside the per-session dir: the streamed `af` binary, the
	// agent-server's stdout banner (one JSON line: addr/token), its
	// stderr log (pulled into the error on a start failure), and the background
	// PID (used to kill it on teardown).
	sshAfBinaryName = "af"
	sshBannerName   = "agent-server.json"
	sshLogName      = "agent-server.log"
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

// sshBanner mirrors daemon.AgentServerInfo field-for-field (the JSON the `af
// agent-server` prints on startup). Duplicated here rather than imported because
// daemon imports session (a cycle); the shared contract is the JSON tags, pinned
// by the round-trip test. Identical to dockerBanner — the same banner, a different
// transport.
type sshBanner struct {
	Addr  string `json:"addr"`
	Token string `json:"token"`
	Title string `json:"title"`
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
	var sshCfg config.SSHConfig
	if cfg.SSH != nil {
		sshCfg = *cfg.SSH
	}
	if strings.TrimSpace(sshCfg.Host) == "" {
		return ProvisionResult{}, fmt.Errorf("backend=ssh requires ssh.host to be set in this repo's .agent-factory/config.json (the remote host the session's workspace + agent run on)")
	}
	if spec.CloneURL == "" {
		return ProvisionResult{}, fmt.Errorf("backend=ssh: repo %q has no `origin` remote to clone the workspace from; add one (GitHub is the durable workspace store) or push the repo first", spec.RepoRoot)
	}

	afBin, err := sshSelfBinary()
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("backend=ssh: cannot locate the af binary to stream onto the remote: %w", err)
	}

	p := &sshProvisioner{
		spec:    spec,
		cfg:     sshCfg,
		afBin:   afBin,
		program: spec.Program,
	}
	res, err := p.provision()
	if err != nil {
		// Reap whatever the failed provision left behind (a dialed connection, a
		// half-created remote dir, a started agent-server, an opened tunnel) so a
		// remote workspace never leaks on a partial failure.
		p.reap()
		return ProvisionResult{}, err
	}
	return res, nil
}

// sshProvisioner holds the state of one remote provisioning so its steps and its
// reap closure share the ssh connection, the remote session dir, the started PID,
// and the tunnel.
type sshProvisioner struct {
	spec    ProvisionSpec
	cfg     config.SSHConfig
	afBin   string
	program string

	client     *ssh.Client
	agentConn  io.Closer
	sessionDir string
	remotePID  string
	tunnelLn   net.Listener
	tunnelWG   sync.WaitGroup
	reapOnce   sync.Once
}

// provision runs the full remote lifecycle and returns the wiring an ssh session
// needs. Each step wraps the remote command's output in the error so a failure is
// self-diagnosing.
func (p *sshProvisioner) provision() (ProvisionResult, error) {
	if err := p.dial(); err != nil {
		return ProvisionResult{}, err
	}
	if err := p.makeSessionDir(); err != nil {
		return ProvisionResult{}, err
	}
	if err := p.configureGit(); err != nil {
		return ProvisionResult{}, err
	}
	if err := p.cloneWorkspace(); err != nil {
		return ProvisionResult{}, err
	}
	if err := p.copyAfBinary(); err != nil {
		return ProvisionResult{}, err
	}
	if err := p.startAgentServer(); err != nil {
		return ProvisionResult{}, err
	}
	banner, err := p.readBanner()
	if err != nil {
		return ProvisionResult{}, err
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
		Backend:  &sshBackend{reap: teardown},
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

// hostKeyCallback verifies the remote's host key against a known_hosts file
// (ssh.known_hosts, else ~/.ssh/known_hosts). Verification is mandatory: an
// InsecureIgnoreHostKey escape hatch would let a MITM capture the bearer token, so
// there is none — seed the known_hosts entry for an ephemeral host out of band.
func (p *sshProvisioner) hostKeyCallback() (ssh.HostKeyCallback, error) {
	path := strings.TrimSpace(p.cfg.KnownHosts)
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("backend=ssh: cannot locate ~/.ssh/known_hosts (set ssh.known_hosts): %w", err)
		}
		path = filepath.Join(home, ".ssh", "known_hosts")
	} else {
		path = expandUserPath(path)
	}
	cb, err := knownhosts.New(path)
	if err != nil {
		return nil, fmt.Errorf("backend=ssh: cannot load known_hosts %q for host-key verification (add the remote's key with `ssh-keyscan`, or point ssh.known_hosts at a file that has it): %w", path, err)
	}
	return cb, nil
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

// makeSessionDir creates a fresh per-session dir under the remote home with
// `mktemp -d` and captures its absolute path — the workspace + af binary + banner
// all live under it, so teardown reaps the whole session with one `rm -rf`.
func (p *sshProvisioner) makeSessionDir() error {
	slug := Slugify(p.spec.Title)
	script := fmt.Sprintf(`mkdir -p "$HOME/%s" && mktemp -d "$HOME/%s/%s.XXXXXX"`, sshSessionRoot, sshSessionRoot, slug)
	out, err := p.runOut(sshShortStepTimeout, script)
	if err != nil {
		return fmt.Errorf("backend=ssh: creating the remote session dir failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return fmt.Errorf("backend=ssh: `mktemp -d` returned no path")
	}
	p.sessionDir = dir
	return nil
}

// configureGit sets a git identity and marks every directory safe on the remote so
// the clone + worktree creation don't trip on "dubious ownership" or a missing
// committer identity. Mirrors the docker runtime.
func (p *sshProvisioner) configureGit() error {
	script := `git config --global user.email "af@agent-factory.local" && ` +
		`git config --global user.name "Agent Factory" && ` +
		`git config --global --add safe.directory "*"`
	out, err := p.runCombined(sshShortStepTimeout, script)
	if err != nil {
		return fmt.Errorf("backend=ssh: git config on the remote failed (is git installed there?): %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// cloneWorkspace clones the repo's origin into <sessionDir>/workspace on the
// remote. A fresh create clones the default branch; the remote agent-server's
// LOCAL backend then creates the session's git worktree + branch off it. On a
// RESTORE (spec.RestoreBranch set, #1592 Phase 4 PR6) it additionally
// materializes the pushed session branch as a local ref so the remote Setup
// checks it out.
func (p *sshProvisioner) cloneWorkspace() error {
	script := fmt.Sprintf("git clone %s %s", shellQuote(p.spec.CloneURL), shellQuote(p.workspacePath()))
	out, err := p.runCombined(sshProvisionStepTimeout, script)
	if err != nil {
		return fmt.Errorf("backend=ssh: cloning %q on the remote failed (is git installed, and the URL reachable from the remote host?): %s: %w",
			p.spec.CloneURL, strings.TrimSpace(string(out)), err)
	}
	if branch := strings.TrimSpace(p.spec.RestoreBranch); branch != "" {
		return p.fetchRestoreBranch(branch)
	}
	return nil
}

// fetchRestoreBranch materializes the archived session branch (pushed to origin
// at archive time) as a LOCAL ref in the fresh remote clone, WITHOUT checking it
// out in the main tree (#1592 Phase 4 PR6) — the ssh mirror of the docker
// runtime's fetchRestoreBranch. The `<branch>:<branch>` refspec creates
// refs/heads/<branch> so the remote local backend's Setup reuses it and brings
// the pushed commits back.
func (p *sshProvisioner) fetchRestoreBranch(branch string) error {
	script := fmt.Sprintf("git -C %s fetch origin %s:%s",
		shellQuote(p.workspacePath()), shellQuote(branch), shellQuote(branch))
	out, err := p.runCombined(sshProvisionStepTimeout, script)
	if err != nil {
		return fmt.Errorf("backend=ssh: restoring archived branch %q on the remote failed (was it pushed to origin?): %s: %w",
			branch, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// copyAfBinary streams the daemon's own `af` binary into <sessionDir>/af over the
// ssh connection (scp-equivalent — no external scp/sftp dependency) and makes it
// executable. Always the daemon's binary (never a reused remote `af`) so the remote
// agent-server is version-matched to the daemon, exactly as the docker runtime docker
// cp's its binary in. The binary must be compatible with the remote (matching
// arch/libc) — a static build runs anywhere; documented in docs/backends.md.
func (p *sshProvisioner) copyAfBinary() error {
	f, err := os.Open(p.afBin)
	if err != nil {
		return fmt.Errorf("backend=ssh: opening the af binary %q to stream to the remote failed: %w", p.afBin, err)
	}
	defer func() { _ = f.Close() }()

	dst := p.afPath()
	script := fmt.Sprintf("cat > %s && chmod +x %s", shellQuote(dst), shellQuote(dst))
	if out, err := p.runStdin(sshProvisionStepTimeout, script, f); err != nil {
		return fmt.Errorf("backend=ssh: streaming the af binary to the remote failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// startAgentServer launches `af agent-server` headless on the remote, detached via
// nohup with its stdout banner + stderr redirected to files under the session dir,
// bound to 127.0.0.1:0 (the tunnel reaches it — no port on the remote's public
// interface). `echo $!` returns the background PID, captured for teardown. The
// workspace title matches the daemon-side session title so the daemon's remote
// client dials /v1/sessions/{title}/stream. readBanner then polls the banner file.
func (p *sshProvisioner) startAgentServer() error {
	inner := fmt.Sprintf("%s agent-server --listen 127.0.0.1:0 --repo %s --title %s",
		shellQuote(p.afPath()), shellQuote(p.workspacePath()), shellQuote(p.spec.Title))
	if strings.TrimSpace(p.program) != "" {
		inner += " --program " + shellQuote(p.program)
	}
	if p.spec.AutoYes {
		inner += " --auto-yes"
	}
	// nohup + background + redirected fds + </dev/null so the agent-server outlives
	// this ssh command; `echo $!` prints the background PID on stdout.
	launch := fmt.Sprintf("nohup %s >%s 2>%s </dev/null & echo $!",
		inner, shellQuote(p.bannerPath()), shellQuote(p.logPath()))
	out, err := p.runOut(sshShortStepTimeout, launch)
	if err != nil {
		return fmt.Errorf("backend=ssh: starting af agent-server on the remote failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	p.remotePID = strings.TrimSpace(string(out))
	return nil
}

// readBanner polls the remote banner file until the agent-server has bound its
// listener and printed its {addr,token} JSON line, or times out. On
// timeout it pulls the agent-server's stderr log into the error so a failure to
// start (missing tmux/git, bad binary, port clash) is self-diagnosing.
func (p *sshProvisioner) readBanner() (sshBanner, error) {
	deadline := time.Now().Add(sshBannerPollTimeout)
	for {
		out, err := p.runOut(sshShortStepTimeout, "cat "+shellQuote(p.bannerPath()))
		if err == nil {
			var b sshBanner
			if jErr := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &b); jErr == nil && b.Addr != "" && b.Token != "" {
				return b, nil
			}
		}
		if time.Now().After(deadline) {
			logOut, _ := p.runCombined(sshShortStepTimeout, "cat "+shellQuote(p.logPath()))
			return sshBanner{}, fmt.Errorf("backend=ssh: af agent-server did not report a startup banner within %s; remote log:\n%s",
				sshBannerPollTimeout, strings.TrimSpace(string(logOut)))
		}
		time.Sleep(sshBannerPollInterval)
	}
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
	go p.acceptLoop(remoteAddr)
	return ln.Addr().String(), nil
}

// acceptLoop accepts local tunnel connections until the listener is closed (by
// reap), forwarding each to the remote agent-server addr.
func (p *sshProvisioner) acceptLoop(remoteAddr string) {
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

// reap tears down the remote sandbox idempotently: close the tunnel listener (stop
// accepting), kill the remote agent-server PID, remove the session dir, and close
// the ssh connection. It runs on the session's Kill (after the remote workspace is
// torn down over REST), on a provisioning failure, and on a bad-endpoint
// NewInstance failure — so a remote workspace/tunnel is never leaked. The sync.Once
// collapses the repeated Kill retries and the Kill-vs-provision-failure races.
func (p *sshProvisioner) reap() error {
	var reapErr error
	p.reapOnce.Do(func() {
		if p.tunnelLn != nil {
			_ = p.tunnelLn.Close()
		}
		// Close the ssh-agent connection unconditionally — it is opened in
		// authMethods() before the dial, so it must be released even when the
		// dial itself failed (p.client == nil). Closing it stops the agent
		// client's readLoop goroutine and frees the Unix socket FD (#1684).
		if p.agentConn != nil {
			_ = p.agentConn.Close()
		}
		if p.client != nil {
			if p.remotePID != "" {
				// SIGTERM lets the agent-server tear its workspace down cleanly (kill
				// tmux, remove the worktree); the SIGKILL is a backstop if it hangs.
				_, _ = p.runCombined(sshShortStepTimeout, fmt.Sprintf("kill %s 2>/dev/null; sleep 0.3; kill -9 %s 2>/dev/null; true", p.remotePID, p.remotePID))
			}
			if p.sessionDir != "" {
				if out, err := p.runCombined(sshReapTimeout, "rm -rf "+shellQuote(p.sessionDir)); err != nil {
					reapErr = fmt.Errorf("backend=ssh: removing remote session dir %q failed: %s: %w", p.sessionDir, strings.TrimSpace(string(out)), err)
					log.WarningLog.Printf("%v", reapErr)
				}
			}
			_ = p.client.Close()
		}
		if p.tunnelLn != nil {
			// Let in-flight forwards drain against the now-closed ssh connection.
			p.tunnelWG.Wait()
		}
		log.InfoLog.Printf("ssh runtime: reaped session %q on %s (remote dir %s)", p.spec.Title, p.cfg.Host, p.sessionDir)
	})
	return reapErr
}

// --- remote command helpers -------------------------------------------------

// runCombined runs script via `sh -c` on the remote and returns its combined
// stdout+stderr — used for setup steps where the error text matters.
func (p *sshProvisioner) runCombined(timeout time.Duration, script string) ([]byte, error) {
	return p.runSession(timeout, script, nil, true)
}

// runOut runs script via `sh -c` on the remote and returns ONLY stdout — used
// where stderr would pollute the captured value (the launch's PID, the banner
// JSON).
func (p *sshProvisioner) runOut(timeout time.Duration, script string) ([]byte, error) {
	return p.runSession(timeout, script, nil, false)
}

// runStdin runs script via `sh -c` on the remote with stdin fed from r (used to
// stream the af binary), returning combined output.
func (p *sshProvisioner) runStdin(timeout time.Duration, script string, r io.Reader) ([]byte, error) {
	return p.runSession(timeout, script, r, true)
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
	if stdin != nil {
		sess.Stdin = stdin
	}

	cmd := "sh -c " + shellQuote(script)
	type result struct {
		out []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		var out []byte
		var runErr error
		if combined {
			out, runErr = sess.CombinedOutput(cmd)
		} else {
			out, runErr = sess.Output(cmd)
		}
		ch <- result{out, runErr}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	select {
	case r := <-ch:
		return r.out, r.err
	case <-ctx.Done():
		_ = sess.Close() // unblock the CombinedOutput/Output goroutine
		return nil, fmt.Errorf("remote command timed out after %s: %q", timeout, script)
	}
}

// --- remote path helpers ----------------------------------------------------

func (p *sshProvisioner) workspacePath() string {
	return p.sessionDir + "/" + sshWorkspaceSubdir
}
func (p *sshProvisioner) afPath() string     { return p.sessionDir + "/" + sshAfBinaryName }
func (p *sshProvisioner) bannerPath() string { return p.sessionDir + "/" + sshBannerName }
func (p *sshProvisioner) logPath() string    { return p.sessionDir + "/" + sshLogName }

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
// It parallels dockerBackend deliberately (the two runtimes are independent per the
// plan — either can ship first); the cross-cutting dedup of the two remote backends
// belongs to PR6, where archive/restore is written once against the Runtime
// interface. The daemon's observation/delivery paths speak to a session ONLY
// through Instance.AgentServer() (the remoteAgentServer), so a backend→AgentServer
// call here is a network hop over the tunnel, never a loop back into the backend.
type sshBackend struct {
	// reap kills the remote agent-server, removes the remote session dir, and
	// closes the tunnel + ssh connection; shared with the runtime's Teardown / the
	// AgentServer Kill path, and idempotent (a sync.Once behind the closure).
	reap func() error
}

var _ Backend = (*sshBackend)(nil)

func (b *sshBackend) Type() string { return "ssh" }

// Capabilities advertises FULL parity for ssh (#1592 Phase 4 PR6, epic §5): the
// workspace is off-box, but attach/preview/liveness/prompt/tabs work through the
// remote agent-server over the tunnel, and Archive/Recover work by pushing the
// branch to GitHub and re-provisioning a fresh remote that clones it back (§5.1)
// — every capability true, no ErrRecoverUnsupported and no locality special-case.
func (b *sshBackend) Capabilities() Capabilities {
	return Capabilities{
		Workspace:        WorkspaceRemote,
		Attach:           true,
		Archive:          true,
		Recover:          true,
		TabManagement:    true,
		TerminalTab:      true,
		InteractiveInput: true,
	}
}

// Start provisions then launches the remote workspace. The remote host +
// agent-server were established by the runtime (during NewInstance); Start drives
// the agent-server's own provision/launch over REST, so the remote LOCAL backend
// creates the git worktree + branch and spawns the agent.
func (b *sshBackend) Start(i *Instance, firstTimeSetup bool) error {
	if err := b.Provision(i, firstTimeSetup); err != nil {
		return err
	}
	return b.Launch(i, firstTimeSetup)
}

// Provision drives the remote agent-server's Provision over the wire (clone is
// already done by the runtime; this creates the remote git worktree).
func (b *sshBackend) Provision(i *Instance, firstTimeSetup bool) error {
	return i.AgentServer().Provision(firstTimeSetup)
}

// Launch drives the remote agent-server's Launch (spawn the agent), sets the
// started flag, and seeds the daemon-side tab model with the agent tab (the remote
// owns the real tabs; the daemon-side list mirrors the agent tab so the UI renders
// it, matching the docker/remote-hook baseline).
func (b *sshBackend) Launch(i *Instance, firstTimeSetup bool) error {
	if err := i.AgentServer().Launch(firstTimeSetup); err != nil {
		return err
	}
	i.mu.Lock()
	i.started = true
	if len(i.Tabs) == 0 {
		i.Tabs = []*Tab{newRemoteAgentTab()}
	}
	i.mu.Unlock()
	return nil
}

// Kill runs the runtime teardown. Instance.Kill routes through the AgentServer
// (which tears the remote workspace down over REST and then runs the same reap), so
// this is normally reached only if something calls backend.Kill directly; the
// shared idempotent reap makes either path safe.
func (b *sshBackend) Kill(i *Instance) error {
	i.mu.Lock()
	i.started = false
	i.mu.Unlock()
	if b.reap != nil {
		return b.reap()
	}
	return nil
}

// CloseAttachOnly discards this instance's local view WITHOUT tearing the remote
// down — used to drop a duplicate Instance built from disk that lost a race to the
// canonical one (#867). Reaping here would tear down the remote workspace the
// canonical Instance shares, so it must not run.
func (b *sshBackend) CloseAttachOnly(i *Instance) error {
	i.mu.Lock()
	i.started = false
	i.mu.Unlock()
	return nil
}

func (b *sshBackend) Preview(i *Instance) (string, error) {
	return i.AgentServer().Preview(0, false)
}

func (b *sshBackend) PreviewFullHistory(i *Instance) (string, error) {
	return i.AgentServer().Preview(0, true)
}

// Attach/AttachTerminal: an ssh session attaches CLIENT-side over the WS PTY stream
// (the daemon proxies the remote's stream through the tunnel), exactly like a local
// session — the client's attach dispatch branches on Capabilities().Workspace and
// never reaches the backend. These satisfy the interface with an explicit
// routing-invariant error rather than a silent no-op.
func (b *sshBackend) Attach(*Instance) (chan struct{}, error) {
	return nil, fmt.Errorf("ssh sessions attach client-side over the WS PTY stream, not through the backend")
}

func (b *sshBackend) AttachTerminal(*Instance, int) (chan struct{}, error) {
	return nil, fmt.Errorf("ssh terminal tabs attach client-side over the WS PTY stream, not through the backend")
}

func (b *sshBackend) HasUpdated(i *Instance) (updated bool, hasPrompt bool, content string) {
	obs, err := i.AgentServer().Snapshot()
	if err != nil {
		return false, false, ""
	}
	return obs.Updated, obs.HasPrompt, obs.Content
}

func (b *sshBackend) SendPromptCommand(i *Instance, prompt string) error {
	return i.AgentServer().SendPrompt(prompt)
}

func (b *sshBackend) IsAlive(i *Instance) bool {
	return i.AgentServer().Alive()
}

// CheckAndHandleTrustPrompt is a daemon-side no-op: the remote agent-server
// dismisses trust/permission prompts itself on every Snapshot (its localAgentServer
// runs CheckAndHandleTrustPrompt before reading the pane), so there is nothing for
// the daemon to do over the wire.
func (b *sshBackend) CheckAndHandleTrustPrompt(*Instance) bool { return false }

func (b *sshBackend) TapEnter(i *Instance) { i.AgentServer().TapEnter() }

// Recover/Respawn re-establish an ssh session by RE-PROVISIONING a fresh remote
// that clones the session's branch back from origin, then relaunching the agent
// (#1592 Phase 4 PR6) — the same recoverSandbox the docker runtime uses (written
// once). A disposable remote dir has no in-place session to reconnect, so
// recovery is always a fresh provision + clone of the durable branch on GitHub;
// only the pushed state survives.
func (b *sshBackend) Recover(i *Instance) error { return recoverSandbox(i) }
func (b *sshBackend) Respawn(i *Instance) error { return recoverSandbox(i) }

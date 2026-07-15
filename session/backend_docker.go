package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// The docker container runtime (#1592 Phase 4 PR4) — the first-class sandboxed
// backend. A session's workspace + agent run in a container; the container
// exposes an `af agent-server` (PR1) on a published loopback port behind a bearer
// token; the daemon drives it through the remoteAgentServer HTTP/WS client (PR2)
// exactly as it drives a local in-process session. This is the "provision-and-
// expose" model the whole epic was built toward, at parity with local.
//
// It drives docker through the CLI (`os/exec`, the same idiom as HookBackend's
// exec.Command), NOT the docker Go SDK: no heavy dependency, and the surface we
// need is small (run/exec/cp/port/rm). Sachin-locked decisions this file
// implements: BYO image (docker.image config, no af-published image — Q3), the
// `af` binary docker cp'd into the running container (always version-matched to
// the daemon), and GitHub as the durable workspace store (the container clones
// repo@origin and is otherwise disposable).
//
// Lifecycle (dockerRuntime.Provision, called from the backend factory during
// NewInstance):
//
//	docker run     — start a container from docker.image, kept alive, publishing
//	                 the in-container agent-server port on a random loopback host port
//	git clone      — clone the repo's origin into /workspace inside the container
//	docker cp      — copy the daemon's own `af` binary into the container
//	docker exec    — start `af agent-server --listen :<port>` headless in the
//	                 container; read its startup banner (addr/token)
//	docker port    — map the published port to build http://127.0.0.1:<hostport>
//
// The result is an AgentServerEndpoint the daemon dials, plus a container-reap
// teardown run when the session is killed. The in-container agent-server itself
// runs the ordinary LOCAL runtime (tmux + git worktree) against /workspace — so
// provision/launch/preview/prompt/stream all work in-container exactly as on the
// daemon's own box, reached over the wire.

const (
	// dockerAgentPort is the fixed port the in-container `af agent-server` binds
	// (0.0.0.0 so the published host port forwards to it). A constant rather than
	// :0 because the host port must be published at `docker run` time, before the
	// server is up to report a chosen port. Documented in docs/backends.md so a
	// BYO image can avoid colliding with it.
	dockerAgentPort = "8000"
	// dockerWorkspaceDir is where the repo is cloned inside the container; the
	// agent-server runs against it (--repo), and its LOCAL backend creates the
	// session's git worktree + branch off it, just like a local session.
	dockerWorkspaceDir = "/workspace"
	// dockerAfBinaryPath is where the daemon's `af` binary is copied in the
	// container so `af agent-server` is on PATH for the exec below.
	dockerAfBinaryPath = "/usr/local/bin/af"
	// dockerBannerPath/dockerLogPath capture the agent-server's stdout banner and
	// stderr log inside the container. The banner is a single JSON line (addr,
	// token) the server prints the instant its listener binds; the
	// runtime polls the file for it because `docker exec -d` detaches the process.
	dockerBannerPath = "/tmp/af-agent-server.json"
	dockerLogPath    = "/tmp/af-agent-server.log"
	// dockerSessionLabel tags every managed container so orphans can be swept with
	// `docker ps -aq -f label=af.session` (documented in docs/backends.md). The
	// runtime reaps by container ID; the label is for operators.
	dockerSessionLabel = "af.session"
)

// docker command timeouts. Provisioning steps (run/clone/cp/exec) get a generous
// budget because a first-run image pull or a large clone can be slow; reaping is
// bounded tighter so a kill never hangs.
const (
	dockerProvisionStepTimeout = 5 * time.Minute
	dockerShortStepTimeout     = 30 * time.Second
	dockerReapTimeout          = 30 * time.Second
	dockerBannerPollTimeout    = 45 * time.Second
	dockerBannerPollInterval   = 400 * time.Millisecond
)

// dockerSelfBinary resolves the `af` binary to docker cp into the sandbox. In
// production it is the running daemon's own executable — the same binary provides
// `af agent-server`, so the sandbox is always version-matched to the daemon
// (Sachin-locked Q3). The round-trip test overrides it with a freshly built
// static binary compatible with the test image.
var dockerSelfBinary = os.Executable

// SetDockerSelfBinaryForTest overrides the `af` binary the docker runtime copies
// into the sandbox and returns a restore function. The round-trip integration
// test uses it to point at a freshly built static binary compatible with its test
// image (the test binary itself is not `af`).
func SetDockerSelfBinaryForTest(path string) func() {
	prev := dockerSelfBinary
	dockerSelfBinary = func() (string, error) { return path, nil }
	return func() { dockerSelfBinary = prev }
}

// dockerBanner mirrors daemon.AgentServerInfo field-for-field (the JSON the
// `af agent-server` prints on startup). It is duplicated here rather than
// imported because daemon imports session (a cycle); the shared contract is the
// JSON tags, pinned by the round-trip test.
type dockerBanner struct {
	Addr  string `json:"addr"`
	Token string `json:"token"`
	Title string `json:"title"`
}

// dockerRuntime provisions a real container sandbox (#1592 Phase 4 PR4). Declared
// in runtime.go's registry; its Provision is here.
type dockerRuntime struct{}

func (dockerRuntime) Provision(spec ProvisionSpec) (ProvisionResult, error) {
	cfg, err := resolveRepoConfig(spec.RepoRoot)
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("backend=docker: cannot resolve repo config for %q: %w", spec.RepoRoot, err)
	}
	image := ""
	var runArgs []string
	if cfg.Docker != nil {
		image = strings.TrimSpace(cfg.Docker.Image)
		runArgs = cfg.Docker.RunArgs
	}
	if image == "" {
		return ProvisionResult{}, fmt.Errorf("backend=docker requires docker.image to be set in this repo's .agent-factory/config.json (the container image that carries git + tmux + the agent CLIs; the `af` binary is copied in automatically)")
	}
	if spec.CloneURL == "" {
		return ProvisionResult{}, fmt.Errorf("backend=docker: repo %q has no `origin` remote to clone the workspace from; add one (GitHub is the durable workspace store) or push the repo first", spec.RepoRoot)
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return ProvisionResult{}, fmt.Errorf("backend=docker: the `docker` CLI is not on PATH; install Docker or select a different backend: %w", err)
	}

	afBin, err := dockerSelfBinary()
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("backend=docker: cannot locate the af binary to copy into the container: %w", err)
	}

	p := &dockerProvisioner{
		spec:    spec,
		image:   image,
		runArgs: runArgs,
		afBin:   afBin,
		program: spec.Program,
	}
	res, err := p.provision()
	if err != nil {
		// Reap anything the failed provision left running so a container never
		// leaks on a partial failure.
		if p.containerID != "" {
			p.reap()
		}
		return ProvisionResult{}, err
	}
	return res, nil
}

// dockerProvisioner holds the state of one container provisioning so its steps
// (run/clone/cp/exec/port) and its reap closure share the container ID.
type dockerProvisioner struct {
	spec    ProvisionSpec
	image   string
	runArgs []string
	afBin   string
	program string

	containerID string
	reapOnce    sync.Once
}

// provision runs the full container lifecycle and returns the wiring a docker
// session needs. Each step wraps docker's combined output in the error so a
// failure is self-diagnosing.
func (p *dockerProvisioner) provision() (ProvisionResult, error) {
	if err := p.runContainer(); err != nil {
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
	hostPort, err := p.publishedPort()
	if err != nil {
		return ProvisionResult{}, err
	}

	endpoint := &AgentServerEndpoint{
		URL:   "http://127.0.0.1:" + hostPort,
		Token: banner.Token,
	}
	teardown := p.reap
	log.InfoLog.Printf("docker runtime: session %q running in container %s, agent-server at %s", p.spec.Title, p.shortID(), endpoint.URL)
	return ProvisionResult{
		Backend:  &dockerBackend{containerID: p.containerID, reap: teardown},
		Endpoint: endpoint,
		Teardown: teardown,
	}, nil
}

// runContainer starts the sandbox container detached and kept alive, publishing
// the in-container agent-server port on a random loopback host port. The image's
// own entrypoint is overridden with a long sleep: the container is a host for the
// agent-server we exec into it, not the image's default process. run_args are
// appended verbatim (extra mounts/env/limits, or — in the round-trip test — the
// bind-mount that serves the clone source).
func (p *dockerProvisioner) runContainer() error {
	args := []string{
		"run", "-d",
		"--label", dockerSessionLabel + "=" + Slugify(p.spec.Title),
		"-e", "HOME=/root",
		"-p", "127.0.0.1::" + dockerAgentPort,
	}
	args = append(args, p.runArgs...)
	args = append(args, "--entrypoint", "sleep", p.image, "2147483647")

	out, err := p.docker(dockerProvisionStepTimeout, args...)
	if err != nil {
		return fmt.Errorf("backend=docker: `docker run` failed for image %q: %s: %w", p.image, strings.TrimSpace(string(out)), err)
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return fmt.Errorf("backend=docker: `docker run` returned no container id (output: %q)", string(out))
	}
	p.containerID = id
	return nil
}

// configureGit sets a git identity and marks every directory safe inside the
// container so the clone + worktree creation (which run as root over a possibly
// foreign-owned bind mount) don't trip on "dubious ownership" or a missing
// committer identity.
func (p *dockerProvisioner) configureGit() error {
	script := `git config --global user.email "af@agent-factory.local" && ` +
		`git config --global user.name "Agent Factory" && ` +
		`git config --global --add safe.directory "*"`
	out, err := p.execSh(dockerShortStepTimeout, script)
	if err != nil {
		return fmt.Errorf("backend=docker: git config in container failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// cloneWorkspace clones the repo's origin into /workspace inside the container.
// A fresh create clones the default branch; the in-container agent-server's LOCAL
// backend then creates the session's git worktree + branch off it. On a RESTORE
// (spec.RestoreBranch set, #1592 Phase 4 PR6) it additionally materializes the
// pushed session branch as a local ref so the in-container Setup checks it out.
func (p *dockerProvisioner) cloneWorkspace() error {
	script := fmt.Sprintf("git clone %s %s", shellQuote(p.spec.CloneURL), shellQuote(dockerWorkspaceDir))
	out, err := p.execSh(dockerProvisionStepTimeout, script)
	if err != nil {
		return fmt.Errorf("backend=docker: cloning %q into the container failed (is git in the image, and the URL reachable from inside the container?): %s: %w",
			p.spec.CloneURL, strings.TrimSpace(string(out)), err)
	}
	if branch := strings.TrimSpace(p.spec.RestoreBranch); branch != "" {
		return p.fetchRestoreBranch(branch)
	}
	return nil
}

// fetchRestoreBranch materializes the archived session branch (pushed to origin
// at archive time) as a LOCAL ref in the fresh clone, WITHOUT checking it out in
// /workspace's main tree (#1592 Phase 4 PR6). The `<branch>:<branch>` refspec
// creates refs/heads/<branch> from origin/<branch>; the in-container local
// backend's Setup then finds that local ref and adds the session worktree from
// it (setupFromExistingBranch), bringing the pushed commits back. It stays on the
// main tree's default branch so the later `git worktree add <path> <branch>` does
// not collide with a checked-out branch.
func (p *dockerProvisioner) fetchRestoreBranch(branch string) error {
	script := fmt.Sprintf("git -C %s fetch origin %s:%s",
		shellQuote(dockerWorkspaceDir), shellQuote(branch), shellQuote(branch))
	out, err := p.execSh(dockerProvisionStepTimeout, script)
	if err != nil {
		return fmt.Errorf("backend=docker: restoring archived branch %q into the container failed (was it pushed to origin?): %s: %w",
			branch, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// copyAfBinary docker cp's the daemon's own `af` binary into the container and
// makes it executable, so `af agent-server` is runnable inside. The binary must
// be compatible with the image (matching arch/libc) — a static build runs
// anywhere; documented in docs/backends.md.
func (p *dockerProvisioner) copyAfBinary() error {
	out, err := p.docker(dockerShortStepTimeout, "cp", p.afBin, p.containerID+":"+dockerAfBinaryPath)
	if err != nil {
		return fmt.Errorf("backend=docker: copying the af binary into the container failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	if out, err := p.execSh(dockerShortStepTimeout, "chmod +x "+shellQuote(dockerAfBinaryPath)); err != nil {
		return fmt.Errorf("backend=docker: chmod on the copied af binary failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// startAgentServer launches `af agent-server` headless in the container,
// detached, with its stdout banner + stderr redirected to files. It binds
// :<port> (all interfaces) so the published host port forwards to it. The
// workspace title matches the daemon-side session title so the daemon's remote
// client dials /v1/sessions/{title}/stream. readBanner then polls the banner file.
func (p *dockerProvisioner) startAgentServer() error {
	inner := fmt.Sprintf("%s agent-server --listen :%s --repo %s --title %s",
		shellQuote(dockerAfBinaryPath), dockerAgentPort, shellQuote(dockerWorkspaceDir), shellQuote(p.spec.Title))
	if strings.TrimSpace(p.program) != "" {
		inner += " --program " + shellQuote(p.program)
	}
	if p.spec.AutoYes {
		inner += " --auto-yes"
	}
	inner += fmt.Sprintf(" >%s 2>%s", shellQuote(dockerBannerPath), shellQuote(dockerLogPath))

	// -d: detach. The agent-server keeps running in the container after this exec
	// client returns; we read its banner from the file below.
	out, err := p.docker(dockerShortStepTimeout, "exec", "-d", p.containerID, "sh", "-c", inner)
	if err != nil {
		return fmt.Errorf("backend=docker: starting af agent-server in the container failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// readBanner polls the in-container banner file until the agent-server has bound
// its listener and printed its {addr,token} JSON line, or times out.
// On timeout it pulls the agent-server's stderr log into the error so a failure
// to start (bad image, missing tmux, port clash) is self-diagnosing.
func (p *dockerProvisioner) readBanner() (dockerBanner, error) {
	deadline := time.Now().Add(dockerBannerPollTimeout)
	for {
		out, err := p.docker(dockerShortStepTimeout, "exec", p.containerID, "cat", dockerBannerPath)
		if err == nil {
			var b dockerBanner
			if jErr := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &b); jErr == nil && b.Addr != "" && b.Token != "" {
				return b, nil
			}
		}
		if time.Now().After(deadline) {
			logOut, _ := p.docker(dockerShortStepTimeout, "exec", p.containerID, "cat", dockerLogPath)
			return dockerBanner{}, fmt.Errorf("backend=docker: af agent-server did not report a startup banner within %s; container log:\n%s",
				dockerBannerPollTimeout, strings.TrimSpace(string(logOut)))
		}
		// The poll interval bounds how long we can outrun the deadline; use a
		// non-wall-clock sleep so it is deterministic under test time control.
		time.Sleep(dockerBannerPollInterval)
	}
}

// publishedPort reads the random host port docker mapped the agent-server port
// to, so the daemon can dial it on loopback.
func (p *dockerProvisioner) publishedPort() (string, error) {
	out, err := p.docker(dockerShortStepTimeout, "port", p.containerID, dockerAgentPort+"/tcp")
	if err != nil {
		return "", fmt.Errorf("backend=docker: reading the published agent-server port failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	port := parseDockerPort(string(out))
	if port == "" {
		return "", fmt.Errorf("backend=docker: could not parse a host port from `docker port` output: %q", string(out))
	}
	return port, nil
}

// reap removes the container, idempotently. It runs on the session's Kill (after
// the in-container workspace is torn down over REST), on a provisioning failure,
// and on a bad-endpoint NewInstance failure — so a container is never leaked. The
// sync.Once makes the repeated Kill retries and the Kill-vs-provision-failure
// races collapse to one `docker rm -f`.
func (p *dockerProvisioner) reap() error {
	var reapErr error
	p.reapOnce.Do(func() {
		if p.containerID == "" {
			return
		}
		out, err := p.docker(dockerReapTimeout, "rm", "-f", p.containerID)
		if err != nil {
			reapErr = fmt.Errorf("backend=docker: `docker rm -f %s` failed: %s: %w", p.shortID(), strings.TrimSpace(string(out)), err)
			log.WarningLog.Printf("%v", reapErr)
			return
		}
		log.InfoLog.Printf("docker runtime: reaped container %s for session %q", p.shortID(), p.spec.Title)
	})
	return reapErr
}

// docker runs `docker <args...>` with a timeout and returns its combined output.
func (p *dockerProvisioner) docker(timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return exec.CommandContext(ctx, "docker", args...).CombinedOutput()
}

// execSh runs a `sh -c <script>` inside the container.
func (p *dockerProvisioner) execSh(timeout time.Duration, script string) ([]byte, error) {
	return p.docker(timeout, "exec", p.containerID, "sh", "-c", script)
}

func (p *dockerProvisioner) shortID() string {
	if len(p.containerID) > 12 {
		return p.containerID[:12]
	}
	return p.containerID
}

// parseDockerPort extracts the host port from `docker port` output. The output is
// one or more `<hostip>:<port>` lines (IPv4 and possibly IPv6); we take the first
// line's trailing port after the last colon, which is what the daemon dials on
// 127.0.0.1.
func parseDockerPort(out string) string {
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if idx := strings.LastIndex(line, ":"); idx >= 0 && idx+1 < len(line) {
			return strings.TrimSpace(line[idx+1:])
		}
	}
	return ""
}

// originRemoteURL returns the `origin` remote URL of the git repo at repoRoot, or
// "" if there is none / the path is not a repo. It is the clone source a
// sandboxed runtime pulls the workspace from (GitHub is the durable store). Best-
// effort by design: the docker runtime surfaces the actionable "no origin" error
// at create time when this is empty.
func originRemoteURL(repoRoot string) string {
	ctx, cancel := context.WithTimeout(context.Background(), dockerShortStepTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", repoRoot, "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// dockerBackend is the in-process Backend for a docker session. Where LocalBackend
// drives a local tmux session, this backend's agent-facing operations delegate to
// the instance's remote AgentServer (the HTTP/WS client to the in-container
// agent-server) — so lifecycle, preview, prompt, and liveness all go over the wire
// to the container. Its ONE local responsibility is reaping the container, which
// it shares (via the same idempotent closure) with the AgentServer Kill path.
//
// The daemon's observation/delivery paths speak to a session ONLY through
// Instance.AgentServer() (the remoteAgentServer), so most of these methods exist
// for interface completeness + the create flow (which drives Preview/SendPrompt
// through the Instance→backend wrappers). None re-enters the backend: the docker
// instance's AgentServer() is the REMOTE client, not localAgentServer, so a
// backend→AgentServer call is a network hop, never a loop.
type dockerBackend struct {
	containerID string
	// reap removes the container; shared with the runtime's Teardown / the
	// AgentServer Kill path, and idempotent (a sync.Once behind the closure).
	reap func() error
}

var _ Backend = (*dockerBackend)(nil)

func (b *dockerBackend) Type() string { return "docker" }

// Capabilities advertises docker's parity (#1592 Phase 4 PR6, epic §5): the
// workspace is off-box, but attach/preview/liveness/prompt work through the
// in-container agent-server, and Archive/Recover work by pushing the branch to
// GitHub and re-provisioning a fresh container that clones it back (§5.1) — no
// ErrRecoverUnsupported and no locality special-case.
//
// TabManagement is false (#1874). It is the ONE capability the in-container
// agent-server does not yet service: the agent-server's tab surface is data
// plane only (Subscribe/Input/Resize address an EXISTING tab by index or stable
// id) — there is no create/close tab RPC, so every Add*Tab path still requires a
// daemon-side git worktree this runtime does not have. Advertising it true
// offered tab affordances (the web menu's new-tab/VS Code items, the TUI `t`/`w`
// keys) that could only ever fail. The honest bit is false until tab creation
// routes through the agent-server; see #1874 for what that needs.
func (b *dockerBackend) Capabilities() Capabilities {
	return Capabilities{
		Workspace:        WorkspaceRemote,
		Archive:          true,
		Recover:          true,
		TabManagement:    false,
		TerminalTab:      true,
		InteractiveInput: true,
	}
}

// Start provisions then launches the in-container workspace. The container +
// agent-server were established by the runtime (during NewInstance); Start drives
// the agent-server's own provision/launch over REST, so the in-container LOCAL
// backend creates the git worktree + branch and spawns the agent.
func (b *dockerBackend) Start(i *Instance, firstTimeSetup bool) error {
	if err := b.Provision(i, firstTimeSetup); err != nil {
		return err
	}
	return b.Launch(i, firstTimeSetup)
}

// Provision drives the in-container agent-server's Provision over the wire (clone
// is already done by the runtime; this creates the in-container git worktree).
func (b *dockerBackend) Provision(i *Instance, firstTimeSetup bool) error {
	return i.AgentServer().Provision(firstTimeSetup)
}

// Launch drives the in-container agent-server's Launch (spawn the agent), sets the
// started flag, and seeds the daemon-side tab model with the agent tab (the
// container owns the real tabs; the daemon-side list mirrors the agent tab so the
// UI renders it, matching the remote-hook baseline).
func (b *dockerBackend) Launch(i *Instance, firstTimeSetup bool) error {
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

// Kill reaps the container. Instance.Kill routes through the AgentServer (which
// tears the in-container workspace down over REST and then runs the same reap), so
// this is normally reached only if something calls backend.Kill directly; the
// shared idempotent reap makes either path safe.
func (b *dockerBackend) Kill(i *Instance) error {
	i.mu.Lock()
	i.started = false
	i.mu.Unlock()
	if b.reap != nil {
		return b.reap()
	}
	return nil
}

// CloseAttachOnly discards this instance's local view WITHOUT reaping the
// container — used to drop a duplicate Instance built from disk that lost a race
// to the canonical one (#867). Reaping here would tear down the container the
// canonical Instance shares, so it must not run.
func (b *dockerBackend) CloseAttachOnly(i *Instance) error {
	i.mu.Lock()
	i.started = false
	i.mu.Unlock()
	return nil
}

func (b *dockerBackend) Preview(i *Instance) (string, error) {
	return i.AgentServer().Preview(0, false)
}

func (b *dockerBackend) PreviewFullHistory(i *Instance) (string, error) {
	return i.AgentServer().Preview(0, true)
}

func (b *dockerBackend) HasUpdated(i *Instance) (updated bool, hasPrompt bool, content string) {
	obs, err := i.AgentServer().Snapshot()
	if err != nil {
		return false, false, ""
	}
	return obs.Updated, obs.HasPrompt, obs.Content
}

func (b *dockerBackend) SendPromptCommand(i *Instance, prompt string) error {
	return i.AgentServer().SendPrompt(prompt)
}

func (b *dockerBackend) IsAlive(i *Instance) bool {
	// Backend.IsAlive is bool by contract, so an unanswerable probe collapses to
	// "not alive" here. That is safe ONLY because this method's callers
	// (Instance.TmuxAlive, for TUI affordance checks) merely gray out a control.
	// The daemon's destructive paths — Lost/re-provision/respawn — must NOT come
	// through here; they call AgentServer().Alive() directly and branch on the
	// error, because for them "unreachable" and "dead" are not the same (#1794).
	alive, _ := i.AgentServer().Alive()
	return alive
}

// CheckAndHandleTrustPrompt is a daemon-side no-op: the in-container agent-server
// dismisses trust/permission prompts itself on every Snapshot (its localAgentServer
// runs CheckAndHandleTrustPrompt before reading the pane), so there is nothing for
// the daemon to do over the wire.
func (b *dockerBackend) CheckAndHandleTrustPrompt(*Instance) bool { return false }

func (b *dockerBackend) TapEnter(i *Instance) { i.AgentServer().TapEnter() }

// Recover/Respawn re-establish a docker session by RE-PROVISIONING a fresh
// container that clones the session's branch back from origin, then relaunching
// the agent (#1592 Phase 4 PR6). Both share recoverSandbox with the ssh runtime
// (written once against the Runtime interface): a disposable container has no
// in-place session to reconnect, so recovery is always a fresh provision + clone
// of the durable branch on GitHub. Only the pushed state survives — a session
// that went Lost without ever pushing recovers to whatever origin last held.
func (b *dockerBackend) Recover(i *Instance) error { return recoverSandbox(i) }
func (b *dockerBackend) Respawn(i *Instance) error { return recoverSandbox(i) }

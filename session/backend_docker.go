package session

import (
	"context"
	"encoding/json"
	"errors"
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
	dockerBannerPollTimeout    = 45 * time.Second
	dockerBannerPollInterval   = 400 * time.Millisecond
)

// dockerReapTimeout bounds the `docker rm -f` container reap. A var (not a const)
// only so tests can shorten it to exercise the deadline path; production never
// reassigns it. Mirrors networkGitTimeout / tmuxCommandTimeout.
var dockerReapTimeout = 30 * time.Second

// dockerWaitDelay bounds how long cmd.Wait blocks after the docker CLI (or the
// origin-lookup git in originRemoteURL) exits or is killed on its deadline,
// before the inherited stdout/stderr pipes are force-closed. exec.CommandContext
// kills only the direct child; any descendant that inherited the capture pipe
// keeps its read end open, and CombinedOutput()/Output() block on pipe EOF until
// that descendant dies — which would silently defeat the timeouts above, so a
// wedged docker daemon could hang the container reap forever (the guarantee
// dockerReapTimeout reads as, but wasn't). Mirrors gitWaitDelay (#856/#896);
// none of these children legitimately background a long-lived process, so
// reaping the straggler is safe (#1967).
const dockerWaitDelay = 2 * time.Second

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

// dockerExec runs `docker <args...>` and returns its combined output. It is a
// package-level seam (mirroring dockerSelfBinary / lookPath) so tests can drive
// the runtime against a fake docker CLI — including the create-then-fail path
// (#2008) — without a real daemon on the box. Production wraps exec.CommandContext.
var dockerExec = func(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.WaitDelay = dockerWaitDelay
	out, err := cmd.CombinedOutput()
	if errors.Is(err, exec.ErrWaitDelay) {
		// docker itself exited cleanly — a non-zero exit surfaces as an
		// *exec.ExitError, not ErrWaitDelay — and only a descendant held the
		// capture pipe open past dockerWaitDelay and was just force-closed. The
		// output is already complete, so this is not a command failure: treating
		// it as one would fail a healthy `docker rm -f` and report a leaked
		// container that was in fact reaped (#1966/#1967, #676/#914 precedent).
		err = nil
	}
	return out, err
}

// SetDockerExecForTest overrides the docker CLI runner and returns a restore func.
func SetDockerExecForTest(f func(ctx context.Context, args ...string) ([]byte, error)) func() {
	prev := dockerExec
	dockerExec = f
	return func() { dockerExec = prev }
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
	// The config precondition is shared with the ListBackends RPC (#1933), so the
	// web states this requirement at choose time in the same words the CLI prints
	// here at create time.
	if err := BackendConfigError(BackendDocker, cfg); err != nil {
		return ProvisionResult{}, err
	}
	// Safe to dereference: BackendConfigError returns non-nil for a missing Docker
	// section, so reaching here means it is set (locked by
	// TestBackendConfigError_ReportsRepoConfigRequirements — weakening that check
	// fails the test rather than panicking the daemon here).
	image := strings.TrimSpace(cfg.Docker.Image)
	runArgs := cfg.Docker.RunArgs
	// Both wordings are shared with BackendUnusableReason (#1933) so the reason the
	// web gives at choose time is the reason printed here at create time. The check
	// ORDER is unchanged: origin before the CLI probe.
	if spec.CloneURL == "" {
		return ProvisionResult{}, missingOriginError(BackendDocker, spec.RepoRoot)
	}
	if _, err := lookPath("docker"); err != nil {
		return ProvisionResult{}, dockerCLIMissingError(err)
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

	// reap memoizes across the repeated Kill retries and the Kill-vs-provision-
	// failure race, but only for a reap that COMPLETED: reaped latches on success
	// or on a failure docker ANSWERED with (already-gone, etc.), storing the outcome
	// in reapErr. A TIMEOUT deliberately does NOT latch — the reap did not complete
	// and the container may still be running, so the daemon's kill-retry must be able
	// to re-run `docker rm -f` (#2049 / #2063 review). A plain sync.Once cannot
	// express "done, but re-runnable", so this is a mutex + explicit latch.
	reapMu  sync.Mutex
	reaped  bool
	reapErr error
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
		Backend: &dockerBackend{
			containerID:        p.containerID,
			remoteAgentBackend: remoteAgentBackend{reap: teardown},
			provisioner:        p,
			cleanup:            &DockerRuntimeCleanupData{ContainerID: p.containerID},
		},
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
		// `docker run -d` can CREATE the container and print its id to stdout, then
		// still exit non-zero when a later start step fails — a run_arg naming a
		// nonexistent device/volume/network, `--gpus all` on a GPU-less host. The
		// container is then left behind in `created` state. Capture the id off the
		// combined output even on this error path and store it so the
		// provision-failure reap in Provision (guarded on p.containerID != "") removes
		// it instead of leaking it (#2008). This is the exec started-vs-succeeded
		// rule: gate cleanup on the container having been CREATED, not on `docker run`
		// exiting zero.
		if id := parseCreatedContainerID(out); id != "" {
			p.containerID = id
		}
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

// reap removes the container. It runs on the session's Kill (after the in-container
// workspace is torn down over REST), on a provisioning failure, and on a
// bad-endpoint NewInstance failure — so a container is never leaked.
//
// It is memoized but only for a reap that COMPLETED: a success, or a failure docker
// ANSWERED with, latches (reaped=true, outcome in reapErr) so the repeated Kill
// retries and the Kill-vs-provision-failure race collapse to that one result.
//
// A TIMEOUT deliberately does NOT latch. The `docker rm -f` was SIGKILLed mid-reap,
// so it did not complete and the container may STILL be running; the daemon's
// kill-retry (finishUserKill, on every poll for the retained record) must therefore
// be able to actually re-run `docker rm -f` rather than see a stale nil. This is the
// #2063-review defect the naive sync.Once had: it latched on the timeout too, so the
// second reap() skipped the closure and returned nil (reapErr was a per-call local),
// the row was deleted, and the container was orphaned exactly one poll later —
// retention that lasted a single tick. Not latching a timeout makes retention
// durable AND the reap genuinely retry-able.
//
// While a reap keeps timing out it keeps returning the ErrWorkspaceStateUnknown
// sentinel (row retained); once docker answers — a later `docker rm -f` succeeds or
// reports (e.g. "No such container", already gone) — it latches and the row may go.
// It never silently returns nil while the container may still be leaked.
func (p *dockerProvisioner) reap() error {
	// Held across the docker call so concurrent reaps serialize (no double
	// `docker rm -f`) and the latch is read/written race-free — the same
	// mutual-exclusion the sync.Once gave, minus its permanent latch.
	p.reapMu.Lock()
	defer p.reapMu.Unlock()
	if p.reaped {
		return p.reapErr
	}
	if p.containerID == "" {
		p.reaped = true
		return nil
	}
	// Own the context here (rather than via p.docker) so a tripped deadline is
	// distinguishable from a reap that docker ANSWERED with an error — the two mean
	// opposite things for the record (#2049).
	ctx, cancel := context.WithTimeout(context.Background(), dockerReapTimeout)
	defer cancel()
	out, err := dockerExec(ctx, "rm", "-f", p.containerID)
	if err == nil {
		p.reaped = true
		p.reapErr = nil
		log.InfoLog.Printf("docker runtime: reaped container %s for session %q", p.shortID(), p.spec.Title)
		return nil
	}
	reapErr := fmt.Errorf("backend=docker: `docker rm -f %s` failed: %s: %w", p.shortID(), strings.TrimSpace(string(out)), err)
	// The #914 guard: classify a TIMEOUT only when err != nil AND the ctx deadline
	// tripped, so a bare exec.ErrWaitDelay that dockerExec already normalized to
	// success (err == nil) never reaches here.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		// Unknown-state, and NOT latched: RETAIN the row now and re-run on the next
		// reap. (#2049 classification + #2063-review retry.)
		reapErr = fmt.Errorf("%w: %w", ErrWorkspaceStateUnknown, reapErr)
		log.WarningLog.Printf("%v", reapErr)
		return reapErr
	}
	// docker ANSWERED with an error: the reap completed and TOLD us something, so it
	// latches as a KNOWN-state error and the row may be deleted, per the documented
	// deleteSessionRecord contract. Latching also stops retries from re-running a
	// command that will keep answering the same way.
	p.reaped = true
	p.reapErr = reapErr
	log.WarningLog.Printf("%v", reapErr)
	return reapErr
}

// docker runs `docker <args...>` with a timeout and returns its combined output.
func (p *dockerProvisioner) docker(timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return dockerExec(ctx, args...)
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

// parseCreatedContainerID extracts the container id `docker run -d` prints to
// stdout the moment it CREATES the container — even when a later start step fails
// and docker exits non-zero (a run_arg naming a nonexistent device/volume/network,
// `--gpus all` on a GPU-less host). docker writes the 64-char id to stdout and the
// start error to stderr, and CombinedOutput interleaves them — so a bare TrimSpace
// of the whole blob can't be trusted on the error path, but a line that is nothing
// but hex digits is unambiguously the id (docker's error lines carry words, spaces
// and colons). Returns "" when no such line is present — docker failed before
// creating anything — leaving containerID empty so nothing is reaped.
func parseCreatedContainerID(out []byte) string {
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if len(line) >= 12 && isHexString(line) {
			return line
		}
	}
	return ""
}

// isHexString reports whether every rune of s is a hex digit. A full docker
// container id is 64 lowercase hex chars; uppercase is accepted too so the match
// never turns on docker's casing.
func isHexString(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
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
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "remote", "get-url", "origin")
	cmd.WaitDelay = dockerWaitDelay
	out, err := cmd.Output()
	// ErrWaitDelay means git exited cleanly and only a straggler held the pipe
	// past dockerWaitDelay; the URL is already in out, so it is not a failure.
	// Without this guard the WaitDelay bound would turn a healthy lookup into a
	// spurious "" and mask a real origin (#1967).
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
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
// Its shared remote-AgentServer behavior lives in remoteAgentBackend; this type
// retains only Docker-specific provisioning, container identity, and the
// serialized backend discriminator.
type dockerBackend struct {
	remoteAgentBackend
	containerID string
	// provisioner owns the concrete reaper; cleanup is its immutable storage-only
	// identity. Both are nil for an ordinary inert backend loaded from a live row.
	provisioner *dockerProvisioner
	cleanup     *DockerRuntimeCleanupData
}

var _ Backend = (*dockerBackend)(nil)

func (b *dockerBackend) Type() string { return "docker" }

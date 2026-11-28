package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
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
	// dockerContainerHome is the HOME the container runs with (set via `-e
	// HOME=…`). Agent credential mounts (agentCredentialMounts) and the agent's
	// own config land under it; a constant keeps those mount targets in lockstep
	// with the runContainer env so the two never drift.
	dockerContainerHome = "/root"
	// Account sessions run as the owner the bind mount reports inside the
	// container, with a separate writable HOME for af/git state. The account
	// itself remains mounted at dockerAccountHome and is selected by CODEX_HOME or
	// CLAUDE_CONFIG_DIR.
	dockerAccountRuntimeHome = "/af-home"
	// dockerAfBinaryPath is where the daemon's `af` binary is copied in the
	// container so `af agent-server` is on PATH for the exec below.
	dockerAfBinaryPath = "/usr/local/bin/af"
	// dockerBannerPath/dockerLogPath capture the agent-server's stdout banner and
	// stderr log inside the container. The banner is a single JSON line (addr,
	// token) the server prints the instant its listener binds; the
	// runtime polls the file for it because `docker exec -d` detaches the process.
	dockerBannerPath = "/tmp/af-agent-server.json"
	dockerLogPath    = "/tmp/af-agent-server.log"
	// dockerPidPath captures the agent-server's in-container PID, written by
	// the startAgentServer wrapper so readBanner can tell "still starting"
	// from "died before printing the banner" (#5571 W2.1).
	dockerPidPath = "/tmp/af-agent-server.pid"
	// dockerContainerShell is the absolute path every `docker exec` af issues
	// resolves as its program — never a bare "sh". A bare name would resolve
	// through the container's PATH env, which a repo-controlled source can
	// shape (docker.run_args -e PATH=…, image ENV): the resolved program
	// could intercept or rewrite every script af runs, including the
	// agent-server launch that carries the credential pins (#7529 R7). POSIX
	// guarantees /bin/sh; a script's INNER commands (git, cat, chmod) still
	// resolve through PATH, which the credential-mount path re-asserts to the
	// image's own Config.Env value — the image's binaries are the documented
	// residual trust there.
	dockerContainerShell = "/bin/sh"
	// dockerContainerCat is the absolute form af execs directly instead of a
	// shell for single-command reads — the same reasoning as
	// dockerContainerShell: never a PATH-resolved name (#7529 R7). cat is
	// /bin/cat on every coreutils and busybox image; an image whose tool
	// lives elsewhere surfaces as the same step error a PATH miss would have
	// produced — fail-closed.
	dockerContainerCat = "/bin/cat"
	// dockerSessionLabel tags every managed container so orphans can be swept with
	// `docker ps -aq -f label=af.session` (documented in docs/backends.md). The
	// runtime reaps by container ID; the label is for operators.
	dockerSessionLabel = "af.session"
	// dockerHomeLabel scopes a container to the AF home (daemon) that created it,
	// stamped with config.GetConfigDir() at `docker run`. The orphan sweeper
	// (#2194) filters on it so a shared Docker engine hosting more than one af
	// home never lets one daemon reap another's containers, and so a container
	// with no af.home label (a pre-upgrade one) is treated as not-ours and left
	// alone. The value is the AF home path — human-legible in `docker inspect`.
	dockerHomeLabel = "af.home"
)

// dockerContainerStatPaths are the absolute stat locations af tries in
// order — coreutils' /usr/bin/stat first, then BusyBox's /bin/stat (BusyBox
// registers applets under BB_DIR_BIN). The list is exhaustive rather than
// PATH-resolved, for the same reason as dockerContainerShell (#7529 R7); an
// image whose stat lives elsewhere fails the owner probe closed with the
// last candidate's error.
var dockerContainerStatPaths = []string{"/usr/bin/stat", "/bin/stat"}

// docker command timeouts. Provisioning steps (run/clone/cp/exec) get a generous
// budget because a first-run image pull or a large clone can be slow; reaping is
// bounded tighter so a kill never hangs.
const (
	dockerProvisionStepTimeout = 5 * time.Minute
	dockerShortStepTimeout     = 30 * time.Second
)

// dockerBannerPollTimeout bounds how long readBanner waits for the in-container
// agent-server to print its startup banner; dockerBannerPollInterval is the
// poll cadence between attempts. Both are vars (not consts) only so tests can
// shorten them to exercise the deadline path; production never reassigns them.
// Mirrors dockerReapTimeout (networkGitTimeout / tmuxCommandTimeout precedent).
var (
	dockerBannerPollTimeout  = 45 * time.Second
	dockerBannerPollInterval = 400 * time.Millisecond
)

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
var dockerExec = func(ctx context.Context, environ []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = environ
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
func SetDockerExecForTest(f func(ctx context.Context, environ []string, args ...string) ([]byte, error)) func() {
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
	// Program is the command the host resolved and bound (#5571 W2.1).
	Program string `json:"program,omitempty"`
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

	// Resolve once in the trusted host process. The agent-server receives this
	// exact command with --program-resolved and must not reinterpret its enum.
	resolvedProgram := config.ResolveProgram(&cfg.Config, spec.Program)
	if spec.Account.Dir != "" {
		if err := refuseAccountAgentDrift(spec.Account.Name, spec.Account.Agent, resolvedProgram); err != nil {
			return ProvisionResult{}, fmt.Errorf("backend=docker: %w", err)
		}
		proof := accountLaunchProof(resolvedProgram, resolvedProgram,
			builtInProgramOverride(cfg, spec.Program, resolvedProgram), nil, "")
		// The built-in proof is established in the host namespace. Docker may not
		// contain that path (or may contain a different file there), so never grant
		// its executable identity across the container boundary. A bare agent name
		// remains independently provable; a host-qualified path fails closed below.
		hostExecutable := proof.TrustedExecutable
		spec.Account.TrustedExecutable = ""
		spec.Account.GeneratedArgs = proof.GeneratedArgs
		// Reuse the local shim's authoritative validation for cloud modes and
		// command-local identity assignments. The returned host environment is not
		// installed in Docker; accountMountAndEnv builds the container boundary.
		if _, aerr := sessionenv.ApplyAccount(os.Environ(), resolvedProgram, spec.Account); aerr != nil {
			if hostExecutable != "" && sessionenv.IsAccountCommandValidationError(aerr) {
				return ProvisionResult{}, fmt.Errorf(
					"backend=docker: resolved program names host executable %q, whose identity cannot be established inside the container: %w",
					hostExecutable, aerr)
			}
			return ProvisionResult{}, fmt.Errorf("backend=docker: %w", aerr)
		}
		// The validator takes the passthrough list AFTER the account filter:
		// the filtered list is what af forwards into the container as bare
		// -e NAME entries, so it is the set a repo -e in run_args could
		// last-wins override.
		filtered := filterAccountPassthrough(spec.SessionEnvPassthrough, spec.Account.Agent)
		if aerr := validateAccountDockerRunArgs(runArgs, spec.Account.Agent, filtered); aerr != nil {
			return ProvisionResult{}, aerr
		}
		spec.SessionEnvPassthrough = filtered
	}
	// Stamp the AF home onto the container (af.home label) so the orphan sweeper
	// can scope to this daemon's home. If it can't be resolved, omit the label —
	// the container just won't be sweep-tracked, which is the safe direction.
	homeID, homeErr := config.GetConfigDir()
	if homeErr != nil {
		log.WarningLog.Printf("backend=docker: cannot resolve the AF home for the af.home container label; this container will not be orphan-sweep-tracked: %v", homeErr)
		homeID = ""
	}
	p := &dockerProvisioner{
		spec:    spec,
		image:   image,
		runArgs: runArgs,
		afBin:   afBin,
		// The wire program is the configured name and flags — never af's
		// auto-detected daemon path, which the container's filesystem does
		// not have. The in-container agent-server resolves it on the host's
		// login PATH (#5571). resolvedProgram stays host-side: it is the
		// value the account boundary above validated.
		program: remoteProgramForHost(cfg, spec.Program),
		homeID:  homeID,
	}
	// docker_mount_agent_credentials is a GLOBAL-only operator grant (config.Config,
	// resolved from the global layer here — a repo cannot set it: the key is absent
	// from InRepoConfig, so a repo that tries hits the standard per-repo hard error).
	// When on, mount ONLY this session's own agent's credential file, read-only, so a
	// repo-selected image never sees another agent's credential or anything else the
	// operator did not grant. Resolved here in the host process (it stats the daemon
	// user's home) so runContainer's argv assembly stays pure.
	// An ACCOUNT replaces the ambient credential mounts entirely (#3082).
	//
	// Independent of docker.mount_agent_credentials on purpose: that key is the
	// opt-in for lending the container the operator's ambient identity, and an
	// account is the opposite request — a specific identity, named explicitly at
	// create time. Gating the account on it would leave an account-scoped session
	// with no credentials at all whenever the key is off, which reads as "the
	// account did not work".
	//
	// The `else` is the exclusivity guarantee: when an account is in play, nothing
	// resolved from the daemon user's home is mounted, so the container cannot hold
	// two identities.
	if spec.Account.Dir != "" {
		mount, env, aerr := accountMountAndEnv(spec.Account, p.bindMountRelabel())
		if aerr != nil {
			return ProvisionResult{}, fmt.Errorf("backend=docker: %w", aerr)
		}
		p.accountMount, p.accountEnv = mount, env
	} else if cfg.DockerMountAgentCredentials {
		// Validate repo-controlled run_args before the mount is installed.
		// docker.run_args is repository-controlled and checked in (the docker
		// table is sourceRepoOnly) and appended AFTER the credential mount with
		// no later env af emits to dominate it, so an entry setting the agent's
		// credential-root env var (CODEX_HOME / CLAUDE_CONFIG_DIR / GEMINI_CLI_HOME
		// — all built-in per-agent names) would redirect the agent's credential
		// lookup away from af's mounted file and start the session
		// unauthenticated while the provisioning log reports the mount as
		// installed. The account path's lexical guard (validateAccountDockerRunArgs)
		// defends the same redirect on its path; this is the symmetric guard for
		// the credential-mount path, refused BEFORE docker run rather than after.
		// The credential path does not filter the operator's passthrough the
		// way the account path does — a pane-boundary entry is refused
		// outright below rather than dropped — so the raw list is the
		// session's effective forward claim here.
		if err := validateCredentialDockerRunArgs(runArgs, p.agentName(), spec.SessionEnvPassthrough); err != nil {
			return ProvisionResult{}, err
		}
		p.credentialMounts = resolveAgentCredentialMounts(p.agentName(), p.bindMountRelabel())
		// The environment boundary behind the mount (#7526): every classified
		// credential-bearing or credential-routing name the operator did not
		// forward is pinned defined-and-empty after run_args, so it is empty
		// whatever its source — the repo-chosen image's ENV, run_args, or a
		// workspace config the agent loads later. Names the image or run_args
		// actually set get a once-per-session warning, names only: a working
		// setup that legitimately carried one (a corporate proxy, a CA bundle)
		// needs it forwarded through session_env_passthrough.
		if len(p.credentialMounts) > 0 {
			p.credentialEnvPins = credentialMountPinnedNames(p.agentName(), p.spec.SessionEnvPassthrough)
		}
	}
	// The pane-boundary blanking in runContainer runs on EVERY docker
	// session, so a passthrough entry naming one of those names would be
	// silently overridden — refuse it with a message rather than let the
	// blank win (#7544).
	for _, name := range spec.SessionEnvPassthrough {
		if _, ok := credentialMountPaneBoundaryNames[name]; ok {
			return ProvisionResult{}, fmt.Errorf(
				"backend=docker: session_env_passthrough cannot forward %s — af and tmux stamp that name at the pane boundary, so the forwarded value would be blanked on every docker session; remove %s from session_env_passthrough", name, name)
		}
	}
	res, err := p.provision()
	if err != nil {
		return ProvisionResult{}, p.reapProvisionFailure(err)
	}
	return res, nil
}

// parseCreatedContainerID extracts the container id `docker run -d` prints to
// stdout the moment it CREATES the container — even when a later start step fails
// and docker exits non-zero (a run_arg naming a nonexistent device/volume/network,
// `--gpus all` on a GPU-less host). docker writes the 64-char id to stdout and the
// start error to stderr, and CombinedOutput interleaves them — so a bare TrimSpace
// of the whole blob can't be trusted on the error path, but a line that is nothing
// but hex digits is unambiguously the id (docker's error lines carry words, spaces
// and colons). Returns "" when no such line is present — docker failed before
// creating anything — leaving containerID empty so nothing is reaped. The id is
// printed after any image-pull progress, so the last such line wins; progress
// can end in a bare hex line only if it is not the id's own output.
func parseCreatedContainerID(out []byte) string {
	id := ""
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if len(line) >= 12 && isHexString(line) {
			id = line
		}
	}
	return id
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

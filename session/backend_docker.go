package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
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
		if aerr := validateAccountDockerRunArgs(runArgs, spec.Account.Agent); aerr != nil {
			return ProvisionResult{}, aerr
		}
		spec.SessionEnvPassthrough = filterAccountPassthrough(spec.SessionEnvPassthrough, spec.Account.Agent)
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
		if err := validateCredentialDockerRunArgs(runArgs, p.agentName()); err != nil {
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

// warnBlankedCredentialNames emits the once-per-session migration warning
// for the classified names the image or docker.run_args had actually set
// and af overrode — pinned empty or re-asserted to a value; names only,
// values never inspected (#7526, #7531). It runs immediately after the
// container exists, BEFORE the dependent steps (trust, clone, agent-server):
// the image side must describe the image the container ACTUALLY runs —
// resolved off the created container, not whatever the configured tag
// inspected as before `docker run` pulled or updated it — and an
// overridden name a later step depended on (a proxy or private CA the
// clone needs) would make provisioning fail first, so the session most in
// need of the passthrough remedy would never see it (#7531). When that
// inspect fails, say so and warn over run_args names alone; the overrides
// are already in argv either way.
//
// The account-scoped half runs the same shape on the same spot: a value
// the image or run_args set for a name the account boundary discards is
// named to the operator, names only, with the identical remedy (#7668).
func (p *dockerProvisioner) warnBlankedCredentialNames() {
	if len(p.credentialMounts) == 0 && p.spec.Account.Dir == "" {
		return
	}
	imageEnv, ok := p.dockerContainerImageEnv()
	if !ok {
		log.WarningLog.Printf(
			"backend=docker: could not inspect the environment of the image the container runs — the overridden-env warning covers docker.run_args names only")
	}
	if len(p.credentialMounts) == 0 {
		p.warnDiscardedAccountEnvNames(imageEnv, ok)
		return
	}
	// Every name af overrode on this session: the defined-empty pins, the
	// re-asserted git config roots, and the re-asserted selector/endpoint
	// names — all post-exemption, so a name the operator claimed through
	// session_env_passthrough is not reported as overridden (#7531).
	overridden := append(slices.Clone(p.credentialEnvPins),
		credentialMountReassertNames(p.agentName(), p.spec.SessionEnvPassthrough, os.Environ())...)
	for _, name := range []string{"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM"} {
		if !slices.Contains(p.spec.SessionEnvPassthrough, name) {
			overridden = append(overridden, name)
		}
	}
	if named := credentialMountOverriddenWarning(imageEnv, p.runArgs, overridden); len(named) > 0 {
		log.WarningLog.Printf(
			"backend=docker: docker.mount_agent_credentials overrode %d env name(s) the image or docker.run_args had set for agent %q (pinned empty or re-asserted): %s — forward any the session needs through the global session_env_passthrough",
			len(named), p.agentName(), strings.Join(named, ", "))
	}
}

// warnDiscardedAccountEnvNames is the account-scoped half of the same
// notice (#7668): names the image's ENV or docker.run_args actually set
// that THIS launch's pipeline will not deliver — the identity pins and
// fills plus every keep-if-set name the operator did not forward — minus
// the proxy case twins reseated to a forwarded sibling's value (the
// operator's own value still arrives). Reported once, names only, with
// the same remedy the mount path gives. The caller already printed the
// inspect-failure line, which says run_args coverage alone is partial.
func (p *dockerProvisioner) warnDiscardedAccountEnvNames(imageEnv []string, inspected bool) {
	_ = inspected // the caller's single inspect-failure line covers the account case too
	if p.spec.Account.Dir == "" {
		return
	}
	agent := p.spec.Account.Agent
	discarded := discardSetForWarning(agent, p.spec.SessionEnvPassthrough)
	if len(discarded) == 0 {
		return
	}
	var set []string
	for name := range discarded {
		set = append(set, name)
	}
	named := credentialMountOverriddenWarning(imageEnv, p.runArgs, set)
	var kept []string
	for _, name := range named {
		// An unforwarded proxy spelling whose case twin was forwarded
		// reseats to the twin's daemon-env value at the boundary — an
		// operator value is delivered, so it is not a discard.
		if sib, twin := sessionenv.ProxyEnvCaseTwin(name); twin &&
			slices.Contains(p.spec.SessionEnvPassthrough, sib) && os.Getenv(sib) != "" {
			continue
		}
		kept = append(kept, name)
	}
	logDiscardedAccountEnvNames("docker", agent, "the image or docker.run_args", kept)
}

// dockerProvisioner holds the state of one container provisioning so its steps
// (run/clone/cp/exec/port) and its reap closure share the container ID.
type dockerProvisioner struct {
	spec    ProvisionSpec
	image   string
	runArgs []string
	// accountMount and accountEnv place a registered credential account inside the
	// container and point the agent at it (#3082). Set only for an account-scoped
	// session, and mutually exclusive with credentialMounts.
	accountMount []string
	accountEnv   []string
	// containerUser is the numeric uid:gid that owns dockerAccountHome as seen by
	// the container. On rootful Docker that is the host af user; on rootless Docker
	// it is commonly 0:0, which maps back to that same host user.
	containerUser string
	// credentialMounts are the read-only `-v host:container:<mode>` agent-credential
	// bind mounts (docker.mount_agent_credentials, #2194), resolved once in
	// Provision and appended to the `docker run` argv before runArgs.
	credentialMounts []string
	// credentialEnvPins are the classified credential-bearing/routing env names
	// the operator did not forward, emitted as `-e NAME=` after runArgs so they
	// are defined and empty whatever their source (#7526). Set only when
	// credentialMounts resolved a file; they are also admitted to the
	// agent-server's --session-env so the empty value survives the inner env
	// filter and reaches the agent — a workspace config loader treats
	// defined-empty as set.
	credentialEnvPins []string
	afBin             string
	program           string
	// hostProgram is the program the container actually resolved and bound,
	// reported back in the startup banner (#5571 W2.1). Empty until the banner
	// arrives; remoteAgentBackend falls back to the wire program without it.
	hostProgram string
	// homeID is the AF home (config.GetConfigDir()) stamped into the af.home
	// container label so the orphan sweeper can scope to this daemon's home
	// (#2194). Empty when the home cannot be resolved — the label is then omitted
	// and the container is simply never sweep-tracked (fail-safe: unlabelled is
	// not-ours), never mis-attributed.
	homeID string

	containerID        string
	engineID           string
	verifyEngineOnReap bool
	// dockerContextPin is the provision's pinned docker CLI selector, resolved
	// once in provisionSteps and materialized into dockerEnvironment as
	// DOCKER_CONTEXT — covering the engine-ID read, `docker run`, and every
	// exec/cp after — so a `docker context use` switch mid-provision cannot
	// put the container on a different engine than engineID and the staging
	// dir's engine-id marker record (Codex on #7616). Empty when the env
	// already carries a selector (DOCKER_HOST/DOCKER_CONTEXT outrank a
	// context switch on their own) and on non-provision paths (reap,
	// teardown), which re-resolve the current context exactly as before.
	dockerContextPin string

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

	// Staged payload delivery (#7577 item 2): stagingDir is the af-owned
	// per-session dir (removed on reap) holding stagedAfPath/stagedPolicyPath;
	// payloadViaBinds marks the bind-delivery decision (preparePayloadDelivery).
	stagingDir       string
	stagedAfPath     string
	stagedPolicyPath string
	payloadViaBinds  bool
}

// provision runs the full container lifecycle and returns the wiring a docker
// session needs. Each step wraps docker's combined output in the error so a
// failure is self-diagnosing.
//
// EVERY step's failure — not only `docker run`'s — can be the data root
// answering ENOSPC: the engine writes there when it creates the container's
// layer, and the in-container steps write through docker exec into the
// writable layer on the same filesystem. docker's combined output loses that
// errno as text the lost-restore classifier refuses to read, so any failed
// step re-derives the verdict from the data-root filesystem itself and joins
// it BEFORE the error returns to Provision's reap — reaping the failed
// container frees its partial layer data and could mask the fullness being
// measured (#5382). The probe still yields nothing when the engine is remote,
// foreign-namespaced, or reports free capacity — no invented errnos — and it
// only runs for step failures that could have BEEN a data-root write: each
// step's error carries its operation class (dockerStepError), so a local
// parse or validation failure on a coincidentally-full root stays structural
// instead of waiting on the environment's unbounded budget (#5382 review).
func (p *dockerProvisioner) provision() (ProvisionResult, error) {
	res, err := p.provisionSteps()
	if err != nil {
		var se *dockerStepError
		if errors.As(err, &se) && se.op != dockerOpLocal {
			if evidence := p.dockerRootFSFullEvidence(se.op); evidence != nil {
				err = errors.Join(err, evidence)
			}
		}
		return ProvisionResult{}, err
	}
	return res, nil
}

func (p *dockerProvisioner) provisionSteps() (ProvisionResult, error) {
	// Pin every provision-time docker call to the ONE selector resolution
	// chose: without an explicit selector each call re-resolves the current
	// context, so a `docker context use` switch landing between the
	// engine-ID read below and `docker run` would put the container on a
	// different engine than p.engineID — and the staging dir's engine-id
	// marker — records (Codex on #7616). The pin materializes into
	// dockerEnvironment FIRST so the locality gates below judge the endpoint
	// that will actually serve, the identity read names the consuming
	// engine, and `docker run` targets it. A selector that cannot even be
	// named fails the provision like any other unresolvable endpoint.
	if pin, err := resolveDockerContextPin(p.dockerEnvironment()); err != nil {
		return ProvisionResult{}, dockerStepErr(fmt.Errorf("backend=docker: cannot pin the Docker engine selector before creating a container: %w", err), dockerOpLocal)
	} else {
		p.dockerContextPin = pin
	}
	if p.spec.Account.Dir != "" {
		if err := p.ensureAccountDockerEngineLocal(); err != nil {
			return ProvisionResult{}, dockerStepErr(err, dockerOpLocal)
		}
	} else {
		// A remote Docker engine publishes the agent-server port on the engine
		// host's loopback while the daemon dials its own, so the dial-back is
		// unreachable and the failure surfaces only later as `connection refused`
		// to 127.0.0.1. The account path refuses the same engine for its own
		// reason (bind-mount identity); refuse here too, before `docker run`, so a
		// non-account session fails fast with the remote-engine cause instead.
		if err := p.ensureDockerEngineLocal(); err != nil {
			return ProvisionResult{}, dockerStepErr(err, dockerOpLocal)
		}
	}
	engineID, err := p.currentEngineID(dockerShortStepTimeout)
	if err != nil {
		return ProvisionResult{}, dockerStepErr(fmt.Errorf("backend=docker: cannot establish the Docker cleanup target before creating a container: %w", err), dockerOpLocal)
	}
	p.engineID = engineID
	// In a throwaway sandbox home, record the engine before the container
	// exists, or create none (#7447).
	if err := p.ensureThrowawayEngineRecorded(); err != nil {
		return ProvisionResult{}, dockerStepErr(err, dockerOpLocal)
	}
	// Deliver af's own payload through read-only binds appended last in
	// `docker run` — no cp/chmod for a run_args mount to redirect (#7577 item 2).
	p.preparePayloadDelivery()
	if err := p.runPayloadContainer(); err != nil {
		return ProvisionResult{}, dockerStepErr(err, dockerOpDaemonWrite)
	}
	// Warn NOW, while the only thing that exists is the created container:
	// a blanked name a dependent step needs (proxy/CA for the clone) makes
	// that step fail, and the error path reaps without ever reaching a
	// post-provision warning (#7531).
	p.warnBlankedCredentialNames()
	// The account boundary, verified against the RUNNING container before anything
	// touches it (#3598). The lexical run_args guard reads strings and cannot see a
	// symlink in the repository-selected image; this reads what Docker configured
	// and what the kernel resolved. A refusal returns here, and Provision's
	// reapProvisionFailure tears the container down on the way out — a shadowed
	// account never reaches the agent. No-op for a session with no account.
	if err := p.verifyAccountRuntimeBoundary(); err != nil {
		return ProvisionResult{}, dockerStepErr(err, dockerOpLocal)
	}
	if err := p.prepareContainerUser(); err != nil {
		return ProvisionResult{}, dockerStepErr(err, dockerOpRootExec)
	}
	if err := p.trustWorkspaceDirectories(); err != nil {
		return ProvisionResult{}, dockerStepErr(err, dockerOpSessionExec)
	}
	if err := p.cloneWorkspace(); err != nil {
		return ProvisionResult{}, dockerStepErr(err, dockerOpSessionExec)
	}
	if err := p.configureGit(); err != nil {
		return ProvisionResult{}, dockerStepErr(err, dockerOpSessionExec)
	}
	if err := p.deliverPayload(); err != nil {
		return ProvisionResult{}, err
	}
	if err := p.startAgentServer(); err != nil {
		return ProvisionResult{}, dockerStepErr(err, dockerOpSessionExec)
	}
	banner, err := p.readBanner()
	if err != nil {
		return ProvisionResult{}, dockerStepErr(err, dockerOpSessionExec)
	}
	// The banner's program is the command the HOST resolved and bound — the
	// true runtime evidence (#5108); the wire value is only its pre-resolution
	// form, kept as the fallback for a banner from an older agent-server.
	p.hostProgram = banner.Program
	if p.hostProgram == "" {
		p.hostProgram = p.program
	}
	hostPort, err := p.publishedPort()
	if err != nil {
		return ProvisionResult{}, dockerStepErr(err, dockerOpLocal)
	}

	endpoint := &AgentServerEndpoint{
		URL:   "http://127.0.0.1:" + hostPort,
		Token: banner.Token,
	}
	teardown := p.reap
	log.InfoLog.Printf("docker runtime: session %q running in container %s, agent-server at %s", p.spec.Title, p.shortID(), endpoint.URL)
	return ProvisionResult{
		// Only the branch this runtime actually handed the agent-server via
		// --branch is the one it ran on (#5971). A runtime that does not take
		// the daemon's name leaves this empty rather than guess.
		Branch: strings.TrimSpace(p.spec.Branch),
		Backend: &dockerBackend{
			// The command the in-container agent-server was bound to launch
			// (#5108) — the host-resolved value reported back in the banner
			// (#5571 W2.1) — recorded as runtime evidence when the launch RPC
			// returns.
			remoteAgentBackend: remoteAgentBackend{resolvedProgram: p.hostProgram},
			containerID:        p.containerID,
			provisioner:        p,
			cleanup: &DockerRuntimeCleanupData{
				ContainerID: p.containerID,
				EngineID:    p.engineID,
				// Base name only: restore re-roots it under the staging root.
				StagingDir: dockerStagingRecordName(p.stagingDir),
			},
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
		"-e", "HOME=" + p.sessionHome(),
		"-p", "127.0.0.1::" + dockerAgentPort,
	}
	for _, name := range p.containerEnvironmentNames() {
		// Docker looks the value up in the CLI process's filtered environment;
		// only the name appears in argv, logs, and test captures.
		args = append(args, "-e", name)
	}
	// Read-only agent credential mounts (docker.mount_agent_credentials, #2194) go
	// before run_args. docker.run_args is repository-controlled and checked in,
	// and for a non-account session nothing af emits later dominates it, so the
	// credential-mount path validates it (validateCredentialDockerRunArgs) before
	// the mount is installed — a repo entry setting the agent's credential-root
	// env var (e.g. CODEX_HOME) would otherwise redirect credential lookup away
	// from this read-only file and start the session unauthenticated while af's
	// log reported the mount. Ordering the mount before run_args keeps the two
	// sources distinct; validation, not precedence, is what closes the gap.
	args = append(args, p.credentialMounts...)
	// The account's mount and its config var. Placed before run_args like the
	// credential mounts, so an operator's own run_args still come last.
	args = append(args, p.accountMount...)
	args = append(args, p.runArgs...)
	// The ownership labels go AFTER run_args for the same reason the env pins
	// do: Docker gives the LAST --label for a key precedence, and run_args is
	// repository-controlled. A repo `--label af.session=`/`af.home=` entry
	// earlier in argv would otherwise relabel the container so the orphan
	// sweeps' label filters could not see it — invisible to `docker ps` while
	// still binding a staging dir whose recorded markers pass the identity
	// check (Codex on #7616). Emitting them last makes af's claim win.
	// Scope the container to this daemon's AF home for the orphan sweeper
	// (#2194). Omitted when the home is unknown, so it is simply never
	// sweep-tracked.
	args = append(args, "--label", dockerSessionLabel+"="+Slugify(p.spec.Title))
	if p.homeID != "" {
		args = append(args, "--label", dockerHomeLabel+"="+p.homeID)
	}
	// The credential-mount env pins go after run_args for the same reason the
	// account env does: Docker gives the LAST -e for a name precedence, so a
	// `-e NAME=` here blanks the name whatever set it earlier — the image's own
	// ENV, or a run_args entry the validator's classification did not reach.
	// Defined-and-empty is the same shape the account boundary installs: a
	// workspace config loader treats it as set and does not supply the repo's
	// value (#7526).
	for _, name := range p.credentialEnvPins {
		args = append(args, "-e", name+"=")
	}
	// af's and tmux's own pane-boundary names are blanked here, at the
	// container boundary, never at the exec shim: the container environment
	// is what every `docker exec` child af starts inherits — the
	// agent-server, the provisioning steps — so an image ENV AF_SESSION_GEN
	// or TMUX would otherwise carry a forged session marker or pane
	// identity into them. Defined-empty after run_args dominates the
	// image's ENV and any repo entry the same last-wins way the pins do.
	// The pane keeps the real values: the in-container tmux stamps them per
	// pane over this baseline. A defined-empty TMUX is safe because tmux
	// itself treats an empty value as unset — measured on tmux 3.4 and
	// 3.2a — so the blanked baseline cannot trip the nested-session guard
	// or fake pane identity anywhere af or the agent reads it (#7544).
	for _, name := range credentialMountPaneBoundaryNamesSorted() {
		args = append(args, "-e", name+"=")
	}
	// The account's env goes LAST, after run_args, and that ordering is the boundary
	// rather than a preference. docker.run_args is repository-controlled and checked
	// in; appended after the account it could set `-e CODEX_HOME=/another/home` and
	// silently move the agent off the account the operator named, which is the
	// wrong-identity failure this feature exists to prevent. Docker gives the LAST
	// -e for a name precedence, so putting the account there makes it dominate an
	// assembly that is otherwise additive by design.
	args = append(args, p.accountEnv...)
	if p.spec.Account.Dir != "" {
		// docker.run_args may set HOME too. It is not an identity source once the
		// agent's config root is explicit, but the non-root agent-server still needs
		// its writable runtime home, so make this execution invariant win last.
		args = append(args, "-e", "HOME="+p.sessionHome())
		// PATH chooses which program every PATH-resolved command in the
		// container runs — af's account provisioning execs (the mount-table
		// probe's mkdir/cat and the chown itself) and the agent's bare
		// executable. validateAccountDockerRunArgs refuses the repo
		// spellings, and this last-wins -e re-asserts the image's own
		// Config.Env PATH after run_args, so even a run_args PATH spelling
		// that escaped the lexical guard cannot put a repository binary in
		// front of af's steps — the #7529 R7 boundary, extended to account
		// sessions (#7563 R2). The image's binaries are the documented
		// residual trust here, the same class as its /bin/sh; the operator
		// keeps their own PATH whenever they forward it, and when the
		// image's PATH cannot be read nothing is emitted — with run_args
		// PATH refused the container inherits that same trusted value.
		if !slices.Contains(p.spec.SessionEnvPassthrough, "PATH") {
			if imagePATH := credentialMountImagePATH(p.dockerImageEnv()); imagePATH != "" {
				args = append(args, "-e", "PATH="+imagePATH)
			}
		}
	} else if len(p.credentialMounts) > 0 {
		// For credential-mount sessions, credential files are mounted at paths
		// relative to dockerContainerHome (/root). docker.run_args is repo-controlled
		// and is appended above with no later af env to dominate it, so a repo entry
		// setting HOME, XDG_DATA_HOME, or XDG_CONFIG_HOME would redirect agents
		// whose credential lookup is HOME- or XDG-relative away from the mounted
		// file. Re-asserting all three last closes the same gap the account path
		// closes with its own HOME re-assertion:
		//   - opencode reads $XDG_DATA_HOME/opencode/auth.json when XDG_DATA_HOME is
		//     set, falling back to $HOME/.local/share/opencode/auth.json.
		//   - amp reads $XDG_CONFIG_HOME/amp/settings.json when XDG_CONFIG_HOME is
		//     set, falling back to $HOME/.config/amp/settings.json.
		//   - codex and claude fall back to os.UserHomeDir() ($HOME) when their
		//     specific config-root variable is absent, so HOME is the load-bearing
		//     anchor for all four agents.
		// Docker gives the LAST -e for a name precedence, so appending these after
		// run_args makes them dominate any repo-supplied redirect.
		home := p.sessionHome()
		args = append(args,
			"-e", "HOME="+home,
			"-e", "XDG_DATA_HOME="+home+"/.local/share",
			"-e", "XDG_CONFIG_HOME="+home+"/.config",
			"-e", "XDG_CACHE_HOME="+home+"/.cache",
			"-e", "XDG_STATE_HOME="+home+"/.local/state",
		)
		// tmux runs each pane command as $default-shell -c …, and default-shell
		// comes from SHELL in the client environment. A repo-chosen SHELL (image
		// ENV, or docker.run_args, which the validator now refuses) would run a
		// program of the repo's choosing between af's pins and the agent's exec —
		// a program that can re-set every pinned name (#7529 R1). The same
		// last-wins -e pins SHELL to POSIX sh unless the operator forwards their
		// own through session_env_passthrough.
		if !slices.Contains(p.spec.SessionEnvPassthrough, "SHELL") {
			args = append(args, "-e", "SHELL=/bin/sh")
		}
		// PATH chooses which program every PATH-resolved command in the
		// container runs — each of af's own provisioning execs and the agent's
		// bare executable. A run_args `-e PATH=/workspace/.x:…` would put
		// repository binaries in front of all of them (#7529 R7). The
		// validator refuses repo run_args PATH, and this last-wins -e
		// re-asserts the image's own Config.Env PATH after run_args, so even
		// a run_args spelling that escaped the lexical guard cannot dominate.
		// The image's binaries are the documented residual trust on this
		// path — the same class as its /bin/sh — while the operator keeps
		// their own PATH whenever they forward it. When the image's PATH
		// cannot be read (image not pulled yet, or none configured) nothing
		// is emitted: with run_args PATH refused the container then inherits
		// the image's own value anyway — the same trusted source.
		if !slices.Contains(p.spec.SessionEnvPassthrough, "PATH") {
			if imagePATH := credentialMountImagePATH(p.dockerImageEnv()); imagePATH != "" {
				args = append(args, "-e", "PATH="+imagePATH)
			}
		}
		// GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM relocate git's config files —
		// a repo-chosen file can set a credential helper, proxy, or trust
		// root. Defined-empty is not neutral for a path-valued name:
		// measured on git 2.43, `git config --global` fails writing to an
		// empty path and /dev/null fails with EACCES, so af re-asserts each
		// to the location git would consult under the boundary's own roots —
		// defined (no late loader can claim it) and working (#7531).
		if !slices.Contains(p.spec.SessionEnvPassthrough, "GIT_CONFIG_GLOBAL") {
			args = append(args, "-e", "GIT_CONFIG_GLOBAL="+home+"/.gitconfig")
		}
		if !slices.Contains(p.spec.SessionEnvPassthrough, "GIT_CONFIG_SYSTEM") {
			args = append(args, "-e", "GIT_CONFIG_SYSTEM=/etc/gitconfig")
		}
		// Value re-asserts for the names an empty pin would break: late-loaded
		// endpoint names whose agent treats defined-empty as a real value
		// rather than falling back to its default (gemini reads
		// CODE_ASSIST_ENDPOINT with a nullish coalesce), and the shared
		// program-selector names where empty literally names "" as the
		// program (measured: `GIT_SSH_COMMAND=` fails every SSH transport
		// `cannot run : No such file or directory`, which would break
		// cloneWorkspace for ssh origins). Re-asserted after run_args, the
		// .env loader sees a defined name and a repo value is dominated by
		// last-wins (#7531). Operator-forwarded names keep the operator's
		// value.
		for _, entry := range credentialMountReassertEntries(p.agentName(), p.spec.SessionEnvPassthrough, os.Environ()) {
			args = append(args, "-e", entry)
		}
	}
	// The staged payload binds go LAST in argv — after run_args and every -e
	// pin — so af's files come from an af-owned read-only source nothing
	// repo-controlled can precede (#7577 item 2).
	args = append(args, p.stagingMounts()...)
	args = append(args, "--entrypoint", "sleep", p.image, "2147483647")

	out, err := p.docker(dockerProvisionStepTimeout, args...)
	if err != nil {
		// `docker run -d` can CREATE the container and print its id to stdout, then
		// still exit non-zero when a later start step fails — a run_arg naming a
		// nonexistent device/volume/network, `--gpus all` on a GPU-less host. The
		// container is then left behind in `created` state. Capture the id off the
		// combined output even on this error path and store it so the
		// provision-failure reap in Provision removes it instead of leaking it
		// (#2008): gate cleanup on the container having been CREATED, not on
		// `docker run` exiting zero.
		if id := parseCreatedContainerID(out); id != "" {
			p.containerID = id
		}
		if derr := dockerDuplicateMountError(string(out), err); derr != nil {
			return derr
		} else if rerr := dockerReadOnlyMountpointError(string(out), err); rerr != nil {
			return rerr
		}
		// The data-root capacity evidence joins at the provision() boundary,
		// shared with every other provisioning step (#5382).
		return fmt.Errorf("backend=docker: `docker run` failed for image %q: %s: %w", p.image, strings.TrimSpace(string(out)), err)
	}
	// The output is stdout and stderr interleaved: when the image is not local
	// docker writes "Unable to find image…", "Pulling from…" and layer progress
	// to stderr before printing the id (#4329). Take the id line, never the blob.
	id := parseCreatedContainerID(out)
	if id == "" {
		return fmt.Errorf("backend=docker: `docker run` exited successfully but printed no container id (output: %q)", strings.TrimSpace(string(out)))
	}
	p.containerID = id
	return nil
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

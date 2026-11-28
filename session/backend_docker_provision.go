// The container-provisioning pipeline behind dockerRuntime.Provision (#1592
// Phase 4 PR4): the one-shot dockerProvisioner struct, the step orchestrator
// (provision/provisionSteps), the blanked-credential warning that runs once the
// container exists, and the `docker run` argv assembly (runContainer) that
// creates the sandbox container itself. The provisioner's other step methods
// live in their own backend_docker_<layer>.go files — the workspace steps in
// backend_docker_workspace.go, staged-payload delivery in
// backend_docker_staging.go, the agent-server start in
// backend_docker_agentserver.go, the account boundary in
// backend_docker_account*.go, the env boundary helpers in
// backend_docker_env.go and agent_credentials*.go, the engine endpoint layer in
// backend_docker_endpoint.go, capacity evidence in backend_docker_capacity.go,
// and teardown in backend_docker_reap.go. backend_docker.go keeps the
// container-boundary constants and exec seams those layers share, the
// dockerRuntime front door, and the dockerBackend the pipeline returns.
package session

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/sachiniyer/agent-factory/log"
)

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
	// defined-empty as set. SSL_CERT_FILE/SSL_CERT_DIR keep that shape only
	// in the container env `docker run` dominates — af's provisioning execs
	// unset them and the exec shim resolves them to a real store (#7889).
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
	if err := p.prepareAfGitConfig(); err != nil {
		return ProvisionResult{}, dockerStepErr(err, dockerOpSessionExec)
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
	forwardedNames := p.containerEnvironmentNames()
	for _, name := range forwardedNames {
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
	// value (#7526). SSL_CERT_FILE/SSL_CERT_DIR keep that shape only here —
	// `docker run` cannot unset an image ENV (#7889).
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
	// itself treats empty as unset — measured on tmux 3.4 and 3.2a — so the
	// blanked baseline cannot trip the nested-session guard or fake pane
	// identity anywhere af or the agent reads it (#7544).
	for _, name := range credentialMountPaneBoundaryNamesSorted() {
		args = append(args, "-e", name+"=")
	}
	// The account's env goes LAST, after run_args, and that ordering is the
	// boundary rather than a preference: a repo `-e CODEX_HOME=/another/home`
	// appended later would silently move the agent off the account the
	// operator named — the wrong-identity failure this feature exists to
	// prevent. Docker's last-wins -e precedence makes the account dominate.
	args = append(args, p.accountEnv...)
	if p.spec.Account.Dir != "" {
		// docker.run_args may set HOME too. It is not an identity source once the
		// agent's config root is explicit, but the non-root agent-server still needs
		// its writable runtime home, so make this execution invariant win last.
		args = append(args, "-e", "HOME="+p.sessionHome())
		// tmux runs each pane command as $default-shell -c …, and
		// default-shell comes from SHELL in the client environment. A
		// repo-chosen SHELL (image ENV, or docker.run_args, which the
		// validator now refuses) would run a program of the repo's
		// choosing between af's pins and the agent's exec — a program
		// that can re-set every pinned name (#7529 R1, extended to
		// account sessions). The same last-wins -e pins SHELL to POSIX
		// sh unless the operator forwards their own through
		// session_env_passthrough. The forwarder only emits a name that
		// resolves in the daemon environment, so claiming SHELL in
		// session_env_passthrough without a host value would suppress
		// this pin and let the image's ENV SHELL survive — re-assert
		// unless SHELL is actually in the forwarded set.
		if !slices.Contains(forwardedNames, "SHELL") {
			args = append(args, "-e", "SHELL=/bin/sh")
		}
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
		args = append(args, p.afGitConfigMarkerArgs(forwardedNames)...)
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
		// own through session_env_passthrough. The forwarder only emits a name
		// that resolves in the daemon environment, so claiming SHELL in
		// session_env_passthrough without a host value would suppress this pin
		// and let the image's ENV SHELL survive — re-assert unless SHELL is
		// actually in the forwarded set (the #7779 rule, mirrored here: #7794).
		if !slices.Contains(forwardedNames, "SHELL") {
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
		// root. Defined-empty is not neutral for a path-valued name: `git
		// config --global` fails writing to an empty path and /dev/null fails
		// with EACCES (measured on git 2.43), so af re-asserts each to the
		// location git would consult under the boundary's own roots (#7531).
		if !slices.Contains(p.spec.SessionEnvPassthrough, "GIT_CONFIG_GLOBAL") {
			args = append(args, "-e", "GIT_CONFIG_GLOBAL="+home+"/.gitconfig")
		}
		if !slices.Contains(p.spec.SessionEnvPassthrough, "GIT_CONFIG_SYSTEM") {
			args = append(args, "-e", "GIT_CONFIG_SYSTEM=/etc/gitconfig")
		}
		// The GIT_CONFIG_* -e's above are the AGENT's own roots, as on master.
		args = append(args, p.afGitConfigMarkerArgs(forwardedNames)...)
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

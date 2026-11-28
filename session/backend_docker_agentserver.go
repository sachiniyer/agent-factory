package session

// The in-container `af agent-server`: af copies the host af binary into the
// container and launches it detached, then polls the stdout banner file for
// the listen address and auth token the daemon dials back on. Split from
// backend_docker.go to keep that file under the production length limit
// (#1145); nothing here is reachable without a running container from
// runContainer.

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

// startAgentServer launches `af agent-server` headless in the container,
// detached, with its stdout banner + stderr redirected to files. It binds
// :<port> (all interfaces) so the published host port forwards to it. The
// workspace title matches the daemon-side session title so the daemon's remote
// client dials /v1/sessions/{title}/stream. readBanner then polls the banner file.
func (p *dockerProvisioner) startAgentServer() error {
	filteredInner, err := p.agentServerCommand()
	if err != nil {
		// A local construction failure, not a docker round-trip — keep the
		// callsite's session-exec default from lending it the fs verdict.
		return dockerStepErr(err, dockerOpLocal)
	}
	filteredInner += fmt.Sprintf(" >%s 2>%s", shellQuote(dockerBannerPath), shellQuote(dockerLogPath))
	// Record the server's in-container PID so readBanner can distinguish a slow
	// start from a dead one (#5571 W2.1). The trailing `wait` keeps this exec's
	// sh alive as the server's parent, so an exited server is reaped immediately
	// — a zombie would keep answering kill -0 and read alive forever.
	filteredInner = fmt.Sprintf("{ %s & echo $! >%s; wait; }", filteredInner, dockerPidPath)

	// -d: detach. The agent-server keeps running in the container after this exec
	// client returns; we read its banner from the file below.
	args := []string{"exec", "-d"}
	args = append(args, p.sessionExecOptions()...)
	args = append(args, p.containerID, dockerContainerShell, "-c", filteredInner)
	out, err := p.docker(dockerShortStepTimeout, args...)
	if err != nil {
		return fmt.Errorf("backend=docker: starting af agent-server in the container failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func (p *dockerProvisioner) agentServerCommand() (string, error) {
	args := []string{"agent-server", "--listen", ":" + dockerAgentPort,
		"--repo", dockerWorkspaceDir, "--title", p.spec.Title}
	if strings.TrimSpace(p.program) != "" {
		args = append(args, "--program", p.program, "--program-resolved")
	}
	// agentServerEnvNames are the names the in-container env filter admits: the
	// operator's passthrough, the credential-mount pins, and the re-asserted
	// names. A pin reaches the agent DEFINED — the container env holds it as
	// `NAME=` from runContainer, and an agent-side workspace config loader
	// treats a defined name as set and does not apply the repo's value
	// (#7526). A pin cannot carry a value instead: every pinned name is a
	// classified name the run_args validator refuses and af emitted empty, so
	// the only value the filter can pass is the empty one. The path-valued
	// trust names are the exception that proves the hop matters: the filter
	// passes their `NAME=` through, and the exec shim's last-hop re-assert
	// turns it into a real platform bundle path rather than the empty value
	// a TLS stack would load as zero roots (#7889). The re-asserted names
	// ride the same extras channel: the container env holds their af-emitted
	// values, and carrying the name as an extra means the value is admitted
	// at both inner hops on af's explicit list rather than depending on the
	// agent allowlist — which a GrantAgentForCommand disagreement at the exec
	// shim would revoke (#7531).
	// The passthrough portion admits the operator's claims — every name
	// listed, host-set or not: a claimed-but-hostless name emits no -e and
	// the image's own ENV value is what the claim then admits, which is
	// the documented passthrough contract for the AGENT's env. For af's own
	// git the rule is stricter (#7891 N2): the same extras reach
	// gitCommandEnvironment's filter, so afGitScopedEnvironment drops every
	// trust-family name the AF_GIT_FORWARDED marker does not list — a
	// claimed-but-hostless name never reaches af's credentialed git. The
	// pins and re-asserts ride the same channel as before.
	agentServerEnvNames := append(
		append([]string(nil), p.spec.SessionEnvPassthrough...),
		p.credentialEnvPins...)
	if len(p.credentialMounts) > 0 {
		agentServerEnvNames = append(agentServerEnvNames,
			credentialMountReassertNames(p.agentName(), p.spec.SessionEnvPassthrough, os.Environ())...)
	}
	// afGitMarkerNames are admitted to the agent-server's own environment
	// through the WRAP's extras only — never --session-env — so the markers
	// af's git reads cannot reach the agent's panes (#7891 N5). Gated on
	// the credential-carrying paths like the -e emission: on a plain
	// session af emits nothing and admitting the names would pass an
	// image's own AF_GIT_* values through to the agent-server env.
	afGitMarkerNames := func() []string {
		forwarded := p.forwardedEnvNames()
		if (len(p.credentialMounts) == 0 && p.spec.Account.Dir == "") ||
			!afGitConfigActive(p.afGitConfigClaims(forwarded)) {
			return nil
		}
		// AF_GIT_FORWARDED is admitted only when the -e actually carries a
		// claim list: with nothing forwarded af emits no -e, and admitting
		// the name would let an image's own AF_GIT_FORWARDED smuggle trust
		// names into af's git as forged "claims".
		names := []string{"AF_GIT_CONFIG"}
		if len(forwarded) > 0 {
			names = append(names, "AF_GIT_FORWARDED")
		}
		return names
	}()
	if p.spec.Account.Dir != "" {
		// The keep-if-set names must be NAMED for the inner pane's filter or the
		// decided value never reaches the agent: a forwarded value arrives via
		// passthrough, but an unforwarded name's defined-empty pin — installed by
		// ScopeAgentServerAccount into the server env — is dropped by
		// FilterForCommand unless it is an admitted name (#7555's R6). They ride
		// --session-env, never --session-env-empty: a re-asserted empty pin at
		// the last hop would blank the forwarded value they exist to keep.
		agentServerEnvNames = append(agentServerEnvNames,
			sessionenv.AccountLoaderFilledNames(p.spec.Account.Agent)...)
	}
	for _, name := range agentServerEnvNames {
		args = append(args, "--session-env", name)
	}
	// The pins are also carried as --session-env-empty so the pane's exec shim
	// re-asserts them defined at the last boundary before the agent execs —
	// after tmux's default-shell and anything else that could re-set a name
	// between docker run's env and the exec (#7529 R1). Most pins re-assert
	// defined-and-empty; the path-valued trust names resolve to a real
	// platform bundle there instead — SSL_CERT_FILE's readers load the
	// empty value as a path, and SSL_CERT_DIR is defended beside it (#7889).
	// The flag list is also the agent-server's own neutralize gate: it
	// re-resolves exactly these names at startup and nothing else, so a
	// session emitting no pins leaves its whole env as-is (#7895).
	// --no-tmux-config closes the same hole on the
	// file side: the image's /etc/tmux.conf or a ~/.tmux.conf could pick
	// default-shell itself, or write session environment with
	// set-environment -g.
	for _, name := range p.credentialEnvPins {
		args = append(args, "--session-env-empty", name)
	}
	if len(p.credentialMounts) > 0 || p.spec.Account.Dir != "" {
		args = append(args, "--no-tmux-config")
	}
	if branch := strings.TrimSpace(p.spec.Branch); branch != "" {
		args = append(args, "--branch", branch)
	}
	if len(p.spec.AgentPolicy) > 0 {
		args = append(args, "--policy-file", dockerPolicyPath)
	}
	// This is an execution binding, not a path proof. The copied af enters a
	// dedicated mode that derives the allowlist from these exact args and execs
	// its current binary into agent-server; no argv spelling authorizes recursion.
	// An account-bound container's agent-server drops every competing credential
	// the image or docker.run_args put in its environment (#5863).
	var filteredInner string
	var err error
	if _, ok := sessionenv.SupportsAccounts(p.spec.Account.Agent); ok && p.spec.Account.Dir != "" {
		// The wrap admits the operator's raw claims like the shared path
		// above, plus the wrap-only af-git markers — gated the same way so
		// a plain or operator-claimed session cannot smuggle image
		// AF_GIT_* values (#7891 R1, N5).
		accountExtras := append([]string(nil), p.spec.SessionEnvPassthrough...)
		accountExtras = append(accountExtras, afGitMarkerNames...)
		filteredInner, err = sessionenv.WrapAgentServerAccountCommand(dockerAfBinaryPath, p.spec.Account.Agent, accountExtras, args)
	} else {
		// wrapExtras carry the agent-server's own admissions: the marker
		// names stay out of the --session-env args above so they never
		// reach the panes (#7891 N5).
		wrapExtras := append(slices.Clone(agentServerEnvNames), afGitMarkerNames...)
		filteredInner, err = sessionenv.WrapAgentServerCommand(dockerAfBinaryPath, wrapExtras, args)
	}
	if err != nil {
		return "", fmt.Errorf("backend=docker: preparing filtered agent-server environment failed: %w", err)
	}
	return filteredInner, nil
}

// readBanner polls the in-container banner file until the agent-server has bound
// its listener and printed its {addr,token} JSON line, or times out.
// On timeout it pulls the agent-server's stderr log into the error so a failure
// to start (bad image, missing tmux, port clash) is self-diagnosing.
//
// Both reads run as the account owner via sessionExecOptions(), the same user
// context startAgentServer used to write the files. A BYO image may set a USER
// directive whose uid differs from the account owner; a banner file created
// with a restrictive umask (mode 0600) is then unreadable by that default user.
// Reading as the writer keeps the diagnostic and the writer in the same security
// context and avoids a misleading "did not report a startup banner" timeout
// whose real cause is a permission denied on the banner file.
func (p *dockerProvisioner) readBanner() (dockerBanner, error) {
	deadline := time.Now().Add(dockerBannerPollTimeout)
	// One exec per poll: cat the banner, then report the server's liveness so a
	// process that died before printing its banner fails fast rather than
	// outwaiting the timeout (#5571 W2.1). A missing pidfile is still
	// "starting" — startAgentServer writes it in the same instant as the spawn,
	// and the first poll can legitimately outrun it.
	pollScript := fmt.Sprintf(
		"cat %s 2>/dev/null; if [ -f %s ]; then kill -0 \"$(cat %s)\" 2>/dev/null && echo %s || echo %s; else echo %s; fi",
		dockerBannerPath, dockerPidPath, dockerPidPath,
		remoteProcAlive, remoteProcDead, remoteProcStarting)
	for {
		args := []string{"exec"}
		args = append(args, p.sessionExecOptions()...)
		args = append(args, p.containerID, dockerContainerShell, "-c", pollScript)
		out, err := p.docker(dockerShortStepTimeout, args...)
		body, liveness := splitRemoteLiveness(string(out))
		if err == nil {
			var b dockerBanner
			if jErr := json.Unmarshal([]byte(strings.TrimSpace(body)), &b); jErr == nil && b.Addr != "" && b.Token != "" {
				return b, nil
			}
		}
		if liveness == remoteProcDead {
			logArgs := []string{"exec"}
			logArgs = append(logArgs, p.sessionExecOptions()...)
			logArgs = append(logArgs, p.containerID, dockerContainerCat, dockerLogPath)
			logOut, _ := p.docker(dockerShortStepTimeout, logArgs...)
			return dockerBanner{}, fmt.Errorf("backend=docker: af agent-server exited before reporting a startup banner; container log:\n%s",
				strings.TrimSpace(string(logOut)))
		}
		if time.Now().After(deadline) {
			logArgs := []string{"exec"}
			logArgs = append(logArgs, p.sessionExecOptions()...)
			logArgs = append(logArgs, p.containerID, dockerContainerCat, dockerLogPath)
			logOut, _ := p.docker(dockerShortStepTimeout, logArgs...)
			bannerErr := fmt.Errorf("backend=docker: af agent-server did not report a startup banner within %s; container log:\n%s",
				dockerBannerPollTimeout, strings.TrimSpace(string(logOut)))
			// A banner that never appeared is always writer-side evidence:
			// the reads may keep failing (daemon exec state is a data-root
			// write), or the file exists empty because the startup shell's
			// redirect created it while the agent-server's own write died
			// ENOSPC — a successful `cat` only proves the file is
			// readable, never that the writer could populate it (#5382
			// review). Classify with the session writer's privilege so the
			// Bavail window is seen either way.
			return dockerBanner{}, dockerStepErr(bannerErr, dockerOpSessionExec)
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

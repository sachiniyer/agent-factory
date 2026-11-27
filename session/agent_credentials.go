package session

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// agentCredentialFiles maps a supported agent to the on-disk credential file(s)
// it reads to authenticate, relative to the daemon user's home directory. When
// the operator grant docker_mount_agent_credentials is set, #2194 bind-mounts
// ONLY the file(s) for the session's OWN resolved agent (agentCredentialMounts
// is called with p.agentName()) read-only into the container — never another
// agent's credential. A codex session must not receive the live Claude token.
//
// Files, not directories, deliberately: (1) an agent writes its own
// state/history/token-refresh into its config dir at runtime, so a read-only
// mount of the whole dir would break it — mounting just the credential file
// leaves the surrounding dir (the container's own writable layer) intact; and
// (2) some of those dirs reach gigabytes of history/db (~/.codex,
// ~/.local/share/opencode), which a whole-dir mount would re-expose. A single
// credential file is the minimum credential surface.
//
// The container runs with HOME=/root (dockerContainerHome), so each host file
// `~/rel` mounts to `/root/rel`, where the container's agent — a clean env, so
// default config locations — looks for it. Several candidates per agent cover
// filename differences across CLI versions (e.g. gemini's OAuth file); af mounts
// whichever exist.
//
// Deliberate exclusions:
//   - aider: authenticates purely via API-key env vars (name them in
//     session_env_passthrough); no credential file to mount.
//   - claude's ~/.claude.json is NOT here: it is not a credential (that is
//     ~/.claude/.credentials.json) — it is the config/privacy blob (mcpServers
//     with third-party tokens, project paths + prompt history, account/machine
//     ids), and it is rewritten on every claude start, so a :ro single-file bind
//     is both useless for auth and breakage-prone.
var agentCredentialFiles = map[string][]string{
	tmux.ProgramClaude: {".claude/.credentials.json"},
	tmux.ProgramCodex:  {".codex/auth.json"},
	tmux.ProgramGemini: {
		".gemini/oauth_creds.json",
		".gemini/gemini-credentials.json",
		".gemini/google_accounts.json",
	},
	// amp stores its API key in a file secret store, $XDG_DATA_HOME/amp/secrets.json
	// (#4305). ~/.config/amp/settings.json is its settings file and carries no
	// credential: amp reports "No API key found" with only settings.json present.
	// With amp's native keyring storage enabled there is no key file at all, and
	// AMP_API_KEY (via session_env_passthrough) is the only way in.
	tmux.ProgramAmp:      {".local/share/amp/secrets.json"},
	tmux.ProgramOpencode: {".local/share/opencode/auth.json"},
	// devin is a cloud agent; config.json holds its settings but the session
	// image ships no devin CLI, so this is a no-op until an image adds devin.
	tmux.ProgramDevin: {".config/devin/config.json"},
}

// The two volume modes an agent-credential bind mount can carry. Both are
// read-only: the container authenticates with the credential, it never rewrites
// it. They differ only in the SHARED SELinux relabel, which is applied on a host
// where the relabel is called for and omitted everywhere else (bindMountRelabel
// decides, for these mounts and the account mount alike).
//
// The relabel is not cosmetic. Without it, on an enforcing host — the
// Fedora/RHEL/CentOS default — the mount is not a broken mount but a DENIED
// read: the host file keeps its user_home_t label, container policy refuses the
// open, and because this whole path is deliberately fail-open (an
// unauthenticated session beats aborting provisioning) the session starts
// UNAUTHENTICATED with nothing logged as wrong (#3451).
//
// z (shared), never Z (private), for a reason specific to what is being mounted:
// this is ONE host file that every concurrent session running that agent mounts.
// Z assigns an SVirt category pair unique to a single container, so the second
// session to start would relabel the credential out from under the first. z is
// the same label dockerAccountMount picks, for the same sharing reason.
const (
	dockerCredentialMountMode          = "ro"
	dockerCredentialMountModeRelabeled = "ro,z"
)

// selinuxRelabelForHost reports whether the kernel af is running on calls for the
// shared relabel. It is the local half of the decision; bindMountRelabel is what
// callers use, because a bind source is labeled on the ENGINE host.
//
// The rule is ONE-SIDED on purpose: af skips the relabel only where it can
// POSITIVELY establish that SELinux cannot deny the read. Applying z where
// SELinux is not enforcing is a verified no-op — Docker ignores the flag, and a
// `-v …:ro,z` mount reads back identically to `:ro` on a host with no SELinux at
// all — while omitting it on an enforcing host reinstates #3451 exactly: a
// silently unauthenticated session. The two directions are not symmetric, so
// every uncertain case relabels.
//
//   - enforcing      → relabel.
//   - permissive     → no relabel. SELinux logs the denial rather than acting on
//     it, so the read succeeds unlabeled and the operator's file is left
//     untouched. And the value is trustworthy wherever it is readable: selinuxfs
//     is a kernel interface, not a namespaced one, so a container that has it
//     mounted still reports its HOST's mode.
//   - unreadable     → relabel.
//   - not observable → ask the kernel. An absent enforce file means "no SELinux"
//     on a bare host, but inside a CONTAINER it usually means only that selinuxfs
//     was not mounted, while the kernel underneath may be enforcing — which is
//     precisely the af-in-a-container-with-the-host-socket case. /proc/filesystems
//     is kernel-global, so it settles it: if the kernel registers selinuxfs at
//     all, af cannot prove SELinux is off and relabels.
//
// Residual, and deliberately not chased: a unix socket forwarded to a DIFFERENT
// kernel (socat, or a VM whose engine af reaches through a local path) is
// indistinguishable from a local daemon by any local means. Docker Desktop is the
// common instance and its VM is not SELinux-enforcing, so the gap is theoretical;
// closing it properly would require asking the engine, which is a container
// execution on the remote host to read one file.
func selinuxRelabelForHost() bool {
	// SELinux is a LINUX LSM. Off linux there is nothing to relabel and no /proc
	// to consult, and without this guard the unobserved branch below would read
	// the expected absence of /proc/filesystems as "cannot prove" and relabel —
	// which on macOS made dockerAccountMount REFUSE a colon-bearing
	// AGENT_FACTORY_HOME that used to mount fine via Docker's --mount form
	// (#3589 review). A non-local engine is still handled before this is
	// reached, so a mac driving a remote enforcing daemon keeps its relabel.
	if runtime.GOOS != "linux" {
		return false
	}
	mode, err := hostSELinuxMode()
	if err != nil {
		log.WarningLog.Printf("backend=docker: cannot establish the host SELinux state (%v); applying the SELinux relabel anyway, which is a no-op where SELinux is not enforcing", err)
		return true
	}
	switch mode {
	case selinuxEnforcing:
		return true
	case selinuxPermissive:
		return false
	}
	if kernelRegistersSELinuxfs() {
		log.InfoLog.Printf("backend=docker: no SELinux enforce file is visible here, but this kernel registers selinuxfs — af may be containerized while the engine host enforces; applying the SELinux relabel")
		return true
	}
	return false
}

// bindMountRelabel decides the SELinux relabel for every bind mount af installs
// itself — the agent-credential mounts and the account mount alike, which is why
// it lives above both rather than inside either (#3589).
//
// It relabels unless it can PROVE the relabel is unnecessary, and the engine
// check is the load-bearing half of that. Docker resolves and labels a bind
// source on the DAEMON host, not the CLI host, so with DOCKER_HOST /
// DOCKER_CONTEXT pointing elsewhere — or af itself running in a container against
// a mounted engine socket — selinuxRelabelForHost() measures the wrong machine. A
// non-SELinux client in front of an enforcing engine would then emit plain :ro
// and the engine would deny the read, which is #3451 with an extra hop. Unless
// the endpoint is proven on this host, af keeps the relabel — and it uses the
// stricter dockerEndpointOnThisHost proof rather than the loopback-connectivity
// classification, because a tcp:// loopback endpoint can be a forwarded remote
// daemon whose labels are applied on a different host.
//
// Costs nothing where it is unnecessary, by construction: every branch that
// cannot prove "no relabel needed" resolves to z, and z is inert wherever SELinux
// is not enforcing.
func (p *dockerProvisioner) bindMountRelabel() bool {
	endpoint, _, err := p.dockerEngineEndpoint()
	if err != nil {
		log.WarningLog.Printf("backend=docker: cannot establish whether the Docker engine is local (%v); applying the SELinux relabel anyway, which is a no-op where SELinux is not enforcing", err)
		return true
	}
	if !dockerEndpointOnThisHost(endpoint) {
		log.InfoLog.Printf("backend=docker: Docker endpoint %q is not proven on this host, so this host's SELinux mode does not describe where bind mounts are labeled; applying the SELinux relabel", endpoint)
		return true
	}
	return selinuxRelabelForHost()
}

// agentCredentialMounts returns the `-v host:container:<mode>` docker run
// arguments that bind-mount the credential file(s) for the single agent `agent`
// that exist under homeDir. relabel selects the volume mode (see
// bindMountRelabel); exists is injected for testability (os.Stat in
// production). The container target is dockerContainerHome + "/" + rel —
// matching where the container's agent, running with a clean env, reads it. Host
// paths are absolute (homeDir is), so the config never has to carry a per-box
// path.
func agentCredentialMounts(agent, homeDir string, relabel bool, exists func(string) bool) []string {
	if homeDir == "" {
		return nil
	}
	mode := dockerCredentialMountMode
	if relabel {
		mode = dockerCredentialMountModeRelabeled
	}
	var args []string
	for _, rel := range agentCredentialFiles[agent] {
		host := filepath.Join(homeDir, rel)
		if !exists(host) {
			continue
		}
		// rel uses '/' and the container is linux, so the target is well-formed.
		target := dockerContainerHome + "/" + rel
		args = append(args, "-v", host+":"+target+":"+mode)
	}
	return args
}

// resolveAgentCredentialMounts resolves agentCredentialMounts for the session's
// resolved agent against the daemon user's real home, stat-ing each candidate.
// It returns nil (mounting nothing) if the home cannot be resolved — an
// unauthenticated session is a better failure than aborting provisioning — and
// logs what it found so an operator can tell "mounted the codex credential" from
// "found none" when a session starts unauthenticated.
func resolveAgentCredentialMounts(agent string, relabel bool) []string {
	home, err := os.UserHomeDir()
	if err != nil {
		log.WarningLog.Printf("backend=docker: docker.mount_agent_credentials: cannot resolve the home dir; mounting no credential for %q: %v", agent, err)
		return nil
	}
	mounts := agentCredentialMounts(agent, home, relabel, func(p string) bool {
		_, statErr := os.Stat(p)
		return statErr == nil
	})
	if len(mounts) == 0 {
		log.WarningLog.Printf("backend=docker: docker.mount_agent_credentials is set, but no credential file for agent %q was found under %s; the session may start unauthenticated", agent, home)
		return nil
	}
	// Each mount is two args ("-v", "<spec>"), so the file count is len/2.
	log.InfoLog.Printf("backend=docker: docker.mount_agent_credentials: mounting %d credential file(s) for agent %q read-only into the container", len(mounts)/2, agent)
	return mounts
}

// validateCredentialDockerRunArgs refuses repo-controlled docker.run_args that
// could redirect the agent's credential lookup away from the read-only file af
// has mounted, or substitute that credential outright with a repo-supplied one.
// docker.run_args is repository-controlled and checked in (config/manifest.go
// marks the `docker` table sourceRepoOnly) and appended AFTER the credential
// mount with no later env af emits to dominate it — so a repo entry setting an
// identity-bearing environment variable for the agent flows unopposed into the
// container, survives the agent-server's env filter because those names are
// BUILT-IN per-agent names (internal/sessionenv/sessionenv.go), and reaches the
// agent process, which then uses a credential the repo supplied instead of af's
// mounted file. The mounted credential is never read; the session authenticates
// as the attacker while af's provisioning log still reports the mount.
//
// Two shapes of identity-bearing env name have to be refused on this path:
//
//   - For the agents whose config-root variable relocates credential lookup —
//     codex (CODEX_HOME), claude (CLAUDE_CONFIG_DIR), gemini (GEMINI_CLI_HOME) —
//     the variable points the CLI at a different credential file. This is the
//     redirect #4454 wrote this guard against.
//   - For the agents whose config-root variable was MEASURED not to relocate
//     — amp and opencode (internal/sessionenv/account.go records the reason
//     each is out) — #4454 made the guard a no-op on the belief that "no
//     environment entry can defeat their mount". That belief was false. Both
//     agents have credential-bearing per-agent env names that survive every
//     af env filter and reach the agent process: opencode's
//     OPENCODE_CONFIG_CONTENT is a built-in per-agent name that opencode
//     merges over the persisted auth.json, and the inline config overrides
//     both the provider endpoint (baseURL) and the API key (apiKey) the CLI
//     uses — measured to send the session's traffic to a fake provider
//     endpoint authenticated with an attacker key while the mounted file was
//     ignored; OPENCODE_CONFIG / OPENCODE_CONFIG_DIR relocate config lookup to
//     a repo-chosen path; and the provider-key names (OPENAI_API_KEY,
//     ANTHROPIC_API_KEY, …) are ambient API keys that outrank a stored
//     credential, the same reason codex refuses OPENAI_API_KEY. amp's
//     AMP_API_KEY is the documented credential route native to amp
//     (docs/backends.md names it in `session_env_passthrough`), so a
//     repo-supplied value beats the mounted secrets.json the same way. The
//     credential-bearing names for these agents live in the identity table of
//     the mount classification; credentialMountDeniedNames unions it with the
//     account-scope names and the shared classes, so the credential-mount
//     guard covers BOTH shapes.
//
// The same lexical guard the account path has had since #3400, applied to the
// credential-mount path (#2194): refuse -e/--env/--env-file setting any of the
// agent's identity names. The account path's validator
// (validateAccountDockerRunArgs) defends the SAME env redirect on its path; the
// credential path predates that hardening and was never retrofitted, leaving the
// gap this closes.
//
// The mount surface mostly needs no guard here because a repo cannot win it:
// Docker refuses a second -v at af's single-file credential mount's
// destination, and a parent-directory mount does not shadow af's single-file
// child (runc mounts parent-then-child, so the child always wins). #7867 R3
// added one bounded check on top: mounts are checked against the name-
// resolution files and TLS trust-store paths, because those route the mounted
// credential's TRAFFIC rather than the file itself — the same reason the
// resolver options (--add-host, --dns*, --link, --net=container:) and
// --volumes-from are refused. A mount anywhere else still passes through, for
// the reason above.
//
// The refused set is credentialMountDeniedNames plus the loader prefix
// family: the roster-derived union of the identity, config-root, routing,
// reasserted, and loader classes for the session's agent
// (session/agent_credentials_env.go), and every BASH_FUNC_<name>%% name —
// the unbounded exported-function family bash imports — refused through
// credentialMountDeniedReason's prefix rule rather than by map membership.
// Every mountable agent has a non-empty set — the shared credential and
// routing names alone guarantee it — so this guard is never a no-op on the
// roster the way #4454's account-only table was for amp and opencode.
func validateCredentialDockerRunArgs(args []string, agent string, passthrough []string) error {
	// Every name the operator claims is checked first: af emits the forwarded
	// names as bare -e entries ahead of run_args, and Docker keeps the last -e
	// for a name, so a repo -e naming ANY of them — whatever class
	// credentialMountDeniedReason would give it — is an override of the
	// operator's forwarded value (#7867 R2). Running this first keeps the
	// refusal honest: for a name the operator already forwards, "have the
	// operator forward it" would be a remedy they already took.
	forwarded := dockerForwardedPassthroughNames(passthrough)
	checkEnv := func(value string) error {
		name, _, _ := strings.Cut(value, "=")
		if _, claimed := forwarded[name]; claimed {
			return fmt.Errorf(
				"backend=docker: docker.run_args cannot set %s for the credential-mount session's %s agent because the operator forwards that name through session_env_passthrough — a repo -e lands after af's forwarded -e entries and Docker keeps the last one for a name, so it would override the operator's forwarded value; drop the entry",
				name, agent)
		}
		if reason, refused := credentialMountDeniedReason(agent, name); refused {
			return fmt.Errorf(
				"backend=docker: docker.run_args cannot set %s for the credential-mount session's %s agent because %s — drop the entry, or have the operator forward %s through the global session_env_passthrough",
				name, agent, reason, name)
		}
		return nil
	}
	// checkMount reads a mount option's value against the resolution and
	// trust-store paths only — the credential path has no account boundary
	// to protect and a mount cannot shadow af's single-file credential
	// mount (runc mounts parent-then-child), but it CAN decide where the
	// agent's endpoint hosts resolve and which certificate authorities its
	// TLS stack trusts (#7867 R3): the routing names the env check refuses
	// are only words for where these paths point.
	checkMount := func(value string) error {
		if protected := dockerRoutingProtectedPath(dockerRoutingMountTarget(value)); protected != "" {
			return fmt.Errorf(
				"backend=docker: docker.run_args cannot install a mount over %s for the credential-mount session's %s agent because %s",
				protected, agent, dockerRoutingProtectedReason(protected))
		}
		return nil
	}
	// checkAddHost and checkNetwork are the account path's resolver and
	// namespace guards applied to the same routing class: an --add-host
	// remap of an endpoint host or a --net=container: share routes the
	// mounted credential's traffic exactly as a proxy env name would.
	endpointHosts := dockerSessionEndpointHosts(agent, passthrough, os.Environ())
	checkAddHost := func(value string) error {
		for _, host := range dockerAddHostNames(value) {
			if protected := dockerHostProtected(host, endpointHosts); protected != "" {
				return fmt.Errorf(
					"backend=docker: docker.run_args cannot use --add-host to remap %s for the credential-mount session's %s agent because that host is (or sits under) one the agent's authenticated traffic resolves to — a repo-chosen address there routes the mounted credential to a server the repository controls",
					host, agent)
			}
		}
		return nil
	}
	checkNetwork := func(value string) error {
		if strings.HasPrefix(value, "container:") {
			return fmt.Errorf(
				"backend=docker: docker.run_args cannot join another container's network namespace for the credential-mount session's %s agent because the donor's resolver and outbound path become this session's — and the session's authenticated traffic becomes the donor's to observe; use a named --network instead",
				agent)
		}
		return nil
	}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "--env-file" || arg == "-env-file":
			return fmt.Errorf("backend=docker: docker.run_args cannot use %s for the credential-mount session's %s agent because af cannot prove the file contains no entry that substitutes or routes the mounted credential — write the non-credential entries inline as -e", arg, agent)
		case strings.HasPrefix(arg, "--env-file="):
			return fmt.Errorf("backend=docker: docker.run_args cannot use --env-file for the credential-mount session's %s agent because af cannot prove the file contains no entry that substitutes or routes the mounted credential — write the non-credential entries inline as -e", agent)
		case arg == "-e" || arg == "--env":
			if index+1 < len(args) {
				index++
				if err := checkEnv(args[index]); err != nil {
					return err
				}
			}
		case strings.HasPrefix(arg, "--env="):
			if err := checkEnv(strings.TrimPrefix(arg, "--env=")); err != nil {
				return err
			}
		case strings.HasPrefix(arg, "-e="):
			if err := checkEnv(strings.TrimPrefix(arg, "-e=")); err != nil {
				return err
			}
		case strings.HasPrefix(arg, "-e") && len(arg) > 2:
			if err := checkEnv(strings.TrimPrefix(arg, "-e")); err != nil {
				return err
			}
		// The mount and resolver options join the guard for the same routing
		// class (#7867 R3): a mount shadowing a resolution file or a trust
		// store, an --add-host remap of an endpoint host, a --dns* resolver,
		// a --link alias, a --net=container: share, or a --volumes-from donor
		// af cannot inspect each routes the mounted credential's traffic
		// without touching an env name at all. Non-endpoint --add-host
		// mappings and named --networks stay allowed.
		case arg == "--volumes-from" || strings.HasPrefix(arg, "--volumes-from="):
			return fmt.Errorf(
				"backend=docker: docker.run_args cannot use --volumes-from for the credential-mount session's %s agent because af cannot prove what the donor container mounts — the donor's mounts land at its own container paths, resolution files and trust stores included; name the mount explicitly with -v or --mount instead",
				agent)
		case arg == "-v" || arg == "--volume" || arg == "--mount" || arg == "--tmpfs":
			if index+1 < len(args) {
				index++
				if err := checkMount(args[index]); err != nil {
					return err
				}
			}
		case strings.HasPrefix(arg, "--volume=") || strings.HasPrefix(arg, "--mount=") || strings.HasPrefix(arg, "--tmpfs="):
			if err := checkMount(strings.SplitN(arg, "=", 2)[1]); err != nil {
				return err
			}
		case strings.HasPrefix(arg, "-v="):
			if err := checkMount(strings.TrimPrefix(arg, "-v=")); err != nil {
				return err
			}
		case strings.HasPrefix(arg, "-v") && len(arg) > 2:
			if err := checkMount(strings.TrimPrefix(arg, "-v")); err != nil {
				return err
			}
		case arg == "--add-host":
			if index+1 < len(args) {
				index++
				if err := checkAddHost(args[index]); err != nil {
					return err
				}
			}
		case strings.HasPrefix(arg, "--add-host="):
			if err := checkAddHost(strings.TrimPrefix(arg, "--add-host=")); err != nil {
				return err
			}
		case arg == "--dns" || arg == "--dns-search" || arg == "--dns-option" || arg == "--dns-opt":
			return fmt.Errorf(
				"backend=docker: docker.run_args cannot use %s for the credential-mount session's %s agent because it chooses the resolver every hostname in the container goes through — including the agent's endpoint hosts — so a repo-chosen resolver routes the mounted credential's traffic the way a proxy does",
				arg, agent)
		case strings.HasPrefix(arg, "--dns=") || strings.HasPrefix(arg, "--dns-search=") || strings.HasPrefix(arg, "--dns-option=") || strings.HasPrefix(arg, "--dns-opt="):
			name, _, _ := strings.Cut(arg, "=")
			return fmt.Errorf(
				"backend=docker: docker.run_args cannot use %s for the credential-mount session's %s agent because it chooses the resolver every hostname in the container goes through — including the agent's endpoint hosts — so a repo-chosen resolver routes the mounted credential's traffic the way a proxy does",
				name, agent)
		case arg == "--link" || strings.HasPrefix(arg, "--link="):
			return fmt.Errorf(
				"backend=docker: docker.run_args cannot use --link for the credential-mount session's %s agent because it injects a host mapping and an env alias for a repo-named container — write the mapping with --add-host to a host that is not an endpoint, or attach with a named --network",
				agent)
		case arg == "--net" || arg == "--network":
			if index+1 < len(args) {
				index++
				if err := checkNetwork(args[index]); err != nil {
					return err
				}
			}
		case strings.HasPrefix(arg, "--net=") || strings.HasPrefix(arg, "--network="):
			if err := checkNetwork(strings.SplitN(arg, "=", 2)[1]); err != nil {
				return err
			}
		case strings.HasPrefix(arg, "--") || (len(arg) == 2 && arg[0] == '-'):
			// Every remaining flag spelling pflag knows: a `--name` or
			// `--name=value` long option, or a bare two-character `-x`
			// shorthand. The guarded spellings above already claimed the env,
			// mount, and resolver options; what remains still needs the
			// consumption half of the pflag model — a bare value-taking
			// option (--label, --user, -l, -u) swallows the NEXT argv element
			// whatever its shape, so a following -e is its value and never a
			// flag af sees — the swallow #7726 measured.
			// dockerRunOptionConsumption reports the consumption so the outer
			// index skips the swallowed token, and refuses the shapes af
			// cannot model: an option not in the `docker run` table in a
			// position where it could take a value, and the `--` terminator.
			consume, err := dockerRunOptionConsumption(arg,
				"the credential-mount session's "+agent+" agent")
			if err != nil {
				return err
			}
			if consume && index+1 < len(args) {
				index++
			}
		case len(arg) > 2 && arg[0] == '-' && arg[1] != '-':
			// A combined short option (e.g. -te, -tv) whose -e sets an
			// environment variable and whose -v installs a mount exactly as
			// each does on its own. The -e can redirect credential lookup and
			// the -v can shadow a resolution file or trust store (#7867 R3),
			// so both guarded characters dispatch to their checks.
			//
			// pflag: a cluster ending in a value-taking option consumes the
			// NEXT argv element as that option's value, so Docker never
			// re-parses the swallowed token as an option of its own. Without
			// advancing, the outer walk re-reads the swallowed token — a real
			// -e after `-ie` — as a fresh env flag and checks its phantom
			// "value" (the real --env=NAME=... behind it), letting a denied
			// name through. dockerClusterConsumesNextArg reports that
			// consumption so the outer index skips the swallowed token; the
			// cluster's own guarded check above already examined the value.
			if err := dockerCheckShorthandCluster(arg, args, index, dockerGuardedShorthands,
				func(character byte, value string) error {
					if character == 'v' {
						return checkMount(value)
					}
					return checkEnv(value)
				},
				"the credential-mount session's "+agent+" agent", "the -v or -e"); err != nil {
				return err
			}
			if dockerClusterConsumesNextArg(arg) && index+1 < len(args) {
				index++
			}
		default:
			// Everything the option-shaped cases did not claim is a
			// non-option argument to pflag — and `docker run` parses
			// non-interspersed, so it would end option parsing and become
			// the image, turning af's own trailing options into the
			// container's Cmd (#7733 R1).
			return dockerRunArgsPositionalError(arg,
				"the credential-mount session's "+agent+" agent")
		}
	}
	return nil
}

// dockerAccountHome is where an account's directory is mounted inside the
// container. A fixed path, not derived from the host's, so nothing about the
// operator's filesystem layout crosses the boundary.
const dockerAccountHome = "/af-account"

// selinuxMode is what af could observe about SELinux, kept as three states
// rather than a bool because "permissive" and "af cannot see selinuxfs at all"
// are NOT the same answer and must not resolve alike (#3589 review).
type selinuxMode int

const (
	// selinuxUnobserved: no enforce file in THIS process's mount namespace. On a
	// bare host that means SELinux is absent; inside a container it usually means
	// only that selinuxfs was not mounted, while the kernel underneath may well
	// be enforcing.
	selinuxUnobserved selinuxMode = iota
	selinuxPermissive
	selinuxEnforcing
)

// selinuxEnforcePaths are the kernel files that report the SELinux mode, newest
// location first. A var, not a const slice, only so a test can point the probe
// at a fixture; production never reassigns it.
var selinuxEnforcePaths = []string{"/sys/fs/selinux/enforce", "/selinux/enforce"}

// procFilesystemsPath lists the filesystems the RUNNING KERNEL registers. Also a
// var only for tests.
var procFilesystemsPath = "/proc/filesystems"

func hostSELinuxMode() (selinuxMode, error) {
	if runtime.GOOS != "linux" {
		return selinuxUnobserved, nil
	}
	for _, enforcePath := range selinuxEnforcePaths {
		value, err := os.ReadFile(enforcePath)
		if err == nil {
			switch strings.TrimSpace(string(value)) {
			case "1":
				return selinuxEnforcing, nil
			case "0":
				return selinuxPermissive, nil
			default:
				return selinuxUnobserved, fmt.Errorf("unexpected value in %s", enforcePath)
			}
		}
		if !os.IsNotExist(err) {
			return selinuxUnobserved, fmt.Errorf("read %s: %w", enforcePath, err)
		}
	}
	return selinuxUnobserved, nil
}

// kernelRegistersSELinuxfs reports whether the running kernel has SELinux
// compiled in, read from /proc/filesystems.
//
// This is the signal that SURVIVES A CONTAINER, and it is the reason an absent
// enforce file is no longer read as "this machine has no SELinux". selinuxfs is
// normally not mounted inside a container, so af running in one against the
// host's Docker socket sees no enforce file at all — while /proc/filesystems is
// a KERNEL-global list rather than a namespaced one, so it still describes the
// machine Docker will label the bind source on.
//
// Unreadable resolves to true, on the same fail-toward-relabel rule as the rest.
func kernelRegistersSELinuxfs() bool {
	data, err := os.ReadFile(procFilesystemsPath)
	if err != nil {
		return true
	}
	for _, line := range strings.Split(string(data), "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && fields[len(fields)-1] == "selinuxfs" {
			return true
		}
	}
	return false
}

// dockerAccountMount builds the account bind mount. relabel comes from
// bindMountRelabel, the SAME decision the credential mounts use (#3589) — the two
// mounts are installed by af into the same container on the same engine, so a
// host that needs the relabel needs it for both.
func dockerAccountMount(accountName, source string, relabel bool) ([]string, error) {
	if !strings.Contains(source, ":") {
		mount := source + ":" + dockerAccountHome
		if relabel {
			// z is the SHARED label: the same account may be used by more than one
			// live session, and the private Z would relabel it out from under the
			// others.
			mount += ":z"
		}
		return []string{"-v", mount}, nil
	}
	if relabel {
		return nil, fmt.Errorf(
			"account %q path %q contains a colon, so Docker needs its --mount form, which cannot carry the SELinux relabel this host requires; move AGENT_FACTORY_HOME to a path without ':'",
			accountName, source)
	}
	// --volume uses ':' as a field delimiter. --mount keeps ':' ordinary, but its
	// own comma delimiter cannot be escaped.
	if strings.Contains(source, ",") {
		return nil, fmt.Errorf("account %q path %q contains a comma, which Docker --mount cannot encode safely", accountName, source)
	}
	return []string{"--mount", "type=bind,src=" + source + ",dst=" + dockerAccountHome}, nil
}

// accountMountSource is the host directory an account is bind-mounted FROM.
//
// ABSOLUTE, always. Docker reads a relative bind source by its own rules — it is
// not resolved against af's working directory — so a relative AGENT_FACTORY_HOME
// (a supported configuration) would bind something that is not the account, or
// create a volume named after the path.
//
// One definition, shared by the code that BUILDS the mount and by the runtime
// verification that later proves that mount is the one the kernel installed
// (#3598). Two spellings of "where the account lives" is exactly how a check and
// the thing it checks drift into disagreeing.
func accountMountSource(account sessionenv.Account) (string, error) {
	source, err := filepath.Abs(account.Dir)
	if err != nil {
		return "", fmt.Errorf("cannot resolve an absolute path for account %q (%s): %w", account.Name, account.Dir, err)
	}
	return source, nil
}

// accountMountAndEnv returns the bind mount that places an account's agent HOME
// inside the container, and the `-e VAR=value` that points the agent at it
// (#3082).
//
// It REPLACES the ambient credential mounts rather than joining them, and that is
// the exclusivity property the feature exists for: a container built for an
// account must carry nothing resolved from the daemon user's home, or the session
// would hold two identities and the agent would pick one by path precedence.
//
// The mount is read-WRITE, unlike the ambient credential mounts. An account is the
// agent's whole home — auth, history, settings, discovered skills — not a
// credential file, so an agent that cannot write to it cannot record a session.
// That is a real consequence of the account model rather than a looser posture:
// the operator asked this session to BE that account.
//
// Returns nothing for an agent that does not support accounts; the create path
// refuses that case earlier, so this is defence rather than a branch anyone takes.
func accountMountAndEnv(account sessionenv.Account, relabel bool) (mount []string, env []string, err error) {
	if account.Dir == "" {
		return nil, nil, nil
	}
	configVar, ok := sessionenv.SupportsAccounts(account.Agent)
	if !ok {
		return nil, nil, nil
	}
	// accountMountSource resolves it absolutely; its failure must REFUSE here
	// rather than return an empty mount. Returning nothing was the shape of this
	// function's first version and it is the exact failure this feature exists to
	// prevent: the container would start with no account and no error, running on
	// whatever identity it could find while the session reported the one the
	// operator named. "af cannot say where the account is" has to reach the create
	// as an error.
	source, err := accountMountSource(account)
	if err != nil {
		return nil, nil, err
	}
	mount, err = dockerAccountMount(account.Name, source, relabel)
	if err != nil {
		return nil, nil, err
	}
	// Blank every alternate identity source, including values baked into the
	// image, then install the selected credential root last. The host environment
	// is separately validated with ApplyAccount before this argv is assembled.
	blank := make(map[string]struct{})
	for _, name := range sessionenv.AccountIdentityNames(account.Agent) {
		if name != configVar {
			blank[name] = struct{}{}
		}
	}
	for _, name := range sessionenv.AgentAuthSelectors(account.Agent) {
		blank[name] = struct{}{}
	}
	names := make([]string, 0, len(blank))
	for name := range blank {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		env = append(env, "-e", name+"=")
	}
	env = append(env, "-e", configVar+"="+dockerAccountHome)
	return mount, env, nil
}

// refuseAccountAgentDrift refuses when the RESOLVED program runs a different
// agent than the account was scoped to (#3082, #3108).
//
// Shared by the create path and the reprovision path deliberately. Both have to
// answer the same question — "has the program changed out from under this
// account?" — and two call sites deciding it independently is how they drift
// apart; #3044 is the precedent for what that costs.
//
// It compares the RESOLVED command's agent, never the configured program name.
// program_overrides can map the `codex` key to another agent's command, so the
// name says codex while the container runs opencode — and a session that reports
// one identity while spending another is the failure this whole feature exists to
// prevent, arriving through config rather than through a backend.
func refuseAccountAgentDrift(accountName, scopedAgent, resolvedProgram string) error {
	resolvedAgent := sessionenv.AgentForCommand(resolvedProgram)
	if resolvedAgent == scopedAgent {
		return nil
	}
	return fmt.Errorf(
		"account %q belongs to the %s agent, but the resolved program runs %s; refusing a session that would report one identity and use another",
		accountName, scopedAgent, resolvedAgent)
}

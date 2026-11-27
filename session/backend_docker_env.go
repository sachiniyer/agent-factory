// The docker provisioner's invocation half: how it names its agent, filters
// the client environment, pins the engine selector, and shells `docker` —
// the seam every provision-time call (info/run/exec/cp/port) passes through.
// Creation lives in backend_docker.go; destruction in backend_docker_reap.go.
package session

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// docker runs `docker <args...>` with a timeout and returns its combined
// output, under the provision's pinned DOCKER_CONTEXT when one was resolved —
// absent on paths that never provisioned (reap, teardown), which re-resolve
// the current context as they always did.
func (p *dockerProvisioner) docker(timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return dockerExec(ctx, p.dockerEnvironment(), args...)
}

func (p *dockerProvisioner) agentName() string {
	// The grant predicate, not the identity classifier: the credential mounts
	// and run_args validation this feeds install operator material, so the
	// agent name must be one af can attribute to the operator. The remote
	// variant is the honest one here — the executable resolves on the image's
	// filesystem, which the host cannot stat; the container's clone lives at
	// dockerWorkspaceDir, so an absolute program path inside it resolves to
	// repository content and earns no grant (#5265).
	agent := sessionenv.GrantAgentForRemoteCommand(p.program, dockerWorkspaceDir)
	if agent == "" && strings.TrimSpace(p.program) == "" {
		return tmux.ProgramClaude
	}
	return agent
}

func (p *dockerProvisioner) dockerEnvironment() []string {
	env := sessionenv.DockerCLIEnvironment(os.Environ(), p.agentName(), p.spec.SessionEnvPassthrough)
	// The provision resolved the operator's selector once (Codex on #7616):
	// when neither DOCKER_HOST nor DOCKER_CONTEXT named one, the env carries
	// none, and every call would re-resolve the CURRENT context — a
	// `docker context use` landing between the engine-ID read and `docker
	// run` would create on an engine the marker never recorded. Materializing
	// the resolved context name pins every later call to the engine the
	// provision chose. A restored reaper never sets it, so the engine-verify
	// on the reap path still reads the operator's live selector.
	if p.dockerContextPin != "" {
		env = append(env, "DOCKER_CONTEXT="+p.dockerContextPin)
	}
	// The bare `-e NAME` emitted for a normalized proxy twin resolves here:
	// synthesize NAME=<forwarded spelling's host value>. The twin is not in the
	// client's allowlist unless the operator forwarded it, so the synthesized
	// entry is the only one that name can resolve to. Same credential-mount
	// scope as containerEnvironmentNames (#7529 R8).
	if len(p.credentialMounts) == 0 {
		return env
	}
	_, synth := credentialMountGroupForwarding(p.spec.SessionEnvPassthrough, os.Environ())
	return append(env, synth...)
}

// execSh runs a `sh -c <script>` inside the container as root (0:0). It is the
// provisioning step that operates on root-owned container state — `docker cp`
// lands the `af` binary owned by root, and `chmod +x` on it must run as root
// regardless of the image's default USER. A non-root default USER (a common
// hardening practice on BYO images) cannot chmod a root-owned file, so gating
// the root override on account sessions left the non-account chmod failing with
// "Operation not permitted" on any such image. Running as 0:0 unconditionally
// is a no-op for a root-default image (its default user IS root) and is the
// only thing that works for a non-root one.
func (p *dockerProvisioner) execSh(timeout time.Duration, script string) ([]byte, error) {
	args := []string{"exec", "--user", "0:0", p.containerID, dockerContainerShell, "-c", p.trustPathUnpinnedScript(script)}
	return p.docker(timeout, args...)
}

// forwardedEnvNames returns the passthrough names that carry a VALUE into
// the container: the operator's claims that resolve in the daemon
// environment (DockerForwardNames emits a bare `-e NAME` only for those)
// plus the proxy case-twins af synthesizes from a forwarded sibling. Every
// boundary exemption keys on this set, never on the raw claim — a
// passthrough name the daemon env lacks emits nothing, and exempting the
// claim would let the image's own ENV value through instead (#7891 R6).
func (p *dockerProvisioner) forwardedEnvNames() []string {
	source := os.Environ()
	names := sessionenv.DockerForwardNames(source, p.agentName(), p.spec.SessionEnvPassthrough)
	if len(p.credentialMounts) > 0 {
		twins, _ := credentialMountGroupForwarding(p.spec.SessionEnvPassthrough, source)
		names = append(names, twins...)
	}
	return names
}

// afGitConfigFile is the global gitconfig af creates fresh inside the
// container for its own credentialed git — the file af's lockdown points
// GIT_CONFIG_GLOBAL at so no image- or repo-authored config file reaches
// the clone, the credential config, the restore fetch, or the
// agent-server's own fetch/push (#7891 R1). It lives under the session
// home, which the session user can write.
func (p *dockerProvisioner) afGitConfigFile() string {
	return p.sessionHome() + "/.af-gitconfig"
}

// afGitConfigLockdown reports the af-owned gitconfig path af's own git
// reads, and whether the lockdown applies at all: an operator who forwards
// ANY git-config-route name (GIT_CONFIG_GLOBAL, GIT_CONFIG_SYSTEM,
// GIT_CONFIG_NOSYSTEM) has claimed the whole route — af then takes no
// part of it, and the ambient environment is the operator's claim
// verbatim. Claim means forwarded-with-value here, the same rule as every
// other boundary exemption (#7891 R6).
func (p *dockerProvisioner) afGitConfigLockdown(forwarded []string) (string, bool) {
	for _, name := range []string{"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM"} {
		if slices.Contains(forwarded, name) {
			return "", false
		}
	}
	return p.afGitConfigFile(), true
}

// afGitConfigMarkerArgs returns the `-e AF_GIT_CONFIG=<af file>` pair that
// carries the lockdown into the container env: af's OWN git inside the
// container (the provisioning clone/config/fetch — whose preamble applies
// the lockdown inline — and the agent-server's fetch, push and worktree
// ops via gitCommandEnvironment, #7891 R1) reads only config files af
// created. Emitted only when the operator forwarded no git-config-route
// name with a host value — a forward of GIT_CONFIG_GLOBAL,
// GIT_CONFIG_SYSTEM or GIT_CONFIG_NOSYSTEM is an all-or-nothing claim on
// the route af then leaves alone.
func (p *dockerProvisioner) afGitConfigMarkerArgs(forwarded []string) []string {
	if file, ok := p.afGitConfigLockdown(forwarded); ok {
		return []string{"-e", "AF_GIT_CONFIG=" + file}
	}
	return nil
}

// agentGitConfigFile resolves the global gitconfig the AGENT's own git
// reads on this session — the file trustWorkspaceDirectories appends
// safe.directory to so the pane's git keeps working in the bind-mounted
// workspace exactly as on master (#7891 R4). On the credential-mount path
// af re-asserts GIT_CONFIG_GLOBAL to the session home's .gitconfig (unless
// forwarded — but a forward means the lockdown is off and this helper is
// never consulted). On the account path the ambient name survives: a repo
// run_args -e is last-wins over the image ENV, matching where master's
// `git config --global` write landed.
func (p *dockerProvisioner) agentGitConfigFile() string {
	def := p.sessionHome() + "/.gitconfig"
	if p.spec.Account.Dir == "" {
		return def
	}
	if value := dockerRunArgsEnvValue(p.runArgs, "GIT_CONFIG_GLOBAL"); value != "" {
		return value
	}
	for _, entry := range p.dockerImageEnv() {
		if value, ok := strings.CutPrefix(entry, "GIT_CONFIG_GLOBAL="); ok && value != "" {
			return value
		}
	}
	return def
}

// dockerRunArgsEnvValue returns the LAST value a docker.run_args env
// spelling assigns name, or "" — mirroring how docker resolves repeated
// -e entries (last wins). A bare `-e NAME` resolves against the daemon
// environment, which is what the container actually receives.
func dockerRunArgsEnvValue(args []string, name string) string {
	value := ""
	for i, arg := range args {
		var entry string
		switch {
		case arg == "-e" || arg == "--env":
			if i+1 < len(args) {
				entry = args[i+1]
			}
		case strings.HasPrefix(arg, "--env="):
			entry = strings.TrimPrefix(arg, "--env=")
		case strings.HasPrefix(arg, "-e"):
			entry = strings.TrimPrefix(arg, "-e")
		}
		if n, v, ok := strings.Cut(entry, "="); ok && n == name {
			value = v
		} else if entry == name {
			if v := os.Getenv(name); v != "" {
				value = v
			}
		}
	}
	return value
}

// cloneTemplateSuppression reports whether af's clone argv suppresses
// image templates (`git clone --template=`, #7891 R2): only where the
// clone's environment is untrusted — a credential-carrying session — AND
// the operator has not claimed the template route by forwarding
// GIT_TEMPLATE_DIR with a host value. Plain docker sessions and the SSH
// clone keep master's argv byte-for-byte: their git config is the
// operator's own, and a forwarded GIT_TEMPLATE_DIR is a claim the flag
// would silently override (Codex on #7891).
func (p *dockerProvisioner) cloneTemplateSuppression() bool {
	if len(p.credentialMounts) == 0 && p.spec.Account.Dir == "" {
		return false
	}
	return !slices.Contains(p.forwardedEnvNames(), "GIT_TEMPLATE_DIR")
}

// trustPathUnpinnedScript prefixes a provisioning script on a
// credential-carrying session (credential-mount or account) with the env
// fixups af's credentialed children need (#7889, #7891). Provisioning
// execs inherit the RAW container env — no FilterForCommand hop — so this
// is the one boundary where the unsets are load-bearing. Everything else
// af spawns (pane shims, the agent-server wrap, its children) is filtered:
// a name that is neither allowlisted nor forwarded never survives, which
// is why no other hop needs to strip these names.
//
// Four groups, each skipped for names carrying a forwarded VALUE
// (forwardedEnvNames — never the bare claim, #7891 R6):
//   - SSL_CERT_FILE/SSL_CERT_DIR: the pinned trust names a defined-empty
//     value empties the trust store for; unset falls them back to the
//     compiled platform store, sibling-guarded where the empty slot
//     contributes nothing beside a forwarded sibling (#7889).
//   - The git trust names (sessionenv.TrustStripNames): UNSET, never
//     resolved — `GIT_SSL_NO_VERIFY=` itself disables verification, an
//     env CAINFO outranks a config-file CA the operator forwarded, and a
//     resolved bundle widens a dir-only forward (#7891 R3).
//   - GIT_CONFIG_COUNT/GIT_CONFIG_PARAMETERS: an image ENV count animates
//     whatever indexed pairs the image ships, spelling http.sslVerify=false
//     for af's credentialed git.
//   - The git-config lockdown: af's own git reads only files af created —
//     GIT_CONFIG_NOSYSTEM=1 plus GIT_CONFIG_GLOBAL pointed at the fresh
//     af-owned file, with GIT_CONFIG_SYSTEM and XDG_CONFIG_HOME dropped so
//     neither redirect survives (#7891 R1). Skipped whole when the
//     operator forwarded any config-route name — an all-or-nothing claim.
//
// Plain sessions keep image TLS config verbatim: with no credential
// carried the image's own CA setup is the operator's choice.
func (p *dockerProvisioner) trustPathUnpinnedScript(script string) string {
	if len(p.credentialMounts) == 0 && p.spec.Account.Dir == "" {
		return script
	}
	forwarded := p.forwardedEnvNames()
	var clauses []string
	for _, name := range sessionenv.TrustPathPinNames() {
		if slices.Contains(forwarded, name) {
			continue
		}
		if sibling, paired := sessionenv.TrustPathSibling(name); paired {
			clauses = append(clauses, fmt.Sprintf(`[ -n "$%s" ] || unset %s`, sibling, name))
			continue
		}
		clauses = append(clauses, "unset "+name)
	}
	for _, name := range sessionenv.TrustStripNames() {
		if slices.Contains(forwarded, name) {
			continue
		}
		clauses = append(clauses, "unset "+name)
	}
	for _, name := range []string{"GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS"} {
		if slices.Contains(forwarded, name) {
			continue
		}
		clauses = append(clauses, "unset "+name)
	}
	if afGitConfig, ok := p.afGitConfigLockdown(forwarded); ok {
		clauses = append(clauses, "unset GIT_CONFIG_SYSTEM XDG_CONFIG_HOME")
		clauses = append(clauses, "export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL="+shellQuote(afGitConfig))
	}
	if len(clauses) == 0 {
		return script
	}
	return strings.Join(clauses, "; ") + "; " + script
}

func (p *dockerProvisioner) shortID() string {
	if len(p.containerID) > 12 {
		return p.containerID[:12]
	}
	return p.containerID
}

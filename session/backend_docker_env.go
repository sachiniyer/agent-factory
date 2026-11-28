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
		// XDG_CONFIG_HOME is never deliverable on this path: af
		// re-asserts it to the session home after run_args, so the
		// operator's value never reaches the container — and a claim
		// here would redirect GIT_CONFIG_GLOBAL at the re-asserted
		// (image-controlled) home's git/config instead (#7891 F15).
		names = slices.DeleteFunc(names, func(n string) bool { return n == "XDG_CONFIG_HOME" })
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

// afGitConfigRouteNames are the env names that relocate git's config
// reads: the global file, the system file, the system kill-switch, and the
// XDG base that finds the global file (#7891 R1). A name the operator
// forwarded WITH a host value exempts ONLY its own slot — forwarding
// GIT_CONFIG_NOSYSTEM no longer reopens the image's global file, and
// forwarding GIT_CONFIG_GLOBAL no longer reopens the system file (#7891
// N1, the re-review's per-slot rule).
var afGitConfigRouteNames = []string{
	"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM", "XDG_CONFIG_HOME",
}

// afGitConfigClaims maps each git-config route name to whether the
// operator claimed its slot by forwarding it with a host value — the same
// forwardedEnvNames set every other boundary exemption keys on (#7891 R6).
func (p *dockerProvisioner) afGitConfigClaims(forwarded []string) map[string]bool {
	claimed := make(map[string]bool, len(afGitConfigRouteNames))
	for _, name := range afGitConfigRouteNames {
		claimed[name] = slices.Contains(forwarded, name)
	}
	return claimed
}

// afGitConfigActive reports whether the lockdown has any slot left to own:
// only an operator claiming EVERY route name lifts it whole — the ambient
// environment is then their claim verbatim.
func afGitConfigActive(claimed map[string]bool) bool {
	for _, name := range afGitConfigRouteNames {
		if !claimed[name] {
			return true
		}
	}
	return false
}

// afGitConfigMarkerArgs returns the `-e` pairs carrying the lockdown into
// the container env: AF_GIT_CONFIG names the af-owned global file and
// AF_GIT_FORWARDED names the forwarded-with-value set the agent-server's
// own git must admit through (the host-valued claims — a claimed-but-
// hostless passthrough name would otherwise deliver the image's value to
// af's credentialed git, #7891 N2). Emitted whenever any route slot is
// unclaimed; the agent-server resolves the per-slot exemptions itself
// from AF_GIT_FORWARDED (#7891 N1). The claim list carries BARE env
// names — containerEnvironmentNames can hold `-e NAME=` blank entries
// (af-generated empties for hostless pair claims), and a NAME= in the
// marker would never match its env name, stripping the pair while its
// GIT_CONFIG_COUNT stayed live (Codex on #7891).
func (p *dockerProvisioner) afGitConfigMarkerArgs(forwarded []string) []string {
	if !afGitConfigActive(p.afGitConfigClaims(forwarded)) {
		return nil
	}
	args := []string{"-e", "AF_GIT_CONFIG=" + p.afGitConfigFile()}
	if len(forwarded) > 0 {
		// An empty AF_GIT_FORWARDED= would be the defined-empty pin shape
		// the classification forbids — and the agent-server reads an absent
		// marker as the empty set anyway (the strictest rule), so the name
		// is emitted only when it carries a claim. Entries arrive as BARE
		// names: containerEnvironmentNames can carry `-e NAME=` blank
		// spellings (af-generated empty values for hostless pair claims),
		// and a NAME= entry in the marker would never match the env name
		// — the pair would be stripped while its count stayed live (Codex
		// on #7891). An af-blanked entry IS the claim to admit: its env
		// value is empty, never an image value.
		names := make([]string, 0, len(forwarded))
		seen := make(map[string]bool, len(forwarded))
		for _, entry := range forwarded {
			name, _, _ := strings.Cut(entry, "=")
			if name != "" && !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
		args = append(args, "-e", "AF_GIT_FORWARDED="+strings.Join(names, ","))
	}
	return args
}

// agentGitConfigFile resolves the global gitconfig the AGENT's own git
// reads on this session — the file trustWorkspaceDirectories appends
// safe.directory to so the pane's git keeps working in the bind-mounted
// workspace exactly as on master (#7891 R4). On the credential-mount path
// af re-asserts GIT_CONFIG_GLOBAL to the session home's .gitconfig UNLESS
// the operator passthrough-claimed it — and under the per-slot rule the
// lockdown can stay active while exactly that claim stands, so the claim
// must resolve here too (Codex on #7891 N1): a run_args -e is last-wins
// over the bare forward's host value, then the image ENV — the same
// ambient resolution master's `git config --global` write followed. On
// the account path the ambient name survives, so the same run_args →
// image ENV chain applies without the claim check.
func (p *dockerProvisioner) agentGitConfigFile() string {
	def := p.sessionHome() + "/.gitconfig"
	if p.spec.Account.Dir == "" &&
		!slices.Contains(p.spec.SessionEnvPassthrough, "GIT_CONFIG_GLOBAL") {
		return def
	}
	if value := dockerRunArgsEnvValue(p.runArgs, "GIT_CONFIG_GLOBAL"); value != "" {
		return value
	}
	if value := os.Getenv("GIT_CONFIG_GLOBAL"); value != "" &&
		slices.Contains(p.forwardedEnvNames(), "GIT_CONFIG_GLOBAL") {
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
//
// Shares the validators' pflag walk (dockerRunArgsEnvEntries) so every
// spelling the validators accept — separate, =value, attached, -e=, and
// combined short clusters (-te NAME=v, -teNAME=v) — resolves here too, and
// the resolver cannot drift from the validators. The resolver is the sole
// decider of where agentGitConfigFile writes safe.directory, so a spelling
// the validators pass but this walk misses leaves the mark in the wrong
// gitconfig while the agent's git reads another (#7891 R4).
func dockerRunArgsEnvValue(args []string, name string) string {
	value := ""
	dockerRunArgsEnvEntries(args, func(entry string) {
		if n, v, ok := strings.Cut(entry, "="); ok && n == name {
			value = v
		} else if entry == name {
			if v := os.Getenv(name); v != "" {
				value = v
			}
		}
	})
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
//   - The git-config lockdown, PER SLOT (#7891 R1/N1): af's own git reads
//     only files af created — GIT_CONFIG_NOSYSTEM=1 plus GIT_CONFIG_GLOBAL
//     pointed at the fresh af-owned file, with GIT_CONFIG_SYSTEM and
//     XDG_CONFIG_HOME dropped. A route name the operator forwarded with a
//     host value exempts only ITS slot; every other slot stays af's. A
//     claimed GIT_CONFIG_SYSTEM additionally needs ambient NOSYSTEM
//     dropped or the pin would defeat the claimed file.
//
// The other container `sh -c` execs af issues bypass this preamble on
// purpose — each is inert by CONTENT, an af-composed probe or bookkeeping
// script that spawns no TLS-reading child (#7895):
//   - prepareNonAccountUser's `id` probe and the mkdir+chown step strings
//     prepareAccountUser/prepareNonAccountUser hand prepareWithMountGuard,
//     and reportSkippedMount's `test`-builtin writability probe
//     (backend_docker_account.go, backend_docker_account_chown.go);
//   - readMountTable/readMountTableFor's mkdir + pwd -P + mountinfo read
//     (backend_docker_account_chown.go);
//   - payloadDeliveryConfirmed's `test`/`wc -c` size check and
//     verifyWriteTargets' cd + pwd -P + mountinfo + `test -L` read
//     (backend_docker_staging.go);
//   - probeResolvedDestinations' throwaway `docker run --entrypoint
//     /bin/sh` probe container — a run, not an exec, on a container af
//     never provisions (backend_docker_staging.go);
//   - readBanner's `cat`/`kill -0` poll (backend_docker_agentserver.go).
//
// The one bypassing exec that is NOT inert is startAgentServer's detached
// launch — the child it spawns is the agent-server itself, which runs the
// startup neutralize gated on its --session-env-empty names before it
// spawns children of its own (#7889 R1, #7895).
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
	claimed := p.afGitConfigClaims(forwarded)
	if afGitConfigActive(claimed) {
		if !claimed["GIT_CONFIG_SYSTEM"] {
			clauses = append(clauses, "unset GIT_CONFIG_SYSTEM")
		}
		if !claimed["XDG_CONFIG_HOME"] {
			clauses = append(clauses, "unset XDG_CONFIG_HOME")
		}
		switch {
		case !claimed["GIT_CONFIG_NOSYSTEM"] && !claimed["GIT_CONFIG_SYSTEM"]:
			clauses = append(clauses, "export GIT_CONFIG_NOSYSTEM=1")
		case claimed["GIT_CONFIG_SYSTEM"] && !claimed["GIT_CONFIG_NOSYSTEM"]:
			// A claimed system file still loses to an ambient or pinned
			// NOSYSTEM — drop it so the operator's file is read.
			clauses = append(clauses, "unset GIT_CONFIG_NOSYSTEM")
		}
		switch {
		case claimed["GIT_CONFIG_GLOBAL"]:
			// The operator's file is af's global — keep it.
		case claimed["XDG_CONFIG_HOME"]:
			// GIT_CONFIG_GLOBAL replaces the XDG global too, so an
			// XDG-only claim would deliver nothing under af's own file —
			// point the redirect at the claimed file instead, which also
			// keeps the image's ~/.gitconfig shadowed (Codex on #7891 N1).
			clauses = append(clauses, `export GIT_CONFIG_GLOBAL="${XDG_CONFIG_HOME%/}/git/config"`)
		default:
			clauses = append(clauses, "export GIT_CONFIG_GLOBAL="+shellQuote(p.afGitConfigFile()))
		}
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

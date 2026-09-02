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
	tmux.ProgramAmp:      {".config/amp/settings.json"},
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

// selinuxRelabelForHost reports whether THIS host's SELinux mode calls for the
// shared relabel on af's docker bind mounts. It is the local half of the
// decision; bindMountRelabel is what callers use, because a bind source is
// labeled on the ENGINE host and this probe can only see the af process's own.
//
// A PROBE FAILURE RELABELS ANYWAY, and that direction is the whole reason this
// is a named function rather than an inline call. Applying z where SELinux is
// off is a verified no-op — Docker ignores the flag, and a `-v …:ro,z` mount
// reads back identically to `:ro` on a host with no SELinux at all — while
// omitting it on an enforcing host reinstates #3451 exactly: a silently
// unauthenticated session. The two failure directions are not symmetric, so an
// unreadable /sys must not resolve to "no relabel".
//
// Permissive (enforce=0) deliberately does NOT relabel: SELinux logs the denial
// there rather than acting on it, so the read succeeds unlabeled and the host
// file is left untouched. The residual case is a host switched to enforcing
// mid-session, which needs the session restarted to pick the label up; that is
// inherent to deciding a mount mode at `docker run` time.
func selinuxRelabelForHost() bool {
	enforcing, err := hostSELinuxEnforcing()
	if err != nil {
		log.WarningLog.Printf("backend=docker: cannot establish the host SELinux state (%v); applying the SELinux relabel anyway, which is a no-op where SELinux is disabled", err)
		return true
	}
	return enforcing
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
// the endpoint is proven local, af keeps the relabel.
//
// Costs nothing where it is unnecessary, by construction: every branch that
// cannot prove "no relabel needed" resolves to z, and z is inert wherever SELinux
// is not enforcing.
func (p *dockerProvisioner) bindMountRelabel() bool {
	endpoint, local, err := p.dockerEngineEndpoint()
	if err != nil {
		log.WarningLog.Printf("backend=docker: cannot establish whether the Docker engine is local (%v); applying the SELinux relabel anyway, which is a no-op where SELinux is not enforcing", err)
		return true
	}
	if !local {
		log.InfoLog.Printf("backend=docker: Docker endpoint %q is not local, so this host's SELinux mode does not describe where bind mounts are labeled; applying the SELinux relabel", endpoint)
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

// dockerAccountHome is where an account's directory is mounted inside the
// container. A fixed path, not derived from the host's, so nothing about the
// operator's filesystem layout crosses the boundary.
const dockerAccountHome = "/af-account"

// selinuxEnforcePaths are the kernel files that report the SELinux mode, newest
// location first. A var, not a const slice, only so a test can point the probe
// at a fixture; production never reassigns it.
var selinuxEnforcePaths = []string{"/sys/fs/selinux/enforce", "/selinux/enforce"}

func hostSELinuxEnforcing() (bool, error) {
	if runtime.GOOS != "linux" {
		return false, nil
	}
	for _, enforcePath := range selinuxEnforcePaths {
		value, err := os.ReadFile(enforcePath)
		if err == nil {
			switch strings.TrimSpace(string(value)) {
			case "1":
				return true, nil
			case "0":
				return false, nil
			default:
				return false, fmt.Errorf("unexpected value in %s", enforcePath)
			}
		}
		if !os.IsNotExist(err) {
			return false, fmt.Errorf("read %s: %w", enforcePath, err)
		}
	}
	return false, nil
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
	// ABSOLUTE, always. Docker reads a relative bind source by its own rules — it is
	// not resolved against af's working directory — so a relative AGENT_FACTORY_HOME
	// (a supported configuration) would bind something that is not the account, or
	// create a volume named after the path. Abs failing means af cannot say where
	// the account is, which must refuse rather than mount a guess.
	// A failure here must REFUSE, not return an empty mount. Returning nothing was
	// the shape of this function's first version and it is the exact failure this
	// feature exists to prevent: the container would start with no account and no
	// error, running on whatever identity it could find while the session reported
	// the one the operator named. "af cannot say where the account is" has to reach
	// the create as an error.
	source, err := filepath.Abs(account.Dir)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot resolve an absolute path for account %q (%s): %w", account.Name, account.Dir, err)
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
		"account %q is a %s account, but the resolved program runs %s; refusing a session that would report one identity and use another",
		accountName, scopedAgent, resolvedAgent)
}

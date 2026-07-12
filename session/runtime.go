package session

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
)

// BackendKind names a session runtime family (#1592 Phase 4 PR3). It is the
// value of the in-repo `backend` config key and the `--backend` create flag,
// and the key the runtime registry maps to a Runtime constructor.
type BackendKind string

const (
	// BackendLocal is today's in-process runtime: the agent runs as a tmux
	// session in a git worktree on the daemon's own box. The default — an empty
	// `backend` selection resolves here, unchanged from before Phase 4.
	BackendLocal BackendKind = config.BackendLocal
	// BackendDocker runs the workspace + agent in a container (Phase 4 PR4).
	BackendDocker BackendKind = config.BackendDocker
	// BackendSSH runs the workspace + agent on a remote host over ssh (PR5).
	BackendSSH BackendKind = config.BackendSSH
	// BackendHook is the existing remote-hook backend (the bring-your-own
	// provisioner escape hatch). Adapted to the Runtime seam here; its
	// provision-and-expose migration is PR7.
	BackendHook BackendKind = config.BackendHook
)

// ParseBackendKind validates a raw `backend` value (from the `--backend` flag or
// the in-repo config) and returns the corresponding BackendKind. An empty value
// is the default, local. An unknown value is a misconfiguration and errors —
// mirroring RemoteHooks.Validate, this validation runs when a runtime is
// resolved at create time, not at config load.
func ParseBackendKind(s string) (BackendKind, error) {
	switch s {
	case "":
		return BackendLocal, nil
	case config.BackendLocal, config.BackendDocker, config.BackendSSH, config.BackendHook:
		return BackendKind(s), nil
	default:
		return "", fmt.Errorf("unknown backend %q (valid: %s, %s, %s, %s)",
			s, config.BackendLocal, config.BackendDocker, config.BackendSSH, config.BackendHook)
	}
}

// ProvisionSpec is the input a Runtime needs to establish a session's execution
// environment. It is deliberately small: the local/hook runtimes provision from
// the repo root alone, and the sandboxed runtimes (docker/ssh) read their own
// section from the repo's resolved config by RepoRoot. PR4/PR5 extend it with
// what a sandbox needs (the repo@branch to clone, the agent-server workspace id)
// when those runtimes start consuming it.
type ProvisionSpec struct {
	// RepoRoot is the absolute repo root the session is created against.
	RepoRoot string
}

// ProvisionResult is what a Runtime hands back: the in-process Backend that
// drives the session plus, for a runtime that runs the agent in a remote
// sandbox, the authed endpoint the daemon's remoteAgentServer (PR2) dials.
type ProvisionResult struct {
	// Backend is the in-process Backend an Instance is built with. Always set
	// on success.
	Backend Backend
	// Endpoint is the authed `af agent-server` URL + token + TLS pin a sandbox
	// runtime exposes (#1592 Phase 4). nil for an in-process runtime
	// (local/hook), whose agent-server is the local tmux facade with no network
	// hop. The docker/ssh runtimes fill this in PR4/PR5; NewInstance threads a
	// non-nil endpoint into the instance's remote agent-server client. The
	// runtime-contract test exercises the non-nil path via a fake runtime.
	Endpoint *AgentServerEndpoint
}

// Runtime is the provision-and-expose seam (#1592 Phase 4 PR3): given a session
// spec it establishes the workspace/sandbox and exposes an agent-server in it,
// returning the wiring a new Instance needs. It is the extensibility point the
// docker and ssh runtimes plug into — the registry below maps a `backend` value
// to a Runtime, and session creation resolves one from config instead of
// hard-coding local-vs-hook.
//
// The local and hook runtimes provision in-process on the daemon's box, so they
// expose an in-process endpoint (Endpoint nil) and the session keeps using the
// local AgentServer over tmux — byte-identical to before Phase 4. The docker and
// ssh runtimes (PR4/PR5) provision a real sandbox, start an `af agent-server`,
// and expose its authed wss:// URL; the session then drives it through the
// remoteAgentServer client (PR2). Both satisfy this one interface, so the create
// flow above does not branch on locality.
type Runtime interface {
	// Provision establishes the session's execution environment and returns the
	// backend (+ optional remote endpoint) the instance is built with.
	Provision(spec ProvisionSpec) (ProvisionResult, error)
}

// runtimeRegistry maps a BackendKind to the constructor for its Runtime. It is
// the single source of truth for "which runtimes exist"; ResolveRuntime looks a
// kind up here. Keeping it a map (not a switch) is what makes the set
// extensible — PR4/PR5 register real docker/ssh runtimes by swapping their
// entries, and a future out-of-tree runtime could register here too.
var runtimeRegistry = map[BackendKind]func() Runtime{
	BackendLocal:  func() Runtime { return localRuntime{} },
	BackendHook:   func() Runtime { return hookRuntime{} },
	BackendDocker: func() Runtime { return dockerRuntime{} },
	BackendSSH:    func() Runtime { return sshRuntime{} },
}

// ResolveRuntime returns the Runtime registered for kind, or an error naming the
// unknown backend. Every registered kind is constructible; the docker/ssh
// runtimes construct fine but return a not-implemented error from Provision
// until PR4/PR5.
func ResolveRuntime(kind BackendKind) (Runtime, error) {
	ctor, ok := runtimeRegistry[kind]
	if !ok {
		return nil, fmt.Errorf("unknown backend %q", kind)
	}
	return ctor(), nil
}

// localRuntime is the default in-process runtime: a git worktree + tmux agent on
// the daemon's own box. Its Provision is exactly what defaultBackendFactory
// returned for a non-remote session before Phase 4 — a bare LocalBackend, no
// endpoint — so a local session is byte-identical to today.
type localRuntime struct{}

func (localRuntime) Provision(ProvisionSpec) (ProvisionResult, error) {
	return ProvisionResult{Backend: &LocalBackend{}}, nil
}

// hookRuntime adapts the existing remote-hook backend to the Runtime seam. It
// loads the repo's validated RemoteHooks and builds a HookBackend — the same
// backend the pre-Phase-4 ForceRemote path produced — so remote-hook sessions
// keep working unchanged. Its migration to a real provision-and-expose contract
// (launch_cmd returns an agent-server URL) is PR7.
type hookRuntime struct{}

func (hookRuntime) Provision(spec ProvisionSpec) (ProvisionResult, error) {
	hook, err := loadHookBackendForPath(spec.RepoRoot)
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("remote hooks not configured for this repo: %w", err)
	}
	return ProvisionResult{Backend: hook}, nil
}

// dockerRuntime is registered so `backend = "docker"` resolves cleanly, but its
// real provisioning (run a container, clone repo@branch, start an
// `af agent-server`, expose its published-port URL) lands in PR4. Until then it
// fails create with an actionable error. It reads the resolved docker.image so a
// user who has already configured it gets it echoed back — and so the config
// field is genuinely consumed here, not dead until PR4.
type dockerRuntime struct{}

func (dockerRuntime) Provision(spec ProvisionSpec) (ProvisionResult, error) {
	image := ""
	if cfg, err := resolveRepoConfig(spec.RepoRoot); err == nil && cfg.Docker != nil {
		image = cfg.Docker.Image
	}
	return ProvisionResult{}, fmt.Errorf("the docker backend is not yet implemented (image %q); it lands in #1592 Phase 4 PR4 — use backend=local for now", image)
}

// sshRuntime is registered so `backend = "ssh"` resolves cleanly; its real
// provisioning (dial the host, clone repo@branch, start an `af agent-server`,
// tunnel its port back) lands in PR5. Until then it fails create with an
// actionable error, echoing the configured ssh.host for the same reason
// dockerRuntime echoes the image.
type sshRuntime struct{}

func (sshRuntime) Provision(spec ProvisionSpec) (ProvisionResult, error) {
	host := ""
	if cfg, err := resolveRepoConfig(spec.RepoRoot); err == nil && cfg.SSH != nil {
		host = cfg.SSH.Host
	}
	return ProvisionResult{}, fmt.Errorf("the ssh backend is not yet implemented (host %q); it lands in #1592 Phase 4 PR5 — use backend=local for now", host)
}

// resolveRepoConfig loads the resolved config for the repo containing absPath.
// Shared by the runtime resolver (to read the `backend` key) and the docker/ssh
// runtimes (to read their sections).
func resolveRepoConfig(absPath string) (*config.ResolvedConfig, error) {
	repo, err := config.RepoFromPath(absPath)
	if err != nil {
		return nil, err
	}
	return config.ResolveConfig(repo.Root)
}

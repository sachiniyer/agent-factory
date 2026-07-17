package session

import (
	"fmt"
	"slices"

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
	// BackendHook is the remote-hook backend: the bring-your-own-provisioner
	// escape hatch, migrated to the same provision-and-expose contract as
	// docker/ssh (#1592 Phase 4 PR7). launch_cmd provisions the workspace on the
	// user's infra and exposes an `af agent-server` URL.
	BackendHook BackendKind = config.BackendHook
)

// ParseBackendKind validates a raw `backend` value (from the `--backend` flag or
// the in-repo config) and returns the corresponding BackendKind. An empty value
// is the default, local. An unknown value is a misconfiguration and errors —
// mirroring RemoteHooks.Validate, this validation runs when a runtime is
// resolved at create time, not at config load.
func ParseBackendKind(s string) (BackendKind, error) {
	if s == "" {
		return BackendLocal, nil
	}
	// Validated against config.SupportedBackends, not a hand-written case list, so
	// a newly registered backend becomes parseable by appearing in the one
	// canonical enum (#1933).
	if slices.Contains(config.SupportedBackends, s) {
		return BackendKind(s), nil
	}
	return "", fmt.Errorf("unknown backend %q (valid: %s)", s, config.SupportedBackendsString())
}

// ProvisionSpec is the input a Runtime needs to establish a session's execution
// environment. The local/hook runtimes provision from the repo root alone; the
// sandboxed runtimes (docker/ssh) additionally need the session's identity
// (Title/Program/AutoYes) to start an `af agent-server` for exactly one
// workspace, and the clone source so the sandbox can pull the repo from GitHub
// (epic decision 4: GitHub is the durable workspace store). They read their own
// section (docker.*/ssh.*) from the repo's resolved config by RepoRoot.
type ProvisionSpec struct {
	// RepoRoot is the absolute repo root the session is created against.
	RepoRoot string
	// Title is the session title. A sandbox runtime uses it as the single
	// workspace's agent-server title (the /v1/sessions/{title}/stream path id the
	// daemon's remote client dials) and — inside the sandbox — as the git branch
	// seed, exactly as the local runtime does.
	Title string
	// Program is the agent program to run in the workspace (empty ⇒ the config
	// default). Passed through to the sandbox's `af agent-server --program`.
	Program string
	// AutoYes enables the workspace's AutoYes accept.
	AutoYes bool
	// CloneURL is the git remote the sandbox clones the workspace from — the
	// repo's `origin` for a docker/ssh session (GitHub for a real repo, a
	// file:// or bind-mounted path for a self-contained test). Empty for the
	// in-process local/hook runtimes, which never clone. On a fresh create the
	// sandbox clones its default branch and the in-sandbox worktree derives the
	// session branch from Title.
	CloneURL string
	// RestoreBranch, when set, makes this a RESTORE provision rather than a fresh
	// create (#1592 Phase 4 PR6): after cloning, the sandbox materializes this
	// exact branch (the one archive pushed to origin) as a LOCAL ref so the
	// in-sandbox local backend's Setup reuses it — bringing the pushed commits
	// back — instead of branching fresh off the default. Empty on a fresh create.
	// Only the sandbox runtimes (docker/ssh) honor it; the in-process runtimes
	// never clone, so it is a no-op for them.
	RestoreBranch string
}

// ProvisionResult is what a Runtime hands back: the in-process Backend that
// drives the session plus, for a runtime that runs the agent in a remote
// sandbox, the authed endpoint the daemon's remoteAgentServer (PR2) dials.
type ProvisionResult struct {
	// Backend is the in-process Backend an Instance is built with. Always set
	// on success.
	Backend Backend
	// Endpoint is the authed `af agent-server` URL + token a sandbox
	// runtime exposes (#1592 Phase 4). nil for an in-process runtime
	// (local/hook), whose agent-server is the local tmux facade with no network
	// hop. The docker/ssh runtimes fill this in (docker: PR4); NewInstance threads
	// a non-nil endpoint into the instance's remote agent-server client. The
	// runtime-contract test exercises the non-nil path via a fake runtime.
	Endpoint *AgentServerEndpoint
	// Teardown reaps the sandbox this Runtime provisioned — `docker rm -f` the
	// container (PR4), close the ssh tunnel + remove the remote dir (PR5). nil for
	// an in-process runtime (nothing off-box to reap). NewInstance stashes it on
	// the instance so the agent-server Kill path runs it AFTER tearing the remote
	// workspace down over REST, and NewInstance itself runs it if wiring the
	// remote client fails, so a provisioned sandbox never leaks. Idempotent (the
	// docker runtime guards it with a sync.Once).
	Teardown func() error
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
// and expose its authed http:// URL; the session then drives it through the
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

// SetRuntimeForTest replaces the Runtime registered for kind with ctor and
// returns a restore function. It is the exported form of the registry swap the
// in-package sandbox tests already do by hand, so tests OUTSIDE this package (the
// daemon's remote limit-resume regression, #1786) can drive the real
// re-provision path — reprovisionRemote resolves the runtime through this
// registry — against a mock sandbox instead of a real docker/ssh host. Mirrors
// the SetBackendFactoryForTest / SetDockerSelfBinaryForTest seam pattern.
func SetRuntimeForTest(kind BackendKind, ctor func() Runtime) func() {
	prev, had := runtimeRegistry[kind]
	runtimeRegistry[kind] = ctor
	return func() {
		if had {
			runtimeRegistry[kind] = prev
			return
		}
		delete(runtimeRegistry, kind)
	}
}

// ResolveRuntime returns the Runtime registered for kind, or an error naming the
// unknown backend. Every registered kind is constructible.
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

// hookRuntime provisions a session on user-provided infrastructure (#1592 Phase 4
// PR7 — the bring-your-own-provisioner escape hatch). Its Provision lives in
// backend_hook_backend.go: run the repo's launch_cmd, which clones the workspace
// and starts an `af agent-server` on the user's infra, parse the authed http://
// URL it echoes, and expose it — the SAME provision-and-expose contract as
// docker/ssh. The type is declared there. Its old terminal/attach contract is
// deleted (clean-break).

// dockerRuntime provisions a real container sandbox (#1592 Phase 4 PR4). Its
// Provision lives in backend_docker.go: run a container from the configured
// docker.image, clone the repo inside it, docker cp the `af` binary in, start an
// `af agent-server` on a published port, and expose its authed http:// URL. The
// type is declared there.

// sshRuntime provisions a real remote-machine sandbox (#1592 Phase 4 PR5). Its
// Provision lives in backend_ssh.go: dial the configured ssh.host, clone the repo
// into a per-session dir, stream the `af` binary onto the remote, start an `af
// agent-server` bound to remote loopback, and expose its authed http:// URL through
// an ssh local-forward tunnel. The type is declared there.

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

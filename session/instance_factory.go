package session

import (
	"crypto/rand"
	"fmt"
	"path/filepath"
	"time"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

// Options for creating a new instance
type InstanceOptions struct {
	// ID, when set, is the stable identity already announced for this instance.
	// The daemon uses it to keep an OpCreating projection and the completed
	// instance on one identity across slow provisioning. Empty mints a new id,
	// which remains the normal path for every direct constructor call.
	ID string
	// CreatedAt, when set, is the creation time already announced with ID. The
	// daemon supplies both together so a pending row does not jump in rail order
	// when provisioning completes. Zero uses the current time.
	CreatedAt time.Time
	// Title is the title of the instance.
	Title string
	// TaskID marks the session as spawned by a task's delivery (#1892). Empty for
	// a user-created session. It is what lets the daemon count a task's in-flight
	// sessions for the watch-task concurrency limit without guessing from titles.
	TaskID string
	// Path is the path to the workspace.
	Path string
	// Program is the program to run in the instance (e.g. "claude", "aider --model ollama_chat/gemma3:1b")
	Program string
	// ProgramResolved marks Program as the final command selected by an outer
	// runtime. It is internal to the sandbox agent-server handoff; ordinary
	// callers pass an agent enum and leave this false.
	ProgramResolved bool
	// ForceRemote forces the instance to use the remote hook backend,
	// even if the repo config would default to local. It is the pre-Phase-4
	// hook selector, equivalent to Backend == BackendHook, and takes precedence
	// over a config-declared backend (it is set by the TUI's "new remote
	// session" action, which means "hook now" regardless of config).
	ForceRemote bool
	// Backend, when set, selects the session's runtime explicitly (the
	// `--backend` create flag, #1592 Phase 4 PR3), overriding the repo's
	// `backend` config key. Empty means "resolve from config" — which defaults
	// to local, so an unset Backend keeps the local default byte-identical.
	Backend BackendKind
	// InPlace attaches the session to the repo's existing working tree at its
	// current branch (`af sessions create --here`) instead of creating a new
	// git worktree+branch. The worktree is marked external so kill/cleanup
	// never removes the user's tree or branch. Local backend only.
	InPlace bool
	// RemoteAgentServer, when set, points the instance's AgentServer() at a REMOTE
	// `af agent-server` reachable at the endpoint's authed URL (#1592 Phase 4 PR2)
	// instead of the local in-process runtime. Validated at NewInstance (a bad URL
	// or a malformed URL fails there). DARK in PR2: no runtime provisions a sandbox to
	// fill this in yet (PR3-PR5); it is exercised by the out-of-process round-trip
	// test.
	RemoteAgentServer *AgentServerEndpoint
	// SessionEnvPassthrough carries exact, operator-approved environment names
	// into an out-of-process agent-server. Ordinary local callers leave it empty
	// and use the global session_env_passthrough config key.
	SessionEnvPassthrough []string
}

// backendFactory provisions the runtime for a new Instance, returning the
// in-process Backend plus (for a sandboxed runtime) the authed remote endpoint
// and the sandbox-reap teardown. It is a package-level variable (not a
// hard-coded branch) so tests can inject a FakeBackend through
// SetBackendFactoryForTest without touching production code paths. Defaults to
// the real runtime resolution.
var backendFactory = defaultBackendFactory

// defaultBackendFactory resolves the session's runtime from the requested
// backend kind (the `--backend` flag / repo `backend` config, or ForceRemote for
// the legacy hook path) and provisions it, returning the whole ProvisionResult.
// It is the production path behind the backendFactory test seam; a test that
// replaces backendFactory injects a FakeBackend directly and never reaches here.
//
// The full ProvisionResult flows to NewInstance (#1592 Phase 4): the local
// runtime provisions in-process (nil Endpoint, nil Teardown — the local path is
// unchanged), while the off-box runtimes (docker/ssh/hook) return the
// agent-server's authed endpoint + a sandbox-reap teardown, which NewInstance
// threads into the instance's remote agent-server client and Kill path.
func defaultBackendFactory(opts InstanceOptions, absPath string) (ProvisionResult, error) {
	kind, err := resolveBackendKind(opts, absPath)
	if err != nil {
		return ProvisionResult{}, err
	}
	rt, err := ResolveRuntime(kind)
	if err != nil {
		return ProvisionResult{}, err
	}
	spec := ProvisionSpec{
		RepoRoot:              absPath,
		Title:                 opts.Title,
		Program:               opts.Program,
		SessionEnvPassthrough: append([]string(nil), opts.SessionEnvPassthrough...),
	}
	// An off-box runtime clones the workspace from the repo's origin (epic
	// decision 4: GitHub is the durable store); resolve it only for those kinds so
	// a local create never pays for an extra git subprocess. Best-effort — a repo
	// with no origin yields "", and each runtime surfaces the actionable
	// "no origin remote" error at create (the hook runtime hands the URL to
	// launch_cmd, which does the clone on the user's infra).
	if kind == BackendDocker || kind == BackendSSH || kind == BackendHook {
		spec.CloneURL = originRemoteURL(absPath)
	}
	return rt.Provision(spec)
}

// resolveBackendKind decides which runtime a new session uses, in precedence
// order: an explicit --backend flag (opts.Backend) wins; then the legacy
// ForceRemote hook selector; otherwise the repo's `backend` config key, which
// defaults to local.
//
// The config read is best-effort for the DEFAULT (no explicit selection) path:
// a path that is not a git repo, or a repo with no readable config, falls back
// to local so a local session is never blocked by config resolution here (the
// same posture as before Phase 4, where this factory read no config for a local
// session). A config that loads but declares an invalid backend value surfaces
// that error — misconfiguration should fail the create, not silently run local.
func resolveBackendKind(opts InstanceOptions, absPath string) (BackendKind, error) {
	if opts.Backend != "" {
		return ParseBackendKind(string(opts.Backend))
	}
	if opts.ForceRemote {
		return BackendHook, nil
	}
	cfg, err := resolveRepoConfig(absPath)
	if err != nil {
		return BackendLocal, nil
	}
	return ParseBackendKind(cfg.Backend)
}

// BackendKindFor reports which runtime a create with these options against
// absPath will use, WITHOUT creating anything. It is the same decision (and the
// same precedence) NewInstance makes internally.
//
// The daemon needs this before it provisions: remote hook names are a global
// namespace (the slug reaches launch_cmd/delete_cmd verbatim), so the hook-name
// checks must run for every create that will end up on the hook backend — not
// just the legacy ForceRemote selector. `--backend hook` and a repo's
// `backend = "hook"` config both reach BackendHook with ForceRemote false, and
// gating on ForceRemote alone let those creates skip the check entirely.
func BackendKindFor(opts InstanceOptions, absPath string) (BackendKind, error) {
	return resolveBackendKind(opts, absPath)
}

// SetBackendFactoryForTest replaces the backend factory with f and returns a
// restore function. Intended for use in tests that need to swap in a
// FakeBackend so NewInstance-driven creation flows stay on the hot path. f
// returns just the Backend — the common case for a local FakeBackend — and is
// adapted to the internal ProvisionResult factory here, so a test that only
// wants to inject a backend needs no knowledge of the endpoint/teardown seam.
func SetBackendFactoryForTest(f func(opts InstanceOptions, absPath string) (Backend, error)) func() {
	prev := backendFactory
	backendFactory = func(opts InstanceOptions, absPath string) (ProvisionResult, error) {
		b, err := f(opts, absPath)
		if err != nil {
			return ProvisionResult{}, err
		}
		return ProvisionResult{Backend: b}, nil
	}
	return func() { backendFactory = prev }
}

// newSessionID mints a random RFC-4122 v4 UUID for an instance's stable identity
// (#1195). It is a package var so tests can inject deterministic IDs. crypto/rand
// is the entropy source; on the (near-impossible) read failure it falls back to a
// timestamp-derived value so session creation never blocks on entropy — still
// unique per call in practice, and the reconcile's title+CreatedAt fallback covers
// any theoretical collision.
var newSessionID = func() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("ts-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// NewInstanceID reserves the same stable identity NewInstance would mint. The
// daemon calls it before a potentially slow backend factory so it can publish an
// authoritative OpCreating projection whose id the finished Instance inherits.
func NewInstanceID() string {
	return newSessionID()
}

func NewInstance(opts InstanceOptions) (*Instance, error) {
	t := opts.CreatedAt
	if t.IsZero() {
		t = time.Now()
	}
	id := opts.ID
	if id == "" {
		id = NewInstanceID()
	}

	// An in-place session runs in the repo's local working tree; a remote
	// session has no local worktree at all — the two are contradictory. This
	// covers both the legacy ForceRemote hook selector and an explicit
	// non-local --backend (#1592 Phase 4 PR3).
	if opts.InPlace && (opts.ForceRemote || (opts.Backend != "" && opts.Backend != BackendLocal)) {
		return nil, fmt.Errorf("remote sessions cannot run in-place in the local repo working tree")
	}

	// Convert path to absolute
	absPath, err := filepath.Abs(opts.Path)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute path: %w", err)
	}
	normalizedSessionEnv, err := sessionenv.NormalizeExtraNames(opts.SessionEnvPassthrough)
	if err != nil {
		return nil, fmt.Errorf("invalid session environment pass-through: %w", err)
	}
	opts.SessionEnvPassthrough = normalizedSessionEnv

	res, err := backendFactory(opts, absPath)
	if err != nil {
		return nil, err
	}
	backend := res.Backend

	// A sandboxed runtime (docker) provisions its workspace during backendFactory
	// and hands back the in-sandbox agent-server's authed endpoint; a caller can
	// also pass one explicitly (the PR2 out-of-process round-trip). Either way the
	// session builds its agent-server transport up front so the endpoint (URL,
	// pin) is validated here rather than on first AgentServer() use — which keeps
	// the AgentServer() factory infallible (#1592 Phase 4 PR2). nil for every local
	// session, so the default path is untouched.
	endpoint := res.Endpoint
	if endpoint == nil {
		endpoint = opts.RemoteAgentServer
	}
	var remoteClient *remoteAgentClient
	if endpoint != nil {
		remoteClient, err = newRemoteAgentClient(*endpoint, opts.Title)
		if err != nil {
			// The sandbox is already up (backendFactory provisioned it); a bad
			// endpoint here would strand it, so reap it before failing rather than
			// leaking a container/remote workspace.
			if res.Teardown != nil {
				_ = res.Teardown()
			}
			return nil, fmt.Errorf("failed to build remote agent-server client: %w", err)
		}
	}

	return &Instance{
		ID:     id,
		TaskID: opts.TaskID,
		Title:  opts.Title,
		// A task delivery's run begins here and ends when the agent goes idle
		// (#1892). Only a task-spawned session has a run to bound; a user's session
		// is never counted against a cap.
		taskRunActive:         opts.TaskID != "",
		liveness:              LiveReady,
		Path:                  absPath,
		Program:               opts.Program,
		Height:                0,
		Width:                 0,
		CreatedAt:             t,
		UpdatedAt:             t,
		preResolvedProgram:    resolvedProgramMarker(opts),
		sessionEnvPassthrough: normalizedSessionEnv,
		inPlace:               opts.InPlace,
		backend:               backend,
		remoteClient:          remoteClient,
		runtimeTeardown:       res.Teardown,
	}, nil
}

func resolvedProgramMarker(opts InstanceOptions) string {
	if opts.ProgramResolved {
		return opts.Program
	}
	return ""
}

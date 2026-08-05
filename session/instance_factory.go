package session

import (
	"crypto/rand"
	"errors"
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
	// ResumeConversation asks the first launch to come up on a provider
	// conversation a previous record held, rather than starting a new one
	// (#2616). Set only by the daemon's root-agent heal, which replaces the
	// vanished root's record instead of re-spawning it; every other Lost session
	// keeps its record and resumes through Recover. Empty for every ordinary
	// create, which is why the fresh-injection path is unchanged.
	ResumeConversation AgentConversationData
	// RestoreTabs asks the first launch to rebuild the tab roster a previous
	// record held, rather than coming up with only its agent tab (#2628). Set by
	// the same single caller as ResumeConversation — the daemon's root-agent
	// heal — because it has the same cause: replacing the record throws away
	// state the general Lost-restore path keeps, and the tab list is the largest
	// piece of it. Index 0 (the agent tab) is ignored: the launch spawns its own.
	// Empty for every ordinary create, which still comes up with just the agent
	// tab (#1100).
	RestoreTabs []TabData
	// PendingRecreateNotice carries an unacknowledged re-create notice from the
	// record this create replaces (#2629), so a second heal cannot erase a
	// warning about the first that nobody has seen yet. Set by the same single
	// caller as the two fields above. Empty for every ordinary create.
	PendingRecreateNotice RootRecreateContext
	// RemoteAgentServer, when set, points the instance's AgentServer() at a REMOTE
	// `af agent-server` reachable at the endpoint's authed URL (#1592 Phase 4 PR2)
	// instead of the local in-process runtime. Validated at NewInstance (a bad URL
	// or a malformed URL fails there). DARK in PR2: no runtime provisions a sandbox to
	// fill this in yet (PR3-PR5); it is exercised by the out-of-process round-trip
	// test.
	RemoteAgentServer *AgentServerEndpoint
	// SessionEnvPassthrough carries durable exact-name grants delegated by an
	// outer agent-server. Ordinary daemon/local callers leave it empty and read
	// the current global session_env_passthrough config on each launch.
	SessionEnvPassthrough []string
	// ProvisionSessionEnvPassthrough carries a current global-config snapshot to
	// the runtime factory for this create only. It is deliberately not retained
	// on the Instance: a removed global grant must disappear from later respawns,
	// handoffs, archive restores, and config-agent launches without a restart.
	ProvisionSessionEnvPassthrough []string
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
	provisionSessionEnv, err := sessionenv.NormalizeExtraNames(append(
		append([]string(nil), opts.SessionEnvPassthrough...),
		opts.ProvisionSessionEnvPassthrough...,
	))
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("invalid provisioning session environment pass-through: %w", err)
	}
	spec := ProvisionSpec{
		RepoRoot:              absPath,
		Title:                 opts.Title,
		Program:               opts.Program,
		SessionEnvPassthrough: provisionSessionEnv,
	}
	// An off-box runtime clones the workspace from the repo's origin (epic
	// decision 4: GitHub is the durable store); resolve it only for those kinds so
	// a local create never pays for an extra git subprocess. Best-effort — a repo
	// with no origin yields "", and each runtime surfaces the actionable
	// "no origin remote" error at create (the hook runtime hands the URL to
	// launch_cmd, which does the clone on the user's infra).
	if kind.ProvisionsOffBox() {
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

// ErrInPlaceRemoteBackend marks the refusal of an in-place create whose session
// would not run on this machine. An in-place session attaches the agent to the
// repo's OWN working tree; a docker/ssh/hook session works in a sandbox clone
// and has no local worktree at all, so the two requests cannot both be honored.
//
// A sentinel rather than a bare message because the surfaces that offer in-place
// (the CLI's --here, the daemon's create RPC, the root agent) need to tell this
// refusal apart from the provisioning-config errors it used to hide behind.
var ErrInPlaceRemoteBackend = errors.New("an in-place session cannot run on a non-local backend")

// InPlaceBackendConflict reports the in-place/remote contradiction for a create
// with these options against absPath, or nil when there is none. NewInstance
// enforces it, so an ordinary caller never needs this.
//
// It is exported for callers that MUTATE state before reaching NewInstance. The
// daemon's reserveCreate is the one that matters: for an explicit title held
// only by an archived session it renames that session — relocating its worktree
// and rewriting its durable record — and only later builds the instance. A
// refusal raised at NewInstance would therefore land after an irreversible
// rename done for a create that could never have succeeded, which is exactly the
// state reserveCreate promises never to leave behind (#2127, #2415). Asking the
// same question ahead of the mutation is what keeps that promise, and asking it
// through THIS function is what keeps the two answers from drifting.
//
// Judged on the RESOLVED kind, not on opts.Backend (#2778). An empty
// opts.Backend does not mean local — it means "resolve from the repo's `backend`
// key" — so a flag-only test waves a repo-configured docker/ssh/hook create
// straight through with InPlace still set. In a half-configured repo that
// surfaces as a provisioning-config error naming nothing about in-place; in a
// fully configured one it SUCCEEDS, and the session's record claims the user's
// working tree while its agent runs in a sandbox clone that cannot see it.
//
// Resolving here mirrors LocalPrereqsRequired (#2592) for the same reason: a
// check that reimplements the backend precedence rules drifts from them.
//
// A kind that will not RESOLVE yields nil. That is not a local create and not a
// remote one — it is an unusable `backend` value, and the runtime factory
// reports it in one place. Converting it into an in-place refusal would name the
// wrong problem.
func InPlaceBackendConflict(opts InstanceOptions, absPath string) error {
	if !opts.InPlace {
		return nil
	}
	kind, err := resolveBackendKind(opts, absPath)
	if err != nil || kind == BackendLocal {
		return nil
	}
	return inPlaceBackendConflict(kind, opts)
}

// inPlaceBackendConflict builds that refusal, naming both the resolved backend
// and WHERE it was selected. The source matters for the case #2778 is about: a
// user who passed no backend flag at all has nothing to correlate the error
// with unless it points at the repo's `backend` key.
func inPlaceBackendConflict(kind BackendKind, opts InstanceOptions) error {
	source := "this repo's `backend` config key"
	switch {
	case opts.Backend != "":
		source = "the requested backend"
	case opts.ForceRemote:
		source = "the remote-session request"
	}
	return fmt.Errorf("%w: %s selected the %s backend, which runs the agent in a sandbox clone with no access to this repo's working tree — create the session on the local backend, or drop the in-place request",
		ErrInPlaceRemoteBackend, source, kind)
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

// LocalPrereqsRequired reports whether a create with these options against
// absPath will run its agent on THIS machine — the only case where the local
// prerequisites (tmux, the agent binary on PATH) decide whether the create can
// succeed. A docker/ssh/hook create runs tmux and the agent inside the sandbox,
// so checking the client's PATH for them refuses a session that would have
// worked (#2592).
//
// It is the ONE predicate behind that gate: the CLI's `sessions create` and
// `send-prompt --create` and the TUI's naming form all ask this rather than
// each deciding for itself which backends are local. That matters because the
// selection has precedence rules (explicit --backend, then ForceRemote, then
// the repo's `backend` key) and a surface that reimplements them drifts —
// gating on the explicit flag alone silently misses the repo-config case, which
// is the shape #2592 arrived in.
//
// The answer is THREE-valued, which is why it returns an error rather than a
// bare bool. A backend value that names nothing resolvable is neither a local
// create nor a sandbox one: the question has no answer, and neither default is
// honest. Reporting it as "local" makes the user hear about missing tmux when
// their `backend` key is the problem; reporting it as "sandbox" skips a check
// that should have run. Callers surface the error.
//
// It answers from the backend KIND rather than a provisioned backend's
// Capabilities on purpose: Capabilities is per-instance, so reading it means
// having provisioned a runtime — the exact thing a pre-create gate must not do
// (#2599).
func LocalPrereqsRequired(opts InstanceOptions, absPath string) (bool, error) {
	kind, err := BackendKindFor(opts, absPath)
	if err != nil {
		return false, err
	}
	return kind == BackendLocal, nil
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

	// Convert path to absolute
	absPath, err := filepath.Abs(opts.Path)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute path: %w", err)
	}

	if err := InPlaceBackendConflict(opts, absPath); err != nil {
		return nil, err
	}
	normalizedSessionEnv, err := sessionenv.NormalizeExtraNames(opts.SessionEnvPassthrough)
	if err != nil {
		return nil, fmt.Errorf("invalid session environment pass-through: %w", err)
	}
	opts.SessionEnvPassthrough = normalizedSessionEnv
	normalizedProvisionEnv, err := sessionenv.NormalizeExtraNames(opts.ProvisionSessionEnvPassthrough)
	if err != nil {
		return nil, fmt.Errorf("invalid provisioning session environment pass-through: %w", err)
	}
	opts.ProvisionSessionEnvPassthrough = normalizedProvisionEnv

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
			// leaking a container/remote workspace. No Instance exists to retain a
			// retry handle, so preserve every cleanup failure in the returned chain
			// and call out an unknown outcome as a possible orphan.
			if res.Teardown != nil {
				if cleanupErr := res.Teardown(); cleanupErr != nil {
					if TeardownStateUnknown(cleanupErr) {
						return nil, fmt.Errorf("failed to build remote agent-server client and sandbox cleanup state is unknown; a sandbox may still be running: %w",
							errors.Join(err, cleanupErr))
					}
					return nil, fmt.Errorf("failed to build remote agent-server client and sandbox cleanup failed: %w",
						errors.Join(err, cleanupErr))
				}
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
		carriedConversation:   opts.ResumeConversation,
		carriedTabs:           append([]TabData(nil), opts.RestoreTabs...),
		carriedRecreateNotice: opts.PendingRecreateNotice,
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

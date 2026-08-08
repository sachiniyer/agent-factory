package session

import (
	"crypto/rand"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/log"
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
	// SandboxCredentials mints and revokes the per-session credential a provisioned
	// sandbox uses to call back into the daemon (#2999, #3068). An INTERFACE rather
	// than a pair of values so it runs only for off-box kinds — session cannot
	// import daemon, and the daemon must not mint (or refuse) for a local create
	// that will never use one — and rather than a bare mint function because the
	// runtime's lifetime drives BOTH halves: a replacement sandbox mints, a reaped
	// runtime revokes. Nil ⇒ no callback, which is every non-daemon caller.
	//
	// Held on the Instance too, so restore and recovery can provision a replacement
	// through the same path as the original create (see sandbox_credentials.go).
	SandboxCredentials SandboxCredentials
	// Path is the path to the workspace.
	Path string
	// Program is the program to run in the instance (e.g. "claude", "aider --model ollama_chat/gemma3:1b")
	Program string
	// Account scopes the session to a registered credential account (#3051).
	Account string
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
	// Mint through the SHARED helper, which the replacement path uses too, so the
	// two cannot diverge on which kinds get a credential or on what a refusal
	// means. Scoped to off-box kinds inside it: minting unconditionally would make
	// every local create fail whenever require_token is off, turning a sandbox-only
	// refusal into a total outage.
	// Resolve the account to its DIRECTORY here, on the host, for a kind that can
	// place it (#3082). The local path never needs this — its shim resolves the name
	// inside the target process against that process's own AF home — but a
	// provisioned machine has no registry to resolve against, so the host must
	// resolve it and the provisioner must place it.
	//
	// Resolving here rather than in the provisioner is deliberate: Selected is what
	// enforces the registry's guarantees (the account exists, and no ancestor of its
	// path is a symlink out of the registry), and those must be checked on THIS
	// machine, where the registry lives, before a path derived from them is handed
	// to a runtime that will mount it.
	if kind.CarriesAccount() && strings.TrimSpace(opts.Account) != "" {
		account, aerr := resolveAccountForProvision(absPath, opts.Program, opts.Account)
		if aerr != nil {
			return ProvisionResult{}, aerr
		}
		spec.Account = account
	}
	cred, err := mintForProvision(&spec, kind, opts.SandboxCredentials)
	if err != nil {
		return ProvisionResult{}, err
	}
	res, err := rt.Provision(spec)
	if err != nil {
		return res, err
	}
	// Revalidate now that the sandbox exists: provisioning is the long window, and
	// a listener move or an auth change inside it leaves the sandbox holding a
	// credential that was revoked while it was being written in.
	//
	// A failure here must REAP what was just provisioned. NewInstance discards the
	// ProvisionResult of a failed factory call, so nothing downstream retains
	// res.Teardown — returning the error alone leaks the sandbox with no handle
	// left to reap it (#3065 review). This is the same discipline every other
	// failure exit on a provisioned runtime already follows; a new exit does not
	// get to skip it.
	if rerr := revalidateAfterProvision(cred); rerr != nil {
		return ProvisionResult{}, discardUnusableSandbox(res, opts.SandboxCredentials, rerr)
	}
	return res, nil
}

// discardUnusableSandbox reaps a sandbox that was provisioned but must not be
// used, and revokes the credential minted for it.
//
// Both halves matter and both were missing. The runtime leaks without the reap,
// because the caller drops the ProvisionResult on error and with it the only
// cleanup handle. The credential leaks without the revoke, because it is
// registered against a session whose sandbox no longer exists — a token minted
// for a runtime that is gone, which is the state this feature exists to prevent.
//
// A teardown that reports an UNKNOWN outcome is joined into the returned error
// rather than swallowed: the sandbox may still be running, and the operator is
// the only one who can settle it.
func discardUnusableSandbox(res ProvisionResult, creds SandboxCredentials, cause error) error {
	if creds != nil {
		creds.Revoke()
	}
	if res.Teardown == nil {
		return cause
	}
	if terr := res.Teardown(); terr != nil {
		if TeardownStateUnknown(terr) {
			return fmt.Errorf("%w; and the sandbox provisioned for it could not be confirmed torn down, so it may still be running: %w", cause, terr)
		}
		log.WarningLog.Printf("discarded sandbox teardown reported a completed error: %v", terr)
	}
	return cause
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

	// An account-scoped session must run LOCALLY, for now.
	//
	// The off-box backends provision and start agent-server before the account is
	// applied, and ProvisionSpec does not carry it — so a docker, ssh, sandbox or
	// hook session would run on the sandbox's ambient credentials while the
	// instance recorded the requested account. That is the silent wrong-identity
	// outcome this feature exists to prevent, reached through a backend instead of
	// through the environment, and it is worse than not offering the combination.
	//
	// Refusing is the first slice deliberately: threading the account through
	// provisioning and the headless launch is real work, and until it is done an
	// honest error beats a session that reports one identity and spends another
	// (#3051, #3082).
	if err := refuseOffBoxAccount(opts); err != nil {
		return nil, err
	}
	if err := refuseUnsupportedAccountAgent(opts, absPath); err != nil {
		return nil, err
	}

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
		ID:           id,
		sandboxCreds: opts.SandboxCredentials,
		TaskID:       opts.TaskID,
		Title:        opts.Title,
		// A task delivery's run begins here and ends when the agent goes idle
		// (#1892). Only a task-spawned session has a run to bound; a user's session
		// is never counted against a cap.
		taskRunActive:         opts.TaskID != "",
		liveness:              LiveReady,
		Path:                  absPath,
		Program:               opts.Program,
		Account:               opts.Account,
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

// refuseOffBoxAccount rejects an account-scoped session on a backend that cannot
// carry the account. For ssh, sandbox and hook this is a DECISION, not a gap
// (#3103) — see offBoxAccountRefusal for each backend's reason.
//
// It still gates on the backend being NOT local rather than on a list of remote
// kinds, and #3082 did not weaken that: a backend added later is off-box until
// someone proves otherwise, so the default for anything new remains refusal
// rather than silent ambient credentials. What changed is that docker can now
// PROVE it, so it is exempted by name.
func refuseOffBoxAccount(opts InstanceOptions) error {
	if strings.TrimSpace(opts.Account) == "" {
		return nil
	}
	kind := opts.Backend
	if kind == "" {
		kind = BackendLocal
	}
	if kind == BackendLocal || kind.CarriesAccount() {
		return nil
	}
	return fmt.Errorf("account %q cannot be used with the %s backend: %s", opts.Account, kind, offBoxAccountRefusal(kind))
}

// offBoxAccountRefusal is WHY a given backend refuses an account, in the
// operator's words.
//
// It is per backend because the reasons are genuinely different, and a merged
// message was actively misleading (#3103). The old wording said af "cannot place
// a credential account on that machine" for all of them — which is FALSE for ssh
// and provably so: that runtime streams af's own binary to the remote, creates a
// per-session directory and starts a process there. Placement was never the
// obstacle. An operator who knows the ssh backend reads that and reasonably
// concludes af simply has not wired it up.
//
// THE ACTUAL OBSTACLE FOR SSH IS THE ROUND TRIP. An account is a writable agent
// HOME (sessionenv.Account.Dir becomes CODEX_HOME / CLAUDE_CONFIG_DIR), not a
// credential file, and the agent writes REFRESHED AUTHENTICATION into it when a
// token rotates. A copy's writes live on the remote and the teardown `rm -rf`
// destroys them, so the account would present as writable while being read-only
// in practice.
//
// And the worst case is not a stale credential, it is an INVALIDATED one. OAuth
// refresh tokens are commonly single-use: presenting one returns a replacement
// and kills the old. If that holds for a given provider, a remote refresh
// invalidates the token the operator still holds LOCALLY while the replacement is
// deleted with the session dir — so a session on a machine af does not control
// breaks the identity on the machine it does. af cannot verify rotation behaviour
// per provider, and that unverifiability is the argument: this is a risk af would
// be taking on the operator's behalf without being able to bound it.
//
// COPY-BACK IS NOT RULED OUT BY TEARDOWN AMBIGUITY, and an earlier draft of this
// comment claimed it was (#3103 review). The ordering is available: quiesce the
// agent, copy, install the replacement locally, and only THEN start the
// destructive reap — a write-back failure retains the record without tearing
// anything down, and an unknown reap outcome afterwards no longer costs the token,
// because it already arrived. So teardown uncertainty alone is not the argument.
//
// What survives that correction is narrower and is still enough:
//
//   - The remote can be LOST before write-back — an evaporated cloud instance, a
//     revoked key, a network partition that outlives the session. Then the refresh
//     is gone and the local copy is dead, which is the invalidation case again,
//     reached by a route no ordering fixes.
//   - Write-back needs the agent QUIESCED to copy a consistent home, and af has no
//     mechanism to make an agent stop writing on request.
//   - It widens the window in which live credentials sit on a machine af does not
//     control, which per-session placement exists to bound.
//
// sandbox and hook get the SAME round-trip reason rather than a location one, and
// that is a correction too: sandboxProvisioner.provision creates the session
// directory and streams af's binary into it, and the provision-hook path reuses
// that provisioner — so af controls those locations exactly as it does for ssh.
// Only hook's launch_cmd mode genuinely leaves the machine's shape to the
// operator, and the round-trip reason covers that mode as well.
// offBoxRoundTripReason is the one reason ssh, sandbox and hook refuse.
//
// It states the property af can ESTABLISH — that it cannot get the agent's
// writes back — rather than a mechanism by which they are lost, and that is the
// correction TEN review findings on this PR converged on. The permanence clause
// obeys the same rule: it says the GUARANTEE is missing, and deliberately does
// NOT say "only a mount can return those writes" — an earlier draft did, and that
// was the same over-claim one layer up, since a launch_cmd may run the
// agent-server on the daemon host or against shared durable storage. Every earlier
// version named a specific consequence ("destroyed with the session directory at
// teardown", "af has no provable location", "omit --account to use that machine's
// own credentials"), and each was false for at least one backend, because the
// consequence depends on each backend's own lifecycle and environment contract
// while the refusal does not. hook's launch_cmd owns no session directory af can
// delete; sandbox reads the operator's ssh_config; only ssh pins -F none.
//
// So: assert the thing that is true for all three and nothing further. It also
// deliberately says "the other off-box backends" nowhere — docker is off-box in
// this repo's taxonomy (backendProvisionsOffBox) and CARRIES an account, so
// "off-box" is the wrong axis to name here at all.
const offBoxRoundTripReason = "an account is a writable agent home, so the agent writes refreshed " +
	"authentication back into it — and af cannot establish that those writes come back from a machine it " +
	"does not own, so a rotated token can be lost. If your provider rotates refresh tokens, losing it also " +
	"invalidates the copy on this machine — so a feature meant to NARROW where an identity is used could " +
	"break it. af refuses this BY DESIGN and not as pending work: what is missing is the GUARANTEE that " +
	"those writes come back, and af will not present an account as writable without one."

func offBoxAccountRefusal(kind BackendKind) string {
	switch kind {
	case BackendSSH:
		// ssh shares the invariant AND may add the concrete consequence, because it is
		// the one refusing backend whose session-directory lifecycle af owns — so
		// "teardown removes it" is established here and nowhere else (#3103 review).
		return "af can put the account on that host, and deliberately does not: " + offBoxRoundTripReason +
			" On this backend af owns the remote session directory, so the teardown that removes it removes " +
			"the refresh with it. Use the docker or local backend for account-scoped sessions, or omit " +
			"--account to use the remote host's own credentials"

	case BackendSandbox:
		// The same round-trip reason as ssh, and NOT a "no provable location" one:
		// sandbox runs through sandboxProvisioner, which creates the session directory
		// itself, so af controls the location there too (#3103 review).
		//
		// And like hook, the workaround must NOT name an identity. sandbox_ssh is the
		// operator's own command and this backend deliberately preserves their
		// ssh_config — so a `SendEnv OPENAI_API_KEY` block copies the DAEMON's value
		// to the sandbox verbatim (measured in #3092), and the session would run on
		// the daemon host's identity while the operator believed otherwise. Only
		// backend=ssh is immune, because af pins -F none there.
		return offBoxRoundTripReason +
			" Use the docker or local backend for account-scoped sessions, or omit --account — though which " +
			"identity the session then runs as depends on your sandbox_ssh command and the ssh_config it " +
			"reads, which af does not override"

	case BackendHook:
		// Same reason, but the WORKAROUND must not promise an identity here.
		// runHookScriptWithResolvedEnvironment hands launch_cmd the DAEMON's filtered
		// agent-authentication environment and the script decides what reaches the
		// machine — so "omit --account to use that machine's own credentials" would be
		// false, and false in the exact direction account scoping exists to prevent:
		// the session could run as the daemon host's ambient identity while the
		// operator believes it is the remote's (#3103 review).
		return offBoxRoundTripReason +
			" Use the docker or local backend for account-scoped sessions, or omit --account — though which " +
			"identity the session then runs as is up to your hooks, since af passes the daemon's filtered " +
			"agent credentials to them"
	default:
		// A backend added later, refused by default. Naming it as unproven rather
		// than as impossible keeps this honest for something nobody has assessed.
		return "af has not established that it can carry a credential account onto that machine, and " +
			"running there would use its ambient credentials while reporting the account you asked for; " +
			"use the docker or local backend for account-scoped sessions, or omit --account"
	}
}

// refuseUnsupportedAccountAgent rejects an account on an agent whose launch af
// rewrites before the boundary sees it.
//
// claude is the case today. The local launch appends --session-id and usually
// --plugin-dir, and the boundary's command guard accepts only a bare,
// argument-free invocation — so the pane exits 127 with a generic message. A
// clear refusal at create time is strictly better than a session that starts and
// dies for a reason nothing explains.
//
// This is deliberately an ALLOWLIST of agents known to work, not a denylist of
// broken ones: an agent added later is unsupported until someone proves the
// boundary accepts its launch, which fails in the safe direction (#3051, #3083).
func refuseUnsupportedAccountAgent(opts InstanceOptions, absPath string) error {
	if strings.TrimSpace(opts.Account) == "" {
		return nil
	}
	// RESOLVE FIRST, then decide (#3083 review, #3108).
	//
	// opts.Program is a LABEL, and program_overrides can point it at another agent
	// entirely. The account name was validated in the namespace of the REQUESTED
	// agent (api/sessions.go), but the launch derives the agent from the RESOLVED
	// command — so `Program=claude` + `program_overrides.claude=codex` validates
	// "work" as a claude account and then runs codex under CODEX's "work". Two
	// namespaces, one name, and the session silently authenticates as someone the
	// user never selected. That is the exact failure this feature exists to prevent,
	// and a gate reading the label cannot see it.
	//
	// config.ResolveProgram is the resolver the launch itself uses
	// (resolveProgramForAgent), so this gate and the launch cannot disagree about
	// what the command is.
	// The config is resolved HERE rather than threaded in, because the gate must read
	// the same overrides the launch will: resolveRepoConfig is what the launch path
	// uses too. A failure to resolve leaves cfg nil, and config.ResolveProgram on a
	// nil config returns the label unchanged — which is the pre-override answer, so an
	// unreadable config cannot silently admit a cross-agent override.
	// Repo config, then GLOBAL — mirroring resolveConfigForInstance exactly, because
	// that is what the launch uses. Trying only the repo would miss a global
	// program_overrides entry that the launch then applies, which is the very
	// gate-disagrees-with-launch divergence this check exists to close.
	var cfg *config.Config
	if resolved, rerr := resolveRepoConfig(absPath); rerr == nil {
		cfg = &resolved.Config
	} else if loaded, lerr := config.LoadConfig(); lerr == nil {
		cfg = loaded
	}
	requested := sessionenv.AgentForCommand(opts.Program)
	agent := sessionenv.AgentForCommand(config.ResolveProgram(cfg, opts.Program))
	if agent != requested {
		return fmt.Errorf(
			"account %q was validated as a %s account, but this session's program_overrides resolves %s to "+
				"a %s command — the account namespaces are separate, so a same-named %s account would be "+
				"used instead of the %s one you selected. Remove the override, or create the session on the "+
				"agent whose account you mean",
			opts.Account, requested, requested, agent, agent, requested)
	}
	// An EXPLICIT list, still, and claude is on it now because af's launch rewrite
	// carries provenance the boundary verifies (#3083): the launcher declares
	// `--session-id`/`--plugin-dir`, so the rewritten command is provable rather
	// than refused. codex needs no declaration — af leaves its command unmodified.
	//
	// Deliberately NOT keyed on sessionenv.SupportsAccounts: that answers "does this
	// agent have a credential-root variable", which is a different question from "can
	// af prove its launch to the boundary". Keeping them separate is what makes an
	// agent added to the first list unsupported here until someone checks the second,
	// which fails in the safe direction (#3051, #3083).
	if agent == "codex" || agent == "claude" {
		return nil
	}
	return fmt.Errorf(
		"account %q cannot be used with %s yet: af has not established that the account boundary can "+
			"verify how it launches that agent, so the session could start on the ambient identity or exit "+
			"immediately; account scoping currently supports claude and codex",
		opts.Account, agent)
}

// resolveAccountForProvision resolves opts.Account to the registered account's
// directory on this host.
//
// The agent is derived from the session's program, matching how every other
// account surface names one: an account belongs to an agent, and "codex" and
// "claude" keep separate registries.
func resolveAccountForProvision(repoRoot, program, accountName string) (sessionenv.Account, error) {
	home, err := config.GetConfigDir()
	if err != nil {
		return sessionenv.Account{}, fmt.Errorf("cannot resolve account %q: %w", accountName, err)
	}
	requestedAgent := sessionenv.AgentForCommand(program)
	resolved, err := resolveRepoConfig(repoRoot)
	if err != nil {
		return sessionenv.Account{}, fmt.Errorf("cannot resolve account %q against the session program: %w", accountName, err)
	}
	resolvedProgram := config.ResolveProgram(&resolved.Config, program)
	resolvedAgent := sessionenv.AgentForCommand(resolvedProgram)
	if resolvedAgent != requestedAgent {
		return sessionenv.Account{}, fmt.Errorf(
			"account %q is a %s account, but this session resolves %s to a %s command; account namespaces are separate, so the Docker session would not use the identity you selected",
			accountName, requestedAgent, requestedAgent, resolvedAgent)
	}
	account, err := agentaccount.Selected(home, requestedAgent, accountName, "")
	if err != nil {
		return sessionenv.Account{}, err
	}
	if account.Dir == "" {
		// Selected returns a zero Account for an empty name, which the caller has
		// already excluded. A zero Dir here would mean an unscoped provision that
		// still reported an account, so refuse rather than proceed.
		return sessionenv.Account{}, fmt.Errorf("account %q resolved to no directory", accountName)
	}
	return account, nil
}

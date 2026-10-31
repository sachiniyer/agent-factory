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
	"github.com/sachiniyer/agent-factory/internal/hostlayout"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
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
	// BacklogItem marks the session as started for a backlog item, and opens its
	// first backlog run.
	BacklogItem int
	// CreatedBySandbox is the id of the session whose sandbox callback credential
	// asked for this create (#5571 W7.8). Set only by the daemon, from the
	// credential that admitted the request, never from a request field.
	CreatedBySandbox string
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
	// RepoIdentityPath is the owning repo's canonical identity path
	// (RepoContext.IdentityPath). The daemon supplies it at create so worktree-
	// less sandbox backends can keep projecting Worktree.RepoPath with the same
	// value the pending row announced — under a bare repository's linked-worktree
	// registration it differs from Path, which is the operational checkout.
	// Empty leaves the projection to its Path fallback.
	RepoIdentityPath string
	// Program is the program to run in the instance (e.g. "claude", "aider --model ollama_chat/gemma3:1b")
	Program string
	// Account scopes the session to a registered credential account (#3051).
	Account string
	// AccountSource says where a NON-REQUESTED Account came from, as a sentence
	// naming the config key, its file, and how to clear it (#3386). Empty when the
	// caller asked for the account.
	//
	// It exists only for the refusals below. `--account work` on a hook-backed repo
	// is answered by "omit --account", which is exactly right for someone who typed
	// it — and useless for someone whose account arrived from a project's
	// `default_accounts`, because there is no flag on their command to omit. The
	// refusal is the same refusal; only the remedy differs, so only the remedy is
	// carried.
	AccountSource string
	// ProgramResolved marks Program as the final command selected by an outer
	// runtime. It is internal to the sandbox agent-server handoff; ordinary
	// callers pass an agent enum and leave this false.
	ProgramResolved bool
	// ForceRemote forces the instance to use the remote hook backend,
	// even if the repo config would default to local. It is the pre-Phase-4
	// hook selector, equivalent to Backend == BackendHook, and takes precedence
	// over a config-declared backend. Retained for older wire clients; new
	// clients and the TUI use Backend, which takes precedence over this flag.
	ForceRemote bool
	// Backend, when set, selects the session's runtime explicitly (the
	// `--backend` create flag, #1592 Phase 4 PR3), overriding the repo's
	// `backend` config key. Empty means "resolve from config" — which defaults
	// to local, so an unset Backend keeps the local default byte-identical.
	Backend BackendKind
	// Host names the listed host (global `hosts`, #5571 W6.3) the session runs on.
	// It selects the ssh runtime: with Backend empty the kind resolves to ssh, and
	// with any other explicit Backend the create is refused. Empty means "no pick"
	// — the default_host applies only where the resolved backend is ssh.
	Host string
	// InPlace attaches the session to the repo's existing working tree at its
	// current branch (`af sessions create --here`) instead of creating a new
	// git worktree+branch. The worktree is marked external so kill/cleanup
	// never removes the user's tree or branch. Local backend only.
	InPlace bool
	// BranchPrefix is the resolved snapshot used to name a fresh local
	// worktree's branch. The daemon supplies its frozen startup value because
	// branch_prefix is EffectNextDaemonStart; nil preserves the direct-constructor
	// behavior of resolving the current on-disk value when provisioning begins.
	// A pointer distinguishes an explicitly configured empty prefix from nil.
	BranchPrefix *string
	// BranchName, when set, is the exact branch a fresh local worktree is
	// created on, instead of one derived from BranchPrefix and the title. The
	// in-sandbox agent-server sets it to the name the DAEMON chose (#5571 W2.4).
	BranchName string
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
	// `af agent-server` reachable at the endpoint's authed URL (#1592 Phase 4)
	// instead of the local in-process runtime. Validated at NewInstance (a bad URL
	// or a malformed URL fails there). The off-box runtimes (docker, sandbox, ssh,
	// hook) do not use this field — backendFactory hands their endpoint back
	// directly (res.Endpoint); this is the explicit-caller seam.
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
	// CallbackUnavailable declares that no daemon callback was delivered to the
	// process building this instance. It is set by the in-sandbox agent-server
	// (daemon/agentserver_headless.go), which learns the fact from the absence
	// of AF_DAEMON_URL/AF_DAEMON_TOKEN in its environment — its Instance has no
	// mint error to quote, so it records the generic undelivered reason. The
	// daemon-side create path instead learns the reason from the mint itself
	// and arrives through ProvisionResult.CallbackUnavailable.
	CallbackUnavailable bool
	// HostedRemotely declares this instance runs inside a sandbox the daemon
	// treats as a remote workspace (set by the in-sandbox agent-server), so its
	// guidance reflects the remote runtime's tab admission rather than the local
	// capabilities it happens to execute on (#5571 W3.3).
	HostedRemotely bool
	// AdvertisedFeatures is the /v1/health feature list the hosting
	// agent-server advertises to the daemon. A hosted instance renders its
	// guidance from what is advertised, not from a static per-backend floor
	// (#5878): the daemon admits shell and process tabs only for a sandbox that
	// advertises "tab-api".
	AdvertisedFeatures []string
}

// backendFactory provisions the already-resolved runtime for a new Instance,
// returning the in-process Backend plus (for a sandboxed runtime) the authed
// remote endpoint and the sandbox-reap teardown. It is a package-level variable
// (not a hard-coded branch) so tests can inject a FakeBackend through
// SetBackendFactoryForTest without touching production code paths. NewInstance
// passes the same resolved kind its pre-provision gates inspected, so config
// cannot produce a different answer between the guard and provisioning.
var backendFactory = defaultBackendFactoryForKind

func defaultBackendFactoryForKind(opts InstanceOptions, absPath string, kind BackendKind) (ProvisionResult, error) {
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
		Host:                  opts.Host,
		SessionEnvPassthrough: provisionSessionEnv,
		OperatorCreated:       opts.CreatedBySandbox == "",
	}
	// An off-box runtime clones the workspace from the repo's origin (epic
	// decision 4: GitHub is the durable store); resolve it only for those kinds so
	// a local create never pays for an extra git subprocess. Best-effort — a repo
	// with no origin yields "", and each runtime surfaces the actionable
	// "no origin remote" error at create (the hook runtime hands the URL to
	// launch_cmd, which does the clone on the user's infra).
	if kind.ProvisionsOffBox() {
		spec.CloneURL = originRemoteURL(absPath)
		spec.GitUserName, spec.GitUserEmail = operatorGitIdentity(absPath)
		spec.Branch = daemonBranchForTitle(opts)
		policy, perr := agentPolicyFor(absPath, opts.BranchPrefix)
		if perr != nil {
			return ProvisionResult{}, perr
		}
		spec.AgentPolicy = policy
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
	if accountCarried(kind, absPath, opts.Host) && strings.TrimSpace(opts.Account) != "" {
		account, aerr := resolveAccountForProvision(absPath, opts.Program, opts.Account)
		if aerr != nil {
			return ProvisionResult{}, aerr
		}
		spec.Account = account
	}
	cred := mintForProvision(&spec, kind, opts.SandboxCredentials)
	res, err := rt.Provision(spec)
	if err != nil {
		// A minted credential must not outlive a sandbox that never came up —
		// the same fail-closed rule the replacement path's replacementLive defer
		// enforces for the identical exit (#5571 W3.1).
		if cred.Granted() && opts.SandboxCredentials != nil {
			opts.SandboxCredentials.Revoke()
		}
		return res, err
	}
	// res.Branch is whatever the runtime reports it ran on: a hook launch_cmd is
	// never handed the daemon's name, so recording it here would display (and
	// later restore) a branch that was never used (#5971).

	// A refused or absent credential did not stop provisioning — it must stop
	// being silent instead. The reason rides the result onto the instance's
	// durable record so every surface can say "af callback unavailable: …"
	// instead of leaving the agent to discover af commands all fail (#5571 W3.1).
	// A runtime may already have set the reason — the mid-provision loss the
	// provisioner detects just before committing the env is more precise than
	// the mint's, so it wins.
	if res.CallbackUnavailable == "" {
		res.CallbackUnavailable = cred.UnavailableReason
	}
	// Revalidate now that the sandbox exists: provisioning is the long window, and
	// a listener move or an auth change inside it leaves the sandbox holding a
	// credential that was revoked while it was being written in.
	//
	// A failure here DEGRADES the same way a refused mint does — the sandbox is
	// provisioned, runnable and owned, just callback-less, so reaping it would
	// destroy a session that is merely missing one capability (#5571 W3.1). The
	// dead grant is revoked for tidiness (the invalidation sweep already voided
	// it), and the reason replaces the earlier one: "the credential died
	// mid-provision" is more specific than a bare absence.
	if rerr := revalidateAfterProvision(cred); rerr != nil {
		if opts.SandboxCredentials != nil {
			opts.SandboxCredentials.Revoke()
		}
		res.CallbackUnavailable = rerr.Error()
	}
	return res, nil
}

// daemonBranchForTitle names the branch an off-box session works on, from the
// daemon's frozen branch_prefix snapshot — the same derivation the local runtime
// uses — so the sandbox's worktree does not re-derive it from the host's own
// config (#5571 W2.4).
func daemonBranchForTitle(opts InstanceOptions) string {
	prefix := ""
	if opts.BranchPrefix != nil {
		prefix = *opts.BranchPrefix
	} else if cfg, err := config.LoadConfig(); err == nil {
		prefix = cfg.BranchPrefix
	}
	return git.BranchForTitle(prefix, opts.Title)
}

// reapUnusableSandbox tears down a sandbox that was provisioned but must not be
// used, and settles the ONE question every such failure exit has to answer: is
// the sandbox confirmed gone, or might it still be running?
//
// THE rule, in one place instead of once per branch: an UNKNOWN teardown outcome
// must never drop the only handle on a sandbox that may still be running. That
// is the same fail-closed posture TeardownStateUnknown already buys everywhere
// else — a record that cannot be proved dead is the last pointer to it.
//
// Where the handle GOES is the caller's business, and the only thing that
// differs between the create and the replacement paths: retain installs it, and
// nil means the caller has nowhere to put one. The classification is NOT the
// caller's business, because a branch that re-derives it drifts — #3475 is
// exactly that drift, two adjacent exits in reprovisionRemote answering this
// condition differently.
//
// A KNOWN teardown error still means the runtime is gone, per the runtimes'
// best-effort contract, so it is logged and the cause is returned alone.
func reapUnusableSandbox(title string, res ProvisionResult, cause error, retain func(ProvisionResult)) error {
	if res.Teardown == nil {
		return cause
	}
	terr := res.Teardown()
	if terr == nil {
		return cause
	}
	if TeardownStateUnknown(terr) {
		if retain != nil {
			retain(res)
		}
		return fmt.Errorf("%w; and the sandbox provisioned for it could not be confirmed torn down, so it may still be running: %w", cause, terr)
	}
	log.WarningLog.Printf("session %q: discarded sandbox teardown reported a completed error: %v", title, terr)
	return cause
}

// resolveBackendKind decides which runtime a new session uses, in precedence
// order: an explicit --backend flag (opts.Backend) wins; then the legacy
// ForceRemote hook selector; otherwise the repo's `backend` config key, which
// defaults to local.
//
// On the DEFAULT (no explicit selection) path a path that is not a git repo, or
// a repo with no in-repo config or no `backend` key, resolves to local. A repo
// whose config exists but cannot be resolved FAILS the create with that error
// (#5801): the backend is undeterminable, and guessing local would run the agent
// somewhere other than the isolation the repo chose.
func resolveBackendKind(opts InstanceOptions, absPath string) (BackendKind, error) {
	if opts.Host != "" {
		// A host pick is an ssh placement. An explicit backend that is not ssh
		// contradicts it; refuse rather than guess which was meant.
		if opts.Backend != "" && opts.Backend != BackendSSH {
			return "", fmt.Errorf("--host %q places a session on a listed host over ssh, which contradicts --backend %s; drop one of them", opts.Host, opts.Backend)
		}
		return BackendSSH, nil
	}
	if opts.Backend != "" {
		return ParseBackendKind(string(opts.Backend))
	}
	if opts.ForceRemote {
		return BackendHook, nil
	}
	// Not a git repo: there is no in-repo config to read, so local as ever.
	repo, err := config.RepoFromPath(absPath)
	if err != nil {
		return BackendLocal, nil
	}
	// A repo whose config EXISTS but cannot be resolved (a TOML syntax error
	// anywhere in it, an invalid value) has an undeterminable backend. Falling
	// back to local would run an agent the repo isolated (docker/ssh) on the
	// daemon host with no error, so refuse with the config error (#5801).
	cfg, err := config.ResolveConfigForRepo(repo)
	if err != nil {
		return "", fmt.Errorf("cannot determine the backend for %s: its configuration could not be resolved, and creating the session on the local machine instead could bypass the isolation the repo asks for — fix the config and retry: %w", absPath, err)
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
	backendFactory = func(opts InstanceOptions, absPath string, _ BackendKind) (ProvisionResult, error) {
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
	// An instance's project path is host layout from the moment it exists.
	hostlayout.Default.Note(opts.Path)
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

	// Resolve once, before any gate that depends on the effective backend. Keep a
	// resolution error until the point where backendFactory used to report it so
	// invalid config retains its established error precedence.
	kind, resolveBackendErr := resolveBackendKind(opts, absPath)
	if resolveBackendErr == nil && opts.InPlace && kind != BackendLocal {
		return nil, inPlaceBackendConflict(kind, opts)
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

	// Judge account support on the RESOLVED backend. An empty opts.Backend means
	// "read the repo config", not local; checking the request field let the most
	// common ssh/sandbox/hook path provision on ambient credentials while the
	// instance recorded the requested account (#3082, #3103).
	accountBackendErr := error(nil)
	if resolveBackendErr == nil {
		accountBackendErr = refuseOffBoxAccountForKind(opts, kind)
		if accountBackendErr != nil && accountCarried(kind, absPath, opts.Host) {
			// An account on a LISTED host is carried by W7.3's synced scope (#5571
			// W7.4); the refusal below stands for every other off-box target.
			accountBackendErr = nil
		} else if accountBackendErr != nil && opts.AccountSource != "" {
			// A DEFAULT account (default_accounts) must never make a sandbox create
			// impossible: nobody typed it, so there is no identity to defend and no
			// flag to fix. The session runs without it — on the host's own
			// credentials, as an unlisted target always did — and the operator is
			// told. An explicit --account is still refused with its remedy.
			log.WarningLog.Printf("not applying account %q to %q on a %s target that cannot carry it: %s; %s",
				opts.Account, opts.Title, kind, opts.AccountSource, listedHostAccountRemedy())
			opts.Account, opts.AccountSource = "", ""
			accountBackendErr = nil
		}
	} else {
		// Preserve the direct guard's established answer for an explicitly invalid
		// backend; the resolver error remains authoritative for repo-config errors.
		accountBackendErr = refuseOffBoxAccount(opts)
	}
	if accountBackendErr != nil {
		return nil, accountBackendErr
	}
	if err := refuseUnsupportedAccountAgent(opts, absPath); err != nil {
		return nil, err
	}
	if resolveBackendErr != nil {
		return nil, resolveBackendErr
	}

	res, err := backendFactory(opts, absPath, kind)
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
			//
			// A KNOWN cleanup failure is SURFACED here rather than logged, unlike on
			// the replacement path. That is deliberate and stays: the sandbox is
			// provably gone either way, and a create that failed for a reason it can
			// name should say so to the person who asked for the session.
			//
			// An UNKNOWN one is the case that used to end here as prose (#3480). No
			// Instance exists to retain a handle on, so the identity leaves through the
			// error instead, and the daemon tombstones it into the cleanup retry it
			// already runs for a failed Start.
			if res.Teardown != nil {
				if cleanupErr := res.Teardown(); cleanupErr != nil {
					if TeardownStateUnknown(cleanupErr) {
						// Classified HERE, by this teardown's own answer, and handed to the
						// constructor already decided — never re-derived from the combined
						// error, which can carry a sentinel from the client-build cause.
						return nil, newSandboxOrphanError(
							sandboxIdentity{id: id, title: opts.Title, path: absPath, createdAt: t, repoIdentityPath: opts.RepoIdentityPath}, res,
							fmt.Errorf("failed to build remote agent-server client and sandbox cleanup state is unknown; a sandbox may still be running: %w",
								errors.Join(err, cleanupErr)))
					}
					return nil, fmt.Errorf("failed to build remote agent-server client and sandbox cleanup failed: %w",
						errors.Join(err, cleanupErr))
				}
			}
			return nil, fmt.Errorf("failed to build remote agent-server client: %w", err)
		}
	}
	branchPrefix := ""
	if opts.BranchPrefix != nil {
		branchPrefix = *opts.BranchPrefix
	}

	accountAgent := ""
	if strings.TrimSpace(opts.Account) != "" {
		// refuseUnsupportedAccountAgent already refused a program whose
		// resolution drifts to another agent, so the requested enum IS the
		// namespace the account was selected in (#4430 review round 4).
		accountAgent = sessionenv.AgentForCommand(opts.Program)
	}

	return &Instance{
		ID:           id,
		sandboxCreds: opts.SandboxCredentials,
		TaskID:       opts.TaskID,
		Title:        opts.Title,
		// The daemon-named branch of an off-box session, known from the first
		// frame rather than after the first push (#5571 W2.4). Empty for local.
		Branch:         res.Branch,
		branchUnpushed: res.Branch != "",
		// A task delivery's run begins here and ends when the agent goes idle
		// (#1892). Only a task-spawned session has a run to bound; a user's session
		// is never counted against a cap.
		taskRunActive:         opts.TaskID != "",
		BacklogItem:           opts.BacklogItem,
		createdBySandbox:      opts.CreatedBySandbox,
		createdByOperator:     opts.CreatedBySandbox == "", // InstanceData.CreatedByOperator
		backlogRunActive:      opts.BacklogItem != 0,
		liveness:              LiveReady,
		Path:                  absPath,
		repoIdentityPath:      opts.RepoIdentityPath,
		Program:               opts.Program,
		Account:               opts.Account,
		Host:                  recordedAccountHost(opts, kind, absPath),
		accountAgent:          accountAgent,
		Height:                0,
		Width:                 0,
		CreatedAt:             t,
		UpdatedAt:             t,
		preResolvedProgram:    resolvedProgramMarker(opts),
		sessionEnvPassthrough: normalizedSessionEnv,
		inPlace:               opts.InPlace,
		branchPrefix:          branchPrefix,
		branchPrefixResolved:  opts.BranchPrefix != nil,
		branchName:            strings.TrimSpace(opts.BranchName),
		carriedConversation:   opts.ResumeConversation,
		carriedTabs:           append([]TabData(nil), opts.RestoreTabs...),
		carriedRecreateNotice: opts.PendingRecreateNotice,
		backend:               backend,
		remoteClient:          remoteClient,
		runtimeTeardown:       res.Teardown,
		callbackUnavailable:   callbackUnavailableReasonFor(opts, res),
		agentConfigSkipped:    append([]string(nil), res.AgentConfigSkipped...),
		hostedRemotely:        opts.HostedRemotely,
		advertisedFeatures:    opts.AdvertisedFeatures,
	}, nil
}

// callbackUnavailableReasonFor picks the callback-absence reason a new instance
// records. The provisioning path's reason wins — it names what the mint or the
// post-provision revalidation actually refused; the caller's bare declaration
// (the in-sandbox agent-server noticing no callback env) is the generic
// fallback, recorded so the instance's own guidance can stop teaching af verbs.
func callbackUnavailableReasonFor(opts InstanceOptions, res ProvisionResult) string {
	if res.CallbackUnavailable != "" {
		return res.CallbackUnavailable
	}
	if opts.CallbackUnavailable {
		return callbackUndeliveredReason
	}
	return ""
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
	return refuseOffBoxAccountForKind(opts, kind)
}

func refuseOffBoxAccountForKind(opts InstanceOptions, kind BackendKind) error {
	if strings.TrimSpace(opts.Account) == "" {
		return nil
	}
	if kind == BackendLocal || kind.CarriesAccount() {
		return nil
	}
	return accountRefusalWithSource(opts,
		fmt.Errorf("account %q cannot be used with the %s backend: %s", opts.Account, kind, offBoxAccountRefusal(kind)))
}

// offBoxAccountRefusal appends only backend-specific facts to the shared
// AccountWriteBackRationale. The common reason must remain location- and
// mechanism-neutral because hook may use shared storage or run on the daemon
// host; see CarriesAccount.
const offBoxRoundTripReason = AccountWriteBackRationale +
	" af refuses this BY DESIGN and not as pending work: what is missing is the GUARANTEE that " +
	"those writes come back, and af will not present an account as writable without one."

func offBoxAccountRefusal(kind BackendKind) string {
	switch kind {
	case BackendSSH:
		// Reached only for an ssh target that is NOT a listed host (#5571 W7.4): a
		// listed host carries the account through W7.3's synced scope, so the old
		// "refused by design" reasoning no longer applies to it. What remains is
		// that af will not send an account to a target an operator did not list.
		return listedHostAccountReason + "; " + listedHostAccountRemedy()

	case BackendSandbox:
		// The same round-trip reason as ssh, and NOT a "no provable location" one:
		// sandbox runs through sandboxProvisioner, which creates the session directory
		// itself, so af controls the location there too (#3103 review).
		//
		// And like hook, the workaround must NOT name an identity. sandbox.ssh is the
		// operator's own command and this backend deliberately preserves their
		// ssh_config — so a `SendEnv OPENAI_API_KEY` block copies the DAEMON's value
		// to the sandbox verbatim (measured in #3092), and the session would run on
		// the daemon host's identity while the operator believed otherwise. Only
		// backend=ssh is immune, because af pins -F none there.
		return offBoxRoundTripReason +
			" Use the docker or local backend for account-scoped sessions, or omit --account — though which " +
			"identity the session then runs as depends on your sandbox.ssh command and the ssh_config it " +
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

// refuseUnsupportedAccountAgent rejects a cross-agent override or an agent whose
// launch/account contract af has not established. It deliberately does NOT judge
// command shape here: program_overrides can be re-read before launch, and only
// prepareCreateLaunch has the frozen resolved command plus af's generated-argument
// declaration. That final admission names any undeclared arguments before the pane
// starts (#3108).
//
// This remains an ALLOWLIST, not a denylist: an agent added later is unsupported
// until someone proves the boundary accepts its launch, which fails in the safe
// direction (#3051, #3083).
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
		return accountRefusalWithSource(opts, fmt.Errorf(
			"account %q was validated as a %s account, but this session's program_overrides resolves %s to "+
				"a %s command — the account namespaces are separate, so a same-named %s account would be "+
				"used instead of the %s one you selected. Remove the override, or create the session on the "+
				"agent whose account you mean",
			opts.Account, requested, requested, agent, agent, requested))
	}
	// Still an EXPLICIT list, and still deliberately NOT sessionenv.SupportsAccounts:
	// that answers "does this agent have a credential-root variable", which is a
	// different question from "can af prove its launch to the boundary". Keeping them
	// separate is what makes an agent added to the roster unsupported HERE until
	// someone checks the second question, which fails in the safe direction (#3051,
	// #3083). claude is on the list because af's launch rewrite carries provenance the
	// boundary verifies — the launcher declares `--session-id`/`--plugin-dir`, so the
	// rewritten command is provable rather than refused; codex needs no declaration,
	// because af leaves its command unmodified.
	//
	// What changed in #3609 is only WHERE the list lives. This function kept its own
	// copy, so once gemini joined the roster `af accounts add gemini work` succeeded
	// and `--account work` answered "supported: claude, codex" — a sentence the roster
	// had just made false, and the user had no way to tell which surface was wrong.
	// sessionenv owns the one list now, and every surface says the same thing about a
	// rostered-but-unproven agent.
	//
	// gemini joined the list in #3639, by running the check rather than waiving it:
	// af hands its pane the resolved command unchanged, so ValidateAccountCommand
	// proves it without a declaration, and a scoped session was observed using the
	// account directory while an unscoped control kept the ambient identity. The two
	// lists agree again, which is the normal state — the registration-only surfaces
	// exist for the gap between them, not as a permanent fixture.
	if sessionenv.AccountLaunchProven(agent) {
		return nil
	}
	// A ROSTERED agent gets the registration-only sentence, because "unsupported" is
	// false for it: the account exists, the login works, and only the launch is
	// unproven. Naming the follow-up is what makes that a state rather than a dead end.
	if reason, ok := sessionenv.AccountRegistrationOnlyReason(agent); ok {
		return accountRefusalWithSource(opts,
			fmt.Errorf("account %q is registered for %s, but %s", opts.Account, agent, reason))
	}
	return accountRefusalWithSource(opts, fmt.Errorf(
		"account %q cannot be used with %s yet: af has not established that the account boundary can "+
			"verify how it launches that agent, so the session could start on the ambient identity or exit "+
			"immediately; account scoping supports %s",
		opts.Account, agent, sessionenv.AccountAgentsSummary()))
}

// accountRefusalWithSource appends where a non-requested account came from, so a
// refusal whose remedy is "omit --account" is not handed to someone with no
// --account to omit (#3386).
//
// The clause goes LAST, after the refusal's own reason, because that reason is
// what the user has to understand first. The separator is " · " and not a bare
// concatenation for a reason the registration-only arm makes concrete: that
// sentence can END IN A URL, and only whitespace terminates one for a terminal's
// link detector — a period or a letter fused to it becomes part of the address.
// AccountSource is itself a complete sentence, so this only joins the two.
func accountRefusalWithSource(opts InstanceOptions, err error) error {
	source := strings.TrimSpace(opts.AccountSource)
	if source == "" || err == nil {
		return err
	}
	return fmt.Errorf("%w · %s", err, source)
}

// resolveAccountForProvision resolves a create's --account to the registered
// account's directory on this host, in the REQUESTED program's namespace.
//
// A create names the account in the enum's registry — the API validated it
// there — so a program_overrides redirect that moves the enum to another
// agent's command must refuse rather than silently spend the resolved agent's
// same-named account (#4430 review, D2). The swap path deliberately differs:
// it selects under the command the session is RUNNING (the resolved one),
// because the session's live identity, not its recorded enum, is what the
// account will pin.
func resolveAccountForProvision(repoRoot, program, accountName string) (sessionenv.Account, error) {
	resolved, err := resolveRepoConfig(repoRoot)
	if err != nil {
		return sessionenv.Account{}, fmt.Errorf("cannot resolve account %q against the session program: %w", accountName, err)
	}
	resolvedProgram := config.ResolveProgram(&resolved.Config, program)
	// A create requests an account in the requested enum's namespace; when
	// program_overrides resolves the enum to another agent's command, silently
	// spending the RESOLVED agent's same-named account is the name-collision
	// hazard the local path refuses via refuseAccountAgentDrift — both backends
	// must give the same answer to the same command (#4430 review).
	requestedAgent := sessionenv.AgentForCommand(program)
	resolvedAgent := sessionenv.AgentForCommand(resolvedProgram)
	if resolvedAgent != requestedAgent {
		return sessionenv.Account{}, fmt.Errorf(
			"account %q is a %s account, but this session resolves %s to a %s command; account namespaces are separate, so the session would not use the identity you selected",
			accountName, requestedAgent, requestedAgent, resolvedAgent)
	}
	return selectAccountInNamespace(requestedAgent, accountName)
}

// selectAccountInNamespace resolves accountName inside agent's account
// registry. It is the shared tail of account selection: fresh provisions reach
// it through resolveAccountForProvision's refused-or-matched requested
// namespace, a swap reaches it with the resolved launch command's agent, and a
// committed manual swap's retry reaches it with the namespace the transaction
// recorded at commit — the one answer a later config flip cannot move (#4430
// review).
func selectAccountInNamespace(agent, accountName string) (sessionenv.Account, error) {
	home, err := config.GetConfigDir()
	if err != nil {
		return sessionenv.Account{}, fmt.Errorf("cannot resolve account %q: %w", accountName, err)
	}
	account, err := agentaccount.Selected(home, agent, accountName)
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

// SetProvisionResultFactoryForTest replaces the backend factory with f and
// returns a restore function.
//
// The full-fidelity sibling of SetBackendFactoryForTest, which adapts a
// Backend-only stub and therefore hands back a ProvisionResult with no endpoint
// and no teardown. A test that needs to drive a POST-provision failure exit — a
// bad endpoint, a teardown that cannot confirm its sandbox is gone — cannot use
// that seam, because the very fields it exercises are the ones that seam cannot
// express.
func SetProvisionResultFactoryForTest(f func(opts InstanceOptions, absPath string, kind BackendKind) (ProvisionResult, error)) func() {
	prev := backendFactory
	backendFactory = f
	return func() { backendFactory = prev }
}

package daemon

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// The backend catalog RPC (#1933). The daemon has always ACCEPTED a backend on
// create (CreateSessionRequest.Backend, the `--backend` flag), but a client had
// no way to ask which backends exist or whether a given repo can actually use one.
// The CLI papered over that by hard-coding the enum in its flag help; the web
// offered no choice at all, so a remote session could only be started from the
// TUI/CLI.
//
// ListBackends closes that gap by making the daemon the one place a client asks.
// It answers from config.SupportedBackends and the repo's own config + environment,
// so a backend added server-side is offered by every client that renders this
// response — no client-side enum to update, and none to drift.
//
// The contract that matters most here: a picker built from this response is a
// PROMISE. Every entry rendered as usable tells the user "choose this and it will
// work". So this RPC never guesses. Where it cannot verify a precondition it says
// so, by name, rather than returning a plausible default — an unverified "sure,
// looks fine" is how a picker becomes the bug it was meant to fix.

// BackendAvailability is the three-outcome answer to "can this repo use this
// backend?". The third value is the point: "I could not check" is a DIFFERENT
// answer from "yes" and from "no", and collapsing it into either is how a probe
// that cannot answer ends up answering anyway. Callers must render all three.
type BackendAvailability string

const (
	// BackendAvailable means every precondition that can be checked without side
	// effects was checked and passed.
	BackendAvailable BackendAvailability = "available"
	// BackendUnavailable means a precondition was checked and FAILED: creating on
	// this backend would fail as the repo is configured right now. Reason says what
	// to fix.
	BackendUnavailable BackendAvailability = "unavailable"
	// BackendUnknown means the preconditions could not be evaluated at all (e.g. the
	// repo's config could not be read), so neither yes nor no is honest. Reason says
	// what stopped the check. A client must not present this as usable.
	BackendUnknown BackendAvailability = "unknown"
)

// ListBackendsRequest asks which runtimes a create against RepoPath may select.
// RepoPath is the repo ROOT (or any path inside it — resolved the same way
// CreateSession resolves it), and is required: availability and the default are
// both properties of a specific repo, so there is no repo-less answer.
type ListBackendsRequest struct {
	RepoPath string `json:"repo_path"`
}

// BackendOption is one selectable runtime and whether this repo can use it.
type BackendOption struct {
	// Name is the wire value: what a client sends back as CreateSessionRequest.Backend.
	Name string `json:"name"`
	// Label is the user-facing name clients render. It normally matches Name, but
	// the hook backend names the remote sandbox launcher the repo configured while
	// retaining "hook" as the literal CLI/config key.
	Label string `json:"label"`
	// Status is the checked answer; see BackendAvailability.
	Status BackendAvailability `json:"status"`
	// Reason is the actionable, user-facing explanation whenever Status is not
	// available — the SAME text the CLI prints at create time, because both come
	// from the session package's precondition checks. It names what to fix ("set
	// docker.image in …"), never merely "unavailable". Empty when available.
	Reason string `json:"reason,omitempty"`
	// CallbackUnavailableReason says a create on this backend will succeed but
	// the session will have NO af callback — the daemon cannot mint a credential
	// while its dedicated sandbox callback endpoint is not running (#5571 W3.1;
	// network.listen_addr and network.require_token do not decide it). It is advisory, not
	// a precondition: the backend stays available because a callback-less
	// session still works, and the text is the SAME refusal the create path
	// degrades with (one shared check in daemon/sandbox_token.go), so picker and
	// create cannot disagree. Set only on kinds that inject a callback; empty
	// when a callback would be delivered.
	CallbackUnavailableReason string `json:"callback_unavailable_reason,omitempty"`
	// Readiness and Note describe what af last learned about the REMOTE host
	// behind a sandbox backend (#5571 W2.3): "ready", "stale", "unprobed" or
	// "blocked". They come from the verdict cached by af's last real contact
	// with the host, so this RPC still dials nothing. A stale or unprobed host
	// stays selectable (a create re-runs the live check, and blocking would make
	// the first contact impossible) but is never presented as plain available:
	// clients render Note beside the backend. Empty for backends with no remote
	// host.
	Readiness string `json:"readiness,omitempty"`
	Note      string `json:"note,omitempty"`
}

// ListBackendsResponse is the catalog for one repo.
type ListBackendsResponse struct {
	// Backends is every supported backend in canonical presentation order
	// (config.SupportedBackends). Unusable ones are included deliberately: a client
	// that hid them would leave the user guessing why the backend they read about is
	// missing, when the useful answer is the Reason.
	Backends []BackendOption `json:"backends"`
	// Default is the backend a create with NO explicit backend resolves to for this
	// repo. Clients render it as the label of their "repo default" choice and must
	// send NO backend to get it, not this value echoed back: sending it explicitly
	// would freeze today's default into the request and ignore a later config change.
	//
	// EMPTY when the repo's `backend` key names something unrecognized — there is no
	// default to report then, because such a create does not fall back to local, it
	// FAILS (session.resolveBackendKind: "misconfiguration should fail the create,
	// not silently run local"). DefaultStatus/DefaultReason carry the misconfiguration.
	Default string `json:"default"`
	// DefaultStatus is the checked answer for the repo-default choice itself, so a
	// client can block a default it knows will fail instead of discovering it at
	// create time.
	DefaultStatus BackendAvailability `json:"default_status"`
	// DefaultReason explains a DefaultStatus that is not available, naming the
	// offending value and the file it is in.
	DefaultReason string `json:"default_reason,omitempty"`
	// Hosts is every host in the operator's global host list, sorted by name, with
	// whether a create picking it (CreateSessionRequest.Host) would work right now
	// (W6.3). Absent when no hosts are configured. Reason follows the same rule as
	// BackendOption.Reason.
	Hosts []HostOption `json:"hosts,omitempty"`
	// DefaultHost is the configured default_host, which applies when a create picks
	// no host and resolves to the ssh backend with no repo [ssh] host. Empty when unset.
	DefaultHost string `json:"default_host,omitempty"`
}

// HostOption is one listed host and whether this repo can place a session on it.
type HostOption struct {
	// Name is what a client sends back as CreateSessionRequest.Host.
	Name string `json:"name"`
	// Address is the host's ssh destination, shown for orientation only.
	Address string              `json:"address"`
	Status  BackendAvailability `json:"status"`
	Reason  string              `json:"reason,omitempty"`
	// Readiness is af's last probe of this host: ready | blocked | stale |
	// unprobed (CheckHost, a create, or the supervisor's refresh).
	Readiness string `json:"readiness,omitempty"`
	// Note is advisory text beside a host that is stale or not yet probed — never
	// a block (a create re-checks the host live).
	Note string `json:"note,omitempty"`
}

// hostCatalog answers for the operator's host list. An unreadable config is
// reported as unknown per host rather than by omitting them.
func hostCatalog(cfg *config.ResolvedConfig, cfgErr error, repoRoot string) []HostOption {
	if cfgErr != nil || cfg == nil || len(cfg.Hosts) == 0 {
		return nil
	}
	names := make([]string, 0, len(cfg.Hosts))
	for name := range cfg.Hosts {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]HostOption, 0, len(names))
	for _, name := range names {
		opt := HostOption{Name: name, Address: cfg.Hosts[name].Host, Status: BackendAvailable}
		if err := session.HostUnusableReason(name, cfg, repoRoot); err != nil {
			opt.Status, opt.Reason = BackendUnavailable, err.Error()
		} else {
			applyListedHostReadiness(&opt)
		}
		out = append(out, opt)
	}
	return out
}

// ListBackends reports the selectable runtimes for a repo, and which backend an
// unspecified create resolves to. It is read-only: it provisions nothing, starts
// nothing, dials nothing, and touches no session state.
func (s *controlServer) ListBackends(req ListBackendsRequest, resp *ListBackendsResponse) error {
	// A remote client sends the path as typed; expand ~ against THIS (the
	// daemon's) home, never the caller's.
	repo, err := config.RepoFromPath(config.ExpandTilde(strings.TrimSpace(req.RepoPath)))
	if err != nil {
		return err
	}

	workspace := repo.WorkspacePath()
	cfg, cfgErr := config.ResolveConfigForRepo(repo)
	// The callback check reads the LIVE global config — the same input the mint
	// reads — never the repo-resolved cfg: callback availability is a daemon
	// property (listener + auth posture), not a project property. A bare
	// controlServer (some tests) has no manager; "" then means "cannot say",
	// which renders as no advisory rather than a fabricated finding.
	callbackReason := ""
	if s.manager != nil {
		callbackReason = s.manager.callbackUnavailableReason()
	}
	resp.Backends = backendCatalog(config.SupportedBackends, cfg, cfgErr, workspace, callbackReason)
	resp.Default, resp.DefaultStatus, resp.DefaultReason = defaultFor(resp.Backends, cfg, cfgErr, workspace)
	resp.Hosts = hostCatalog(cfg, cfgErr, workspace)
	if cfgErr == nil && cfg != nil {
		resp.DefaultHost = cfg.DefaultHost
	}
	return nil
}

// backendCatalog answers for a LIST of backends, preserving its order and adding,
// dropping and renaming nothing.
//
// It takes the list as a PARAMETER rather than reading config.SupportedBackends
// itself, which is what makes the anti-drift property — whatever the canonical
// enum holds is offered — testable by passing a list instead of swapping the
// global out from under the process (#2079).
//
// That swap is a DATA RACE, not just a smell: config.SupportedBackends is
// read by session.ParseBackend (session/runtime.go, slices.Contains) and Go runs a
// package's tests concurrently, so a test assigning to it races every other test
// resolving a backend name. Its twin — the same pattern on tmux.SupportedPrograms
// — is what `go test -race` failed on in #1970, and this one had simply not been
// unlucky yet. Neither enum is ever written in production; both are read-only
// after init, which is precisely why their read paths need no lock.
func backendCatalog(names []string, cfg *config.ResolvedConfig, cfgErr error, repoRoot string, callbackReason string) []BackendOption {
	out := make([]BackendOption, 0, len(names))
	for _, name := range names {
		out = append(out, backendOptionFor(session.BackendKind(name), cfg, cfgErr, repoRoot, callbackReason))
	}
	return out
}

// backendOptionFor answers for ONE backend, honestly.
//
// The unreadable-config case is why Status is a tri-state. Reporting "docker
// requires docker.image" for a repo whose config we could not parse would be a
// fabricated finding: docker.image might be set perfectly well in a file with a
// stray comma elsewhere. The user needs to hear about the comma.
func backendOptionFor(kind session.BackendKind, cfg *config.ResolvedConfig, cfgErr error, repoRoot string, callbackReason string) BackendOption {
	opt := BackendOption{Name: string(kind), Label: backendLabel(kind, cfg)}
	// The callback advisory is set BEFORE the early returns below so it renders
	// on every status — an "unknown" or "unavailable" backend still deserves the
	// same answer the create path would degrade with.
	if kind.InjectsSandboxCallback() {
		opt.CallbackUnavailableReason = callbackReason
	}

	if cfgErr != nil {
		// local is the one backend that reads nothing from the repo config, so an
		// unreadable config genuinely does not affect it — and a create with no
		// explicit backend still runs local (resolveBackendKind falls back on a
		// config-resolve failure). Saying "unknown" for it would be its own lie.
		if kind == session.BackendLocal {
			opt.Status = BackendAvailable
			return opt
		}
		opt.Status = BackendUnknown
		// Surface-neutral phrasing. These reasons are rendered verbatim by every
		// client — the web's picker, and `af sessions backends` in a terminal — so
		// telling the reader to "reopen this form" names a thing a CLI user does
		// not have. Say what to fix; each surface knows how to re-ask.
		opt.Reason = fmt.Sprintf("cannot tell whether this repo can use backend=%s: its %s could not be read (%v). Fix that file to get a definite answer.", kind, config.InRepoConfigFileName(repoRoot), cfgErr)
		return opt
	}

	if reason := session.BackendUnusableReason(kind, cfg, repoRoot); reason != nil {
		opt.Status = BackendUnavailable
		opt.Reason = reason.Error()
		return opt
	}
	opt.Status = BackendAvailable
	applyHostReadiness(&opt, kind, cfg)
	return opt
}

// applyListedHostReadiness folds the cached probe verdict for a listed host into
// its option, by the same rule applyHostReadiness applies to sandbox.ssh: only a
// fresh blocking verdict makes it unavailable.
func applyListedHostReadiness(opt *HostOption) {
	r := session.ReadHostReadiness(session.HostReadinessKey(opt.Name))
	opt.Readiness = string(r.State)
	switch r.State {
	case session.HostReadinessBlocked:
		opt.Status = BackendUnavailable
		opt.Reason = r.Reason + " (" + r.Describe() + "; a create re-checks the host)"
	case session.HostReadinessReady:
	default:
		opt.Note = r.Describe()
	}
}

// applyHostReadiness folds the cached remote-host verdict into a sandbox
// option. Only a FRESH blocking verdict makes the backend unavailable — the
// reason is the same text a create would fail with.
func applyHostReadiness(opt *BackendOption, kind session.BackendKind, cfg *config.ResolvedConfig) {
	if kind != session.BackendSandbox || cfg == nil {
		return
	}
	r := session.ReadHostReadiness(cfg.SandboxSSH)
	opt.Readiness = string(r.State)
	switch r.State {
	case session.HostReadinessBlocked:
		opt.Status = BackendUnavailable
		opt.Reason = r.Reason + " (" + r.Describe() + "; a create re-checks the host)"
	case session.HostReadinessReady:
		// plain available
	default:
		opt.Note = r.Describe()
	}
}

// backendLabel turns the daemon's resolved repo config into presentation text so
// every client describes a backend the same way. "hook" is an implementation key;
// the configured launcher's basename is the useful identity. The key stays visible
// because users still select it through `--backend hook` and `backend = "hook"`.
func backendLabel(kind session.BackendKind, cfg *config.ResolvedConfig) string {
	if kind != session.BackendHook {
		return string(kind)
	}

	if cfg != nil && cfg.RemoteHooks != nil {
		launcher := filepath.Base(strings.TrimSpace(cfg.RemoteHooks.LaunchCmd))
		if launcher != "" && launcher != "." {
			return fmt.Sprintf("Remote sandbox · %s (hook)", launcher)
		}
	}
	return "Remote sandbox (hook)"
}

// defaultFor reports the backend an unspecified create resolves to, and whether
// THAT is usable.
//
// It asks the factory's own decision function rather than reimplementing the
// precedence, so this answer cannot disagree with what a real create does — and
// crucially it propagates the function's ERROR instead of swallowing it. An
// invalid `backend` value makes a create fail; a catalog that answered "local"
// there would tell a user with a broken config that everything is normal, and
// their sessions would land on a backend they never chose.
func defaultFor(backends []BackendOption, cfg *config.ResolvedConfig, cfgErr error, repoRoot string) (string, BackendAvailability, string) {
	kind, kerr := session.BackendKindFor(session.InstanceOptions{}, repoRoot)
	if kerr != nil && cfgErr != nil {
		// The repo's config exists but could not be resolved, so a create with no
		// explicit backend REFUSES (#5801) rather than guessing local. That is not
		// "unavailable" (a checked precondition that failed) but "could not be
		// evaluated", like the per-backend rows, and the user can still choose a
		// backend explicitly (local reads no repo config) while they fix the file.
		return "", BackendUnknown, kerr.Error()
	}
	if kerr != nil {
		// Reached only when the config LOADED and named a backend we do not
		// recognize (BackendKindFor falls back to local on a resolve failure). Name
		// the value and the file — "unknown backend" alone leaves the user hunting.
		raw := ""
		if cfgErr == nil && cfg != nil {
			raw = cfg.Backend
		}
		// "or pick a backend above" assumed a rendered list above the reader; see the
		// note on the unknown-status reason. Name the ACTION both surfaces have.
		return "", BackendUnavailable, fmt.Sprintf("this repo's %s sets backend = %q, which is not a known backend (valid: %s). A session created here fails rather than falling back to local — fix that key, or choose a backend explicitly for this session.", config.InRepoConfigFileName(repoRoot), raw, config.SupportedBackendsString())
	}

	// The default's usability IS the resolved backend's usability; reuse the answer
	// already computed for it rather than re-deriving one that could disagree.
	for _, opt := range backends {
		if opt.Name == string(kind) {
			return string(kind), opt.Status, opt.Reason
		}
	}
	// A registered backend missing from the catalog would be an enum/registry drift
	// the session package's guard test exists to prevent; say so rather than
	// claiming it is fine.
	return string(kind), BackendUnknown, fmt.Sprintf("this repo defaults to backend=%s, which is not in the supported list (%s)", kind, config.SupportedBackendsString())
}

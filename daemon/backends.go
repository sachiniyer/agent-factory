package daemon

import (
	"fmt"

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
	// Status is the checked answer; see BackendAvailability.
	Status BackendAvailability `json:"status"`
	// Reason is the actionable, user-facing explanation whenever Status is not
	// available — the SAME text the CLI prints at create time, because both come
	// from the session package's precondition checks. It names what to fix ("set
	// docker.image in …"), never merely "unavailable". Empty when available.
	Reason string `json:"reason,omitempty"`
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
}

// ListBackends reports the selectable runtimes for a repo, and which backend an
// unspecified create resolves to. It is read-only: it provisions nothing, starts
// nothing, dials nothing, and touches no session state.
func (s *controlServer) ListBackends(req ListBackendsRequest, resp *ListBackendsResponse) error {
	repo, err := config.RepoFromPath(req.RepoPath)
	if err != nil {
		return err
	}

	cfg, cfgErr := config.ResolveConfig(repo.Root)
	resp.Backends = backendCatalog(config.SupportedBackends, cfg, cfgErr, repo.Root)
	resp.Default, resp.DefaultStatus, resp.DefaultReason = defaultFor(resp.Backends, cfg, cfgErr, repo.Root)
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
func backendCatalog(names []string, cfg *config.ResolvedConfig, cfgErr error, repoRoot string) []BackendOption {
	out := make([]BackendOption, 0, len(names))
	for _, name := range names {
		out = append(out, backendOptionFor(session.BackendKind(name), cfg, cfgErr, repoRoot))
	}
	return out
}

// backendOptionFor answers for ONE backend, honestly.
//
// The unreadable-config case is why Status is a tri-state. Reporting "docker
// requires docker.image" for a repo whose config we could not parse would be a
// fabricated finding: docker.image might be set perfectly well in a file with a
// stray comma elsewhere. The user needs to hear about the comma.
func backendOptionFor(kind session.BackendKind, cfg *config.ResolvedConfig, cfgErr error, repoRoot string) BackendOption {
	opt := BackendOption{Name: string(kind)}

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
		opt.Reason = fmt.Sprintf("cannot tell whether this repo can use backend=%s: its %s could not be read (%v). Fix that file, then reopen this form.", kind, config.InRepoConfigFileName(repoRoot), cfgErr)
		return opt
	}

	if reason := session.BackendUnusableReason(kind, cfg, repoRoot); reason != nil {
		opt.Status = BackendUnavailable
		opt.Reason = reason.Error()
		return opt
	}
	opt.Status = BackendAvailable
	return opt
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
	if kerr != nil {
		// Reached only when the config LOADED and named a backend we do not
		// recognize (BackendKindFor falls back to local on a resolve failure). Name
		// the value and the file — "unknown backend" alone leaves the user hunting.
		raw := ""
		if cfgErr == nil && cfg != nil {
			raw = cfg.Backend
		}
		return "", BackendUnavailable, fmt.Sprintf("this repo's %s sets backend = %q, which is not a known backend (valid: %s). A session created here fails rather than falling back to local — fix that key, or pick a backend above.", config.InRepoConfigFileName(repoRoot), raw, config.SupportedBackendsString())
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

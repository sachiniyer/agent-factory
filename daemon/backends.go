package daemon

import (
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// The backend catalog RPC (#1933). The daemon has always ACCEPTED a backend on
// create (CreateSessionRequest.Backend, the `--backend` flag), but a client had
// no way to ask which backends exist or whether a given repo is configured for
// one. The CLI papered over that by hard-coding the enum in its flag help; the
// web offered no choice at all, so a remote session could only be started from
// the TUI/CLI.
//
// ListBackends closes that gap by making the daemon the one place a client asks.
// It answers from config.SupportedBackends and the repo's own config, so a
// backend added server-side is offered by every client that renders this response
// — no client-side enum to update, and none to drift.

// ListBackendsRequest asks which runtimes a create against RepoPath may select.
// RepoPath is the repo ROOT (or any path inside it — it is resolved the same way
// CreateSession resolves it), and is required: availability and the default are
// both properties of a specific repo's config, so there is no repo-less answer.
type ListBackendsRequest struct {
	RepoPath string `json:"repo_path"`
}

// BackendOption is one selectable runtime and whether this repo is configured for
// it.
//
// Available reflects the repo CONFIG preconditions only (session.BackendConfigError):
// docker.image, ssh.host, remote_hooks. It is a "may the user pick this without a
// guaranteed config error" signal, NOT a promise that provisioning will succeed —
// environment preconditions (the `docker` CLI on PATH, an `origin` remote to clone
// from) are not knowable from config and still surface at create time. Available
// false is therefore reliable (it WILL fail as configured); available true means
// "not ruled out".
type BackendOption struct {
	// Name is the wire value: what a client sends back as CreateSessionRequest.Backend.
	Name string `json:"name"`
	// Available is false when this repo's config cannot satisfy the backend.
	Available bool `json:"available"`
	// Reason is the actionable, user-facing explanation when Available is false —
	// the SAME text the CLI prints at create time, because both come from
	// session.BackendConfigError. Empty when Available is true.
	Reason string `json:"reason,omitempty"`
}

// ListBackendsResponse is the catalog for one repo.
type ListBackendsResponse struct {
	// Backends is every supported backend in canonical presentation order
	// (config.SupportedBackends), available or not. Unavailable ones are included
	// deliberately: a client that hid them would leave the user guessing why the
	// backend they read about is missing, when the useful answer is the Reason.
	Backends []BackendOption `json:"backends"`
	// Default is the backend a create with NO explicit backend resolves to for this
	// repo — the repo's `backend` config key, else local. Clients render it as the
	// label of their "repo default" choice and must send NO backend to get it, not
	// this value echoed back: sending it explicitly would freeze today's default
	// into the request and silently ignore a later repo-config change.
	Default string `json:"default"`
}

// ListBackends reports the selectable runtimes for a repo, and which backend an
// unspecified create resolves to. It is read-only: it provisions nothing, starts
// nothing, and touches no session state.
func (s *controlServer) ListBackends(req ListBackendsRequest, resp *ListBackendsResponse) error {
	repo, err := config.RepoFromPath(req.RepoPath)
	if err != nil {
		return err
	}

	// Resolution mirrors create's precedence exactly (session.resolveBackendKind):
	// a config that fails to resolve is not fatal there — the create falls back to
	// local — so it must not be fatal here either, or the web would refuse to offer
	// a choice for a repo the CLI can still create in. A nil cfg reads as "every
	// optional section absent", which is the honest answer for both a repo with no
	// in-repo config and one whose config could not be read.
	var cfg *config.ResolvedConfig
	if resolved, rerr := config.ResolveConfig(repo.Root); rerr == nil {
		cfg = resolved
	}

	// The default comes from the factory's own decision function rather than a
	// reimplementation of the precedence here, so this answer cannot disagree with
	// what a real create actually does.
	def := string(session.BackendLocal)
	if kind, kerr := session.BackendKindFor(session.InstanceOptions{}, repo.Root); kerr == nil {
		def = string(kind)
	}
	resp.Default = def

	resp.Backends = make([]BackendOption, 0, len(config.SupportedBackends))
	for _, name := range config.SupportedBackends {
		opt := BackendOption{Name: name, Available: true}
		if cerr := session.BackendConfigError(session.BackendKind(name), cfg); cerr != nil {
			opt.Available = false
			opt.Reason = cerr.Error()
		}
		resp.Backends = append(resp.Backends, opt)
	}
	return nil
}

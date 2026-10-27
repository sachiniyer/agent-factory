package daemon

// Project-registry control-plane request/response types (#2355). Split out of
// control_types.go alongside the handlers in control_server_projects.go so the
// RPCs that write and read the durable project registry — register, rebind,
// delete, list — read as one surface.

import (
	"github.com/sachiniyer/agent-factory/config"
)

// DeleteProjectRequest asks the daemon to delete a project — a repo grouping of
// sessions (#1735). Every regular live session is archived (worktree relocated,
// branch/state preserved, restorable via RestoreArchived); in-place/external
// sessions are torn down. The repo's root_agents opt-in is removed, as is any
// durable registration whose root matches RepoPath, and the always-on root agent
// (if any) is stopped. The user's real git repo is never touched. Restoring an
// archived session makes the repo active again, but does not restore its
// registration or root opt-in.
//
// RepoPath is the repo root (the stable project id clients group by:
// worktree.repo_path). RepoID is the precomputed id; when either is omitted the
// daemon derives it from RepoPath or its durable registry record, respectively.
// When both are set they must identify the same project. At least one must be
// set. Deleting an unknown project is a clean no-op; a registered project with no
// live sessions is deregistered.
//
// When RepoPath is set it must be absolute (or ~-prefixed) after surrounding
// whitespace is trimmed, under the same rule as RegisterProjectRequest.Path and
// RebindProjectRequest.Path (#4821, 6616c129): the daemon has no access to the
// caller's working directory, so a relative RepoPath would resolve against the
// daemon's own cwd and delete whatever project it landed on. The daemon REFUSES
// one at the RPC boundary before any mutation, inverting the most destructive
// operation in the projects family onto the same guard its siblings already
// carry. Callers whose input can be relative resolve it against the user's cwd
// BEFORE sending: the CLI's `af projects delete` does this (api/projects.go), the
// TUI refuses to dispatch when RepoID is empty, and the web only ever supplies
// daemon-host paths. RepoID-only requests (no RepoPath) are unaffected — no path
// is resolved, the root is derived from the registry.
type DeleteProjectRequest struct {
	RepoPath string `json:"repo_path"`
	RepoID   string `json:"repo_id"`
}

type DeleteProjectResponse struct {
	OK bool `json:"ok"`
	// ArchivedCount is how many live sessions were archived (restorable).
	ArchivedCount int `json:"archived_count"`
	// KilledCount is how many live sessions could not be archived and were torn
	// down instead — only in-place/external worktrees (the root agent, `--here`
	// sessions), whose kill never touches the user's tree or branch.
	KilledCount int `json:"killed_count"`
	// Deregistered reports whether the durable project record was removed.
	Deregistered bool `json:"deregistered"`
	// Warning is the FLAT, pre-#3036 wire field. See ArchiveSessionResponse: it
	// is what keeps a committed hook failure visible to an OLDER gob client,
	// which has no slot for the nested envelope.
	Warning string `json:"warning,omitempty"`
	// MutationOutcome carries nonfatal on-archive hook failures from sessions
	// whose archive and project deletion both committed.
	MutationOutcome
}

// RegisterProjectRequest asks the daemon to register a git checkout as a durable,
// sessionless project in the #2355 registry (#2456). Path names a directory on
// the DAEMON's filesystem; the daemon expands ~, resolves symlinks, and walks to
// the git checkout's canonical main-repo root, then validates it.
//
// Path must already be absolute (or ~-prefixed) after surrounding whitespace is
// trimmed — the daemon has no access to the caller's working directory, so a
// relative path would resolve against the daemon's own cwd, which is not the
// caller's. The daemon REFUSES one before touching the registry (#4821), and
// registers the trimmed, expanded value it checked. Callers whose input can be
// relative resolve it against the user's cwd BEFORE sending: the CLI's
// `af projects add` does this (see api/projects.go), and the web only ever
// supplies daemon-host paths. Registration is idempotent: a known checkout is a
// no-op success that returns the existing identity.
type RegisterProjectRequest struct {
	Path string `json:"path"`
}

// RegisterProjectResponse carries the durable identity the registration
// resolved to. The project is now in the registry ON THE DAEMON HOST, and once
// #2456's UI slices land it shows as an empty row in that daemon's project
// switcher until a session is created into it.
//
// Scope caveat for a REMOTE daemon: `af projects list` still runs in-process
// against the CLIENT's local registry, so it will not see a project that `add`
// or `rebind` just wrote to the daemon's registry. For the common local daemon
// the two registries are the same store, so it round-trips as expected. Routing
// list through the daemon too is the follow-up needed for full remote parity
// (#2491).
//
// OK is always true on a nil error (a redundant flag kept for wire symmetry with
// the other project RPCs).
type RegisterProjectResponse struct {
	OK      bool           `json:"ok"`
	Project config.Project `json:"project"`
}

// RebindProjectRequest asks the daemon to move a registered project's stable
// identity (ID, a prj_… registry id) to the checkout at Path — the repair after
// the checkout it names was moved or recloned elsewhere (`af projects rebind`,
// the #2355 registry's explicit rebind).
//
// Path names a directory on the DAEMON's filesystem, under the same rule as
// RegisterProjectRequest.Path: absolute or ~-prefixed, because the daemon has no
// access to the caller's working directory. The CLI resolves its argument
// against the user's cwd before sending (api/projects.go); the web only ever
// supplies daemon-host paths.
type RebindProjectRequest struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

// RebindProjectResponse carries the re-bound durable identity: the same ID,
// its new Root, and the RepoID the replacement checkout resolved to (a real→real
// transition — config.RebindProject never writes an invented id back).
//
// OK is always true on a nil error (wire symmetry with the other project RPCs).
type RebindProjectResponse struct {
	OK      bool           `json:"ok"`
	Project config.Project `json:"project"`
}

// ListProjectsRequest asks the daemon for every durable project in ITS registry
// (#2456). It carries no fields: the registry is machine-local and unscoped, so
// there is one answer per daemon. This is the READ that makes the local project
// list a union of derived-from-sessions projects and explicitly-registered ones
// — a web/TUI client cannot call config.ListProjects in-process the way the CLI
// does, so the union needs this RPC. It is the read half of #2491 (closed as not
// worth it for remote parity alone; it turns out load-bearing for the LOCAL
// union, a different rationale).
type ListProjectsRequest struct{}

// ListProjectsResponse carries every registered project, each a durable identity
// plus its last-known root and whether that path still exists. Clients union
// these roots with the projects they derive from live sessions/tasks so a
// registered-but-sessionless project is still shown and creatable-into.
type ListProjectsResponse struct {
	Projects []config.Project `json:"projects"`
}

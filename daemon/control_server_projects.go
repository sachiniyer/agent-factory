package daemon

// The project-registry control plane (#2355). Split out of control_server.go
// alongside its request/response types (control_types_projects.go) so the four
// handlers that touch the durable registry — and the EventProjectsChanged
// publish every mutation ends with — read as one surface.

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// DeleteProject deletes a project (a repo grouping of sessions, #1735):
// archive-then-remove, reversible. The manager archives every live session
// (restorable), tears down in-place sessions (repo untouched), and drops the
// repo's root_agents opt-in. It publishes one archived/killed event per affected
// session — so every client's rail moves the sessions exactly as a per-session
// archive/kill would — plus a projects-changed signal for clients keying a
// projects view. On a partial failure it still publishes what DID happen before
// surfacing the error, so the rail never lags reality.
func (s *controlServer) DeleteProject(req DeleteProjectRequest, resp *DeleteProjectResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	if err := validateRPCRepoID(req.RepoID); err != nil {
		return err
	}
	// Enforce DeleteProjectRequest's absolute-path contract at the RPC boundary,
	// the same guard RegisterProject and RebindProject apply (#4821, 6616c129);
	// see normalizeDeleteProjectRequestRepoPath. The checked value is the one
	// passed on, and the RepoID-only form (no RepoPath) is preserved.
	if err := normalizeDeleteProjectRequestRepoPath(&req); err != nil {
		return err
	}
	result, err := s.manager.DeleteProject(req)
	for _, k := range result.Killed {
		s.manager.publishEvent(agentproto.EventSessionKilled, session.InstanceData{ID: k.ID, Title: k.Title})
	}
	if len(result.Archived) > 0 || len(result.Killed) > 0 || result.Deregistered {
		s.manager.publishEvent(agentproto.EventProjectsChanged, nil)
	}
	if err != nil {
		return err
	}
	resp.OK = true
	resp.ArchivedCount = len(result.Archived)
	resp.KilledCount = len(result.Killed)
	resp.Deregistered = result.Deregistered
	resp.Warning = strings.Join(result.Warnings, "\n")
	return nil
}

// RegisterProject records a git checkout as a durable, sessionless project in
// the #2355 registry (#2456). The daemon is the single writer (#960): the CLI,
// web, and TUI all route their add-project action here rather than writing the
// registry in-process, so one process owns the store and — for a web or remote
// client — the path is resolved on the daemon's filesystem, not the caller's.
//
// config.RegisterProject does the work (resolve the git root, validate,
// persist, idempotent) under its own file lock, so this handler adds only the
// admission gate, the absolute-path boundary, and the projects-changed publish. It does NOT take a
// manager lock: the registry is independent of the session roster, and
// RegisterProject's own lock already serializes concurrent registrations.
//
// The publish rides the same EventProjectsChanged signal DeleteProject uses, so
// a client showing a projects view re-fetches and the new empty project appears
// without a manual refresh. It is emitted only on a successful, genuinely new or
// re-confirmed registration — an error returns before it, and an idempotent
// re-registration still publishes (harmless: the re-fetch is a no-op diff).
func (s *controlServer) RegisterProject(req RegisterProjectRequest, resp *RegisterProjectResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	// Enforce RegisterProjectRequest's contract here, before the registry is
	// touched (#4821): config.RegisterProject would resolve a relative path
	// against THIS process's cwd. The normalized value is the one registered, so
	// what was checked is exactly what is stored.
	path, err := config.ResolveDaemonHostPath(req.Path)
	if err != nil {
		return fmt.Errorf("project %w", err)
	}
	project, err := config.RegisterProject(path)
	if err != nil {
		return err
	}
	s.manager.publishEvent(agentproto.EventProjectsChanged, nil)
	resp.OK = true
	resp.Project = project
	return nil
}

// RebindProject moves a registered project's stable identity to a replacement
// checkout (`af projects rebind`) — the repair when the checkout a registration
// names was moved or recloned elsewhere. The daemon is the single writer (#960),
// the same reason RegisterProject routes here: the CLI, TUI, and web all call
// this rather than writing the registry in-process, and for a web or remote
// client the path is resolved on the daemon's filesystem, not the caller's.
//
// config.RebindProject does the work under its own file lock — resolving the
// replacement path's binding, refusing a root another project owns, and carrying
// or minting the checkout marker — so this handler adds only the admission gate
// and the projects-changed publish a client showing a projects view re-fetches
// on (the rebound row's root changes; without the event a web switcher would
// keep naming the dead path until the next manual refresh).
func (s *controlServer) RebindProject(req RebindProjectRequest, resp *RebindProjectResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	// Enforce RebindProjectRequest's contract here, before the registry moves:
	// config.RebindProject would resolve a relative path against THIS process's
	// cwd — an unrelated checkout for an ad-hoc daemon, / under systemd — and
	// silently repoint the stable id there. The same boundary RegisterProject
	// applies (#4821); the checked value is the one passed on.
	path, err := config.ResolveDaemonHostPath(req.Path)
	if err != nil {
		return fmt.Errorf("rebind %w", err)
	}
	project, err := config.RebindProject(req.ID, path)
	if err != nil {
		return err
	}
	s.manager.publishEvent(agentproto.EventProjectsChanged, nil)
	resp.OK = true
	resp.Project = project
	return nil
}

// ListProjects returns every durable project in this daemon's #2355 registry
// (#2456). It is a pure config read: config.ListProjects walks the registry
// directory with no lock and no manager, so — like ListBackends — this handler
// takes no admission gate and answers even while the daemon is still restoring
// sessions (a client building its project list must not have to wait on the
// session restore for the registry, which is independent of it). The read is the
// half of #2491 the LOCAL union needs; see ListProjectsRequest.
func (s *controlServer) ListProjects(_ ListProjectsRequest, resp *ListProjectsResponse) error {
	projects, err := config.ListProjects()
	if err != nil {
		return err
	}
	resp.Projects = projects
	return nil
}

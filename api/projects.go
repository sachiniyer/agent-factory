package api

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/projectclone"
	"github.com/sachiniyer/agent-factory/log"
)

// ProjectsCmd is the top-level command group for project management.
var ProjectsCmd = &cobra.Command{
	Use:   "projects",
	Short: "Manage projects and durable registrations",
}

var projectsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List registered projects",
	Long: `List durable machine-local project bindings.
With --daemon-url this is the targeted daemon's registry, not this machine's.

path_exists reports only whether the last-known path is present. It does not
claim that a new checkout at a reused path has the registered identity.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		projects, err := routedListProjects()
		if err != nil {
			return jsonError(err)
		}
		return jsonOut(projects)
	},
}

// registerProjectViaDaemon is the daemon seam, overridable in tests.
var registerProjectViaDaemon = routedRegisterProject

func newProjectsAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "add <path-or-git-url>",
		Aliases: []string{"register"},
		Short:   "Add a project: register a repo by path with a stable local identity",
		Long: `Add a project by registering a git checkout with a stable, machine-local
identity, so it appears as an (initially sessionless) project you can create
sessions into.

The path may be relative (including '.'), absolute, or start with ~. A relative
path or '~' is resolved against YOUR shell's working directory before the
request is sent — so 'af projects add .' registers the repo you are standing in.
The daemon then walks to the checkout's canonical main-repo root and validates
it is a git repository (an actionable error otherwise). Any directory inside a
checkout resolves to that root. Registration is idempotent: adding a known
checkout is a no-op success that returns its existing identity.

The returned project id survives an explicit rebind after the checkout moves.
Two clones remain separate projects. Identity is anchored in an AF-home-scoped
agent-factory/checkout-id-<home-id> marker under the Git common directory, so
one AF home's reset cannot remove another home's identity. No working-tree file
is created, and adding a project does NOT start an always-on agent for it.

With --daemon-url (or AF_DAEMON_URL) the registration goes to that daemon and the
path is a path ON ITS HOST, sent as typed: absolute or starting with ~, never
relative.

Instead of a path, give a git URL (https://host/owner/repo, ssh://host/owner/repo
or git@host:owner/repo) and the DAEMON clones it on its own host, with its own
credentials (ssh keys, credential helper), under its projects_root (default
<af home>/projects), then registers it. The URL is validated by the daemon: file://,
local paths, option-like values and URLs with an embedded password are refused.
Re-adding the same URL returns the existing project.

'register' is a deprecated alias for 'add'.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log.Initialize(false)
			defer log.Close()

			// Registry mutations still use the local control socket, even with a
			// remote target selected. Resolve against the caller's cwd before the
			// daemon (which may run from /) sees the path. ResolveUserPath expands
			// ~ and calls filepath.Abs; remote registry routing is a separate change.
			var project config.Project
			var err error
			if projectclone.LooksLikeURL(args[0]) {
				// A URL names a repository, not a path on any machine: it goes to
				// the daemon verbatim (local or remote) and is cloned THERE. The
				// daemon validates it; this side resolves nothing.
				project, err = registerProjectViaDaemon(daemon.RegisterProjectRequest{URL: strings.TrimSpace(args[0])})
			} else {
				var path string
				if path, err = projectArgPath(args[0]); err != nil {
					return jsonError(err)
				}
				project, err = registerProjectViaDaemon(daemon.RegisterProjectRequest{Path: path})
			}
			if err != nil {
				return jsonError(err)
			}
			return jsonOut(project)
		},
	}
}

// rebindProjectViaDaemon is the daemon seam, overridable in tests.
var rebindProjectViaDaemon = routedRebindProject

var projectsRebindCmd = &cobra.Command{
	Use:   "rebind <project-id> <path>",
	Short: "Rebind a registered project after its checkout moves",
	Long: `Rebind a stable project id to a new checkout path.

The project id is preserved. Rebinding refuses to take a path already owned by
another registered project.

The path may be relative (including '.'), absolute, or start with ~. A relative
path or '~' is resolved against YOUR shell's working directory before the
request is sent — the daemon, which owns the registry write, then resolves the
checkout it lands on.

Against a remote daemon (--daemon-url) the path is on the daemon's host and is
sent as typed (absolute or ~/…).`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		// Same contract as `projects add`: resolve the caller-relative path
		// here, because the daemon resolves what it is sent against ITS own
		// filesystem and has no access to this shell's cwd.
		path, err := projectArgPath(args[1])
		if err != nil {
			return jsonError(err)
		}
		project, err := rebindProjectViaDaemon(daemon.RebindProjectRequest{ID: args[0], Path: path})
		if err != nil {
			return jsonError(err)
		}
		return jsonOut(project)
	},
}

// deleteProjectViaDaemon is the daemon seam, overridable in tests.
var deleteProjectViaDaemon = routedDeleteProject

var projectsDeleteCmd = &cobra.Command{
	Use:   "delete [repo]",
	Short: "Delete a project, archiving its restorable sessions",
	Long: `Delete a project for a git repository and remove its live sessions.

Every regular worktree session is archived (its tmux is torn down and its
worktree moved to the archive dir, but its branch and uncommitted changes are
preserved). The always-on root agent (if any) is stopped and its root-agent
opt-in removed. In-place sessions (the root agent, 'af sessions create --here')
are torn down instead of archived — their cleanup never touches your working
tree or branch.

The durable project registration, if any, is removed so the project leaves the
project list. Restoring an archived session makes its repository active again,
but does not restore the durable registration or root-agent opt-in.

Your real git repository is never touched. To undo a mis-click, restore any
archived session with 'af sessions restore <title>'.

[repo] is a path inside the repository to delete (default: the current repo).
Deleting an unknown project is a clean no-op; deleting a registered project
with no live sessions still removes its registration. Prints how many sessions
were archived.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		req, name, err := resolveProjectDeleteTarget(args)
		if err != nil {
			return jsonError(err)
		}

		resp, err := deleteProjectViaDaemon(req)
		warning := ""
		if err != nil && apiclient.IsMutationCommitted(err) {
			warning = err.Error()
		} else if err != nil {
			return jsonError(err)
		}

		result := map[string]any{
			"ok":             true,
			"project":        name,
			"repo_path":      req.RepoPath,
			"archived_count": resp.ArchivedCount,
			"killed_count":   resp.KilledCount,
			"deregistered":   resp.Deregistered,
		}
		if warning != "" {
			result["warning"] = warning
		}
		return jsonOut(result)
	},
}

// resolveProjectDeleteTarget turns the optional [repo] arg (default: cwd) into a
// DeleteProjectRequest and a friendly project name. It resolves the path to its
// canonical main-repo root when possible so a subdirectory still targets the
// whole project; a path that no longer resolves to a git repo falls back to the
// cleaned absolute path, so deleting a moved/removed project is still a clean
// daemon-side no-op (it archives nothing and sweeps any stale opt-in).
func resolveProjectDeleteTarget(args []string) (daemon.DeleteProjectRequest, string, error) {
	if apiclient.IsRemoteTarget() {
		// The current directory and the git probe below describe THIS machine.
		if len(args) != 1 {
			return daemon.DeleteProjectRequest{}, "", fmt.Errorf("a project path on the daemon host is required against the daemon at %s: the current directory names a repository on this machine", apiclient.RemoteTargetURL())
		}
		p, err := remoteProjectPath(args[0])
		if err != nil {
			return daemon.DeleteProjectRequest{}, "", err
		}
		return daemon.DeleteProjectRequest{RepoPath: p}, filepath.Base(p), nil
	}
	path := "."
	if len(args) == 1 {
		path = args[0]
	}
	abs, err := config.ResolveUserPath(path)
	if err != nil {
		return daemon.DeleteProjectRequest{}, "", fmt.Errorf("failed to resolve project path %q: %w", path, err)
	}
	if repo, err := config.RepoFromPath(abs); err == nil {
		return daemon.DeleteProjectRequest{RepoPath: repo.Root, RepoID: repo.ID}, filepath.Base(repo.Root), nil
	}
	cleaned := filepath.Clean(abs)
	return daemon.DeleteProjectRequest{RepoPath: cleaned}, filepath.Base(cleaned), nil
}

// projectArgPath is the path a registry mutation sends. Locally it is resolved
// against the caller's cwd (the daemon may run from /). Against a remote target
// it is the daemon-host path exactly as typed.
func projectArgPath(raw string) (string, error) {
	if apiclient.IsRemoteTarget() {
		return remoteProjectPath(raw)
	}
	path, err := config.ResolveUserPath(raw)
	if err != nil {
		return "", fmt.Errorf("failed to resolve project path %q: %w", raw, err)
	}
	return path, nil
}

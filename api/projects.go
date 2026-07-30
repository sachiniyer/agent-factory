package api

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
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

path_exists reports only whether the last-known path is present. It does not
claim that a new checkout at a reused path has the registered identity.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		projects, err := config.ListProjects()
		if err != nil {
			return jsonError(err)
		}
		return jsonOut(projects)
	},
}

// registerProjectViaDaemon is the daemon seam, overridable in tests.
var registerProjectViaDaemon = daemon.RegisterProject

func newProjectsAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "add <path>",
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

'register' is a deprecated alias for 'add'.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log.Initialize(false)
			defer log.Close()

			// Resolve the path against the USER's shell cwd before forwarding when
			// the daemon is LOCAL — mirroring resolveProjectDeleteTarget. A local
			// daemon shares this filesystem but NOT this working directory (an ad-hoc
			// daemon inherits its spawner's cwd; a systemd one runs from /), so a raw
			// relative path would make `af projects add .` resolve against the
			// daemon's cwd — the wrong repo silently, or a confusing "not a git
			// repository" when the daemon runs from /. The daemon still expands
			// ~/validates; resolving here only fixes WHICH directory a relative input
			// names.
			//
			// When --daemon-url targets a remote daemon the path names a directory on
			// the REMOTE host, so resolving it against this client's cwd is
			// meaningless — forward it raw and let the daemon resolve it. (Register's
			// transport is still the local control socket today, so a remote target
			// does not yet actually reach a remote daemon; #2491 tracks routing the
			// registry verbs remotely — at which point this branch is already correct.)
			path := args[0]
			if !apiclient.IsRemoteTarget() {
				resolved, err := config.ResolveUserPath(args[0])
				if err != nil {
					return jsonError(fmt.Errorf("failed to resolve project path %q: %w", args[0], err))
				}
				path = resolved
			}
			project, err := registerProjectViaDaemon(daemon.RegisterProjectRequest{Path: path})
			if err != nil {
				return jsonError(err)
			}
			return jsonOut(project)
		},
	}
}

var projectsRebindCmd = &cobra.Command{
	Use:   "rebind <project-id> <path>",
	Short: "Rebind a registered project after its checkout moves",
	Long: `Rebind a stable project id to a new checkout path.

The project id is preserved. Rebinding refuses to take a path already owned by
another registered project.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		project, err := config.RebindProject(args[0], args[1])
		if err != nil {
			return jsonError(err)
		}
		return jsonOut(project)
	},
}

// deleteProjectViaDaemon is the daemon seam, overridable in tests.
var deleteProjectViaDaemon = daemon.DeleteProject

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
		if err != nil {
			return jsonError(err)
		}

		return jsonOut(map[string]any{
			"ok":             true,
			"project":        name,
			"repo_path":      req.RepoPath,
			"archived_count": resp.ArchivedCount,
			"killed_count":   resp.KilledCount,
		})
	},
}

// resolveProjectDeleteTarget turns the optional [repo] arg (default: cwd) into a
// DeleteProjectRequest and a friendly project name. It resolves the path to its
// canonical main-repo root when possible so a subdirectory still targets the
// whole project; a path that no longer resolves to a git repo falls back to the
// cleaned absolute path, so deleting a moved/removed project is still a clean
// daemon-side no-op (it archives nothing and sweeps any stale opt-in).
func resolveProjectDeleteTarget(args []string) (daemon.DeleteProjectRequest, string, error) {
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

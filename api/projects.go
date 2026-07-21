package api

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

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

var projectsRegisterCmd = &cobra.Command{
	Use:   "register <path>",
	Short: "Register a project with a stable local identity",
	Long: `Register a project directory with a stable, machine-local identity.

The returned project id survives an explicit rebind after the checkout moves.
Two clones remain separate projects. Any directory inside a checkout resolves
to that checkout's canonical main-repo root, and registration is idempotent for
the same checkout. Identity is anchored in agent-factory/checkout-id under the
Git common directory; no working-tree file is created.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		project, err := config.RegisterProject(args[0])
		if err != nil {
			return jsonError(err)
		}
		return jsonOut(project)
	},
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
	Short: "Archive and remove a project's sessions (reversibly)",
	Long: `Archive and remove every live session for a git repository.

This is archive-then-remove and reversible. Every live session of the repo is
archived (its tmux is torn down and its worktree moved to the archive dir, but
its branch and uncommitted changes are preserved), and its always-on root agent
(if any) is stopped and its root-agent opt-in removed. In-place sessions (the
root agent, 'af sessions create --here') are torn down instead of archived —
their cleanup never touches your working tree or branch.

The durable project registration, if any, is preserved. This command removes
session state; it does not unregister the project.

Your real git repository is never touched. To undo a mis-click, restore any
archived session with 'af sessions restore <title>'.

[repo] is a path inside the repository to delete (default: the current repo).
Deleting an unknown or already-empty project is a clean no-op. Prints how many
sessions were archived.`,
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

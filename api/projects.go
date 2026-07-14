package api

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
)

// ProjectsCmd is the top-level command group for project (repo-grouping)
// management (#1735). A "project" is derived: the set of sessions sharing a repo
// root. Today the only verb is delete; more can join it as the surface grows.
var ProjectsCmd = &cobra.Command{
	Use:   "projects",
	Short: "Manage projects (repo groupings of sessions)",
}

// deleteProjectViaDaemon is the daemon seam, overridable in tests.
var deleteProjectViaDaemon = daemon.DeleteProject

var projectsDeleteCmd = &cobra.Command{
	Use:   "delete [repo]",
	Short: "Delete a project: archive its sessions and remove it (reversibly)",
	Long: `Delete a project — the group of sessions sharing a git repository.

This is ARCHIVE-THEN-REMOVE and reversible. Every live session of the repo is
archived (its tmux is torn down and its worktree moved to the archive dir, but
its branch and uncommitted changes are preserved), the project drops out of the
active projects list, and its always-on root agent (if any) is stopped and its
root_agents opt-in removed. In-place sessions (the root agent, 'af sessions
create --here') are torn down instead of archived — their cleanup never touches
your working tree or branch.

Your real git repository is never touched. To undo a mis-click, restore any
archived session with 'af sessions restore <title>' — its project reappears.

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
	abs, err := filepath.Abs(path)
	if err != nil {
		return daemon.DeleteProjectRequest{}, "", fmt.Errorf("failed to resolve project path %q: %w", path, err)
	}
	if repo, err := config.RepoFromPath(abs); err == nil {
		return daemon.DeleteProjectRequest{RepoPath: repo.Root, RepoID: repo.ID}, filepath.Base(repo.Root), nil
	}
	cleaned := filepath.Clean(abs)
	return daemon.DeleteProjectRequest{RepoPath: cleaned}, filepath.Base(cleaned), nil
}

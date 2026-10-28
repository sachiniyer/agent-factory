package app

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui/overlay"
)

// Remote mode (W9.7). With --daemon-url / AF_DAEMON_URL / a saved target the
// projects, tasks and paths the TUI shows belong to the TARGET daemon's host.
// Everything in this file is the remote half of a read the local TUI does from
// this machine's registry, tasks.json, config and git; none of it ever falls
// back to a local source, because a local answer would describe the wrong
// machine.

// remoteReadTimeout bounds one remote registry/task read made from the poll or
// the event loop, so an unreachable target degrades the view instead of hanging it.
const remoteReadTimeout = 5 * time.Second

// remoteListProjects reads the target's project registry. A seam so the app
// tests can stub it without dialing a daemon.
var remoteListProjects = func() ([]config.Project, error) {
	c, err := apiclient.NewTargeted()
	if err != nil {
		return nil, err
	}
	defer c.CloseIdleConnections()
	return c.ListProjects()
}

// remoteListTasks reads every task on the target. A seam like remoteListProjects.
var remoteListTasks = func() ([]task.Task, error) {
	c, err := apiclient.NewTargeted()
	if err != nil {
		return nil, err
	}
	defer c.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), remoteReadTimeout)
	defer cancel()
	return c.ListTasks(ctx)
}

// tasksForScope is the TUI's task loader for the active project: the local
// tasks.json normally, the target's tasks in remote mode. An empty repoID is
// registry mode — no active scope — and yields no tasks either way.
func tasksForScope(repoRoot, repoID string) ([]task.Task, error) {
	if !isRemoteTarget() {
		return task.LoadTasksForKnownRepo(repoRoot, repoID)
	}
	if repoID == "" {
		return nil, nil
	}
	all, err := remoteListTasks()
	if err != nil {
		return nil, err
	}
	return filterTasksToProject(all, repoRoot, repoID), nil
}

// filterTasksToProject keeps the tasks owned by one project. The recorded
// RepoID wins when present, as in the local scope; otherwise the project path
// must equal the root EXACTLY, since resolving a path needs the daemon's
// filesystem and a task with neither binding belongs to no project's pane.
func filterTasksToProject(all []task.Task, repoRoot, repoID string) []task.Task {
	var out []task.Task
	for _, t := range all {
		switch {
		case t.RepoID != "":
			if t.RepoID == repoID {
				out = append(out, t)
			}
		case t.ProjectPath != "" && t.ProjectPath == repoRoot:
			out = append(out, t)
		}
	}
	return out
}

// remoteRepoContext is the project context for a registry row of the target.
// It is built from the row — never from a local git probe of a path that only
// exists on the daemon's host.
func remoteRepoContext(root, repoID string) *config.RepoContext {
	return &config.RepoContext{Root: root, IdentityRoot: root, ID: repoID}
}

// buildRemoteProjectList derives the switcher rows from the target's registry
// and the session snapshot it served. There is no root_agents union (that is a
// LOCAL config key) and no path probing: identity is the registry's own.
func (m *home) buildRemoteProjectList(data []session.InstanceData) ([]overlay.Project, bool) {
	registered, err := remoteListProjects()
	degraded := err != nil
	if err != nil {
		registered = nil
	}
	type aggregate struct {
		root           string
		count, inPlace int
	}
	byID := map[string]*aggregate{}
	roots := map[string]string{}
	ensure := func(id, root string) *aggregate {
		a := byID[id]
		if a == nil {
			a = &aggregate{root: root}
			byID[id] = a
		}
		return a
	}
	records := map[string]config.Project{}
	for _, p := range registered {
		if p.Root == "" {
			continue
		}
		id := config.ReconciledRepoIDForProject(p)
		ensure(id, filepath.Clean(p.Root))
		records[id] = p
		roots[pathutil.ResolveForCompare(filepath.Clean(p.Root))] = id
	}
	for _, d := range data {
		if session.IsArchivedData(d) || d.Worktree.RepoPath == "" {
			continue
		}
		root := filepath.Clean(d.Worktree.RepoPath)
		id, ok := roots[pathutil.ResolveForCompare(root)]
		if !ok {
			id = config.RepoIDFromRoot(root)
		}
		a := ensure(id, root)
		a.count++
		if d.Worktree.ExternalWorktree {
			a.inPlace++
		}
	}
	if m.repoID != "" && m.repoRoot != "" {
		ensure(m.repoID, m.repoRoot)
	}
	projects := make([]overlay.Project, 0, len(byID))
	for id, a := range byID {
		p := overlay.Project{
			RepoID: id, Name: filepath.Base(a.root), Root: a.root,
			SessionCount: a.count, InPlaceCount: a.inPlace,
		}
		if rec, ok := records[id]; ok {
			p.RegistryID, p.RegistryRoot, p.RegistryCheckoutID = rec.ID, rec.Root, rec.CheckoutID
			p.MissingPath = !rec.PathExists
		}
		projects = append(projects, p)
	}
	sort.Slice(projects, func(i, j int) bool {
		if projects[i].Name != projects[j].Name {
			return projects[i].Name < projects[j].Name
		}
		return projects[i].Root < projects[j].Root
	})
	return projects, degraded
}

// errRemoteHooksSave is the refusal for a hooks edit against a remote target.
// Hooks are written to the in-repo config of the project's checkout, which is on
// the daemon's host; writing m.repoRoot here would create a file in a directory
// of THIS machine that the target never reads.
func errRemoteHooksSave() error {
	return fmt.Errorf("post-worktree hooks live in the project's checkout on the daemon at %s — edit .agent-factory/config.toml there; the TUI will not write a copy on this machine", apiclient.RemoteTargetURL())
}

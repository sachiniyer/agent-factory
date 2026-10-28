package app

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/config"
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
var remoteReadTimeout = 5 * time.Second

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

// remoteReads is one poll's worth of the target's registry and tasks. It is
// fetched OFF the event loop (the snapshot poll's tea.Cmd, or the pre-loop
// launch) and delivered to the loop in snapshotFetchedMsg; nothing on the
// Update/View path ever dials the target for these.
type remoteReads struct {
	projects    []config.Project
	projectsErr error
	tasks       []task.Task
	tasksErr    error
}

// remoteCache is the loop-owned copy of the last remoteReads. A failed read
// keeps the previous rows (stale but responsive) and records the error, which
// the Projects section renders as a degraded registry.
type remoteCache struct {
	projects       []config.Project
	projectsErr    error
	projectsLoaded bool
	tasks          []task.Task
	tasksErr       error
	tasksLoaded    bool
}

func (c *remoteCache) apply(r *remoteReads) {
	if r == nil {
		return
	}
	c.projectsErr, c.tasksErr = r.projectsErr, r.tasksErr
	if r.projectsErr == nil {
		c.projects, c.projectsLoaded = r.projects, true
	}
	if r.tasksErr == nil {
		c.tasks, c.tasksLoaded = r.tasks, true
	}
}

// fetchRemoteReads reads the target's registry and tasks concurrently, each
// bounded by remoteReadTimeout. The client's own deadline is minutes, so the
// bound is enforced here: a target that accepts the connection and never
// answers costs one timeout per poll in the background goroutine, not a frozen
// TUI. Call it only off the event loop.
func fetchRemoteReads() *remoteReads {
	type projectsResult struct {
		p   []config.Project
		err error
	}
	type tasksResult struct {
		t   []task.Task
		err error
	}
	pc, tc := make(chan projectsResult, 1), make(chan tasksResult, 1)
	go func() { p, err := remoteListProjects(); pc <- projectsResult{p, err} }()
	go func() { t, err := remoteListTasks(); tc <- tasksResult{t, err} }()
	timeout := time.After(remoteReadTimeout)
	out := &remoteReads{}
	gotP, gotT := false, false
	for !gotP || !gotT {
		select {
		case r := <-pc:
			out.projects, out.projectsErr, gotP = r.p, r.err, true
		case r := <-tc:
			out.tasks, out.tasksErr, gotT = r.t, r.err, true
		case <-timeout:
			err := fmt.Errorf("the daemon at %s did not answer within %s", apiclient.RemoteTargetURL(), remoteReadTimeout)
			if !gotP {
				out.projectsErr = err
			}
			if !gotT {
				out.tasksErr = err
			}
			return out
		}
	}
	return out
}

// activeTasks is the TUI's task loader for the active project on the EVENT LOOP:
// the local tasks.json normally; in remote mode the poll's cached copy of the
// target's tasks, filtered to the project (no network here). An empty repoID is
// registry mode — no active scope — and yields no tasks either way.
func (m *home) activeTasks(repoRoot, repoID string) ([]task.Task, error) {
	if !isRemoteTarget() {
		return task.LoadTasksForKnownRepo(repoRoot, repoID)
	}
	if repoID == "" {
		return nil, nil
	}
	if !m.remote.tasksLoaded {
		return nil, m.remote.tasksErr
	}
	return filterTasksToProject(m.remote.tasks, repoRoot, repoID), nil
}

// repoContextForSelection resolves a chosen project row to a RepoContext — the
// one place every selection entry point (picker, sidebar Enter, mouse, rebind
// follow-up) goes through. Remotely the row's root is a path on the daemon's
// host, so the context comes from the row's registry identity, never from a
// local git probe.
func (m *home) repoContextForSelection(root, repoID string) (*config.RepoContext, error) {
	if !isRemoteTarget() {
		return config.RepoFromPath(root)
	}
	if repoID == "" {
		for _, p := range m.sidebarProjects {
			if p.Root == root {
				repoID = p.RepoID
				break
			}
		}
	}
	if repoID == "" {
		return nil, fmt.Errorf("%s is not in the daemon's project list yet; wait for the next refresh", root)
	}
	return remoteRepoContext(root, repoID), nil
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
	// The cached registry, never a dial: this runs on the event loop. A failed
	// refresh leaves the last good rows and reports the registry as degraded.
	registered := m.remote.projects
	degraded := m.remote.projectsErr != nil || !m.remote.projectsLoaded
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
		roots[filepath.Clean(p.Root)] = id
	}
	for _, d := range data {
		if session.IsArchivedData(d) || d.Worktree.RepoPath == "" {
			continue
		}
		root := filepath.Clean(d.Worktree.RepoPath)
		id, ok := roots[root]
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

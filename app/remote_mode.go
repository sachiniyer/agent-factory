package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
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

// remoteListProjects reads the target's project registry, abandoning the read
// when ctx ends. A seam so the app tests can stub it without dialing a daemon.
var remoteListProjects = func(ctx context.Context) ([]config.Project, error) {
	c, err := apiclient.NewTargeted()
	if err != nil {
		return nil, err
	}
	defer c.CloseIdleConnections()
	return c.ListProjectsContext(ctx)
}

// remoteListTasks reads every task on the target, abandoning the read when ctx
// ends. A seam like remoteListProjects.
var remoteListTasks = func(ctx context.Context) ([]task.Task, error) {
	c, err := apiclient.NewTargeted()
	if err != nil {
		return nil, err
	}
	defer c.CloseIdleConnections()
	return c.ListTasks(ctx)
}

// remoteTargetLabel names the target the user configured, so a message about a
// silent daemon says which one. It falls back to a generic noun rather than
// printing "the daemon at " with nothing after it.
func remoteTargetLabel() string {
	if url := apiclient.RemoteTargetURL(); url != "" {
		return "the daemon at " + url
	}
	return "the target daemon"
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
	// seq counts acknowledged (or possibly-landed) task mutations. A poll captures
	// it when it STARTS and its task list is applied only if it is unchanged when
	// the result lands: a read that began before a mutation was acknowledged can
	// carry the state from before it, and applying that would resurrect a deleted
	// row, undo an edit, or hide a create until the next poll. A poll that began
	// after the acknowledgement reads a daemon that already holds the write.
	seq uint64
}

// apply installs one poll's reads. The project registry always applies; the
// task list applies only when no task mutation was acknowledged since the poll
// began (readSeq is the seq captured at its start).
func (c *remoteCache) apply(r *remoteReads, readSeq uint64) {
	if r == nil {
		return
	}
	c.projectsErr = r.projectsErr
	if r.projectsErr == nil {
		c.projects, c.projectsLoaded = r.projects, true
	}
	if readSeq != c.seq {
		return // older than an acknowledged mutation: keep the newer cache
	}
	c.tasksErr = r.tasksErr
	if r.tasksErr == nil {
		c.tasks, c.tasksLoaded = r.tasks, true
	}
}

// taskRowKey is a task row's identity in the cache: its project binding AND its
// ID. A task ID is the store's key, but the store tolerates legacy duplicate IDs
// (two rows, often in different projects), and a mutation acts on exactly one of
// them — so a cache keyed on the ID alone would rewrite, or delete, the rows it
// never touched (#5900). project is the row's ProjectPath, the same binding a
// mutation's ProjectExpectation pins.
type taskRowKey struct{ project, id string }

func rowKeyOf(t task.Task) taskRowKey { return taskRowKey{project: t.ProjectPath, id: t.ID} }

// indexOfRow is the first cached row with key k, or -1. First, because the store
// updates and removes the first row carrying an ID.
func (c *remoteCache) indexOfRow(k taskRowKey) int {
	for i, t := range c.tasks {
		if rowKeyOf(t) == k {
			return i
		}
	}
	return -1
}

// noteTaskStored records a create or update the daemon acknowledged: rec is the
// stored record (or the caller's best reconstruction) and prev names the row the
// mutation targeted — rec's own key for a create. Only that row is replaced; a
// row that shares the ID in another project is left exactly as it was. The cache
// is replaced at once, so the reload that follows the mutation sees the mutation.
func (c *remoteCache) noteTaskStored(prev taskRowKey, rec task.Task) {
	c.seq++
	next := append([]task.Task(nil), c.tasks...)
	if i := c.indexOfRow(prev); i >= 0 {
		next[i] = rec
	} else {
		next = append(next, rec)
	}
	c.tasks = next
}

// noteTaskRemoved records a delete the daemon acknowledged: the one row k.
func (c *remoteCache) noteTaskRemoved(k taskRowKey) {
	c.seq++
	i := c.indexOfRow(k)
	if i < 0 {
		return
	}
	next := append([]task.Task(nil), c.tasks[:i]...)
	c.tasks = append(next, c.tasks[i+1:]...)
}

// noteTaskMutationUncertain is for a mutation that may or may not have landed:
// the cache cannot be advanced, but every poll already in flight is now older
// than the write it may have missed, so none of them may overwrite what the next
// poll will read.
func (c *remoteCache) noteTaskMutationUncertain() { c.seq++ }

// fetchRemoteReads reads the target's registry and tasks concurrently under one
// deadline of remoteReadTimeout, derived from parent (the model's lifetime
// context). It touches no model state: both reads write only their own locals
// and the function RETURNS the result for Update to apply. It does not return
// until both reads have, and both are cancelled by the deadline or by parent, so
// no goroutine outlives the call — or, through parent, the model that owns it.
// Call it only off the event loop.
func fetchRemoteReads(parent context.Context) *remoteReads {
	// Read the seams on the caller's goroutine, not inside the workers.
	listProjects, listTasks := remoteListProjects, remoteListTasks
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, remoteReadTimeout)
	defer cancel()
	out := &remoteReads{}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); out.projects, out.projectsErr = listProjects(ctx) }()
	go func() { defer wg.Done(); out.tasks, out.tasksErr = listTasks(ctx) }()
	wg.Wait()
	if err := ctx.Err(); err != nil {
		unanswered := fmt.Errorf("%s did not answer within %s", remoteTargetLabel(), remoteReadTimeout)
		if out.projectsErr != nil {
			out.projectsErr = unanswered
		}
		if out.tasksErr != nil {
			out.tasksErr = unanswered
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
		if m.remote.tasksErr != nil {
			return nil, m.remote.tasksErr
		}
		return nil, errors.New("the task list has not been read from the daemon yet")
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

// taskMutationAck carries the record the daemon returned for the LAST task
// create/update, from the seam to the call site that acknowledges it. The seams
// keep their error-only signatures (the app tests stub them), and — like the
// seams — it is read and written only on the event loop. nil means the daemon
// did not say (an older daemon, or a committed-with-warning reply); the caller
// then advances the cache from what it sent instead.
type taskMutationAck struct{ record *task.Task }

var taskAck taskMutationAck

// The note* hooks run right after each task mutation's daemon call and advance
// the remote cache from the mutation's OWN result (W9.7 follow-up). In local mode
// they do nothing: tasks.json is re-read from disk, which already holds the
// write. A definitive refusal changes nothing; a committed or successful write
// updates the cache now; an outcome that may have landed only poisons the polls
// already in flight.

func (m *home) noteRemoteCreate(sent task.Task, err error) {
	if !isRemoteTarget() {
		return
	}
	ack := taskAck.record
	taskAck = taskMutationAck{}
	switch {
	case err == nil || apiclient.IsMutationCommitted(err):
		rec := sent
		if ack != nil {
			rec = *ack
		}
		m.remote.noteTaskStored(rowKeyOf(rec), rec)
	case mutationMayHaveLanded(err):
		m.remote.noteTaskMutationUncertain()
	}
}

// noteRemoteUpdate advances the cache after an edit. expect is the project the
// pane displayed the task in, which with the ID names the row the daemon edited.
func (m *home) noteRemoteUpdate(id string, update task.TaskUpdate, expect task.ProjectExpectation, err error) {
	if !isRemoteTarget() {
		return
	}
	ack := taskAck.record
	taskAck = taskMutationAck{}
	switch {
	case err == nil || apiclient.IsMutationCommitted(err):
		prev, found := m.remote.targetRow(id, expect)
		if ack != nil {
			if !found {
				prev = rowKeyOf(*ack)
			}
			m.remote.noteTaskStored(prev, *ack)
			return
		}
		// The daemon did not return the merged record (a committed-with-warning
		// reply carries no body): advance the cached row by the patch it accepted.
		if !found {
			m.remote.noteTaskMutationUncertain()
			return
		}
		cached := m.remote.tasks[m.remote.indexOfRow(prev)]
		// The patch changes the definition, but the schedule-health fields on the
		// cached row (armed state, next fire, lateness) were observed for the OLD
		// definition. Carrying them forward would show live times for a cron that
		// is no longer the task's, so they are marked unknown until a poll that
		// began after this write observes the new one (#5899).
		m.remote.noteTaskStored(prev, update.ApplyTo(cached).WithScheduleHealthUnknown())
	case mutationMayHaveLanded(err):
		m.remote.noteTaskMutationUncertain()
	}
}

func (m *home) noteRemoteRemove(tsk task.Task, err error) {
	if !isRemoteTarget() {
		return
	}
	switch {
	case err == nil || apiclient.IsMutationCommitted(err):
		m.remote.noteTaskRemoved(rowKeyOf(tsk))
	case mutationMayHaveLanded(err):
		m.remote.noteTaskMutationUncertain()
	}
}

// targetRow is the cached row an id+expectation addresses: the first row with the
// ID, in the expected project when the expectation pins one.
func (c *remoteCache) targetRow(id string, expect task.ProjectExpectation) (taskRowKey, bool) {
	for _, t := range c.tasks {
		if t.ID == id && (!expect.Enforce || t.ProjectPath == expect.ProjectPath) {
			return rowKeyOf(t), true
		}
	}
	return taskRowKey{}, false
}

// remoteHooksReadOnlyReason is shown in the hooks pane for a remote target, so the
// edit keys are inert instead of producing an edit no save can ever commit.
func remoteHooksReadOnlyReason() string {
	return "Hooks live in the project's checkout on the daemon host — edit .agent-factory/config.toml there."
}

// discardUnsavedPaneEdits drops every unsaved edit the content panes hold —
// hooks back to what is stored, the task pane back to the list as last read — so
// a save that cannot succeed never traps the user in the TUI.
func (m *home) discardUnsavedPaneEdits() {
	m.hooksPane.Discard()
	sp := m.automations.TaskPane()
	sp.ResetTasks(m.store.GetTasks())
}

// takePendingPaneNotice drains the one-shot notice a save left for the user.
func (m *home) takePendingPaneNotice() string {
	n := m.pendingPaneNotice
	m.pendingPaneNotice = ""
	return n
}

package api

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// This file holds the ONE project-context contract the whole CLI resolves
// against (#1893). Before it, each command invented its own rule: `sessions
// get` scoped to the cwd's repo, `tasks list` listed every project's tasks, and
// `tasks get/update/remove/trigger` accepted --repo and silently discarded it —
// so `af tasks remove --repo /a <id-owned-by-b>` deleted b's task and reported
// {"ok":true}. That drift is what let a DLQ automation be created and managed
// outside its intended project (#1891).
//
// The contract, matching the TUI and web (both project-scoped):
//
//  1. An explicit --repo always wins, and names the project.
//  2. Otherwise the cwd's git repository is the project. Linked worktrees
//     resolve to the main root (config.CurrentRepo), so an agent working inside
//     a session's worktree still resolves to the real project.
//  3. Otherwise there is no project context, and behavior depends on what the
//     command needs:
//     - Commands that BIND a new project (sessions create, tasks add) fail with
//     an actionable "--repo is required" rather than guessing.
//     - Listing spans every project, because breadth is honest here, not a
//     guess. `--all` asks for that breadth explicitly from inside a repo.
//     - A command taking a handle (session title, task id) resolves it across
//     projects, but refuses to pick when the handle is held by several — the
//     #1814 ambiguity rule, extended to tasks by this change.
//
// Rule 3 is what keeps `af` usable from a systemd unit or a CI step, where
// there is no cwd repo; it never guesses, because "unique across all projects"
// is deterministic in a way "first match" was not.
//
// Remote targets (--daemon-url) are deliberately exempt from rule 2 wherever a
// command's request actually reaches the remote: the client's cwd names a repo
// on THIS machine, which says nothing about the daemon's projects. That
// exemption lives in resolveRepoIDForLookup (api.go), NOT here — the dividing
// line is the transport, not the command.
//
// resolveProjectScope below therefore always consults the cwd, which is correct
// only because its callers are the task commands, and every task RPC goes over
// the LOCAL control socket regardless of --daemon-url (daemon.ListTasksNoSpawn,
// AddTask, …). If a task command is ever migrated onto the apiclient transport,
// it must take resolveRepoIDForLookup's remote branch too, or a bare id against
// a remote daemon will be scoped by a repo the remote has never heard of.

// projectScope is the resolved project context for one command invocation. A
// nil Repo means "no project context" (rule 3): unscoped, not "every project by
// default" — the distinction matters because only listing is allowed to widen
// an absent scope into all-projects breadth.
type projectScope struct {
	Repo *config.RepoContext // nil = no project context
	All  bool                // --all: span every project, explicitly
}

// resolveProjectScope applies rules 1-3 for the commands that can be scoped but
// do not require a project. allFlag is the command's --all, if it has one;
// commands without one pass false.
//
// --repo and --all are mutually exclusive: --repo names one project and --all
// asks for all of them, so passing both is a contradiction rather than a
// precedence puzzle. This mirrors sessions send-prompt's --repo/--all-repos
// rule instead of inventing a second convention.
func resolveProjectScope(allFlag bool) (projectScope, error) {
	if allFlag && repoFlag != "" {
		return projectScope{}, fmt.Errorf("--repo and --all are mutually exclusive: --repo names one project, --all spans every project")
	}
	if allFlag {
		return projectScope{All: true}, nil
	}
	if repoFlag != "" {
		// A provided-but-invalid --repo must name the path it could not
		// resolve rather than silently falling back to the cwd (#892).
		repo, err := repoFromFlag()
		if err != nil {
			return projectScope{}, err
		}
		return projectScope{Repo: repo}, nil
	}
	repo, err := config.CurrentRepo()
	if err != nil {
		return projectScope{}, nil // rule 3: no project context
	}
	return projectScope{Repo: repo}, nil
}

// scopeMatches reports whether a project path belongs to this scope.
//
// It compares repo IDENTITY, not path strings. The CLI stores a task's
// ProjectPath as the git main-worktree root (config.CurrentRepo), but the TUI
// stores whatever absolute path the user typed (app/home_tasks.go,
// ui/task_pane_edit.go) — a subdirectory or a linked worktree both round-trip
// to the same project but never string-match its root. String equality would
// therefore hide TUI-created tasks from `af tasks list` in their own project,
// which is the opposite of what this change is for.
//
// A path that no longer resolves as a repo (deleted project, stray clone) falls
// back to an ID derived from the cleaned path, which reduces to path equality —
// so an orphaned task is still addressable in its recorded project rather than
// becoming invisible.
//
// An EMPTY project path is treated as unbound and matches every scope. No
// supported path creates one (the CLI stores repo.Root, the TUI an absolute
// path) and the daemon refuses to run one — taskrun.go rejects a ProjectPath
// that is not a git repo — so it only arises from a hand-edited tasks.json.
// Scoping such a task OUT would strand it: hidden from every project's list and
// refused by remove, leaving no way to clean it up. Matching everywhere keeps it
// visible and deletable, and there is no binding for a scope to protect.
func (s projectScope) matchesTask(t *task.Task, ids *projectIDCache) bool {
	if s.All || s.Repo == nil {
		return true // no project context: nothing to filter against
	}
	// The RETAINED id wins when present: it was resolved at bind time, while the
	// recorded path was known to resolve, so it survives that path being deleted
	// or moved. Re-deriving would be strictly worse information.
	if t.RepoID != "" {
		return t.RepoID == s.Repo.ID
	}
	if strings.TrimSpace(t.ProjectPath) == "" {
		return true // unbound: nothing to violate, and must stay reachable
	}
	return ids.idFor(t.ProjectPath) == s.Repo.ID
}

// resolvedProject is a project path resolved to its owning repository. root is
// "" when nothing could be resolved, which is what makes an invented identity
// distinguishable from a real one.
type resolvedProject struct {
	id   string
	root string
}

// projectIDCache memoizes path→repo resolution for one command invocation.
// config.RepoFromPath shells out to git, and a scoped list resolves every
// task's project, so without this a list of N tasks costs N git invocations —
// most of them for the same handful of paths.
type projectIDCache struct {
	resolved map[string]resolvedProject
}

func newProjectIDCache() *projectIDCache {
	return &projectIDCache{resolved: map[string]resolvedProject{}}
}

func (c *projectIDCache) idFor(projectPath string) string {
	return c.resolve(projectPath).id
}

// resolve maps a recorded project path to its owning repository.
//
// An EXISTING path — including a subdirectory or a linked worktree — resolves
// through git to the main repo root, which is why identity matching (rather than
// path-string equality) sees TUI-created tasks in their own project.
//
// A path that no longer exists is the hard case. Hashing the stale leaf, as this
// used to, invents an ID that equals nothing: the surviving project's ID is
// sha256 of ITS root, never of a deleted child. That stranded the task — hidden
// from its project's list, refused by every id-taking command, and the --repo we
// suggested could not resolve either. So walk up to the nearest ANCESTOR that
// still resolves: a deleted subdirectory of a surviving project resolves back to
// that project, which is the reported case (#1893 review).
//
// The walk answers what git itself would say about the path if it existed, so it
// cannot be more wrong than the path is. When nothing up the chain resolves,
// fall back to the leaf hash — path equality, which at least keeps an orphan
// addressable in its own recorded path — and report root "" so callers can tell
// this identity is derived rather than real.
//
// Retaining the id at bind time (task.Task.RepoID) is the durable fix and takes
// precedence; this path is the fallback for rows written before that field, and
// for paths that never resolved.
func (c *projectIDCache) resolve(projectPath string) resolvedProject {
	if got, ok := c.resolved[projectPath]; ok {
		return got
	}
	got := resolveProjectPath(projectPath)
	c.resolved[projectPath] = got
	return got
}

func resolveProjectPath(projectPath string) resolvedProject {
	if repo, err := config.RepoFromPath(projectPath); err == nil {
		return resolvedProject{id: repo.ID, root: repo.Root}
	}
	cleaned := filepath.Clean(projectPath)
	for dir := filepath.Dir(cleaned); ; dir = filepath.Dir(dir) {
		if repo, err := config.RepoFromPath(dir); err == nil {
			return resolvedProject{id: repo.ID, root: repo.Root}
		}
		if parent := filepath.Dir(dir); parent == dir {
			break // reached the filesystem root
		}
	}
	return resolvedProject{id: config.RepoIDFromRoot(cleaned)}
}

// sessionRepoRoot derives the root of the project a session belongs to FROM THE
// SESSION'S OWN RECORD, mirroring Storage's root→repoID derivation (#667): the
// worktree's RepoPath is the canonical root (sessions create stores the
// git-resolved repo.Root there), and Path is the fallback for worktree-less
// rows (remote backends). Returns "" when neither is known.
//
// Shared by `archive --self` and `whoami` so the two cannot drift. Hashing
// data.Path directly is the trap this exists to prevent: Path is stored as
// entered and may never have been git-resolved, so RepoIDFromRoot(data.Path)
// can differ from the canonical ID of the very same project.
func sessionRepoRoot(data *session.InstanceData) string {
	if data.Worktree.RepoPath != "" {
		return data.Worktree.RepoPath
	}
	return data.Path
}

// requireTaskInScope enforces the contract on a task command that takes an id.
//
// Task ids are globally unique, so this is not an ambiguity guard — it is a
// blast-radius guard. Without it, an id is a capability to mutate ANY project's
// automation from anywhere, which is exactly how the #1891 DLQ task was managed
// from outside its project. With no project context (rule 3) the id still
// resolves, matching the bare-title convenience sessions already grant.
//
// The error names the owning project AND the --repo that would authorize the
// action, so the fix is copy-pasteable rather than a hunt.
func requireTaskInScope(t *task.Task, scope projectScope) error {
	ids := newProjectIDCache()
	if scope.matchesTask(t, ids) {
		return nil
	}
	// Suggest a --repo that RESOLVES. The recorded path may be a subdirectory
	// that no longer exists, and telling a user to pass a path we would reject
	// is worse than not suggesting one: it reads as a fix and cannot work.
	suggest := ids.resolve(t.ProjectPath).root
	if suggest == "" {
		suggest = t.ProjectPath
	}
	return fmt.Errorf("task %q belongs to project %s, not the current project %s — pass --repo %s to act on it",
		t.ID, t.ProjectPath, scope.Repo.Root, suggest)
}

// guardProjectBinding refuses to BIND a new session or task to a project that
// was derived from the cwd and turns out to live inside af's own home (#1891).
//
// af's home holds af's state, never a user's project. A git repo whose MAIN
// root resolves inside it is a stray full clone — the #1891 DLQ agent cloned
// into $AGENT_FACTORY_HOME/runtime/detail-dlq-monitor and ran `af tasks add`
// there, binding every watcher-created worktree to the clone instead of the
// intended project, and leaving the automation invisible from that project's
// view.
//
// This deliberately keys off the RESOLVED repo root, not the cwd. Sessions run
// in linked worktrees under af's home (worktrees/, archived/) and those resolve
// back to the real project root (config.CurrentRepo), so agents working inside
// a normal session never trip this — only a self-contained clone does, which is
// precisely the accident being guarded.
//
// An explicit --repo is the escape hatch: a caller who names the path has
// stated the binding rather than inherited it, so legitimate uses stay open.
func guardProjectBinding(repo *config.RepoContext, explicit bool) error {
	if explicit {
		return nil
	}
	home, err := config.GetConfigDir()
	if err != nil {
		return nil // cannot tell; never block on an unrelated failure
	}
	if !pathIsInside(home, repo.Root) {
		return nil
	}
	return fmt.Errorf("--repo is required here: the current directory resolves to the git repository %s, which is inside af's home (%s) — that is a stray clone, not a project, and binding to it hides the automation from the intended project's view. Pass --repo <project path> to name the project explicitly",
		repo.Root, home)
}

// pathIsInside reports whether child is parent or lives beneath it, comparing
// symlink-resolved paths so a symlinked AGENT_FACTORY_HOME (or a macOS /tmp
// → /private/tmp) is not read as a different tree. Resolution is best-effort:
// an unresolvable path falls back to its cleaned form rather than failing, since
// this only decides whether to ask for an explicit --repo.
func pathIsInside(parent, child string) bool {
	p := resolveRealPath(parent)
	c := resolveRealPath(child)
	rel, err := filepath.Rel(p, c)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

func resolveRealPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

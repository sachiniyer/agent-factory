package api

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
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
//     - A command taking a handle (session title, task id or name) resolves it
//     across projects, but refuses to pick when the handle is held by several —
//     the #1814 ambiguity rule, extended to tasks by this change and to task
//     names by #4676.
//
// Rule 3 is what keeps `af` usable from a systemd unit or a CI step, where
// there is no cwd repo; it never guesses, because "unique across all projects"
// is deterministic in a way "first match" was not.
//
// Remote targets (--daemon-url) are deliberately exempt from rule 2 wherever a
// command's request actually reaches the remote: the client's cwd names a repo
// on THIS machine, which says nothing about the daemon's projects. That
// exemption lives in resolveRepoIDForLookup for the session reads, and in
// resolveProjectScope below for the task verbs — the dividing line is the
// transport, not the command, and #3730 moved the whole `af tasks` group across
// it.
//
// The task verbs go one step further than the session reads: against a remote
// target they refuse --repo instead of honouring it. A session lookup can send
// its repo ID to the daemon and let the daemon decide, with the documented
// limitation that the ID only matches when both hosts have the project at the
// same absolute path. A task scope has no such out — the daemon serves no
// repo-filtered task read, so the filtering happens HERE, against records the
// remote sent, using an identity derived by hashing a path on THIS machine
// (config.RepoIDFromRoot). When the two hosts disagree about the path, that
// filter matches nothing and `af tasks list --daemon-url … --repo …` prints an
// empty list for a project that has tasks. A silently empty answer about the
// wrong machine is the #3730 shape exactly, so it is refused rather than
// rendered. Scoping a remote by a project the daemon owns needs a daemon-side
// lookup, which is additive when it exists.

// repoFromFlag resolves the --repo flag to a RepoContext. Its errors name the
// offending path and distinguish "could not make the path absolute" from "the
// path is not a git repository" so callers never mislabel a provided-but-invalid
// --repo as missing (#892). Only call when repoFlag != "".
func repoFromFlag() (*config.RepoContext, error) {
	absPath, err := config.ResolveUserPath(repoFlag)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve --repo path %q: %w", repoFlag, err)
	}
	repo, err := config.RepoFromPath(absPath)
	if err != nil {
		if config.RepoProbeUnanswered(err) {
			return nil, fmt.Errorf("%s — retry, or pass a different --repo: %w", config.RepoProbeUnansweredClaim("--repo", absPath), err)
		}
		return nil, fmt.Errorf("--repo %q is not a valid git repository: %w", absPath, err)
	}
	return repo, nil
}

// optionalCurrentRepo resolves the cwd's project when one exists. Only the
// positively identified outside-Git condition means "no project context";
// treating any other discovery failure as absence would silently widen scoped
// reads and mutations to every project (#3134).
func optionalCurrentRepo() (*config.RepoContext, error) {
	repo, err := config.CurrentRepo()
	if err == nil {
		return repo, nil
	}
	if errors.Is(err, config.ErrNotGitRepository) {
		return nil, nil
	}
	return nil, fmt.Errorf("resolve current repository: %w", err)
}

// resolveRepoID resolves a repo ID from flags, cwd, or returns "" for all-repo mode.
//
// This ALWAYS consults the cwd, including when --daemon-url is set. That looks
// wrong for a remote target — the client's cwd names a repo on this machine —
// but its callers are the write commands (kill/archive/restore/send-prompt, tab
// mutations) whose transport does NOT honor --daemon-url: daemon.* goes over the
// local control socket (callDaemon → DaemonSocketPath). Dropping the cwd scope
// for them would send an UNSCOPED destructive request to the LOCAL daemon, which
// could then resolve a same-titled session in a different local repo and kill or
// archive it. Keeping the cwd scope keeps those commands pointed where they
// already point.
//
// Reads that genuinely reach the targeted daemon (sessions list/get/watch/preview)
// use resolveRepoIDForLookup instead. If a write command is ever migrated onto the
// apiclient transport, move it across too.
func resolveRepoID() (string, error) {
	if repoFlag != "" {
		repo, err := repoFromFlag()
		if err != nil {
			return "", err
		}
		return repo.ID, nil
	}
	// Try cwd
	repo, err := optionalCurrentRepo()
	if err != nil {
		return "", err
	}
	if repo == nil {
		return "", nil // all-repo mode
	}
	return repo.ID, nil
}

// resolveRepoIDForLookup resolves the repo scope for a READ that is actually
// served by the targeted daemon: the snapshot-based reads (`sessions list`,
// `get`, `watch`) and `preview`, all of which route through apiclient and so
// follow --daemon-url/AF_DAEMON_URL to the remote.
//
// The dividing line is the TRANSPORT, not the command: a caller belongs here if
// its request reaches the targeted daemon, and on resolveRepoID if it goes over
// the local control socket regardless of the target.
//
// It differs from resolveRepoID in one way: against a REMOTE target the cwd is
// ignored. The client's cwd names a repo that exists HERE, not on the daemon's
// machine, so scoping by it asks the remote for a repo ID it has never seen —
// and the remote read path has no disk fallback (snapshotRead), so a bare-title
// lookup that used to succeed would report a spurious not-found. Against a
// remote only an EXPLICIT --repo scopes; a bare title resolves across the
// remote's repos, with the ambiguity guard refusing to pick between them.
//
// Deliberately NOT shared with the write commands: their transport is the local
// control socket regardless of --daemon-url, so an unscoped request there is a
// destructive mis-target rather than a remote lookup (see resolveRepoID).
//
// Known limitation: --repo becomes an ID by hashing the path on THIS machine
// (config.RepoIDFromRoot), so against a remote it only disambiguates when the
// daemon has that project checked out at the same absolute path. Scoping a
// remote by a repo identity the daemon owns needs a daemon-side repo lookup — a
// separate change; until then, prefer a bare title against a remote.
func resolveRepoIDForLookup() (string, error) {
	if repoFlag != "" {
		repo, err := repoFromFlag()
		if err != nil {
			return "", err
		}
		return repo.ID, nil
	}
	if apiclient.IsRemoteTarget() {
		return "", nil // the client's cwd says nothing about the remote's repos
	}
	repo, err := optionalCurrentRepo()
	if err != nil {
		return "", err
	}
	if repo == nil {
		return "", nil // all-repo mode, guarded by the ambiguity check
	}
	return repo.ID, nil
}

// resolveRepo is the single binding resolver for commands that can create
// persistent project state (sessions create, tasks add, and send-prompt
// --create). Besides resolving the repo, it enforces the shared AF-home refusal;
// putting that invariant here keeps a new caller from remembering resolution
// while forgetting the destructive binding guard (#1891/#2205).
//
// Errors are fully formed for callers to surface directly: a provided `--repo`
// that does not resolve names the path, while an absent `--repo` whose cwd is
// also not a repo reports that `--repo is required` (#892). Wrapping every
// failure as "--repo is required" would be wrong when the user did provide it.
func resolveRepo() (*config.RepoContext, error) {
	var (
		repo *config.RepoContext
		err  error
	)
	if repoFlag != "" {
		repo, err = repoFromFlag()
	} else {
		repo, err = config.CurrentRepo()
		if err != nil {
			if config.RepoProbeUnanswered(err) {
				// Name the directory the probe actually ran in: a user reading
				// this may not be where they think they are, and the claim
				// helper wants a path rather than a pronoun.
				cwd, cwdErr := os.Getwd()
				if cwdErr != nil {
					cwd = "."
				}
				return nil, fmt.Errorf("--repo is required: %s — retry, or pass --repo <path> to target a project (%w)", config.RepoProbeUnansweredClaim("the current directory", cwd), err)
			}
			return nil, fmt.Errorf("--repo is required: the current directory is not a git repository — pass --repo <path> to target a project (%w)", err)
		}
	}
	if err != nil {
		return nil, err
	}
	if err := guardProjectBinding(repo, repoFlag != ""); err != nil {
		return nil, err
	}
	return repo, nil
}

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
	if apiclient.IsRemoteTarget() {
		if repoFlag != "" {
			return projectScope{}, fmt.Errorf(
				"--repo cannot scope tasks on the daemon at %s: a project's identity is derived from its path on THIS machine, "+
					"so filtering the daemon's tasks by it would silently match nothing when the two hosts hold the project at "+
					"different paths. Drop --repo to act across the daemon's projects (task ids are unique), or run the command on the daemon host",
				apiclient.RemoteTargetURL())
		}
		// No project context (rule 3), for the reason resolveRepoIDForLookup gives:
		// the client's cwd names a repository here, not on the daemon's machine. An
		// id therefore resolves across the remote's projects, exactly as it does
		// from outside a git repository locally.
		return projectScope{}, nil
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
	repo, err := optionalCurrentRepo()
	if err != nil {
		return projectScope{}, err
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

// projectIDCache memoizes path→repo resolution for one command invocation.
// config.ResolveProjectPath shells out to git, and a scoped list resolves every
// task's project, so without this a list of N tasks costs N git invocations —
// most of them for the same handful of paths.
//
// The resolution rule itself lives in config.ResolveProjectPath, shared with the
// TUI task pane's repo filter (#2098). Retaining the id at bind time
// (task.Task.RepoID) is the durable fix and takes precedence; resolution is the
// fallback for rows written before that field, and for paths that never
// resolved.
type projectIDCache struct {
	resolved map[string]config.ResolvedProject
}

func newProjectIDCache() *projectIDCache {
	return &projectIDCache{resolved: map[string]config.ResolvedProject{}}
}

func (c *projectIDCache) idFor(projectPath string) string {
	return c.resolve(projectPath).ID
}

func (c *projectIDCache) resolve(projectPath string) config.ResolvedProject {
	if got, ok := c.resolved[projectPath]; ok {
		return got
	}
	got := config.ResolveProjectPath(projectPath)
	c.resolved[projectPath] = got
	return got
}

// sessionRepoRoot returns the record's preferred project spelling for
// diagnostics; identity decisions belong in sessionRepoID below.
func sessionRepoRoot(data *session.InstanceData) string {
	if data.Worktree.RepoPath != "" {
		return data.Worktree.RepoPath
	}
	return data.Path
}

// sessionRepoID derives project identity FROM THE SESSION'S OWN RECORD. A
// worktree's RepoPath is already the authoritative identity root (sessions
// create stores RepoContext.IdentityPath there), so resolving it through Git
// would let a surviving directory from a deleted nested repo adopt an unrelated
// ancestor. Worktree-less remote rows retain the historical Path resolution.
//
// Shared by `archive --self` and `whoami` so the two cannot drift.
func sessionRepoID(data *session.InstanceData) string {
	if data.Worktree.RepoPath != "" {
		// Canonical role (#3530): RepoPath is already the identity root, and
		// this must stay bit-identical to session storage's key or scoping
		// stops finding the rows it filters.
		return config.RepoIDFromRoot(filepath.Clean(data.Worktree.RepoPath))
	}
	if data.Path != "" {
		return session.RepoIDForStoragePath(data.Path)
	}
	return ""
}

// requireTaskInScope enforces the contract on a task command that takes an id
// or a name (#4676): both resolve through resolveTaskArg to the same check.
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
	suggest := ids.resolve(t.ProjectPath).Root
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
// This deliberately keys off the RESOLVED repo identity, not the cwd or
// operational workspace. Sessions run in linked worktrees under af's home
// (worktrees/, archived/), and a bare repository has no main checkout to put in
// Root, so IdentityPath is the one value that distinguishes those legitimate
// workspaces from a self-contained clone under the home.
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
	identity := repo.IdentityPath()
	if !pathIsInside(home, identity) {
		return nil
	}
	return fmt.Errorf("--repo is required here: the current directory resolves to the git repository %s, which is inside af's home (%s) — that is a stray clone, not a project, and binding to it hides the automation from the intended project's view. Pass --repo <project path> to name the project explicitly",
		identity, home)
}

// pathIsInside reports whether child is parent or lives beneath it, comparing
// symlink-resolved paths so a symlinked AGENT_FACTORY_HOME (or a macOS /tmp
// → /private/tmp) is not read as a different tree. Resolution is best-effort:
// an unresolvable path falls back to its cleaned form rather than failing, since
// this only decides whether to ask for an explicit --repo.
func pathIsInside(parent, child string) bool {
	p := resolveRealPath(parent)
	c := resolveRealPath(child)
	return pathutil.IsAtOrInside(c, p)
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

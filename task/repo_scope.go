package task

import (
	"strings"
	"sync"

	"github.com/sachiniyer/agent-factory/config"
)

// This file holds the repo filter behind the TUI task pane (#2098).
//
// A task records the project path it was BOUND to, which is not necessarily the
// project's root: the TUI stores the absolute path the user typed or defaulted
// to, so creating a task from `repo/some/subdir` records that subdirectory. git
// resolves any subdirectory — and any linked worktree — back to the same
// top-level, but string comparison of raw paths does not, so raw equality hid
// those tasks from their own project's pane while `af tasks list` showed them
// fine. That divergence is the bug: the CLI had already moved to repo-identity
// matching (#1893, api/scope.go) and the pane's loader had not.
//
// Both sides are therefore reduced to the same canonical form — the repo ID,
// sha256 of the main-worktree root — before comparing, exactly as the CLI does.

// repoScope answers "does this task belong to repoRoot?" for one load.
//
// It resolves at most once per distinct project path per load, and prefers
// answers that cost no git at all: this runs on the TUI's snapshot poll
// (app/sync.go, every 750ms), so a filter that shelled out per task per tick
// would burn subprocesses continuously on an otherwise idle TUI.
type repoScope struct {
	root string
	id   string
	// seen memoizes resolution within this load. The process-level cache below
	// carries positives ACROSS loads, which is what keeps the poll cheap.
	seen map[string]string
}

// newRepoScope canonicalizes the target side.
//
// repoRoot is contractually a main-worktree root (config.CurrentRepo /
// RepoFromPath), which every caller passes, so hashing it directly yields the
// same canonical ID the task side resolves to — no git call needed for the
// common path. A caller that passes something else degrades to the raw path
// equality this filter used before, never to "no tasks".
func newRepoScope(repoRoot string) *repoScope {
	return &repoScope{
		root: repoRoot,
		id:   config.RepoIDFromRoot(repoRoot),
		seen: map[string]string{},
	}
}

// matches reports whether t belongs to this scope.
func (s *repoScope) matches(t Task) bool {
	// The RETAINED id wins when present: it was resolved at bind time, while the
	// recorded path was known to resolve, so it survives that path being deleted
	// or moved. Re-deriving would be strictly worse information — and it is free.
	if t.RepoID != "" {
		return t.RepoID == s.id
	}
	// An unbound task belongs to no project, so no project's pane claims it.
	// This deliberately diverges from api/scope.go, which matches an unbound task
	// into EVERY scope so it stays deletable: reachability is the CLI's job, and
	// duplicating one orphan into every project's pane is noise, not a fix. No
	// supported writer creates one (see Task.ProjectPath).
	if strings.TrimSpace(t.ProjectPath) == "" {
		return false
	}
	// Exact match short-circuits the git resolution for every task created from
	// the repo root — the majority, and the case that already worked.
	if t.ProjectPath == s.root {
		return true
	}
	return s.resolve(t.ProjectPath) == s.id
}

func (s *repoScope) resolve(projectPath string) string {
	if got, ok := s.seen[projectPath]; ok {
		return got
	}
	id := resolveProjectID(projectPath)
	s.seen[projectPath] = id
	return id
}

// projectIDMemo caches path→repo-ID resolutions that RESOLVED to a real
// repository, for the life of the process.
//
// Only rows written before Task.RepoID existed reach resolution at all, but for
// those the TUI's 750ms poll would otherwise re-run `git rev-parse` per task
// forever — they are never rewritten, so they never pick the field up.
//
// Only positives are cached, and only real ones. A path that does not resolve
// today may resolve tomorrow (the user creates the subdirectory, or adds the
// worktree, after binding the task), and caching that miss would pin the task to
// the wrong scope for the whole session; re-resolving a miss costs a git call on
// a row that is rare by construction. A cached positive can go stale only if the
// project is moved or deleted mid-session, which mis-scopes a displayed list
// until restart and destroys nothing.
var projectIDMemo sync.Map // map[string]string

func resolveProjectID(projectPath string) string {
	if cached, ok := projectIDMemo.Load(projectPath); ok {
		return cached.(string)
	}
	resolved := config.ResolveProjectPath(projectPath)
	// Root == "" means the ID was DERIVED from the path rather than read off a
	// real repository — the negative case, which is not cached.
	if resolved.Root != "" {
		projectIDMemo.Store(projectPath, resolved.ID)
	}
	return resolved.ID
}

package task

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
)

// LoadTasksForRepoID returns tasks belonging to an already-resolved repo ID.
// It is the path-independent counterpart to LoadTasksForRepo for daemon
// lifecycle checks, including remote sessions with no local git worktree. Legacy
// rows without a retained RepoID are resolved freshly and every proven binding
// is committed before scope exclusion: a later symlink/worktree rebind therefore
// cannot move an excluded task into the project after its target is archived.
// An enabled targeted legacy row whose nonempty path cannot currently resolve
// is an unknown relationship and returns an error rather than silently excluding
// a possible blocker; disabled and untargeted rows cannot create that retry
// state and remain non-blocking.
func LoadTasksForRepoID(repoID string) ([]Task, error) {
	filtered, _, err := LoadTasksForRepoIDWithBindingUpdates(repoID)
	return filtered, err
}

// LoadTasksForRepoIDWithBindingUpdates also returns the authoritative task
// projections whose legacy ProjectPath bindings this load durably backfilled.
// Daemon callers publish those commits so push-only clients cannot retain a
// different dynamic scope after the server has made RepoID authoritative.
//
// The backfill commits before scope exclusion, so a scope error is a PARTIAL
// success: updates are returned WITH that error and are already durable on
// disk. Callers must publish them before propagating — the error branch is the
// one path where nothing else will ever republish them.
func LoadTasksForRepoIDWithBindingUpdates(repoID string) ([]Task, []Task, error) {
	all, unresolved, updated, err := loadTasksWithStableRepoBindings()
	if err != nil {
		return nil, nil, err
	}
	var filtered []Task
	for _, t := range all {
		if t.RepoID != "" {
			if t.RepoID == repoID {
				filtered = append(filtered, t)
			}
			continue
		}
		if strings.TrimSpace(t.ProjectPath) == "" {
			continue
		}
		resolved, ok := unresolved[t.ProjectPath]
		matched := ok && resolved.id == repoID
		// An unresolved fallback equal to the target ID still proves membership:
		// both IDs hash the exact same cleaned spelling. A mismatch, or a row
		// introduced after the pre-lock resolution snapshot, stays unknown.
		known := ok && (resolved.known || matched)
		if !known && t.Enabled && CanonicalTargetSession(t.TargetSession) != "" {
			return nil, updated, fmt.Errorf("could not determine whether enabled legacy task %q targeting %q belongs to repo %s: project_path %q does not resolve to a repository; refusing lifecycle decision", t.ID, t.TargetSession, repoID, t.ProjectPath)
		}
		if matched {
			filtered = append(filtered, t)
		}
	}
	return filtered, updated, nil
}

// LoadTasksWithStableRepoBindingUpdates is the all-project counterpart to
// LoadTasksForRepoIDWithBindingUpdates. It returns the authoritative task list
// after committing every legacy ProjectPath that currently resolves to a real
// repository. Resolution runs before the tasks-file lock because it shells out
// to git; the lock then re-reads the store and applies only path-keyed answers
// from that snapshot. Rows added with a new path in the interval remain
// unresolved, never guessed. Callers that depend on identity must treat their
// empty RepoID as unknown.
func LoadTasksWithStableRepoBindingUpdates() ([]Task, []Task, error) {
	tasks, _, updated, err := loadTasksWithStableRepoBindings()
	return tasks, updated, err
}

func loadTasksWithStableRepoBindings() ([]Task, map[string]repoResolution, []Task, error) {
	path, err := getTasksPathFn()
	if err != nil {
		return nil, nil, nil, err
	}
	if err := ensureTasksSchemaMigrated(path); err != nil {
		return nil, nil, nil, err
	}
	snapshot, err := LoadTasks()
	if err != nil {
		return nil, nil, nil, err
	}
	resolvedPaths := make(map[string]repoResolution)
	for _, t := range snapshot {
		if t.RepoID != "" || strings.TrimSpace(t.ProjectPath) == "" {
			continue
		}
		if _, seen := resolvedPaths[t.ProjectPath]; seen {
			continue
		}
		resolved := config.ResolveProjectPath(t.ProjectPath)
		resolvedPaths[t.ProjectPath] = repoResolution{
			id: resolved.ID, known: resolved.Root != "" && resolved.ID != "",
		}
	}

	var authoritative []Task
	var updated []Task
	unresolved := make(map[string]repoResolution)
	lockErr := config.WithFileLock(path, func() error {
		current, err := loadTasksLocked(path)
		if err != nil {
			return err
		}
		changed := false
		for i := range current {
			if current[i].RepoID != "" || strings.TrimSpace(current[i].ProjectPath) == "" {
				continue
			}
			resolved, ok := resolvedPaths[current[i].ProjectPath]
			if ok && resolved.known {
				current[i].RepoID = resolved.id
				updated = append(updated, current[i])
				changed = true
				continue
			}
			if ok {
				unresolved[current[i].ProjectPath] = resolved
			} else {
				unresolved[current[i].ProjectPath] = repoResolution{}
			}
		}
		if changed {
			if err := saveTasks(current); err != nil {
				return err
			}
		}
		authoritative = current
		return nil
	})
	if lockErr != nil {
		return nil, nil, nil, lockErr
	}
	return authoritative, unresolved, updated, nil
}

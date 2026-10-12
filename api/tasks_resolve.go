package api

import (
	"fmt"
	"sort"
	"strings"

	"github.com/sachiniyer/agent-factory/task"
)

// taskIDOrNameDoc is the shared account of how the positional argument to an
// id-taking task verb resolves (#4676), spliced into each verb's Long so the
// commands cannot drift apart on the rule a destructive call depends on.
const taskIDOrNameDoc = "The argument may be the task's id or its exact name. An exact id match " +
	"always wins — a name that collides with another task's id still resolves to that id. " +
	"Otherwise an exact name match resolves only when exactly one task in scope has it: a name " +
	"shared by several tasks is refused with their ids rather than acted on at random, and a name " +
	"held only by other projects is refused with the --repo that would reach it."

// The positional argument of an id-taking task verb accepts a task id OR a task
// name (#4676). `af tasks list`, the TUI, and the web UI all present the NAME
// prominently, while the verbs accepted only the id — so the identifier every
// surface shows failed everywhere it was used
// (`af tasks show hello-cron` → `task with id "hello-cron" not found`).
//
// Resolution is deliberately asymmetric, and each asymmetry is load-bearing:
//
//   - An EXACT id match wins, searched across every project. Task ids are
//     globally unique, so a hit is identity rather than a guess — and because
//     the id pass runs first, a task name that collides with another task's id
//     can never shadow it. Only args that pass task.ValidateTaskID may match
//     as ids, so a name containing "/" or whitespace is looked up as a name
//     and never reaches the daemon as an id.
//   - Otherwise an EXACT name match resolves, but only when exactly ONE task
//     in the command's project scope carries it. Names are a display label
//     with no uniqueness rule — task.AddTaskChecked enforces none — so a name
//     is a search that may return 0, 1, or N rows rather than a lookup.
//
// Several in-scope name matches are refused with each match's id listed; a
// name found only OUTSIDE the scope is refused naming the project (and the
// --repo) that would reach it; a miss everywhere reports
// "task with id or name … not found", keeping the store's not-found shape while
// naming both things that were searched. Picking a match at random is never an
// option: for `remove` and `update` a wrong pick deletes or rewrites another
// project's automation.
//
// The returned expectation is the contract enforceTaskScope carried (#1893):
// the scope check here is client-side and the mutation is a separate daemon
// call carrying only the id, so on its own it authorizes a record that may be
// rebound before the mutation lands. task.ExpectProject re-states the binding
// under the daemon's lock, making the authorization atomic with the action.
// Every mutating call site must thread it through — discarding it silently
// reopens the race.
func resolveTaskArg(verb, arg string) (*task.Task, task.ProjectExpectation, error) {
	// Resolve the scope BEFORE the lookup so an invalid --repo reports the
	// path it could not resolve rather than being masked by a not-found for
	// the argument (#892 semantics, and what "an explicit --repo always wins"
	// means).
	scope, err := resolveProjectScope(false)
	if err != nil {
		return nil, task.ProjectExpectation{}, err
	}
	t, err := findTaskByIDOrName(verb, arg, scope)
	if err != nil {
		return nil, task.ProjectExpectation{}, fmt.Errorf("failed to get task: %w", err)
	}
	if scope.Repo == nil {
		// Rule 3: with no project context the argument resolves globally —
		// the same convenience sessions grant a bare title, which keeps
		// unscoped scripts and systemd units working.
		return t, task.ProjectExpectation{}, nil
	}
	if err := requireTaskInScope(t, scope); err != nil {
		return nil, task.ProjectExpectation{}, err
	}
	return t, task.ExpectProject(*t), nil
}

// findTaskByIDOrName runs the two-pass resolution described on resolveTaskArg
// over the SAME task list the verb would read — listTasks owns the transport:
// the daemon's authoritative snapshot, a disk read when no daemon is running,
// or the targeted daemon's list for a remote target (with no local fallback).
// A reachable daemon's miss stays authoritative; nothing re-reads disk behind
// its back.
func findTaskByIDOrName(verb, arg string, scope projectScope) (*task.Task, error) {
	if arg == "" {
		// ExactArgs(1) keeps this nearly unreachable, but a caller passing ""
		// earns the same refusal ValidateTaskID gave when the argument could
		// only be an id — an unnamed task must not become addressable by the
		// empty string.
		return nil, task.ValidateTaskID(arg)
	}
	tasks, err := listTasks(verb)
	if err != nil {
		return nil, err
	}
	// Id pass. Gating on ValidateTaskID keeps a non-id-shaped arg out of the
	// id path entirely, so a stored row with a malformed (hand-edited) id
	// cannot be reached by it either — matching what the daemon's own
	// validation would do with such an id.
	if task.ValidateTaskID(arg) == nil {
		for i := range tasks {
			if tasks[i].ID == arg {
				return &tasks[i], nil
			}
		}
	}
	// Name pass, inside the resolved scope only. matchesTask compares repo
	// identity (a TUI-entered subdirectory or linked worktree resolves to its
	// main root), not path strings.
	ids := newProjectIDCache()
	var inScope, foreign []task.Task
	for i := range tasks {
		if tasks[i].Name != arg {
			continue
		}
		if scope.matchesTask(&tasks[i], ids) {
			inScope = append(inScope, tasks[i])
		} else {
			foreign = append(foreign, tasks[i])
		}
	}
	switch {
	case len(inScope) == 1:
		return &inScope[0], nil
	case len(inScope) > 1:
		return nil, ambiguousTaskNameError(arg, inScope, scope.Repo != nil)
	case len(foreign) > 0:
		// foreign is only non-empty when a scope exists — a nil Repo matches
		// every task, so this branch always has scope.Repo set.
		return nil, foreignTaskNameError(arg, foreign, scope, ids)
	}
	return nil, fmt.Errorf("task with id or name %q not found", arg)
}

// ambiguousTaskNameError refuses a name that several in-scope tasks share. It
// lists each match's id because passing the id is the fix — and, when the
// invocation carried no project context at all, each match's project so the
// reader can tell the duplicates apart. Sorted by id so the refusal is
// deterministic rather than at the mercy of store order.
func ambiguousTaskNameError(arg string, matches []task.Task, scoped bool) error {
	sort.Slice(matches, func(i, j int) bool { return matches[i].ID < matches[j].ID })
	if scoped {
		ids := make([]string, len(matches))
		for i, m := range matches {
			ids[i] = m.ID
		}
		return fmt.Errorf("task name %q matches %d tasks in this project: %s — pass one of the ids to pick one",
			arg, len(matches), strings.Join(ids, ", "))
	}
	entries := make([]string, len(matches))
	for i, m := range matches {
		entries[i] = m.ID + " (" + m.ProjectPath + ")"
	}
	return fmt.Errorf("task name %q matches %d tasks: %s — pass one of the ids to pick one",
		arg, len(matches), strings.Join(entries, ", "))
}

// foreignTaskNameError refuses a name that resolves only OUTSIDE the scope —
// the blast-radius refusal requireTaskInScope gives an id, worded so the
// caller sees the name they typed, the id it would resolve to, and the --repo
// that reaches it.
func foreignTaskNameError(arg string, foreign []task.Task, scope projectScope, ids *projectIDCache) error {
	// Suggest a --repo that RESOLVES, mirroring requireTaskInScope: the
	// recorded path may be a subdirectory that no longer exists, and
	// suggesting a path we would then reject is worse than not suggesting one.
	suggest := func(t task.Task) string {
		if root := ids.resolve(t.ProjectPath).Root; root != "" {
			return root
		}
		return t.ProjectPath
	}
	if len(foreign) == 1 {
		m := foreign[0]
		return fmt.Errorf("task named %q is task %s in project %s, not the current project %s — pass --repo %s to act on it",
			arg, m.ID, m.ProjectPath, scope.Repo.Root, suggest(m))
	}
	sort.Slice(foreign, func(i, j int) bool { return foreign[i].ID < foreign[j].ID })
	entries := make([]string, len(foreign))
	for i, m := range foreign {
		entries[i] = m.ID + " (" + m.ProjectPath + ")"
	}
	return fmt.Errorf("task name %q matches %d tasks in other projects: %s — pass --repo <path> to pick one",
		arg, len(foreign), strings.Join(entries, ", "))
}

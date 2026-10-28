package task

import (
	"fmt"
	"strings"
)

// ProjectExpectation is an optional compare-and-swap on a task's recorded
// ProjectPath, evaluated inside the SAME locked operation that mutates the task.
//
// It closes a check-then-act race (#1893 review). The CLI authorizes a task id
// against the current project client-side, but the mutation is a separate RPC
// carrying only the id. ProjectPath is a supported patch field (#1836), so
// another client can rebind the task between the two: the check authorizes the
// old project and the daemon acts on the newly-rebound task, letting a
// current-project command mutate ANOTHER project's task — precisely what the
// scoping exists to prevent. An authorization that is not atomic with the action
// it authorizes is not an authorization.
//
// It compares the recorded path STRING rather than resolving repo identity on
// the daemon side, for two reasons. Identity resolution shells out to git, and
// this runs while the tasks.json file lock is held — a subprocess under a held
// lock is a hazard this repo already knows. And a string compare is the right
// question here: the caller is asking "is this still the record I authorized?",
// not "are these the same project". Any rebind changes the string, so it fails
// closed. A rebind between two DIFFERENT paths naming the SAME project is
// refused too — a false rejection, but a safe one that a re-run resolves.
//
// Zero value = no expectation, which is what a caller with no project context
// (rule 3) sends and what an older daemon decodes an absent field as. Both
// fields are plain values, never pointers: net/rpc gob elides zero-value
// POINTER fields, so a *string would arrive nil and silently disable the check
// (#1700).
// The json tags define the HTTP body shape (#1029 PR 4) and must stay
// snake_case like every other route's fields; gob ignores them entirely.
type ProjectExpectation struct {
	// Enforce distinguishes "no expectation" from "expected to be unbound"
	// (ProjectPath == ""), which an empty ProjectPath alone cannot express.
	Enforce     bool   `json:"enforce"`
	ProjectPath string `json:"project_path"`
}

// Verify reports whether t still matches the expectation. Callers must run it
// against a FRESHLY loaded record inside the locked operation — verifying a
// record the caller already held would re-introduce the race it closes.
func (e ProjectExpectation) Verify(t Task) error {
	if !e.Enforce || t.ProjectPath == e.ProjectPath {
		return nil
	}
	return fmt.Errorf(
		"task %q was re-bound to a different project while this command was running (expected %s, now %s) — nothing was changed; re-run the command to act on it in its current project",
		t.ID, describeProjectPath(e.ProjectPath), describeProjectPath(t.ProjectPath))
}

func describeProjectPath(path string) string {
	if strings.TrimSpace(path) == "" {
		return "no project"
	}
	return path
}

// ExpectProject returns the expectation a mutating caller must attach when it
// authorized the task from a loaded record: "act only if this is still bound
// where I saw it". It is the ONE constructor every surface shares — the CLI's
// scope check, the TUI's pane/trigger paths, and (mirrored in web/src/api.ts)
// the web client — so the admission predicate is defined here once rather than
// re-derived per surface (#3190, #3230). Enforce is always true: a caller
// holding a record always has a binding to pin, including the unbound one
// (ProjectPath == ""), which Enforce distinguishes from "no expectation".
func ExpectProject(t Task) ProjectExpectation {
	return ProjectExpectation{Enforce: true, ProjectPath: t.ProjectPath}
}

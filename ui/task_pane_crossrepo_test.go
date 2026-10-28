package ui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTakeCrossRepoRetargetNotice is the unit guard for the cross-repo retarget
// notice channel saveContentPaneState feeds and handleQuit / the snapshot poll
// drain. The notice is the pane-level mirror of the cross-repo CREATE notice
// home_tasks.go raises (#1836 + #1891): a retarget that crossed repos drops the
// row from the active-scope pane, and the queue carries where it landed so the
// silent drop does not look like a failed save.
func TestTakeCrossRepoRetargetNotice(t *testing.T) {
	tp := NewTaskPane()

	// No retargets queued: the notice is empty and draining is a no-op.
	assert.Empty(t, tp.TakeCrossRepoRetargetNotice(),
		"a fresh pane raises no cross-repo retarget notice")

	// One retarget: a singular sentence naming the task and the new binding.
	tp.RecordCrossRepoRetarget("nightly", "/repo/other")
	assert.Equal(t,
		`Task "nightly" was rebound to /repo/other; it no longer appears in this project's list`,
		tp.TakeCrossRepoRetargetNotice(),
		"one retarget formats as a singular sentence naming the task and its new binding")

	// The notice is one-shot: a second take drains nothing.
	assert.Empty(t, tp.TakeCrossRepoRetargetNotice(),
		"TakeCrossRepoRetargetNotice clears the queue, so a second call returns nothing")

	// Multiple retargets in one drain: one plural sentence aggregating the
	// names without losing any. saveContentPaneState does not deduplicate by
	// design (a save that retargets two tasks in one overlay-close flush is
	// rare, but each task's landing deserves naming). The plural branch keeps
	// each task's destination binding — the whole point of the notice is to
	// tell the user where a vanished task went (#1891), so a bare list of names
	// is not enough, especially when several retargets land in different repos.
	tp.RecordCrossRepoRetarget("alpha", "/repo/a")
	tp.RecordCrossRepoRetarget("beta", "/repo/b")
	notice := tp.TakeCrossRepoRetargetNotice()
	assert.Contains(t, notice, `"alpha"`, "the notice names every retargeted task")
	assert.Contains(t, notice, `"beta"`, "the notice names every retargeted task")
	assert.Contains(t, notice, "/repo/a",
		"the plural notice names each retarget's destination, not just its task name")
	assert.Contains(t, notice, "/repo/b",
		"the plural notice names each retarget's destination, not just its task name")
	assert.Contains(t, notice, "were rebound",
		"two-plus retargets format as the plural sentence")

	// An empty name falls back rather than emitting an awkward quoted empty
	// string: the TaskPane would only record an empty name when the in-memory
	// row never had one (a fresh create that has not been named), and the task's
	// id is a safe placeholder for the wait until saveContentPaneState is
	// rerun with a real name. The notice is still informative — it names the
	// path the binding landed at.
	tp.RecordCrossRepoRetarget("", "/repo/anon")
	anon := tp.TakeCrossRepoRetargetNotice()
	assert.Contains(t, anon, "/repo/anon",
		"the notice still names the binding for a retarget with no in-memory name")
	assert.Contains(t, anon, "the task",
		"a missing name falls back to a generic noun rather than an awkward quoted empty string")
}

// ResetTasks is the reload for a different scope (a project switch). The
// crossRepoRetargets queue is project-relative — it names where a save landed
// out of the project this pane was showing — so a retarget the previous scope
// raised must not follow the pane into the new one. Switching to the
// retarget's destination project re-shows the row, which would make a stale
// "no longer appears in this project's list" notice wrong, and any other
// switch surfaces a notice that describes a project the pane no longer holds.
func TestResetTasksClearsCrossRepoRetargetQueue(t *testing.T) {
	tp := NewTaskPane()
	tp.RecordCrossRepoRetarget("nightly", "/repo/other")
	require.NotEmpty(t, tp.TakeCrossRepoRetargetNotice(),
		"precondition: a retarget was recorded")

	// Re-record so the queue holds the notice ResetTasks must drop.
	tp.RecordCrossRepoRetarget("nightly", "/repo/other")
	tp.ResetTasks(nil)
	assert.Empty(t, tp.TakeCrossRepoRetargetNotice(),
		"ResetTasks (a project switch) must clear the project-relative cross-repo retarget queue")
}

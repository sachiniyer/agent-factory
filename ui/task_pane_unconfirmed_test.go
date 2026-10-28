package ui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests in this file pin #4824 against #4798: an edit whose save could not
// be confirmed is kept, but no automatic save re-sends it.

// heldUnconfirmed returns a pane whose edit to "demo" (Enabled true → false)
// went through one save that could not be confirmed.
func heldUnconfirmed(t *testing.T) *TaskPane {
	t.Helper()
	demo := reloadTask("demo", "p")
	s := NewTaskPane()
	s.SetTasks([]task.Task{demo})
	s.SetFocus(true)
	require.True(t, s.HandleKeyPress(keyRunes("x")))
	edits := s.ConsumeDirty()
	require.Len(t, edits, 1)
	s.HoldUnconfirmedEdit(edits[0].ID)
	return s
}

func TestTaskPaneUnconfirmedEditIsKeptButNotResent(t *testing.T) {
	s := heldUnconfirmed(t)
	s.SetTasks(load(reloadTask("demo", "p"))()) // the re-read: the edit did not land

	assert.True(t, s.IsDirty(), "the draft is kept (#4798)")
	require.Len(t, s.GetTasks(), 1)
	assert.False(t, s.GetTasks()[0].Enabled, "the edited value survives the reload")
	assert.Empty(t, s.ConsumeDirty(), "an automatic save does not re-send it")
	assert.True(t, s.IsDirty(), "and it is still held after that save")
	assert.Empty(t, s.TakeSettledDraftNotice())
}

// A new edit is the explicit re-save: the edit form's Enter marks the task
// dirty the same way, and the whole patch goes out again.
func TestTaskPaneUnconfirmedEditIsSentAfterANewEdit(t *testing.T) {
	s := heldUnconfirmed(t)
	s.markTaskDirty("demo")

	edits := s.ConsumeDirty()
	require.Len(t, edits, 1)
	require.NotNil(t, edits[0].Update.Enabled)
	assert.False(t, *edits[0].Update.Enabled, "the kept edit is part of the re-sent patch")
}

// The re-read shows the edit landed: settle it as clean, and say so.
func TestTaskPaneUnconfirmedEditSettlesWhenTheReloadCarriesIt(t *testing.T) {
	s := heldUnconfirmed(t)
	landed := reloadTask("demo", "p")
	landed.Enabled = false
	s.SetTasks(load(landed)())

	assert.False(t, s.IsDirty())
	assert.Equal(t, `Saved edits to "demo" — the task list now shows them`, s.TakeSettledDraftNotice())
	assert.Empty(t, s.TakeSettledDraftNotice(), "the notice is raised once")
	assert.Empty(t, s.TakeUnconfirmedQuitNotice(), "nothing is left to warn about")
}

func TestTaskPaneUnconfirmedQuitNoticeIsRaisedOnce(t *testing.T) {
	s := heldUnconfirmed(t)
	assert.Contains(t, s.TakeUnconfirmedQuitNotice(), `Edits to "demo" could not be confirmed`)
	assert.Empty(t, s.TakeUnconfirmedQuitNotice(), "the next quit goes through")
}

// retargetedHeldUnconfirmed drives the edit form to add a target session to base,
// consumes the resulting patch, and holds its save as unconfirmed. The returned
// pane has the retarget held in unconfirmedIDs/dirtyIDs; orig is base (the
// pre-edit baseline the patch is diffed against).
func retargetedHeldUnconfirmed(t *testing.T, base task.Task, target string) *TaskPane {
	t.Helper()
	s := NewTaskPane()
	s.SetSize(100, 40)
	s.SetTasks([]task.Task{base})
	s.SetFocus(true)

	s.EnterEditSelected()
	require.True(t, s.IsEditing())
	s.editTarget.SetValue(target)
	require.True(t, s.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter}))
	require.False(t, s.IsEditing())

	edits := s.ConsumeDirty()
	require.Len(t, edits, 1)
	require.NotNil(t, edits[0].Update.TargetSession)
	s.HoldUnconfirmedEdit(edits[0].ID)
	return s
}

// The primary reproduction: retargeting a capped watch task lands at the daemon,
// which clears the now-inapplicable cap via clearInapplicableCap. The pane has no
// cap control, so the held draft still carries the old cap and the patch carries
// only TargetSession. A field-scoped settle compares TargetSession (the patched
// field) and ignores MaxConcurrentRuns (never patched), so the edit settles.
func TestTaskPaneUnconfirmedCapSettlesWhenReloadClearsTheCap(t *testing.T) {
	repo := newGitRepo(t)
	base := task.Task{
		ID: "demo", Name: "demo", Prompt: "p", WatchCmd: "echo hi",
		Enabled: true, ProjectPath: repo, MaxConcurrentRuns: 3,
	}
	s := retargetedHeldUnconfirmed(t, base, "build")

	// The patch must carry ONLY TargetSession — the pane never edited the cap.
	edits := s.ConsumeDirty()
	assert.Empty(t, edits, "a held unconfirmed edit is not re-sent automatically")

	// The daemon applied the retarget and cleared the now-inapplicable cap.
	landed := base
	landed.TargetSession = "build"
	landed.MaxConcurrentRuns = 0
	s.SetTasks([]task.Task{landed})

	assert.False(t, s.IsDirty(), "the landed edit should be settled despite the cleared cap")
	assert.Equal(t, `Saved edits to "demo" — the task list now shows them`, s.TakeSettledDraftNotice())
	assert.Empty(t, s.TakeUnconfirmedQuitNotice(),
		"the user must not be told the edit could not be confirmed — it landed")
	assert.Empty(t, s.ConsumeDirty(), "a settled edit is not re-sent on a later save")
}

// The secondary reproduction: a whitespace-only target session canonicalizes to ""
// on every daemon save. The held draft carries the raw whitespace; the reload
// carries "". A field-scoped settle compares through CanonicalTargetSession, so
// the whitespace and the empty string match and the edit settles.
func TestTaskPaneUnconfirmedWhitespaceTargetSettlesWhenReloadCanonicalizes(t *testing.T) {
	repo := newGitRepo(t)
	base := task.Task{
		ID: "demo", Name: "demo", Prompt: "p", CronExpr: "* * * * *",
		Enabled: true, ProjectPath: repo,
	}
	s := retargetedHeldUnconfirmed(t, base, "   ")

	landed := base
	landed.TargetSession = "" // daemon canonicalized the whitespace to empty
	s.SetTasks([]task.Task{landed})

	assert.False(t, s.IsDirty(), "the landed edit should settle through the canonical form")
	assert.Equal(t, `Saved edits to "demo" — the task list now shows them`, s.TakeSettledDraftNotice())
	assert.Empty(t, s.TakeUnconfirmedQuitNotice())
	assert.Empty(t, s.ConsumeDirty())
}

// A target-session edit that did NOT land stays held. The reload shows no target
// session, so the patched TargetSession disagrees (even through its canonical
// form) and the edit does not settle — the recovery feature does not drop real
// unsaved work.
func TestTaskPaneUnconfirmedTargetEditStaysHeldWhenItDidNotLand(t *testing.T) {
	repo := newGitRepo(t)
	base := task.Task{
		ID: "demo", Name: "demo", Prompt: "p", WatchCmd: "echo hi",
		Enabled: true, ProjectPath: repo,
	}
	s := retargetedHeldUnconfirmed(t, base, "build")

	// The reload shows no target session: the retarget did not land.
	s.SetTasks([]task.Task{base})

	assert.True(t, s.IsDirty(), "the unlanded retarget stays held")
	assert.Empty(t, s.TakeSettledDraftNotice())
	assert.Contains(t, s.TakeUnconfirmedQuitNotice(), `could not be confirmed`)
}

// Field-scoping is the load-bearing change: a daemon-side change to a field the
// user never patched must not block settlement of the user's actual edit. The
// old whole-record equality refused to settle here, stranding a landed edit
// whenever a concurrent writer touched a different field.
func TestTaskPaneUnconfirmedSettleIgnoresConcurrentEditToUnpatchedField(t *testing.T) {
	s := heldUnconfirmed(t) // edits "demo" Enabled true -> false, then holds
	// The user's disable landed, and a concurrent writer changed the prompt the
	// user never touched; the reload carries both.
	landed := reloadTask("demo", "cli")
	landed.Enabled = false
	s.SetTasks([]task.Task{landed})

	assert.False(t, s.IsDirty(),
		"the user's edit landed; the unpatched prompt change must not block settle")
	assert.Equal(t, `Saved edits to "demo" — the task list now shows them`, s.TakeSettledDraftNotice())
	assert.Empty(t, s.TakeUnconfirmedQuitNotice())
}

// A damaged file listing an ID twice pairs an unedited sibling with the copy
// the user actually edited. The unedited sibling's patch is empty, so without
// deferring it the empty-patch settle would clear the shared ID first and let
// SetTasks replace the edited row — silently dropping the held edit. The
// unedited sibling must defer to the edited one, and the unlanded edit stays
// held.
func TestTaskPaneUnconfirmedSkipsEmptySiblingWhenDuplicateIDHasAnEdit(t *testing.T) {
	s := NewTaskPane()
	s.SetTasks([]task.Task{reloadTask("demo", "p"), reloadTask("demo", "p")})
	require.Equal(t, []string{"demo", "demo"}, paneIDs(s))
	s.SetFocus(true)
	s.SelectTask(1)                                  // the user edits the second copy
	require.True(t, s.HandleKeyPress(keyRunes("x"))) // Enabled true -> false
	edits := s.ConsumeDirty()
	require.Len(t, edits, 1)
	s.HoldUnconfirmedEdit(edits[0].ID)

	// The edit did not land; a concurrent writer changed the unedited first
	// copy's prompt. The reload carries the unedited copy (Enabled still true).
	s.SetTasks([]task.Task{reloadTask("demo", "cli")})

	require.True(t, s.IsDirty(), "the unlanded edit on the second copy stays held")
	assert.Empty(t, s.TakeSettledDraftNotice(), "the unedited sibling must not settle on the edit's behalf")
	require.Len(t, s.GetTasks(), 1)
	assert.False(t, s.GetTasks()[0].Enabled, "the edited copy is the one kept")
	assert.Empty(t, s.ConsumeDirty(), "a held unconfirmed edit is not re-sent automatically")
	assert.Contains(t, s.TakeUnconfirmedQuitNotice(), `could not be confirmed`)
}

package ui

import (
	"strconv"
	"strings"
)

// This file holds the TaskPane's notice channel for edits a save retargeted to
// a project other than the TUI's active one (#1836, the same hazard the cross-
// repo create pathsurfaced in app/home_tasks.go).
//
// A cross-repo retarget is supported: the edit form's Path field accepts any
// real git repo (mirroring `af tasks add --repo`), and the patch carries the
// new binding. The save commits, the daemon re-arms its schedules, and the
// task lives at the new project. The hazard is the visible side: the save's
// reload is scoped to the active project (m.repoRoot/m.repoID, the same scope
// the snapshot poll uses), so a task that crossed repos is no longer in the
// reloaded list and its row vanishes from the pane with no error and no
// notice — indistinguishable at a glance from a failed save. The user then
// either re-edits the task they can no longer see or assumes the save dropped
// it and starts over. The CLI echoes the resolved binding for the same
// reason (#1891: the id alone did not tell the caller which project the task
// attached to); the TUI's pane rows do not show ProjectPath, so without this
// notice the cross-repo landing is invisible until the user switches projects.
//
// saveContentPaneState records each retarget it sees leave the active scope,
// and handleQuit / the snapshot poll drain the notice via Take*-the same
// pattern DiscardDeletedDraft and settledDrafts already use.

// crossRepoRetarget pairs the task's name with the new binding a save landed,
// for a notice that names where it went.
type crossRepoRetarget struct {
	name string
	path string
}

// RecordCrossRepoRetarget queues one retargeted task for the next
// TakeCrossRepoRetargetNotice. name is the task's name as the pane displayed it
// (the editing form's Name field); path is the absolute ProjectPath the save
// committed. Empty names fall back to the task's id at format time.
func (s *TaskPane) RecordCrossRepoRetarget(name, path string) {
	s.crossRepoRetargets = append(s.crossRepoRetargets, crossRepoRetarget{name: name, path: path})
}

// TakeCrossRepoRetargetNotice returns one notice naming every retarget the
// save moved out of the active project since the last call, and clears them.
// It returns "" when nothing was recorded. The save that retargets a task
// usually runs as the overlay closes (Esc) or as the snapshot poll reloads the
// pane, so the app raises this on its own notice bar rather than the pane.
//
// Mirrors the wording of TakeDiscardedDraftNotice / TakeSettledDraftNotice:
// one task gets a singular sentence, two-or-more get a plural one. The path is
// the echo the CLI also prints (#1891), keeping the surface that names the
// binding consistent across surfaces.
func (s *TaskPane) TakeCrossRepoRetargetNotice() string {
	items := s.crossRepoRetargets
	s.crossRepoRetargets = nil
	if len(items) == 0 {
		return ""
	}
	if len(items) == 1 {
		name := items[0].name
		if name == "" {
			name = "the task"
		} else {
			name = strconv.Quote(name)
		}
		return "Task " + name + " was rebound to " + items[0].path +
			"; it no longer appears in this project's list"
	}
	// Name each retarget with the binding it landed at, not just the task name:
	// the whole point of the notice is to tell the user where a vanished task went
	// (#1891), and that is doubly necessary when several retargets land in different
	// repos — a bare list of names leaves the user to re-open each one to find it.
	quoted := make([]string, 0, len(items))
	for _, r := range items {
		if r.name == "" {
			quoted = append(quoted, "a task (rebound to "+r.path+")")
			continue
		}
		quoted = append(quoted, strconv.Quote(r.name)+" to "+r.path)
	}
	return "Tasks " + strings.Join(quoted, ", ") +
		" were rebound and no longer appear in this project's list"
}

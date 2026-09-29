package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	xansi "github.com/charmbracelet/x/ansi"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"
)

// testMutationCommittedError is a test stub for the apiproto.MutationCommittedError
// interface — the marker the daemon's HTTP/client/control transports raise when a
// mutation durably committed but a non-transactional follow-up failed. Used to
// drive procedure 6 (committed-but-failed warning path) deterministically via the
// deleteProjectThroughDaemon seam, the same seam the bug report's reproduction
// test stubs; killing a real daemon mid-delete would yield a transport error
// (classified as a hard failure, not committed) and would not exercise the
// committedWarning branch of handleProjectDeleted.
type testMutationCommittedError struct{ msg string }

func (e *testMutationCommittedError) Error() string           { return e.msg }
func (e *testMutationCommittedError) MutationCommitted() bool { return true }

var _ apiproto.MutationCommittedError = (*testMutationCommittedError)(nil)

func TestDeleteProjectCarriesRetainedRecordedIdentity(t *testing.T) {
	ancestor := t.TempDir()
	cmd := exec.Command("git", "init", ancestor)
	require.NoError(t, cmd.Run())
	recorded := filepath.Join(ancestor, "deleted-nested-repo")
	require.NoError(t, os.Mkdir(recorded, 0o755))
	require.NotEqual(t, config.RepoIDFromRoot(filepath.Clean(recorded)), config.RepoIDForPath(recorded))
	data := []session.InstanceData{{
		Title: "legacy", Path: recorded,
		Worktree: session.GitWorktreeData{RepoPath: recorded, WorktreePath: "/archive/legacy"},
	}}
	h, _ := armDeleteProjectDialog(t, data)
	model, cmdFn := h.handleStateConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	h = model.(*home)
	require.NotNil(t, cmdFn)
	start, ok := cmdFn().(startDeleteProjectMsg)
	require.True(t, ok)
	assert.Equal(t, config.RepoIDFromRoot(filepath.Clean(recorded)), start.repoID,
		"delete must carry the selected aggregate identity, not adopt the Git ancestor of its display root")
}

// deleteProjectTestRoot is the repo root the delete-project tests build their
// snapshot around. RepoIDFromRoot is a pure hash of the path and the delete verb
// never touches the filesystem, so no real git repo is needed here.
const deleteProjectTestRoot = "/tmp/af-delete-project-test/acme"

// deleteProjectSession builds a live snapshot record for the test repo.
// external mirrors Worktree.ExternalWorktree — the in-place/`--here` flag that
// ToInstanceData copies straight from gitWorktree.IsExternalWorktree(), i.e. the
// exact predicate daemon.deleteProject branches kill-vs-archive on (#1973).
func deleteProjectSession(title string, external bool) session.InstanceData {
	return session.InstanceData{
		Title: title,
		Worktree: session.GitWorktreeData{
			RepoPath:         deleteProjectTestRoot,
			WorktreePath:     deleteProjectTestRoot,
			SessionName:      title,
			BranchName:       "af/" + title,
			ExternalWorktree: external,
		},
	}
}

// dialogText reduces a rendered overlay to the prose the user reads: it strips
// ANSI and the box-drawing frame, then collapses the wrap, so an assertion can
// match a whole phrase the overlay happened to break across two lines.
//
// ANSI must go first. lipgloss's colour profile is process-global, so depending
// on which tests ran before, the border arrives either as "│" glyphs or as
// colour-escaped spaces — strip only the glyphs and the assertions pass alone
// but fail in a full run.
func dialogText(rendered string) string {
	frame := strings.NewReplacer(
		"│", " ", "─", " ", "╭", " ", "╮", " ", "╰", " ", "╯", " ",
	)
	return strings.Join(strings.Fields(frame.Replace(xansi.Strip(rendered))), " ")
}

// armDeleteProjectDialog drives the REAL prod gate up to the confirmation: it
// feeds the snapshot through buildProjectListFrom (the derivation that populates
// InPlaceCount), pushes the rows into the Projects section, focuses it, and
// presses the actual delete-project key on the cursor's row — rather than
// hand-building a ui.SidebarProject or calling the message builder directly.
// Returns the home and the dialog text as the user sees it, unwrapped.
func armDeleteProjectDialog(t *testing.T, data []session.InstanceData) (*home, string) {
	t.Helper()
	// Roomy window: the assertions are about the copy, not the viewport.
	return armDeleteProjectDialogAt(t, data, 120, 45)
}

// armDeleteProjectDialogAt is armDeleteProjectDialog at a specific terminal
// size, so a test can assert on what the overlay ACTUALLY RENDERS at that size.
// The message string was always correct — the rendering is what lied — so a test
// that inspects the input copy reproduces nothing.
func armDeleteProjectDialogAt(t *testing.T, data []session.InstanceData, w, hgt int) (*home, string) {
	t.Helper()
	h := newTestHome(t)
	t.Cleanup(SetAllReposSnapshotFetcherForTest(func() ([]session.InstanceData, error) {
		return data, nil
	}))
	resizeHome(h, w, hgt)

	// The section's rows come from the same discovery prod uses on launch, poll,
	// and project switch. This is what makes the test cover the derivation.
	h.refreshSidebarProjects()
	h.relayout()
	require.Len(t, h.projects.Projects(), 1, "the snapshot must yield exactly the test project")

	h.showProjectPickerOverlay()
	// A single project has no rail block; its picker retains deletion.
	model, _ := h.handleStateSwitchProject(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})
	h = model.(*home)
	require.Equal(t, stateConfirm, h.state, "delete project must open a confirmation")
	require.NotNil(t, h.confirmationOverlay)

	return h, dialogText(h.confirmationOverlay.Render())
}

// TestDeleteProjectConfirmStatesRealSplit is the #1973 guarantee on the surface
// that matters most: the confirmation is the entire basis on which the user
// consents to a destructive action, so it must state what actually happens to
// each class of session BEFORE they answer. In-place/external-worktree sessions
// are torn down by daemon.deleteProject, never archived — promising them back via
// restore is the bug.
func TestDeleteProjectConfirmStatesRealSplit(t *testing.T) {
	t.Run("only normal sessions are archived and restorable", func(t *testing.T) {
		_, dialog := armDeleteProjectDialog(t, []session.InstanceData{
			deleteProjectSession("alpha", false),
			deleteProjectSession("beta", false),
		})

		assert.Contains(t, dialog, "2 sessions archived — restorable",
			"a project of ordinary sessions is fully restorable and must say so")
		assert.Contains(t, dialog, "Restore an archived session",
			"the dialog must offer the in-TUI restore affordance")
		assert.NotContains(t, dialog, "af sessions restore",
			"restore is a key in this interface — do not send the user to a shell (#2479)")
		assert.NotContains(t, dialog, "not restorable",
			"nothing is killed here — the dialog must not invent a scary split")
		assert.NotContains(t, dialog, "in-place")
	})

	t.Run("only in-place sessions are torn down and not restorable", func(t *testing.T) {
		_, dialog := armDeleteProjectDialog(t, []session.InstanceData{
			deleteProjectSession("root", true),
		})

		assert.Contains(t, dialog, "1 in-place session torn down — not restorable",
			"an in-place session is killed; the user must be told before consenting")
		// The honest half of the split: the kill does not destroy their work.
		// GitWorktree.Cleanup() no-ops for an external worktree, so the branch and
		// uncommitted changes survive — only the session and its agent are gone.
		assert.Contains(t, dialog, "stay exactly where they are")
		assert.Contains(t, dialog, "the session and its agent are gone")
		assert.NotContains(t, dialog, "archived — restorable",
			"nothing here is restorable — claiming otherwise is exactly bug #1973")
		assert.NotContains(t, dialog, "af sessions restore",
			"restore cannot bring a killed in-place session back; do not offer it")
	})

	t.Run("mixed project names both outcomes", func(t *testing.T) {
		_, dialog := armDeleteProjectDialog(t, []session.InstanceData{
			deleteProjectSession("alpha", false),
			deleteProjectSession("beta", false),
			deleteProjectSession("root", true),
		})

		// The case that matters: the user must see BOTH numbers, each with its
		// real consequence, not one blended count.
		assert.Contains(t, dialog, "2 sessions archived — restorable",
			"the archived count must exclude the in-place session")
		assert.Contains(t, dialog, "1 in-place session torn down — not restorable",
			"the killed count must be stated with its consequence")
		assert.Contains(t, dialog, "Your real git repository is untouched.")
		assert.NotContains(t, dialog, "3 sessions archived",
			"the total must never be reported as if it were all archived")
	})
}

// TestDeleteProjectConfirmRendersConsequencesWhenCompact is the #1973 P1: the
// honest split is worth nothing if the terminal renders it below the fold. The
// overlay clips from the BOTTOM (windowOverlayBody keeps lines[:limit-1]), so a
// dialog that led with the reassuring "N archived (restorable)" pushed the
// non-restorable count off-screen — the user reads the safe half, presses y, and
// loses sessions that were never restorable. That is the very bug this fix
// exists to prevent, reintroduced by the layout.
//
// Both sizes are real: 40x10 is the floor this app DECLARES it supports
// (ui/layout/grid.go HardMinWidth/HardMinHeight — below it ui/fallback.go takes
// over), and 59x14 is where the review gate reproduced the clip. These assert on
// the RENDERED overlay, not the message string handed to it.
//
// The dialog is armed at a roomy size and the terminal is THEN shrunk, because
// that is the only way a user reaches this: below MinimalWidth/MinimalHeight
// (60x15) the layout drops the Projects section entirely, so `D` cannot be
// pressed at 40x10 in the first place. An open dialog, however, re-fits on every
// relayout (relayout -> layoutModalOverlays -> SetMaxSize), so shrinking the
// terminal — or a tmux pane resize — re-renders it at the smaller size with the
// consequences clipped. The dialog outlives the size it was opened at.
func TestDeleteProjectConfirmRendersConsequencesWhenCompact(t *testing.T) {
	mixed := []session.InstanceData{
		deleteProjectSession("alpha", false),
		deleteProjectSession("beta", false),
		deleteProjectSession("root", true),
	}

	for _, tc := range []struct {
		name string
		w, h int
	}{
		{name: "declared floor 40x10", w: 40, h: 10},
		{name: "gate reproduction 59x14", w: 59, h: 14},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := armDeleteProjectDialog(t, mixed)
			// Shrink with the dialog open — the overlay re-fits to the new size.
			resizeHome(h, tc.w, tc.h)
			require.NotNil(t, h.confirmationOverlay, "the dialog must survive the resize")
			dialog := dialogText(h.confirmationOverlay.Render())

			// The destructive fact must survive the clip.
			assert.Contains(t, dialog, "1 in-place session torn down — not restorable",
				"the non-restorable count must render at %dx%d — clipping it is the bug", tc.w, tc.h)
			// And the user must still be able to see what key commits them.
			assert.Regexp(t, `(?i)confirm`, dialog,
				"the confirm prompt must render at %dx%d", tc.w, tc.h)
			// Reading exactly one consequence line must mean reading the dangerous
			// one: the reassuring half is what gives ground, never the reverse.
			if strings.Contains(dialog, "archived — restorable") {
				assert.Less(t, strings.Index(dialog, "not restorable"), strings.Index(dialog, "archived — restorable"),
					"the non-restorable count must lead the restorable one")
			}
			// Nothing may be swallowed in silence: if the elaboration was clipped,
			// the dialog says so rather than looking complete.
			if !strings.Contains(dialog, "Your real git repository is untouched") {
				assert.Contains(t, dialog, "resize to read",
					"clipped detail must be announced, not silently dropped")
			}
			// The dialog must genuinely be confirmable at a supported size.
			require.NotNil(t, h.confirmationOverlay)
			assert.NotContains(t, dialog, "Too small to confirm safely",
				"%dx%d is a supported size — delete must work here, not refuse", tc.w, tc.h)
		})
	}
}

// TestDeleteProjectResultReportsBothCounts closes the loop: after the user
// consents, the completion must report the SAME split the confirmation promised,
// using the daemon's own counts. The TUI used to discard resp.KilledCount, so it
// could only ever say "archived N (restorable)" — false whenever anything was
// torn down (#1973). Drives the full prod chain: confirm → deleteProjectCmd →
// projectDeletedMsg → handleProjectDeleted → the transient notice.
func TestDeleteProjectResultReportsBothCounts(t *testing.T) {
	for _, tc := range []struct {
		name        string
		data        []session.InstanceData
		archived    int
		killed      int
		wantContain []string
		wantAbsent  []string
		// killedLeads asserts the torn-down half comes FIRST. The notice is a
		// single line that the error box clips to the pane width, so the tail is
		// what gets dropped — the half the user must not lose has to lead.
		killedLeads bool
	}{
		{
			name:        "only normal sessions",
			data:        []session.InstanceData{deleteProjectSession("alpha", false), deleteProjectSession("beta", false)},
			archived:    2,
			killed:      0,
			wantContain: []string{"archived 2 sessions (restorable)"},
			wantAbsent:  []string{"tore down"},
		},
		{
			name:        "only in-place sessions",
			data:        []session.InstanceData{deleteProjectSession("root", true)},
			archived:    0,
			killed:      1,
			wantContain: []string{"tore down 1 in-place session (not restorable, worktree and branch untouched)"},
			wantAbsent:  []string{"restorable)", "archived"},
		},
		{
			name:     "mixed project",
			data:     []session.InstanceData{deleteProjectSession("alpha", false), deleteProjectSession("beta", false), deleteProjectSession("root", true)},
			archived: 2,
			killed:   1,
			wantContain: []string{
				"archived 2 sessions (restorable)",
				"tore down 1 in-place session (not restorable, worktree and branch untouched)",
			},
			wantAbsent:  []string{"archived 3"},
			killedLeads: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := armDeleteProjectDialog(t, tc.data)

			// Stub the daemon at the same seam prod dials through, returning the
			// split the daemon would compute for this project.
			var gotRepoID string
			prev := deleteProjectThroughDaemon
			deleteProjectThroughDaemon = func(repoRoot, repoID string) (daemon.DeleteProjectResponse, error) {
				gotRepoID = repoID
				return daemon.DeleteProjectResponse{
					OK:            true,
					ArchivedCount: tc.archived,
					KilledCount:   tc.killed,
				}, nil
			}
			t.Cleanup(func() { deleteProjectThroughDaemon = prev })

			// 'y' confirms; the overlay forwards the start message into the loop.
			model, cmd := h.handleStateConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
			h = model.(*home)
			require.Equal(t, stateDefault, h.state, "confirm must close the dialog")
			require.NotNil(t, cmd, "confirm must forward the start-delete message")
			startMsg, ok := cmd().(startDeleteProjectMsg)
			require.True(t, ok, "confirm must emit startDeleteProjectMsg")

			// The async command → the completion message the handler consumes.
			done, ok := h.deleteProjectCmd(startMsg)().(projectDeletedMsg)
			require.True(t, ok, "deleteProjectCmd must emit projectDeletedMsg")
			require.NotEmpty(t, gotRepoID, "the delete must reach the daemon seam")
			assert.Equal(t, tc.archived, done.archived)
			assert.Equal(t, tc.killed, done.killed,
				"the killed count must survive the daemon→message hop; dropping it is bug #1973")

			model, _ = h.handleProjectDeleted(done)
			h = model.(*home)

			notice := h.errBox.FullError()
			for _, want := range tc.wantContain {
				assert.Contains(t, notice, want, "the result must report what actually happened")
			}
			for _, absent := range tc.wantAbsent {
				assert.NotContains(t, notice, absent, "the result must not overstate what is restorable")
			}
			if tc.killedLeads {
				assert.Less(t, strings.Index(notice, "tore down"), strings.Index(notice, "archived"),
					"the torn-down half must lead: the notice is width-clipped, so a trailing 'not restorable' is the part that disappears")
			}
		})
	}
}

// TestDeleteActiveProjectLeavesStaleScopeAndZombieRow reproduces the bug:
// deleting the project the TUI is currently scoped to neither re-scopes the
// active identity (m.repoID/m.repoRoot) nor drops the now-empty row from the
// Projects section. The line-288 active pre-seed re-derives the deleted
// project as an Active row with SessionCount 0, and the next snapshot poll
// re-zombies it. Expected to FAIL on current code; passes once the bug is
// fixed (either handleDeleteProject refuses the active project, or
// handleProjectDeleted re-scopes on completion).
func TestDeleteActiveProjectLeavesStaleScopeAndZombieRow(t *testing.T) {
	h := newTestHome(t)
	repoID := config.RepoIDFromRoot(deleteProjectTestRoot)
	h.repoRoot = deleteProjectTestRoot
	h.repoID = repoID

	// One live session for the active project, pointing its identity at the
	// active root so the live-session row merges with the line-288 pre-seed.
	live := []session.InstanceData{deleteProjectSession("alpha", false)}
	restoreFetcher := SetAllReposSnapshotFetcherForTest(func() ([]session.InstanceData, error) {
		return live, nil
	})
	t.Cleanup(restoreFetcher)

	resizeHome(h, 120, 45)
	h.refreshSidebarProjects()
	h.relayout()
	require.Len(t, h.projects.Projects(), 1, "snapshot + active pre-seed yield exactly the active project")
	require.True(t, h.projects.Projects()[0].Active, "the active project must be the highlighted row")
	require.Equal(t, repoID, h.projects.Projects()[0].RepoID)

	// Open the picker; NewProjectPickerOverlay pre-selects proj.Root == currentRoot.
	h.showProjectPickerOverlay()
	model, _ := h.handleStateSwitchProject(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})
	h = model.(*home)
	require.Equal(t, stateConfirm, h.state, "delete must open a confirmation")

	// Daemon archives the live session and deregisters the project.
	prev := deleteProjectThroughDaemon
	deleteProjectThroughDaemon = func(root, id string) (daemon.DeleteProjectResponse, error) {
		return daemon.DeleteProjectResponse{OK: true, ArchivedCount: 1}, nil
	}
	t.Cleanup(func() { deleteProjectThroughDaemon = prev })

	model, cmd := h.handleStateConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	h = model.(*home)
	require.NotNil(t, cmd)
	startMsg, ok := cmd().(startDeleteProjectMsg)
	require.True(t, ok)

	done, ok := h.deleteProjectCmd(startMsg)().(projectDeletedMsg)
	require.True(t, ok)

	// Post-delete snapshot: every live session archived -> only archived rows
	// remain, which buildProjectListFrom skips (IsArchivedData, line 148). The
	// registry row is gone. The only remaining source for the repoID is the
	// line-288 active pre-seed.
	SetAllReposSnapshotFetcherForTest(func() ([]session.InstanceData, error) { return nil, nil })

	model, _ = h.handleProjectDeleted(done)
	h = model.(*home)

	// (a) The active scope should be cleared/moved, but is left stale. (FAILS today.)
	assert.Equal(t, "", h.repoID, "BUG (a): repoID is left pinned to the deleted project; expected registry mode (\"\")")
	// (b) Same defect, second field. (FAILS today.)
	assert.Equal(t, "", h.repoRoot, "BUG (b): repoRoot is left pinned to the deleted project; expected registry mode (\"\")")

	// (c) The deleted project should be absent, but reappears as Active with 0 sessions.
	// Use t.Errorf (non-halting) so step (d) below still runs and demonstrates the
	// snapshot-poll re-zombie, which is a separate facet of the same defect —
	// otherwise the first failure would mask the second, and claim (d) would be
	// unverified.
	for _, r := range h.projects.Projects() {
		if r.RepoID == repoID {
			t.Errorf("BUG (c): deleted project repoID %s reappears as Active=%v SessionCount=%d; expected it to be gone", repoID, r.Active, r.SessionCount)
		}
	}

	// (d) A followup snapshot poll does not heal the zombie either. (FAILS today.)
	h.refreshSidebarProjectsFromSnapshot(nil, nil)
	for _, r := range h.projects.Projects() {
		if r.RepoID == repoID {
			t.Errorf("BUG (d): snapshot poll re-derived the zombie row repoID %s Active=%v SessionCount=%d", repoID, r.Active, r.SessionCount)
		}
	}
}

// TestDeleteActiveProjectClosesOpenPanes covers the open-panes facet of the bug
// the report calls out: handleProjectDeleted never called m.store.ResetInstances
// or m.closePaneWindow, so the panes survived as dead attachments after the
// daemon's archive step tore down their tmux sessions. With the re-scope fix the
// panes are closed (releasing their windows and live termpane attachments) and
// the active scope is cleared, mirroring switchProject's teardown.
func TestDeleteActiveProjectClosesOpenPanes(t *testing.T) {
	h := newTestHome(t)
	repoID := config.RepoIDFromRoot(deleteProjectTestRoot)
	h.repoRoot = deleteProjectTestRoot
	h.repoID = repoID

	t.Cleanup(SetAllReposSnapshotFetcherForTest(func() ([]session.InstanceData, error) {
		return []session.InstanceData{deleteProjectSession("alpha", false)}, nil
	}))
	resizeHome(h, 120, 45)
	h.refreshSidebarProjects()

	// Stage a live instance + an open pane on it, exactly the mid-session shape
	// a user is in when they confirm a delete of the active project. openTestPane
	// goes through the real openPaneWindow path so the window ends up in
	// m.paneWindows like production.
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "alpha", Path: deleteProjectTestRoot, Program: "claude",
	})
	require.NoError(t, err)
	inst.SetStatusForTest(session.Running)
	h.store.AddInstance(inst)
	pane := openTestPane(t, h, inst, 0)
	require.Len(t, h.store.OpenPanes(), 1, "precondition: a pane is open on the active project")
	require.NotNil(t, h.paneWindows[pane.ID()], "precondition: the pane has a window")

	prev := deleteProjectThroughDaemon
	deleteProjectThroughDaemon = func(root, id string) (daemon.DeleteProjectResponse, error) {
		return daemon.DeleteProjectResponse{OK: true, ArchivedCount: 1}, nil
	}
	t.Cleanup(func() { deleteProjectThroughDaemon = prev })
	// Post-delete: the daemon archived the project's sessions, so the snapshot
	// the next refreshSidebarProjects reads carries nothing for this repoID.
	SetAllReposSnapshotFetcherForTest(func() ([]session.InstanceData, error) { return nil, nil })

	done := projectDeletedMsg{root: deleteProjectTestRoot, repoID: repoID, name: "acme", archived: 1}
	model, _ := h.handleProjectDeleted(done)
	h = model.(*home)

	assert.Empty(t, h.store.OpenPanes(), "open panes must be torn down on delete of the active project")
	assert.Nil(t, h.paneWindows[pane.ID()], "the pane's window must be dropped from m.paneWindows")
	assert.Equal(t, 0, h.store.NumInstances(), "the projection's instances must be reset")
	assert.Equal(t, "", h.repoID, "the active scope must be cleared")
	assert.Equal(t, "", h.repoRoot, "the active scope must be cleared")
	assert.Equal(t, "", h.sidebar.ProjectName(), "the sidebar project name must be reset to the registry-mode default")
}

// TestDeleteActiveProjectFromSidebarRescopes covers the SECOND entry point the
// report names: the sidebar's own `D`-key handler in app/handle_overlay.go feeds
// m.projects.SelectedProject() straight through to handleDeleteProject, so a
// single re-scope in handleProjectDeleted heals both the picker `D` path (above)
// and this sidebar `D` path. Without the fix the sidebar entry point strands the
// TUI on the deleted identity exactly as the picker path does.
func TestDeleteActiveProjectFromSidebarRescopes(t *testing.T) {
	h := newTestHome(t)
	repoID := config.RepoIDFromRoot(deleteProjectTestRoot)
	h.repoRoot = deleteProjectTestRoot
	h.repoID = repoID

	// The grid hides the Projects section when it holds <= 1 row (grid.go:
	// "a single project reserves no rail rows"), which would make the section
	// unfocusable — so stage a DECOY project alongside the active one. The
	// decoy survives the delete (its session is not archived), proving the
	// re-scope removes only the active row and not the whole section.
	decoyRoot := "/tmp/af-delete-project-decoy/zzz-decoy"
	decoySession := session.InstanceData{
		Title: "decoy",
		Worktree: session.GitWorktreeData{
			RepoPath: decoyRoot, WorktreePath: decoyRoot, SessionName: "decoy",
		},
	}
	t.Cleanup(SetAllReposSnapshotFetcherForTest(func() ([]session.InstanceData, error) {
		return []session.InstanceData{deleteProjectSession("alpha", false), decoySession}, nil
	}))
	resizeHome(h, 120, 45)
	h.refreshSidebarProjects()
	h.relayout() // the grid rebuilds the focus ring's pane entries from the visible sections; project rows must be in the grid before focusRegion can land on RegionProjects.
	require.Len(t, h.projects.Projects(), 2, "precondition: active + decoy so the Projects section is visible")
	// Sorted by basename: "acme" (active) < "zzz-decoy" (decoy); cursor defaults
	// to index 0, which is the active project.
	require.True(t, h.projects.Projects()[0].Active)
	require.Equal(t, repoID, h.projects.Projects()[0].RepoID)

	// Focus the bottom Projects section and press `D` on its cursor's row — the
	// cursor rests on the active project, so SelectedProject() returns the very
	// project the TUI is scoped to.
	h.focusRegion(layout.RegionProjects)
	require.Equal(t, layout.RegionProjects, h.ring.Active())
	proj, ok := h.projects.SelectedProject()
	require.True(t, ok)
	require.Equal(t, repoID, proj.RepoID, "the section's cursor rests on the active project")

	model, _, consumed := h.handleProjectsFocus(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})
	require.True(t, consumed, "`D` on the focused Projects section must route to handleDeleteProject")
	h = model.(*home)
	require.Equal(t, stateConfirm, h.state, "delete must open a confirmation from the sidebar entry point")
	require.NotNil(t, h.confirmationOverlay)

	prev := deleteProjectThroughDaemon
	deleteProjectThroughDaemon = func(root, id string) (daemon.DeleteProjectResponse, error) {
		return daemon.DeleteProjectResponse{OK: true, ArchivedCount: 1}, nil
	}
	t.Cleanup(func() { deleteProjectThroughDaemon = prev })
	// Post-delete snapshot: the daemon archived the active project's sessions;
	// the decoy's session survives, so the section still holds the decoy row —
	// and the re-scope must have removed the active row, not the whole section.
	SetAllReposSnapshotFetcherForTest(func() ([]session.InstanceData, error) {
		return []session.InstanceData{decoySession}, nil
	})

	// `y` confirms; the overlay forwards startDeleteProjectMsg into the loop.
	model, confirmCmd := h.handleStateConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	h = model.(*home)
	require.NotNil(t, confirmCmd)
	startMsg, ok := confirmCmd().(startDeleteProjectMsg)
	require.True(t, ok)

	done, ok := h.deleteProjectCmd(startMsg)().(projectDeletedMsg)
	require.True(t, ok)
	model, _ = h.handleProjectDeleted(done)
	h = model.(*home)

	// The sidebar entry point must re-scope the same way the picker path does;
	// both flow through the single handleProjectDeleted re-scope.
	assert.Equal(t, "", h.repoID, "the sidebar `D` path must also clear the active scope")
	assert.Equal(t, "", h.repoRoot)
	for _, r := range h.projects.Projects() {
		if r.RepoID == repoID {
			t.Errorf("BUG: deleted project repoID %s reappears via the sidebar entry point (Active=%v count=%d)", repoID, r.Active, r.SessionCount)
		}
	}
}

// TestDeleteNonActiveProjectLeavesActiveScopeIntact is the regression guard for
// the re-scope fix: deleting a project the TUI is NOT scoped to must leave the
// active scope untouched. The existing tests in this file drive handleProjectDeleted
// against a project the test home happens not to be scoped to, but only assert
// the result-message copy — never the scope fields — so a fix that over-reaches
// and re-scopes on EVERY delete would sail past them. This pins the boundary:
// re-scope fires on `msg.repoID == m.repoID` and nothing else.
func TestDeleteNonActiveProjectLeavesActiveScopeIntact(t *testing.T) {
	h := newTestHome(t)
	const activeRoot = "/repos/still-active"
	const activeID = "still-active-repo-id"
	h.repoRoot = activeRoot
	h.repoID = activeID
	// Mirror what switchProject / newHome would do for an active project: the
	// sidebar's title chip reads the active project's basename, so the post-
	// delete assertion against h.sidebar.ProjectName() has a non-empty baseline
	// to compare against.
	h.sidebar.SetProjectName(filepath.Base(activeRoot))

	deletedRoot := "/repos/to-delete"
	deletedID := config.RepoIDFromRoot(deletedRoot)
	t.Cleanup(SetAllReposSnapshotFetcherForTest(func() ([]session.InstanceData, error) {
		return []session.InstanceData{{
			Title: "beta",
			Worktree: session.GitWorktreeData{
				RepoPath:     deletedRoot,
				WorktreePath: deletedRoot,
				SessionName:  "beta",
			},
		}}, nil
	}))
	resizeHome(h, 120, 45)
	h.refreshSidebarProjects()
	require.Len(t, h.projects.Projects(), 2, "precondition: the active project and the to-delete project are both listed")
	var sawDeleted bool
	for _, r := range h.projects.Projects() {
		if r.RepoID == deletedID {
			sawDeleted = true
		}
	}
	require.True(t, sawDeleted, "precondition: the to-delete project is in the section before delete")

	prev := deleteProjectThroughDaemon
	deleteProjectThroughDaemon = func(root, id string) (daemon.DeleteProjectResponse, error) {
		return daemon.DeleteProjectResponse{OK: true, ArchivedCount: 1}, nil
	}
	t.Cleanup(func() { deleteProjectThroughDaemon = prev })
	// Post-delete: the daemon archived the to-delete project's sessions, so the
	// snapshot no longer carries it; the active project's pre-seed is unaffected.
	SetAllReposSnapshotFetcherForTest(func() ([]session.InstanceData, error) { return nil, nil })

	done := projectDeletedMsg{root: deletedRoot, repoID: deletedID, name: "to-delete", archived: 1}
	model, _ := h.handleProjectDeleted(done)
	h = model.(*home)

	assert.Equal(t, activeID, h.repoID, "deleting a non-active project must NOT clear the active scope")
	assert.Equal(t, activeRoot, h.repoRoot)
	assert.Equal(t, filepath.Base(activeRoot), h.sidebar.ProjectName(), "the active sidebar project name must stay")

	// The deleted non-active project must leave the Projects section (it was a
	// live-session row, not the active-root pre-seed), and the active project
	// must remain.
	var stillActive bool
	for _, r := range h.projects.Projects() {
		assert.NotEqual(t, deletedID, r.RepoID, "the deleted non-active project must leave the list")
		if r.RepoID == activeID {
			stillActive = true
		}
	}
	assert.True(t, stillActive, "the active project must remain in the section after a non-active delete")
}

// TestDeleteActiveProjectReScopesOnCommittedWarning covers procedure 6 of the
// test plan: when the daemon's DeleteProject returns a mutation-COMMITTED error
// (the archive transaction landed but a non-transactional follow-up such as the
// registry-entry removal failed), handleProjectDeleted must BOTH re-scope the
// TUI to registry mode (the archive is durable, so the active project's
// sessions are gone) AND surface the committed warning via handleError. The
// existing tea.Batch(success, m.handleError(msg.err)) branch in handleProjectDeleted
// is unchanged by the fix — but it must run ALONGSIDE the re-scope, not instead
// of it. Killing a real daemon mid-delete yields a transport error classified
// as a hard failure (not committed), so the committed-warning path is exercised
// here through the same deleteProjectThroughDaemon seam the bug report's
// reproduction test stubs.
func TestDeleteActiveProjectReScopesOnCommittedWarning(t *testing.T) {
	h := newTestHome(t)
	repoID := config.RepoIDFromRoot(deleteProjectTestRoot)
	h.repoRoot = deleteProjectTestRoot
	h.repoID = repoID

	t.Cleanup(SetAllReposSnapshotFetcherForTest(func() ([]session.InstanceData, error) {
		return []session.InstanceData{deleteProjectSession("alpha", false)}, nil
	}))
	resizeHome(h, 120, 45)
	h.refreshSidebarProjects()
	require.Len(t, h.projects.Projects(), 1, "precondition: the active project is the section's only row")
	require.True(t, h.projects.Projects()[0].Active)

	const warningText = "registry removal failed after archive committed"
	prev := deleteProjectThroughDaemon
	deleteProjectThroughDaemon = func(root, id string) (daemon.DeleteProjectResponse, error) {
		return daemon.DeleteProjectResponse{OK: true, ArchivedCount: 1}, &testMutationCommittedError{msg: warningText}
	}
	t.Cleanup(func() { deleteProjectThroughDaemon = prev })
	// Post-delete snapshot: the daemon archived alpha, so the snapshot no
	// longer carries it; the registry entry may or may not be gone (the
	// committed warning's whole point is that the non-transactional follow-up
	// failed), but the re-scope fires identically either way.
	SetAllReposSnapshotFetcherForTest(func() ([]session.InstanceData, error) { return nil, nil })

	startMsg := startDeleteProjectMsg{root: deleteProjectTestRoot, repoID: repoID, name: "acme"}
	done, ok := h.deleteProjectCmd(startMsg)().(projectDeletedMsg)
	require.True(t, ok, "deleteProjectCmd must emit projectDeletedMsg")
	require.NotNil(t, done.err, "the committed error must propagate through deleteProjectCmd")
	require.True(t, apiclient.IsMutationCommitted(done.err),
		"the stubbed error must classify as mutation-committed (apiproto.MutationCommittedError)")

	model, _ := h.handleProjectDeleted(done)
	h = model.(*home)

	// (a) The re-scope fires EVEN on a committed warning: the archive is
	// durable, so the active project's sessions are gone and the TUI must not
	// stay pinned to the deleted identity.
	assert.Equal(t, "", h.repoID, "the active scope must be cleared even when the delete returned a committed warning")
	assert.Equal(t, "", h.repoRoot, "the active scope must be cleared even when the delete returned a committed warning")
	for _, r := range h.projects.Projects() {
		if r.RepoID == repoID {
			t.Errorf("BUG: the deleted active project must not reappear as a zombie row on the committed-warning path (Active=%v count=%d)", r.Active, r.SessionCount)
		}
	}

	// (b) The committed warning surfaces to the user via handleError. Both
	// showTransientMessage and handleError are returned as batched cmds, but
	// each also runs an inline side-effect on the errBox — setTransientNotice
	// then setTransientFailure — so the failure (warning) is the final
	// user-visible state, exactly as in production. The errBox carries the
	// warning text, confirming the batched handleError cmd was constructed
	// alongside the re-scope (the fix must not suppress the warning).
	notice := h.errBox.FullError()
	assert.Contains(t, notice, warningText, "the committed warning must surface via handleError alongside the re-scope")
}

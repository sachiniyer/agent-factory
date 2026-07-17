package app

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"
)

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
// the box-drawing frame and collapses the wrap, so an assertion can match a
// whole phrase that the overlay happened to break across two lines (the border
// glyphs otherwise land mid-sentence and every multi-word match silently fails).
func dialogText(rendered string) string {
	frame := strings.NewReplacer(
		"│", " ", "─", " ", "╭", " ", "╮", " ", "╰", " ", "╯", " ",
	)
	return strings.Join(strings.Fields(frame.Replace(rendered)), " ")
}

// armDeleteProjectDialog drives the REAL prod gate up to the confirmation: it
// feeds the snapshot through buildProjectListFrom (the derivation that populates
// InPlaceCount), pushes the rows into the Projects section, focuses it, and
// presses the actual delete-project key on the cursor's row — rather than
// hand-building a ui.SidebarProject or calling the message builder directly.
// Returns the home and the dialog text as the user sees it, unwrapped.
func armDeleteProjectDialog(t *testing.T, data []session.InstanceData) (*home, string) {
	t.Helper()
	h := newTestHome(t)
	t.Cleanup(SetAllReposSnapshotFetcherForTest(func() ([]session.InstanceData, error) {
		return data, nil
	}))
	// Roomy window so the dialog body is never windowed away by the overlay's
	// height fit — the assertions are about the copy, not the viewport.
	resizeHome(h, 120, 45)

	// The section's rows come from the same discovery prod uses on launch, poll,
	// and project switch. This is what makes the test cover the derivation.
	h.refreshSidebarProjects()
	require.Len(t, h.projects.Projects(), 1, "the snapshot must yield exactly the test project")

	h.focusRegion(layout.RegionProjects)
	require.Equal(t, layout.RegionProjects, h.ring.Active())

	// 'D' on the focused Projects section — the binding handleProjectsFocus routes
	// to handleDeleteProject in prod.
	model, _, consumed := h.handleProjectsFocus(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})
	require.True(t, consumed, "the delete-project key must be consumed by the focused Projects section")
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

		assert.Contains(t, dialog, "2 sessions are archived and restorable",
			"a project of ordinary sessions is fully restorable and must say so")
		assert.Contains(t, dialog, "af sessions restore")
		assert.NotContains(t, dialog, "torn down and cannot be restored",
			"nothing is killed here — the dialog must not invent a scary split")
		assert.NotContains(t, dialog, "in-place")
	})

	t.Run("only in-place sessions are torn down and not restorable", func(t *testing.T) {
		_, dialog := armDeleteProjectDialog(t, []session.InstanceData{
			deleteProjectSession("root", true),
		})

		assert.Contains(t, dialog, "1 in-place session is torn down and cannot be restored",
			"an in-place session is killed; the user must be told before consenting")
		// The honest half of the split: the kill does not destroy their work.
		// GitWorktree.Cleanup() no-ops for an external worktree, so the branch and
		// uncommitted changes survive — only the session and its agent are gone.
		assert.Contains(t, dialog, "stay exactly where they are")
		assert.Contains(t, dialog, "the session and its agent are gone")
		assert.NotContains(t, dialog, "archived and restorable",
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
		assert.Contains(t, dialog, "2 sessions are archived and restorable",
			"the archived count must exclude the in-place session")
		assert.Contains(t, dialog, "The other 1 in-place session is torn down and cannot be restored",
			"the killed count must be stated with its consequence")
		assert.Contains(t, dialog, "Your real git repository is untouched.")
		assert.NotContains(t, dialog, "3 sessions are archived",
			"the total must never be reported as if it were all archived")
	})
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

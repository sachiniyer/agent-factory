package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/stretchr/testify/require"
)

// TestProjectsFocusActionGroupMatchesDesign pins the #1623 action-group design
// for the Projects footer: its highlighted "action" pair is [SwitchProjectRow,
// Search] and its muted "chrome" tail is [DeleteProject, Tab, Help, Quit].
//
// Commit 48784b95 (#1740) inserted KeyDeleteProject at index 1 of
// projectsMenuOptions, shifting KeySearch to index 2, so the action range
// {start:0, end:2} silently covered [SwitchProjectRow, DeleteProject] instead
// of [SwitchProjectRow, Search]. The fix reorders the slice; this test guards
// against both the original drift and a future reorder re-introducing it.
func TestProjectsFocusActionGroupMatchesDesign(t *testing.T) {
	for _, rows := range []bool{false, true} {
		t.Run("rows="+boolStr(rows), func(t *testing.T) {
			m := NewMenu()
			m.SetInstance(readyUIInstance())
			m.SetFocusRegion(layout.RegionProjects)
			m.SetProjectRowsAvailable(rows)

			if rows {
				// With rows, the action group must be exactly [SwitchProjectRow, Search]
				// and the chrome group must contain DeleteProject.
				require.Len(t, m.groups, 2)
				action := m.groups[0]
				require.True(t, action.isAction, "first group must be the action group")

				opts := m.projectsOptions()
				require.Equal(t, 0, action.start)
				require.Equal(t, 2, action.end)
				require.Equal(t, []keys.KeyName{keys.KeySwitchProjectRow, keys.KeySearch},
					opts[action.start:action.end],
					"action group must be [SwitchProjectRow, Search]")

				chrome := m.groups[1]
				require.False(t, chrome.isAction)
				require.Equal(t, action.end, chrome.start)
				require.Equal(t, len(opts), chrome.end)
				require.Contains(t, opts[chrome.start:chrome.end], keys.KeyDeleteProject,
					"DeleteProject must be in the muted chrome group, not the action group")
				require.NotContains(t, opts[action.start:action.end], keys.KeyDeleteProject,
					"DeleteProject must NOT be in the action group")
				require.Contains(t, opts[action.start:action.end], keys.KeySearch,
					"Search must be in the action group")
			} else {
				// Without rows, the row verbs are dropped and actionEnd is 0, so
				// every surviving option is chrome. Verify the gate did not strand
				// Search in the action group while dropping the row verbs.
				require.Len(t, m.groups, 2)
				require.Equal(t, 0, m.groups[0].end, "no rows means an empty action group")
				require.False(t, m.groups[1].isAction)
				for _, leak := range []keys.KeyName{keys.KeySwitchProjectRow, keys.KeyDeleteProject} {
					require.NotContains(t, m.options, leak,
						"row verb %v must be hidden with no rows", leak)
				}
				require.Contains(t, m.options, keys.KeySearch,
					"Search must survive the gate (it is not a row verb)")
			}
		})
	}
}

// TestProjectsFocusDeleteProjectDescriptionIsMuted verifies the rendered SGR
// styling that the group boundary actually controls. Because keyStyle and
// actionGroupStyle are identical (both Bold+Accent), the only visible effect of
// group membership is the description-label style: actionGroupStyle (Bold +
// Accent) for action hints, descStyle (plain Ink, no bold) for chrome hints.
//
// After the fix, "delete project" must render as chrome (Ink, no bold) and
// "search" must render as an action (Bold + Accent). Before the fix they were
// inverted.
func TestProjectsFocusDeleteProjectDescriptionIsMuted(t *testing.T) {
	profile, dark := lipgloss.ColorProfile(), lipgloss.HasDarkBackground()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile); lipgloss.SetHasDarkBackground(dark) })

	for _, mode := range []bool{false, true} {
		lipgloss.SetHasDarkBackground(mode)
		roles := CurrentTheme()

		accent := termenv.TrueColor.FromColor(roles.Accent).Sequence(false)
		ink := termenv.TrueColor.FromColor(roles.Ink).Sequence(false)

		m := NewMenu()
		m.SetInstance(readyUIInstance())
		m.SetFocusRegion(layout.RegionProjects)
		m.SetProjectRowsAvailable(true)
		m.SetSize(200, 1)
		out := m.String()

		// Build the exact styled description strings the renderer emits, then
		// check for/against them in the raw (SGR-laden) output.
		actionDesc := actionGroupStyle.Render("search")
		chromeDesc := descStyle.Render("delete project")
		switchDesc := actionGroupStyle.Render("switch")

		require.Contains(t, out, actionDesc,
			"the 'search' hint must carry the action-group style (Bold + Accent), got:\n%s", out)
		require.Contains(t, out, chromeDesc,
			"the 'delete project' hint must carry the chrome style (plain Ink), got:\n%s", out)
		require.Contains(t, out, switchDesc,
			"the 'switch' hint must carry the action-group style (Bold + Accent), got:\n%s", out)

		// The inversion guard: "delete project" must NOT appear in the action
		// style, and "search" must NOT appear in the chrome style.
		require.NotContains(t, out, actionGroupStyle.Render("delete project"),
			"'delete project' must NOT be styled as an action (Bold + Accent), got:\n%s", out)
		require.NotContains(t, out, descStyle.Render("search"),
			"'search' must NOT be styled as chrome (plain Ink), got:\n%s", out)

		// Sanity-check the SGR sequences for this mode to confirm the test is
		// really pinning distinct colours: the Accent and Ink sequences must
		// differ and must both be present.
		if accent == ink {
			t.Fatalf("Accent and Ink sequences must differ for dark=%v", mode)
		}
		require.True(t, strings.Contains(out, accent) && strings.Contains(out, ink),
			"both Accent and Ink SGR sequences must be present in the output; dark=%v\n%s", mode, out)
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

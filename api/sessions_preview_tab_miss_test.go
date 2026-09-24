package api

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
)

// setPreviewTabFlags sets the package-level selector flags for a test and
// restores their previous values on cleanup. previewTabMissErr reads these
// directly (it is a zero-argument function with no *cobra.Command, so it
// cannot consult Flags().Changed), so these tests exercise it by mutating the
// same variables the bound flags write.
func setPreviewTabFlags(t *testing.T, tab int, name, id string) {
	t.Helper()
	prevTab, prevName, prevID := previewTabFlag, previewTabNameFlag, previewTabIDFlag
	previewTabFlag, previewTabNameFlag, previewTabIDFlag = tab, name, id
	t.Cleanup(func() {
		previewTabFlag, previewTabNameFlag, previewTabIDFlag = prevTab, prevName, prevID
	})
}

// TestPreviewTabMissErr_NamesTheSelectorTheUserPassed is the regression guard
// for the close-during-preview race the daemon routes into previewTabMissErr.
//
// The daemon maps a resolved-then-closed tab to TabGone selector-agnostically
// (daemon/preview.go: as.PreviewByID -> ErrTabGone), and the CLI routes TabGone
// into previewTabMissErr unconditionally. The branches here must mirror
// ResolveTabIndex's precedence (id, then name, then ordinal, session/tab.go) so
// the message names the FIRST selector the user supplied. Before the fix there
// was no --tab-name branch, so a name-only miss fell through to "--tab %d" with
// the default 0 — blaming a flag the user never passed and a slot (the agent
// tab, never closeable) that is provably still present.
func TestPreviewTabMissErr_NamesTheSelectorTheUserPassed(t *testing.T) {
	cases := []struct {
		name    string
		tab     int
		tabName string
		tabID   string
		want    string
	}{
		{
			// The bug: a --tab-name that resolved then closed mid-capture is
			// mapped to TabGone, and the miss must name --tab-name.
			name:    "tab-name only (race-promoted TabGone) names --tab-name, not default --tab 0",
			tabName: "work",
			want:    `--tab-name "work" matches no live tab in this session; it may have been closed mid-capture`,
		},
		{
			name: "tab only names --tab",
			tab:  3,
			want: "--tab 3 is not a slot in this session",
		},
		{
			// No selector defaults --tab to 0 (the agent tab); the existing arm.
			name: "no selector names default --tab 0 (agent tab)",
			tab:  0,
			want: "--tab 0 is not a slot in this session",
		},
		{
			name:  "tab-id only names --tab-id",
			tabID: "tab-abc",
			want:  `--tab-id "tab-abc" matches no tab in this session; it may have been closed`,
		},
		{
			// Precedence mirrors ResolveTabIndex: tabID is checked first.
			name:    "tab-id wins over tab-name (id precedence)",
			tabID:   "tab-abc",
			tabName: "work",
			want:    `--tab-id "tab-abc" matches no tab in this session; it may have been closed`,
		},
		{
			// The dual-selector case from the report: --tab N --tab-name X (no
			// --tab-id) resolves by name and ignores N, so the miss must name
			// --tab-name X, not the companion --tab N the daemon never used.
			name:    "tab-name wins over --tab N (name precedence over ordinal)",
			tab:     3,
			tabName: "work",
			want:    `--tab-name "work" matches no live tab in this session; it may have been closed mid-capture`,
		},
		{
			// Full precedence: id -> name -> ordinal.
			name:    "tab-id wins over both --tab-name and --tab (id -> name -> ordinal)",
			tab:     3,
			tabName: "work",
			tabID:   "tab-abc",
			want:    `--tab-id "tab-abc" matches no tab in this session; it may have been closed`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setPreviewTabFlags(t, tc.tab, tc.tabName, tc.tabID)
			require.EqualError(t, previewTabMissErr(), tc.want)
		})
	}
}

// previewTabGoneError drives the real sessionsPreviewCmd.RunE against a daemon
// transport stubbed to return the TabGone signal the daemon emits when the
// addressed tab is gone — including a name that resolved then closed mid-capture
// (as.PreviewByID -> ErrTabGone, daemon/preview.go). The full CLI error path
// (snapshot.TabGone check -> previewTabMissErr -> jsonError -> stderr) runs
// unmodified; only the daemon transport is stubbed. It returns the {"error":...}
// message a human or script sees on stderr.
func previewTabGoneError(t *testing.T, tab int, tabName, tabID string) string {
	t.Helper()
	setupRepoForCmd(t)
	setPreviewTabFlags(t, tab, tabName, tabID)
	prevPreview := previewSessionViaDaemon
	previewSessionViaDaemon = func(daemon.PreviewRequest) (daemon.PreviewResponse, error) {
		return daemon.PreviewResponse{Gone: true, TabGone: true}, nil
	}
	t.Cleanup(func() { previewSessionViaDaemon = prevPreview })

	var runErr error
	stderr := captureStderr(t, func() {
		runErr = sessionsPreviewCmd.RunE(sessionsPreviewCmd, []string{"worker"})
	})
	require.Error(t, runErr, "a tab-level miss is a non-zero exit, not a dead session")
	var envelope struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(stderr), &envelope),
		"stderr must be the bare {\"error\":...} envelope, got %q", stderr)
	return envelope.Error
}

// TestPreviewTabMissErr_TabNameOnly_RacePromotedTabGone is the close-during-preview
// race reproduction at the CLI layer: with only --tab-name supplied, the daemon's
// TabGone response must name --tab-name (the selector the user passed), not fall
// through to "--tab 0 is not a slot" — a flag the user never passed and a slot (the
// agent tab) that is provably still present. It exercises the full path
// (snapshot.TabGone check -> previewTabMissErr -> jsonError -> stderr) the unit
// test above does not.
func TestPreviewTabMissErr_TabNameOnly_RacePromotedTabGone(t *testing.T) {
	got := previewTabGoneError(t, 0, "work", "")
	require.Equal(t, `--tab-name "work" matches no live tab in this session; it may have been closed mid-capture`, got)
	require.NotContains(t, got, "--tab 0",
		"a --tab-name selector must not be misattributed to the unused --tab default")
	require.NotContains(t, got, "is not a slot",
		"the agent tab slot (index 0, which is never closeable) must not be named as gone")
}

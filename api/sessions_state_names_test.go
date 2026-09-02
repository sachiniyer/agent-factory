package api

import (
	"encoding/json"
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"

	"github.com/stretchr/testify/require"
)

// The round trip #3631 asked for: list, read a row's own words, filter on them,
// get that row back.
//
// Before this, `af sessions list --help` documented six status words and
// `af sessions list` printed integers, so a script had to hardcode enum numbers
// that no document defined in order to act on what it had just read.
//
// Each row here is shaped the way the daemon actually projects one
// (ToInstanceData writes the composed legacy Status beside the liveness), which
// is what makes the divergence in the limit-reached row real rather than
// invented: composeStatus has no legacy value for LiveLimitReached and settles
// for Ready, so `status`/`status_name` honestly report the legacy axis while
// `liveness_name` carries the state the filter actually matched on. That is the
// case the twin fields exist for, so it is asserted rather than avoided.
var stateNameRows = []struct {
	title        string
	data         session.InstanceData
	statusName   string
	livenessName string
}{
	{"working", session.InstanceData{Title: "working", Status: session.Running, Liveness: session.LiveRunning}, "running", "running"},
	{"idle", session.InstanceData{Title: "idle", Status: session.Ready, Liveness: session.LiveReady}, "ready", "ready"},
	{"vanished", session.InstanceData{Title: "vanished", Status: session.Lost, Liveness: session.LiveLost}, "lost", "lost"},
	{"corpse", session.InstanceData{Title: "corpse", Status: session.Dead, Liveness: session.LiveDead}, "dead", "dead"},
	{"shelved", session.InstanceData{Title: "shelved", Status: session.Archived, Liveness: session.LiveArchived}, "archived", "archived"},
	{"walled", session.InstanceData{Title: "walled", Status: session.Ready, Liveness: session.LiveLimitReached}, "ready", "limit-reached"},
	// A pre-#1195 record: no `liveness` key at all, its state only in the legacy
	// int. The filter resolves it through the same fallback the name does, so it
	// must still be selectable by, and reported as, a real word.
	{"legacy", session.InstanceData{Title: "legacy", Status: session.Archived}, "archived", "archived"},
}

// TestSessionsList_StatusFilterRoundTripsThroughTheReportedNames is the
// end-to-end claim: for every word `--status` accepts, the rows it returns are
// exactly the rows whose own `liveness_name` is that word.
func TestSessionsList_StatusFilterRoundTripsThroughTheReportedNames(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	t.Chdir(mkRepo(t, "names"))

	all := make([]session.InstanceData, 0, len(stateNameRows))
	for _, row := range stateNameRows {
		all = append(all, row.data)
	}

	// Every word the flag advertises is exercised, derived from the vocabulary
	// itself so a value added later is covered here without an edit.
	words := session.LivenessNameList()
	require.NotEmpty(t, words)

	for _, word := range words {
		t.Run(word, func(t *testing.T) {
			stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
				return all, nil
			})
			setSessionsListFlag(t, "status", word)

			var want []string
			for _, row := range stateNameRows {
				if row.livenessName == word {
					want = append(want, row.title)
				}
			}
			require.NotEmpty(t, want, "the fixture must cover %q", word)

			got := listSessionsAsMaps(t)

			var titles []string
			for _, row := range got {
				titles = append(titles, row["title"].(string))
				require.Equal(t, word, row["liveness_name"],
					"--status %s must return only rows that report themselves as %s", word, word)
			}
			require.ElementsMatch(t, want, titles)
		})
	}
}

// TestSessionsList_EveryRowNamesItsOwnIntegers pins the payload contract on the
// real CLI path: both twins present on every row, spelling the integers beside
// them, with the integers themselves untouched.
func TestSessionsList_EveryRowNamesItsOwnIntegers(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	t.Chdir(mkRepo(t, "names-all"))

	all := make([]session.InstanceData, 0, len(stateNameRows))
	for _, row := range stateNameRows {
		all = append(all, row.data)
	}
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) { return all, nil })

	got := listSessionsAsMaps(t)
	require.Len(t, got, len(stateNameRows))

	byTitle := map[string]map[string]any{}
	for _, row := range got {
		byTitle[row["title"].(string)] = row
	}
	for _, want := range stateNameRows {
		row := byTitle[want.title]
		require.NotNil(t, row, "row %q must be listed", want.title)
		require.Equal(t, float64(want.data.Status), row["status"], "the integer keeps its type and value")
		require.Equal(t, want.statusName, row["status_name"], "row %q", want.title)
		require.Equal(t, want.livenessName, row["liveness_name"], "row %q", want.title)
	}

	// The legacy row proves the twins are DERIVED, not stored: its record carries
	// no `liveness` key at all, and it is still named.
	legacy := byTitle["legacy"]
	require.NotContains(t, legacy, "liveness", "the fixture's legacy row must have no liveness key")
	require.Equal(t, "archived", legacy["liveness_name"])
}

// TestSessionsList_TabKindNamesMatchTheTabKindsArray closes the in-document
// inconsistency: `kind` meant an integer in `tabs` and a word in `tab_kinds`,
// eleven lines apart in one payload.
func TestSessionsList_TabKindNamesMatchTheTabKindsArray(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	t.Chdir(mkRepo(t, "tab-names"))

	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return []session.InstanceData{{
			Title:    "tabs",
			Liveness: session.LiveReady,
			Tabs: []session.TabData{
				{Name: "agent", Kind: session.TabKindAgent},
				{Name: "shell", Kind: session.TabKindShell},
				{Name: "vscode", Kind: session.TabKindVSCode},
			},
			TabKinds: []session.TabKindAllowance{{Kind: "vscode", Allowed: true}},
		}}, nil
	})

	got := listSessionsAsMaps(t)
	require.Len(t, got, 1)

	tabs := got[0]["tabs"].([]any)
	require.Len(t, tabs, 3)
	var names []string
	for _, raw := range tabs {
		names = append(names, raw.(map[string]any)["kind_name"].(string))
	}
	require.Equal(t, []string{"agent", "shell", "vscode"}, names)

	// The same word, in both arrays, for the same concept.
	allowances := got[0]["tab_kinds"].([]any)
	require.Len(t, allowances, 1)
	require.Equal(t, "vscode", allowances[0].(map[string]any)["kind"])
	require.Equal(t, tabs[2].(map[string]any)["kind_name"], allowances[0].(map[string]any)["kind"])
}

// TestSessionsGet_WarningPayloadKeepsBothHalves guards the embedding hazard
// InstanceData.MarshalJSON introduces: sessionGetResult embeds InstanceData, so
// the promoted marshaler would encode a bare session and silently drop
// `warnings` — the ambiguity-widening notice #3511 added.
func TestSessionsGet_WarningPayloadKeepsBothHalves(t *testing.T) {
	data := session.InstanceData{Title: "widened", Status: session.Ready, Liveness: session.LiveLimitReached}

	raw, err := json.Marshal(sessionGetPayload(&data, "some projects could not be read"))
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))
	require.Equal(t, "widened", got["title"], "the session half must survive")
	require.Equal(t, []any{"some projects could not be read"}, got["warnings"], "and so must the notice")
	require.Equal(t, "ready", got["status_name"])
	require.Equal(t, "limit-reached", got["liveness_name"])

	// No notice: byte-identical to the bare record, as before (#3511).
	bare, err := json.Marshal(sessionGetPayload(&data, ""))
	require.NoError(t, err)
	direct, err := json.Marshal(data)
	require.NoError(t, err)
	require.Equal(t, direct, bare)
}

// listSessionsAsMaps runs `sessions list` and decodes it as raw JSON objects.
// The sibling helper decodes into []session.InstanceData, which cannot see these
// fields at all: they are derived at marshal time and absent from the struct, so
// a typed decode would report an empty map and prove nothing.
func listSessionsAsMaps(t *testing.T) []map[string]any {
	t.Helper()
	var got []map[string]any
	require.NoError(t, json.Unmarshal(captureJSON(t, func() error {
		return sessionsListCmd.RunE(sessionsListCmd, nil)
	}), &got))
	return got
}

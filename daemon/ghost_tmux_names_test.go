package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/sachiniyer/agent-factory/session"
)

// TestGhostTmuxNames_OrderDedupeAndEmptySkip pins the contract ghostCleanup
// depends on: the legacy agent-tab name first, then each tab in persisted order,
// deduped, with empty names dropped.
//
// This is a REGRESSION GUARD, not a fail-first repro. #2036 changed only the two
// make() capacity hints (dropping a `+1` that CodeQL flagged as a possible
// allocation-size overflow), which is behaviour-neutral by construction — there
// is no input for which the old and new code return different names, so no test
// can fail on the old code. What this pins is that the restructure did not
// disturb the observable contract, and that a future re-tune of the pre-size
// cannot either.
//
// TestGhostCleanup_KillsEveryTabTmux covers the same function end-to-end against
// a real tmux server, but it asserts the OUTCOME (every tab's session is dead),
// which is order- and dedupe-blind. This asserts the list itself, needs no tmux
// server, and names the property the doc comment claims.
func TestGhostTmuxNames_OrderDedupeAndEmptySkip(t *testing.T) {
	tests := []struct {
		name string
		data *session.InstanceData
		want []string
	}{
		{
			// Pre-#953: no Tabs at all. This is the case the dropped `+1` used to
			// pre-size for, so it is the one where the new cap (0) must still grow
			// to hold the legacy name.
			name: "pre-#953 record yields just the legacy name",
			data: &session.InstanceData{TmuxName: "af_solo"},
			want: []string{"af_solo"},
		},
		{
			// Post-#953: the agent tab is repeated in BOTH TmuxName and Tabs[0].
			// The dedupe must collapse it to a single kill, or ghostCleanup would
			// kill an already-dead session and read the second failure as "not
			// confirmed dead" — which strands the workspace (#1917).
			name: "post-#953 record collapses the repeated agent tab",
			data: &session.InstanceData{
				TmuxName: "af_multi",
				Tabs: []session.TabData{
					{Name: "agent", Kind: session.TabKindAgent, TmuxName: "af_multi"},
					{Name: "shell", Kind: session.TabKindShell, TmuxName: "af_multi__shell"},
					{Name: "btop", Kind: session.TabKindProcess, TmuxName: "af_multi__btop"},
				},
			},
			want: []string{"af_multi", "af_multi__shell", "af_multi__btop"},
		},
		{
			// A tab that never got a tmux session (e.g. a web tab) carries an empty
			// name; killing "" would target the caller's own server-wide default.
			name: "empty tab names are skipped",
			data: &session.InstanceData{
				TmuxName: "af_web",
				Tabs: []session.TabData{
					{Name: "agent", Kind: session.TabKindAgent, TmuxName: "af_web"},
					{Name: "docs", Kind: session.TabKindWeb, TmuxName: ""},
					{Name: "shell", Kind: session.TabKindShell, TmuxName: "af_web__shell"},
				},
			},
			want: []string{"af_web", "af_web__shell"},
		},
		{
			// An empty legacy name must not become a leading "" entry either.
			name: "empty legacy name is skipped, tabs still ordered",
			data: &session.InstanceData{
				TmuxName: "",
				Tabs: []session.TabData{
					{Name: "shell", Kind: session.TabKindShell, TmuxName: "af_orphan__shell"},
					{Name: "btop", Kind: session.TabKindProcess, TmuxName: "af_orphan__btop"},
				},
			},
			want: []string{"af_orphan__shell", "af_orphan__btop"},
		},
		{
			name: "a record with nothing to kill yields no names",
			data: &session.InstanceData{},
			want: []string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Equal (not ElementsMatch) is deliberate: persisted order is part of
			// the contract, and the legacy name leads.
			assert.Equal(t, tc.want, ghostTmuxNames(tc.data))
		})
	}
}

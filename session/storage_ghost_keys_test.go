package session

import (
	"encoding/json"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/require"
)

// TestSaveInstances_LegacyTransientGhostWithTabReaped is the derived-field
// carve-out for the unrecognized-field detector (#NX_): a disposable
// Loading/Deleting row carrying a tab is still the transient ghost it always
// was, because the tab's `kind_name` is a wire field this binary's own
// TabData.MarshalJSON derives (not a struct tag reflection discovers) and the
// detector's recursive walk would otherwise treat it as a future durable marker
// the binary does not know. Without registering the derived key the row would
// be preserved verbatim instead of reaped, keeping a Deleting variant's title
// reserved in findTitleConflictLocked across every checkpoint. The derived
// `kind_name` is registered for both tabs and pending_tabs, so the row is reaped
// for both a tab and a pending_tab; a tab carrying a genuinely unknown field is
// still preserved verbatim.
func TestSaveInstances_LegacyTransientGhostWithTabReaped(t *testing.T) {
	for _, status := range []Status{Loading, Deleting} {
		for _, field := range []string{"tabs", "pending_tabs"} {
			t.Run(statusName(status)+"/"+field, func(t *testing.T) {
				t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
				repoPath := t.TempDir()
				repoID := config.RepoIDFromRoot(repoPath)
				state := newMockStorage()

				// A bare Loading/Deleting row has no known durable markers, so it
				// is a disposable transient ghost — except that it carries a tab,
				// whose MarshalJSON-derived `kind_name` the reflection-based
				// detector does not see. The detector must recognize the derived
				// key so the row is still reaped.
				row := InstanceData{
					ID: "loading-tab-id", Title: "loading-tab", Path: repoPath, Program: "claude",
					Status: status,
				}
				if field == "tabs" {
					row.Tabs = []TabData{{ID: "tab1", Name: "agent", Kind: TabKindAgent}}
				} else {
					row.PendingTabs = []TabData{{ID: "ptab1", Name: "shell", Kind: TabKindShell}}
				}
				require.True(t, isLegacyTransientGhostRow(row),
					"fixture: the decoded row has no known durable markers and would be reaped")
				// Confirm the detector recognizes the derived kind_name rather than
				// flagging the row as carrying a future marker.
				rowBytes, mErr := json.Marshal(row)
				require.NoError(t, mErr)
				require.False(t, rowHasUnrecognizedInstanceFields(rowBytes),
					"a %s row whose tab carries only the binary's own kind_name must not be flagged as "+
						"carrying a newer marker (#NX_)", field)

				raw, err := json.Marshal([]InstanceData{row})
				require.NoError(t, err)
				state.data[repoID] = raw

				alive := makeAliveInstance("alive", repoPath)
				storage, err := NewStorage(state, "")
				require.NoError(t, err)
				require.NoError(t, storage.SaveInstances([]*Instance{alive}))

				got := rowsByTitle(t, state, repoPath)
				require.NotContains(t, got, "loading-tab",
					"a %s row whose only tab carries the binary's own kind_name must be reaped, "+
						"not preserved as if kind_name were a newer binary's durable marker (#NX_)",
					statusName(status)+" "+field)
			})
		}
	}
}

// TestSaveInstances_LegacyTransientGhostWithTabAndUnknownFieldPreserved is the
// companion to #NX_: registering the derived `kind_name` must not regress the
// version-skew carve-out (#9f7). A tab whose own `kind_name` is recognized still
// carries a genuinely unknown nested field — a newer binary's additive marker —
// so the row must still be preserved verbatim for the next start.
func TestSaveInstances_LegacyTransientGhostWithTabAndUnknownFieldPreserved(t *testing.T) {
	for _, status := range []Status{Loading, Deleting} {
		t.Run(statusName(status), func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			repoPath := t.TempDir()
			repoID := config.RepoIDFromRoot(repoPath)
			state := newMockStorage()

			row := InstanceData{
				ID: "loading-tab-unknown-id", Title: "loading-tab-unknown", Path: repoPath, Program: "claude",
				Status: status,
				Tabs:   []TabData{{ID: "tab1", Name: "agent", Kind: TabKindAgent}},
			}
			require.True(t, isLegacyTransientGhostRow(row),
				"fixture: the decoded row has no known durable markers and would be reaped")
			raw, err := json.Marshal([]InstanceData{row})
			require.NoError(t, err)
			var arr []map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(raw, &arr))
			var tabs []map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(arr[0]["tabs"], &tabs))
			tabs[0]["future_tab_marker"] = json.RawMessage(`"owed-to-the-next-start"`)
			arr[0]["tabs"], err = json.Marshal(tabs)
			require.NoError(t, err)
			raw, err = json.Marshal(arr)
			require.NoError(t, err)
			state.data[repoID] = raw

			// The recognized kind_name does not mask a genuinely unknown nested
			// field; the row is still preserved verbatim.
			rowBytes, mErr := json.Marshal(arr[0])
			require.NoError(t, mErr)
			require.True(t, rowHasUnrecognizedInstanceFields(rowBytes),
				"a tab with an unknown nested field must still be flagged for verbatim preservation (#9f7)")

			alive := makeAliveInstance("alive", repoPath)
			storage, err := NewStorage(state, "")
			require.NoError(t, err)
			require.NoError(t, storage.SaveInstances([]*Instance{alive}))

			got := rowsByTitle(t, state, repoPath)
			require.Contains(t, got, "loading-tab-unknown",
				"a %s row whose tab carries a genuine unknown nested field must be preserved, "+
					"not reaped (#9f7)", statusName(status))
			var afterArr []map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(state.data[repoID], &afterArr))
			for _, r := range afterArr {
				if string(r["title"]) == `"loading-tab-unknown"` {
					var afterTabs []map[string]json.RawMessage
					require.NoError(t, json.Unmarshal(r["tabs"], &afterTabs))
					require.Contains(t, afterTabs[0], "future_tab_marker",
						"the unknown nested field must survive the checkpoint verbatim (#6/#9f7)")
					require.Contains(t, afterTabs[0], "kind_name",
						"the binary's own derived kind_name must also survive verbatim")
				}
			}
		})
	}
}

// TestSaveInstances_DurableGhostStripsRemovedPrInfoKey is the removed-wire-key
// strip carve-out for the verbatim-preservation path (#NYA): a non-transient
// unloadable ghost row (a durable one the carve-out does not reap) that still
// carries the legacy pr_info key must not write it back. The previous
// decode/encode checkpoint dropped pr_info by side effect; the verbatim raw
// copy the carve-out now preserves would re-persist it on every shutdown
// otherwise. The strip removes the known retired key without touching a
// genuinely unknown field the verbatim path is here to preserve.
func TestSaveInstances_DurableGhostStripsRemovedPrInfoKey(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := t.TempDir()
	repoID := config.RepoIDFromRoot(repoPath)
	state := newMockStorage()

	// A durable ghost: an unloadable relocation-recovery row the carve-out
	// preserves verbatim (not a disposable transient Loading/Deleting row).
	ghost := unloadableRelocationRecoveryRow(t, "durable-pr-info", repoPath)
	require.False(t, isLegacyTransientGhostRow(ghost),
		"fixture: the row must be durable (not a transient ghost) so the verbatim path preserves it")

	// Plant the legacy pr_info key directly on the raw bytes; seedDisk marshals
	// InstanceData, which drops it. The verbatim path must strip it on write.
	raw, err := json.Marshal([]InstanceData{ghost})
	require.NoError(t, err)
	var arr []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &arr))
	arr[0]["pr_info"] = json.RawMessage(`"removed-legacy-key"`)
	// Also plant a genuinely unknown field the verbatim path must keep.
	arr[0]["future_recovery_handle"] = json.RawMessage(`"abc-123"`)
	raw, err = json.Marshal(arr)
	require.NoError(t, err)
	state.data[repoID] = raw

	alive := makeAliveInstance("alive", repoPath)
	storage, err := NewStorage(state, "")
	require.NoError(t, err)
	require.NoError(t, storage.SaveInstances([]*Instance{alive}))

	got := rowsByTitle(t, state, repoPath)
	require.Contains(t, got, "durable-pr-info",
		"a durable ghost must be preserved verbatim, not reaped")
	// pr_info must be stripped from the written bytes; the unknown field must
	// survive verbatim.
	var afterArr []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(state.data[repoID], &afterArr))
	var found bool
	for _, r := range afterArr {
		if string(r["title"]) == `"durable-pr-info"` {
			found = true
			require.NotContains(t, r, "pr_info",
				"a removed wire key must not be written back by the verbatim-preservation path (#NYA)")
			require.Contains(t, r, "future_recovery_handle",
				"a genuinely unknown field must survive the checkpoint verbatim (#6)")
		}
	}
	require.True(t, found, "the durable ghost must survive the checkpoint")
}

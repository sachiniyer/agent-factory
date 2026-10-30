package session

import (
	"encoding/json"
	"testing"

	"github.com/sachiniyer/agent-factory/log"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAddDocsTab_MetadataOnlyWithProposal(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_docs_add")
	tab, err := inst.AddDocsTab("auth-rework", "")
	require.NoError(t, err)
	assert.Equal(t, "docs", tab.Name)
	assert.Equal(t, TabKindDocs, tab.Kind)
	assert.Equal(t, "auth-rework", tab.Proposal)
	assert.Nil(t, tab.tmux, "a docs tab has no tmux session")
	assert.Equal(t, "Proposal", TabLabel(tab))

	second, err := inst.AddDocsTab("search-v2", "")
	require.NoError(t, err)
	assert.Equal(t, "docs-2", second.Name, "a colliding name must be suffixed")
}

func TestDocsTab_PersistRoundTrip(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_docs_persist")
	_, err := inst.AddDocsTab("auth-rework", "")
	require.NoError(t, err)

	data := inst.ToInstanceData()
	var docs *TabData
	for i := range data.Tabs {
		if data.Tabs[i].Kind == TabKindDocs {
			docs = &data.Tabs[i]
		}
	}
	require.NotNil(t, docs, "serialized data must contain the docs tab")
	assert.Equal(t, "auth-rework", docs.Proposal)
	assert.Equal(t, "", docs.TmuxName)

	raw, err := json.Marshal(docs)
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(raw, &wire))
	assert.Equal(t, "docs", wire["kind_name"])
	assert.Equal(t, "auth-rework", wire["proposal"])
	assert.EqualValues(t, 5, wire["kind"])

	restored := &Instance{}
	restoreLocalTabs(restored, data)
	require.GreaterOrEqual(t, len(restored.Tabs), 2)
	assert.Equal(t, TabKindDocs, restored.Tabs[1].Kind)
	assert.Equal(t, "auth-rework", restored.Tabs[1].Proposal, "the slug must survive a restart")
}

func TestDocsTabKindProperties(t *testing.T) {
	assert.Equal(t, 5, int(TabKindDocs), "tab kinds are append only")
	assert.Equal(t, TabKindDocs, tabKindForData(TabKindDocs))
	assert.False(t, TabKindDocs.HasTmux())
	assert.True(t, TabKindRenameable(TabKindDocs))
	kind, ok := ParseTabKindName("docs")
	assert.True(t, ok)
	assert.Equal(t, TabKindDocs, kind)
	assert.Equal(t, "docs", TabKindName(TabKindDocs))
	assert.NoError(t, Capabilities{}.RefuseTabKind(TabKindDocs, ""), "a docs tab needs nothing from the workspace")
	assert.Equal(t, "My notes", TabLabel(&Tab{Name: "My notes", Kind: TabKindDocs}))
}

func TestDocsTab_PushModePersists(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_docs_push_mode")
	_, err := inst.AddDocsTab("auth-rework", "")
	require.NoError(t, err)
	_, err = inst.AddDocsTab("other", "")
	require.NoError(t, err)
	before := inst.GetTabs()[1]

	assert.Equal(t, 1, inst.SetDocsTabsPushMode("auth-rework", "manual"))
	assert.Equal(t, 0, inst.SetDocsTabsPushMode("missing", "manual"))
	assert.Equal(t, "", before.PushMode, "the mode is set copy-on-write")

	data := inst.ToInstanceData()
	raw, err := json.Marshal(data.Tabs[1])
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(raw, &wire))
	assert.Equal(t, "manual", wire["push_mode"])
	assert.NotContains(t, string(mustJSON(t, data.Tabs[2])), "push_mode", "an unset mode is omitted")

	restored := &Instance{}
	restoreLocalTabs(restored, data)
	assert.Equal(t, "manual", restored.Tabs[1].PushMode, "the mode must survive a restart")
	assert.Equal(t, "", restored.Tabs[2].PushMode)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return raw
}

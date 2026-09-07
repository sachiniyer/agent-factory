package session

import (
	"testing"

	"github.com/sachiniyer/agent-factory/log"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAddWebTab_AppendsWithURLAndNoTmux verifies a web tab is appended with the
// web kind, its target URL, an auto-derived name, and NO tmux session (it has no
// PTY).
func TestAddWebTab_AppendsWithURLAndNoTmux(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_web_add")

	tab, err := inst.AddWebTab("http://localhost:3000", "")
	require.NoError(t, err)
	assert.Equal(t, "web", tab.Name, "default web-tab name is \"web\"")
	assert.Equal(t, TabKindWeb, tab.Kind)
	assert.Equal(t, "http://localhost:3000", tab.URL)
	assert.Nil(t, tab.tmux, "a web tab has no tmux session")
	assert.Equal(t, 2, inst.TabCount())
	assert.False(t, inst.TabAlive(1), "a web tab is never TabAlive (no PTY)")
	assert.Equal(t, "", inst.TabTmuxName(1), "a web tab has no tmux name")
}

// TestAddWebTab_ExplicitNameAndCollisionSuffixing covers explicit --name and
// auto-suffixing of a colliding name.
func TestAddWebTab_ExplicitNameAndCollisionSuffixing(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_web_collide")

	first, err := inst.AddWebTab("http://localhost:3000", "preview")
	require.NoError(t, err)
	assert.Equal(t, "preview", first.Name)

	second, err := inst.AddWebTab("http://localhost:3001", "preview")
	require.NoError(t, err)
	assert.Equal(t, "preview-2", second.Name, "a colliding name must be suffixed")
}

// TestWebTab_PersistRoundTrip verifies a web tab's URL survives a
// serialize/restore cycle and restores with no tmux binding.
func TestWebTab_PersistRoundTrip(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_web_persist")
	_, err := inst.AddWebTab("http://localhost:4321", "livepreview")
	require.NoError(t, err)

	data := inst.ToInstanceData()

	var web *TabData
	for i := range data.Tabs {
		if data.Tabs[i].Kind == TabKindWeb {
			web = &data.Tabs[i]
		}
	}
	require.NotNil(t, web, "serialized data must contain a web tab")
	assert.Equal(t, "livepreview", web.Name)
	assert.Equal(t, "http://localhost:4321", web.URL)
	assert.Equal(t, "", web.TmuxName, "a web tab serializes with no tmux name")

	// Restore into a fresh instance and confirm the web tab comes back intact,
	// with no tmux session bound.
	restored := &Instance{}
	restoreLocalTabs(restored, data)
	require.GreaterOrEqual(t, len(restored.Tabs), 2)
	rw := restored.Tabs[1]
	assert.Equal(t, TabKindWeb, rw.Kind)
	assert.Equal(t, "http://localhost:4321", rw.URL)
	assert.Nil(t, rw.tmux, "a restored web tab has no tmux session")
}

// TestTabKindForData_WebPreserved verifies a persisted web kind is not clamped
// away to shell (the forward-compat degrade only hits truly-unknown kinds).
func TestTabKindForData_WebPreserved(t *testing.T) {
	assert.Equal(t, TabKindWeb, tabKindForData(TabKindWeb))
}

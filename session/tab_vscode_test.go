package session

import (
	"testing"

	"github.com/sachiniyer/agent-factory/log"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAddVSCodeTab_AppendsWithNoTmuxAndNoURL verifies a vscode tab is appended
// with the vscode kind, an auto-derived name, no tmux session (it has no PTY),
// and — the design point — NO URL: its editor is a daemon-managed per-session
// code-server on an ephemeral port, resolved at proxy time.
func TestAddVSCodeTab_AppendsWithNoTmuxAndNoURL(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_vscode_add")

	tab, err := inst.AddVSCodeTab("")
	require.NoError(t, err)
	assert.Equal(t, "vscode", tab.Name, "default vscode-tab name is \"vscode\"")
	assert.Equal(t, TabKindVSCode, tab.Kind)
	assert.Equal(t, "", tab.URL, "a vscode tab stores no URL: the port is chosen fresh on every spawn, so a stored one would always be stale")
	assert.Nil(t, tab.tmux, "a vscode tab has no tmux session")
	assert.Equal(t, 2, inst.TabCount())
	assert.False(t, inst.TabAlive(1), "a vscode tab is never TabAlive (no PTY)")
	assert.Equal(t, "", inst.TabTmuxName(1), "a vscode tab has no tmux name")
}

// TestAddVSCodeTab_ExplicitNameAndCollisionSuffixing covers explicit --name and
// auto-suffixing, so two VS Code panes on one session are addressable.
func TestAddVSCodeTab_ExplicitNameAndCollisionSuffixing(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_vscode_collide")

	first, err := inst.AddVSCodeTab("editor")
	require.NoError(t, err)
	assert.Equal(t, "editor", first.Name)

	second, err := inst.AddVSCodeTab("editor")
	require.NoError(t, err)
	assert.Equal(t, "editor-2", second.Name, "a colliding name must be suffixed")

	third, err := inst.AddVSCodeTab("")
	require.NoError(t, err)
	assert.Equal(t, "vscode", third.Name)
}

// TestAttachVSCodeTabReflectsDaemonOwnedTab verifies the TUI-side projection
// path adds no second editor and invents no competing stable identity. A raced
// snapshot that already reflected the daemon row is a name-keyed no-op.
func TestAttachVSCodeTabReflectsDaemonOwnedTab(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_vscode_attach")
	tab, err := inst.AttachVSCodeTab("editor", "")
	require.NoError(t, err)
	assert.Equal(t, TabKindVSCode, tab.Kind)
	assert.Equal(t, "editor", tab.Name)
	assert.Empty(t, tab.ID, "the next authoritative snapshot must supply the daemon-minted id")
	assert.Nil(t, tab.tmux, "reflecting a VS Code tab must not spawn or attach a tmux session")
	assert.Equal(t, 2, inst.TabCount())

	again, err := inst.AttachVSCodeTab("editor", "")
	require.NoError(t, err)
	assert.Same(t, tab, again, "a snapshot that won the race must not duplicate the tab")
	assert.Equal(t, 2, inst.TabCount())
}

// TestVSCodeTab_PersistRoundTrip verifies a vscode tab survives a
// serialize/restore cycle. This is what makes "restore, then respawn the editor
// lazily" work: the tab carries no editor state at all, so there is nothing to go
// stale across a restart.
func TestVSCodeTab_PersistRoundTrip(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_vscode_persist")
	_, err := inst.AddVSCodeTab("editor")
	require.NoError(t, err)

	data := inst.ToInstanceData()

	var vs *TabData
	for i := range data.Tabs {
		if data.Tabs[i].Kind == TabKindVSCode {
			vs = &data.Tabs[i]
		}
	}
	require.NotNil(t, vs, "serialized data must contain a vscode tab")
	assert.Equal(t, "editor", vs.Name)
	assert.Equal(t, "", vs.URL, "a vscode tab serializes with no URL")
	assert.Equal(t, "", vs.TmuxName, "a vscode tab serializes with no tmux name")

	restored := &Instance{}
	restoreLocalTabs(restored, data)
	require.GreaterOrEqual(t, len(restored.Tabs), 2)
	rv := restored.Tabs[1]
	assert.Equal(t, TabKindVSCode, rv.Kind, "a restored vscode tab keeps its kind")
	assert.Equal(t, "editor", rv.Name)
	assert.Nil(t, rv.tmux, "a restored vscode tab has no tmux session")
}

// TestTabKindForData_VSCodePreserved is a guard against a silent data-corrupting
// bug, not a formality. tabKindForData clamps any kind it does not recognize to
// TabKindShell, so omitting a kind from its allow-list does not fail loudly — it
// lets the tab persist correctly and then quietly come back as a SHELL tab on the
// next daemon restart.
func TestTabKindForData_VSCodePreserved(t *testing.T) {
	assert.Equal(t, TabKindVSCode, tabKindForData(TabKindVSCode))
}

// TestTabKindWireValues pins the on-disk/wire integers. TabKind is serialized as
// a bare int and mirrored by hand in the web client (web/src/types.ts), so
// renumbering would silently reinterpret every persisted tab and desync the two.
func TestTabKindWireValues(t *testing.T) {
	assert.Equal(t, 0, int(TabKindAgent))
	assert.Equal(t, 1, int(TabKindShell))
	assert.Equal(t, 2, int(TabKindProcess))
	assert.Equal(t, 3, int(TabKindWeb))
	assert.Equal(t, 4, int(TabKindVSCode))
}

// TestParseTabKindName covers the shared kind vocabulary the CLI validates
// against and the daemon dispatches on — one source of truth, so a kind the CLI
// accepts can never be one the daemon rejects.
func TestParseTabKindName(t *testing.T) {
	for name, want := range map[string]TabKind{"web": TabKindWeb, "vscode": TabKindVSCode} {
		got, ok := ParseTabKindName(name)
		assert.True(t, ok, "%q must be a known kind", name)
		assert.Equal(t, want, got)
	}
	// "" is the shell/process default, not a kind: callers handle it themselves.
	_, ok := ParseTabKindName("")
	assert.False(t, ok, "the empty kind is the caller's default, not a kind")

	_, ok = ParseTabKindName("nope")
	assert.False(t, ok)

	assert.Equal(t, []string{"vscode", "web"}, TabKindNameList(), "sorted, for stable help text and error messages")
}

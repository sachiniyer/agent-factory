package session

import (
	"testing"

	"github.com/sachiniyer/agent-factory/log"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The stable-tab-identity contract (#1738): a tab is minted with a stable id at
// creation, persisted, and never reused, so a stream/pane binding addressed by
// that id can never misroute after the ordinal list shifts under a reorder/close.
// These tests pin the id itself (unique, persisted, backfilled) and the id→index
// resolution the daemon stream endpoint runs — the concrete "reorder/close does
// not misroute an id-addressed stream" guarantee.

// TestTabsMintUniqueStableIDs: every tab constructor mints a non-empty id, and
// spawning several tabs never collides — the property a stable key needs.
func TestTabsMintUniqueStableIDs(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst, _ := raceMockInstance(t, "af_stable_mint", func() {})
	shell, err := inst.AddShellTab()
	require.NoError(t, err)
	proc, err := inst.AddProcessTab("btop", "")
	require.NoError(t, err)

	agentID, ok := inst.TabIDAt(0)
	require.True(t, ok)
	assert.NotEmpty(t, agentID, "the agent tab must carry a stable id")
	assert.NotEmpty(t, shell.ID, "a shell tab must carry a stable id")
	assert.NotEmpty(t, proc.ID, "a process tab must carry a stable id")

	ids := map[string]bool{agentID: true}
	for _, id := range []string{shell.ID, proc.ID} {
		assert.False(t, ids[id], "tab ids must be unique within an instance")
		ids[id] = true
	}
}

// TestTabIndexByID_ResolvesAfterClose is the core misroute-prevention proof: with
// [agent, A, B, C], closing the MIDDLE tab A shifts B and C down by one ordinal,
// yet each surviving tab's STABLE id still resolves to the tab the user meant —
// B and C to their NEW ordinals, the closed A to nothing. A client that captured
// B's id before the close therefore still streams B afterward, where a captured
// ordinal (2) would now address C.
func TestTabIndexByID_ResolvesAfterClose(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst, _ := raceMockInstance(t, "af_stable_close", func() {})
	a, err := inst.AddProcessTab("a", "a")
	require.NoError(t, err)
	b, err := inst.AddProcessTab("b", "b")
	require.NoError(t, err)
	c, err := inst.AddProcessTab("c", "c")
	require.NoError(t, err)

	// Baseline: [agent, a, b, c] at ordinals 0..3.
	requireIndex(t, inst, b.ID, 2)
	requireIndex(t, inst, c.ID, 3)

	// Close the middle tab a (ordinal 1); b and c shift down by one.
	require.NoError(t, inst.CloseTab(1))

	// b and c still resolve to the tab the user grabbed, now at their NEW ordinals.
	requireIndex(t, inst, b.ID, 1)
	requireIndex(t, inst, c.ID, 2)

	// The captured ordinal 2, which named b before the close, now names c — the
	// exact positional misroute a stable id prevents.
	name, ok := inst.TabIDAt(2)
	require.True(t, ok)
	assert.Equal(t, c.ID, name, "ordinal 2 now names c (proving the ordinal drifted)")

	// The closed tab's id resolves to nothing — a stream addressed by it is a clean
	// miss, not a silent bind to whatever now sits at its old ordinal.
	_, ok = inst.TabIndexByID(a.ID)
	assert.False(t, ok, "a closed tab's id must resolve to no live tab")
}

// TestTabIndexByID_EmptyNeverResolves: an empty id (a legacy tab not yet
// backfilled, or the no-tab_id path) never matches a real tab, so the daemon
// falls back to the ordinal rather than binding id "" to Tabs[0].
func TestTabIndexByID_EmptyNeverResolves(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst, _ := raceMockInstance(t, "af_stable_empty", func() {})
	_, ok := inst.TabIndexByID("")
	assert.False(t, ok, "an empty id must never resolve to a tab")
}

// TestTabStableID_PersistRoundTrip: a tab's stable id round-trips through
// ToInstanceData → restoreLocalTabs unchanged, so a stream keyed on it survives a
// daemon/af restart. A legacy record with no id is backfilled with a fresh one on
// load (rollforward), so every restored tab is addressable by a stable id.
func TestTabStableID_PersistRoundTrip(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst, _ := raceMockInstance(t, "af_stable_persist", func() {})
	shell, err := inst.AddShellTab()
	require.NoError(t, err)

	data := inst.ToInstanceData()
	require.Len(t, data.Tabs, 2)
	agentID, _ := inst.TabIDAt(0)
	assert.Equal(t, agentID, data.Tabs[0].ID, "the agent tab id must be serialized")
	assert.Equal(t, shell.ID, data.Tabs[1].ID, "the shell tab id must be serialized")

	// Restore into a fresh instance: ids are preserved verbatim.
	restored := &Instance{Title: inst.Title, Program: inst.Program}
	restoreLocalTabs(restored, data)
	require.Len(t, restored.Tabs, 2)
	assert.Equal(t, agentID, restored.Tabs[0].ID)
	assert.Equal(t, shell.ID, restored.Tabs[1].ID)

	// A legacy record (ids cleared) is backfilled with fresh, non-empty ids.
	legacy := data
	legacy.Tabs = make([]TabData, len(data.Tabs))
	copy(legacy.Tabs, data.Tabs)
	for i := range legacy.Tabs {
		legacy.Tabs[i].ID = ""
	}
	backfilled := &Instance{Title: inst.Title, Program: inst.Program}
	restoreLocalTabs(backfilled, legacy)
	require.Len(t, backfilled.Tabs, 2)
	for i, tab := range backfilled.Tabs {
		assert.NotEmpty(t, tab.ID, "legacy tab %d must be backfilled with a stable id", i)
	}
}

// TestEnsureBrokerFollowsTabAcrossClose proves the data-plane half of the fix: the
// localAgentServer keys its per-tab PTY broker by the tab's STABLE id, not its
// ordinal. So when the middle tab closes and a later tab shifts down, ensuring a
// broker for that tab's NEW ordinal returns the SAME broker (bound to the same
// live tmux) — never the closed tab's stale, dead-tmux broker the way an
// ordinal-keyed map would after the shift.
func TestEnsureBrokerFollowsTabAcrossClose(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst, _ := raceMockInstance(t, "af_stable_broker", func() {})
	_, err := inst.AddProcessTab("a", "a")
	require.NoError(t, err)
	b, err := inst.AddProcessTab("b", "b")
	require.NoError(t, err)

	las := inst.AgentServer().(*localAgentServer)

	// Bind a broker to b at its current ordinal (2). It is stored under b's id.
	brBefore, err := las.ensureBroker(2)
	require.NoError(t, err)
	las.mu.Lock()
	_, keyedByID := las.brokers[b.ID]
	nBrokers := len(las.brokers)
	las.mu.Unlock()
	assert.True(t, keyedByID, "the broker must be keyed by the tab's stable id, not its ordinal")
	assert.Equal(t, 1, nBrokers)

	// Close the middle tab a (ordinal 1); b shifts to ordinal 1.
	require.NoError(t, inst.CloseTab(1))
	requireIndex(t, inst, b.ID, 1)

	// Ensuring a broker for b's NEW ordinal returns the SAME broker — it followed the
	// tab by id — and no stale duplicate is created.
	brAfter, err := las.ensureBroker(1)
	require.NoError(t, err)
	assert.Same(t, brBefore, brAfter, "the broker must follow its tab across the ordinal shift")
	las.mu.Lock()
	nAfter := len(las.brokers)
	las.mu.Unlock()
	assert.Equal(t, 1, nAfter, "no stale second broker may be created for the shifted ordinal")
}

func requireIndex(t *testing.T, inst *Instance, id string, want int) {
	t.Helper()
	idx, ok := inst.TabIndexByID(id)
	require.Truef(t, ok, "tab id %q must resolve to a live tab", id)
	assert.Equalf(t, want, idx, "tab id %q must resolve to ordinal %d", id, want)
}

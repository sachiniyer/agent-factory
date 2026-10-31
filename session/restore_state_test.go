package session

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stateWire is a loopback agent-server whose health can fail for its first
// failHealth calls and whose launch and restore-tabs routes answer a scripted
// outcome. It records the bodies it was sent.
type stateWire struct {
	mu          sync.Mutex
	failHealth  int
	features    []string
	outcome     map[string]any
	launches    []string
	restores    []string
	healthCalls int
}

func newStateWire(t *testing.T, w *stateWire) *remoteAgentClient {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		w.healthCalls++
		fail := w.healthCalls <= w.failHealth
		w.mu.Unlock()
		if fail {
			http.Error(rw, "boom", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(rw).Encode(map[string]any{"data": map[string]any{"ok": true, "features": w.features}})
	})
	record := func(dst *[]string) http.HandlerFunc {
		return func(rw http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			w.mu.Lock()
			*dst = append(*dst, string(b))
			out := w.outcome
			w.mu.Unlock()
			_ = json.NewEncoder(rw).Encode(map[string]any{"data": out})
		}
	}
	mux.HandleFunc("/v1/agent/launch", record(&w.launches))
	mux.HandleFunc("/v1/agent/restore-tabs", record(&w.restores))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	rc, err := newRemoteAgentClient(AgentServerEndpoint{URL: srv.URL, Token: "t"}, "srv")
	require.NoError(t, err)
	return rc
}

func stateOf(inst *Instance, id string) TabRestoreState {
	for _, tab := range inst.GetTabs() {
		if tab.ID == id {
			return tab.restoreState
		}
	}
	return "missing"
}

func (w *stateWire) sent(dst *[]string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(*dst)
}

// #6235: an unknown tab is a durable debt. Reloaded after a daemon restart it is
// still unknown, so the healthy-server sweep keeps its row instead of retiring
// it for a rebind the server never made.
func TestUnknownRestoreStateSurvivesADaemonRestartAndIsNotRetired(t *testing.T) {
	rc := newStateWire(t, &stateWire{features: []string{tabAPIFeature, tabRestoreFeature}, outcome: map[string]any{"ok": true}})
	before := offBoxInstanceWithSurvivors(rc)
	before.restoreRosterForRestart(featureAbsent) // an adoption that could not hand them back
	require.Equal(t, TabRestoreUnknown, stateOf(before, "t-shell"))

	raw, err := json.Marshal(before.ToInstanceData())
	require.NoError(t, err)
	var reloaded InstanceData
	require.NoError(t, json.Unmarshal(raw, &reloaded))
	reloaded.BackendType = "docker"
	reloaded.Liveness = LiveRunning
	inst, err := FromInstanceData(reloaded)
	require.NoError(t, err)
	inst.materializeRestorableTabs()

	assert.Equal(t, TabRestoreUnknown, stateOf(inst, "t-shell"), "the debt is durable")
	assert.Equal(t, TabRestoreUnknown, stateOf(inst, "t-dead"))
	inst.mu.Lock()
	rows := inst.offBoxPTYRowsLocked()
	inst.mu.Unlock()
	assert.Empty(t, inst.retireLostOffBoxTabs(rows, map[string]TabData{}), "a healthy server that never rebound the id must not cost the row")
	assert.Len(t, inst.GetTabs(), 3)
}

// #6236: a health probe that has not answered is not an old server. Launch asks
// again; if the server then advertises the feature the roster is sent.
func TestLaunchResolvesAnUnansweredAdvertisementBeforeChoosingTheFallback(t *testing.T) {
	w := &stateWire{failHealth: 1, features: []string{tabAPIFeature, tabRestoreFeature}, outcome: map[string]any{"ok": true, "tabs_restored": []string{"t-shell"}, "tabs_gone": []string{"t-dead"}}}
	inst := offBoxInstanceWithSurvivors(newStateWire(t, w))
	rs := inst.AgentServer().(*remoteAgentServer)
	rs.restartAdoption = true
	rs.idNative() // Provision's warm: fails transiently
	require.False(t, rs.idPlaneKnown)

	require.NoError(t, rs.Launch(false))

	require.Equal(t, 1, w.sent(&w.launches))
	assert.Contains(t, w.launches[0], `"tabs"`, "the unanswered probe was retried, so the roster went out")
	assert.Equal(t, TabRestored, stateOf(inst, "t-shell"))
	assert.Equal(t, TabRestoreGone, stateOf(inst, "t-dead"))
}

// #6236: when the advertisement still has not answered at Launch, nothing is
// sent and nothing is written off: every tab stays unknown, and the retry restores
// them once the server answers.
func TestUnansweredAdvertisementLeavesTheTabsUnknownAndTheRetryRestoresThem(t *testing.T) {
	w := &stateWire{failHealth: 2, features: []string{tabAPIFeature, tabRestoreFeature, tabRestoreRetryFeature},
		outcome: map[string]any{"ok": true, "tabs_restored": []string{"t-shell"}, "tabs_gone": []string{"t-dead"}}}
	inst := offBoxInstanceWithSurvivors(newStateWire(t, w))
	rs := inst.AgentServer().(*remoteAgentServer)
	rs.restartAdoption = true
	rs.idNative()

	require.NoError(t, rs.Launch(false)) // health fails again here
	assert.NotContains(t, w.launches[0], `"tabs"`)
	for _, id := range []string{"t-shell", "t-dead", "t-old"} {
		assert.Equal(t, TabRestoreUnknown, stateOf(inst, id), id)
	}
	assert.False(t, inst.TabAlive(1), "unknown reads as unavailable")

	// The server answers now: the retry sends the roster of the tabs it can rebind.
	require.True(t, inst.retryUnknownTabRestores(rs))
	require.Equal(t, 1, w.sent(&w.restores))
	assert.Equal(t, TabRestored, stateOf(inst, "t-shell"))
	assert.Equal(t, TabRestoreGone, stateOf(inst, "t-dead"))
	assert.Equal(t, TabRestoreUnknown, stateOf(inst, "t-old"), "no recorded session: stays unknown, never fabricated")

	// A retry with nothing unknown but the unbound tab sends nothing more.
	assert.False(t, inst.retryUnknownTabRestores(rs))
	assert.Equal(t, 1, w.sent(&w.restores))
}

// An old server (feature confirmed absent) stays unknown and is neither sent a
// roster, nor retried, nor retired.
func TestAnOldServerLeavesTabsUnknownAndIsNeverRetried(t *testing.T) {
	w := &stateWire{features: []string{tabAPIFeature}, outcome: map[string]any{"ok": true}}
	inst := offBoxInstanceWithSurvivors(newStateWire(t, w))
	rs := inst.AgentServer().(*remoteAgentServer)
	rs.restartAdoption = true

	require.NoError(t, rs.Launch(false))
	assert.NotContains(t, w.launches[0], `"tabs"`)
	assert.False(t, inst.retryUnknownTabRestores(rs))
	assert.Zero(t, w.sent(&w.restores), "the retry route is never called on a server that lacks it")
	for _, id := range []string{"t-shell", "t-dead", "t-old"} {
		assert.Equal(t, TabRestoreUnknown, stateOf(inst, id), id)
	}
	inst.mu.Lock()
	rows := inst.offBoxPTYRowsLocked()
	inst.mu.Unlock()
	assert.Empty(t, inst.retireLostOffBoxTabs(rows, map[string]TabData{}))
}

// #6237: a tab the server confirmed gone is retired by the sweep, even though an
// earlier pass had left it unknown.
func TestConfirmedGoneTabIsRetiredByTheSweep(t *testing.T) {
	w := &stateWire{features: []string{tabAPIFeature, tabRestoreFeature}, outcome: map[string]any{"ok": true, "tabs_gone": []string{"t-dead"}}}
	inst := offBoxInstanceWithSurvivors(newStateWire(t, w))
	inst.restoreRosterForRestart(featureAbsent) // earlier pass: everything unknown
	rows := inst.unknownRestoreRowsLocked()
	require.True(t, inst.applyRestoreOutcome(rows, nil, []string{"t-dead"}))
	assert.Equal(t, TabRestoreGone, stateOf(inst, "t-dead"))
	assert.False(t, inst.TabAlive(2))

	inst.mu.Lock()
	before := inst.offBoxPTYRowsLocked()
	inst.mu.Unlock()
	retired := inst.retireLostOffBoxTabs(before, map[string]TabData{})
	assert.Equal(t, []string{"t-dead"}, retired, "only the confirmed-gone row goes; the unknown ones stay")
	assert.Len(t, inst.GetTabs(), 3)
}

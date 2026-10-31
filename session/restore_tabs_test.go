package session

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// restartedServerInstance is the fresh instance a restarted agent-server builds:
// only the adopted agent tab, over a tmux whose answers the test scripts per
// session name. calls records every tmux command that ran.
func restartedServerInstance(t *testing.T, alive, gone map[string]bool) (*Instance, *recordingPtyFactory, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	answer := func(c *exec.Cmd) error {
		line := strings.Join(c.Args, " ")
		mu.Lock()
		calls = append(calls, line)
		mu.Unlock()
		if !strings.Contains(line, "has-session") {
			return nil
		}
		for name := range alive {
			if strings.Contains(line, "="+name) {
				return nil
			}
		}
		for name := range gone {
			if strings.Contains(line, "="+name) {
				var exit *exec.ExitError
				require.ErrorAs(t, exec.Command("sh", "-c", "exit 1").Run(), &exit)
				exit.Stderr = []byte("can't find session: " + name)
				return exit
			}
		}
		return errors.New("tmux: a wedged server gave no answer")
	}
	cmdExec := cmd_test.MockCmdExec{
		RunFunc:    answer,
		OutputFunc: func(c *exec.Cmd) ([]byte, error) { return nil, answer(c) },
	}
	pty := &recordingPtyFactory{t: t}
	agentTmux := tmux.NewTmuxSessionWithDeps("srv", tmux.ProgramClaude, pty, cmdExec)
	inst := &Instance{Title: "srv", started: true, backend: &LocalBackend{}, Tabs: []*Tab{newAgentTab(agentTmux)}}
	return inst, pty, &calls
}

// W1.4 (#6162): every tab that survived in tmux is resolvable, streamable and
// closable on the restarted server; a tab whose session died is reported gone and
// never recreated; nothing is executed.
func TestRestoreSurvivingTabsRebindsSurvivorsAndNeverRecreatesTheDead(t *testing.T) {
	inst, pty, calls := restartedServerInstance(t,
		map[string]bool{"srv__shell": true, "srv__build": true},
		map[string]bool{"srv__dead": true})

	rep := inst.RestoreSurvivingTabs([]TabData{
		{ID: "t-shell", Gen: "g1", Name: "shell", Kind: TabKindShell, TmuxName: "srv__shell"},
		{ID: "t-build", Gen: "g2", Name: "build", Kind: TabKindProcess, Command: "make forever", TmuxName: "srv__build"},
		{ID: "t-dead", Gen: "g3", Name: "dead", Kind: TabKindProcess, Command: "rm -rf /tmp/x", TmuxName: "srv__dead"},
		{ID: "t-wedged", Gen: "g4", Name: "wedged", Kind: TabKindShell, TmuxName: "srv__wedged"},
		{ID: "t-nobind", Name: "nobind", Kind: TabKindShell},
		{ID: "t-agent", Name: "agent2", Kind: TabKindAgent, TmuxName: "srv__shell"},
	})

	assert.Equal(t, []string{"t-shell", "t-build"}, rep.Restored)
	assert.Equal(t, []string{"t-dead"}, rep.Gone, "confirmed absence is reported gone")
	assert.ElementsMatch(t, []string{"t-wedged", "t-nobind", "t-agent"}, rep.Unresolved,
		"no answer, no recorded binding, or a non-PTY kind is never guessed into a binding")

	// Resolved by id, bound to exactly the recorded session.
	idx, ok := inst.TabIndexByID("t-shell")
	require.True(t, ok, "a survivor resolves, so its stream no longer answers ErrTabGone")
	assert.Equal(t, "srv__shell", inst.TabTmuxName(idx))
	for _, id := range []string{"t-dead", "t-wedged", "t-nobind", "t-agent"} {
		_, ok := inst.TabIndexByID(id)
		assert.False(t, ok, "%s must not resolve", id)
	}

	assert.Zero(t, newSessionCount(pty), "restoring must never spawn a session, let alone replay a command")
	for _, c := range *calls {
		assert.NotContains(t, c, "make forever")
		assert.NotContains(t, c, "rm -rf")
	}

	// Close reaches the tmux process.
	require.NoError(t, inst.CloseTabByID("t-shell"))
	killed := false
	for _, c := range *calls {
		if strings.Contains(c, "kill-session") && strings.Contains(c, "srv__shell") {
			killed = true
		}
	}
	assert.True(t, killed, "closing a restored tab stops its tmux session")
	_, ok = inst.TabIndexByID("t-shell")
	assert.False(t, ok)

	// A second adoption of the same roster is idempotent.
	// A retry after a lost launch response reports the already-rebound tab as
	// restored (not silence the daemon reads as unavailable), binds nothing twice,
	// and does not claim another incarnation or session under the id.
	again := inst.RestoreSurvivingTabs([]TabData{
		{ID: "t-build", Gen: "g2", Name: "build", Kind: TabKindProcess, TmuxName: "srv__build"},
		{ID: "t-build", Gen: "other", Name: "build", Kind: TabKindProcess, TmuxName: "srv__build"},
		{ID: "t-build", Gen: "g2", Name: "build", Kind: TabKindProcess, TmuxName: "srv__elsewhere"},
	})
	assert.Equal(t, []string{"t-build"}, again.Restored)
	assert.Len(t, again.Unresolved, 2)
	assert.Equal(t, 2, inst.TabCount(), "agent tab and the one remaining survivor")
}

// restoreWire is a loopback agent-server for the daemon side: it records the
// launch body and answers with the scripted restore outcome.
func restoreWire(t *testing.T, features []string, launchData map[string]any) (*remoteAgentClient, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var launches []string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"ok": true, "features": features}})
	})
	mux.HandleFunc("/v1/agent/launch", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		launches = append(launches, string(b))
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"data": launchData})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	rc, err := newRemoteAgentClient(AgentServerEndpoint{URL: srv.URL, Token: "t"}, "srv")
	require.NoError(t, err)
	return rc, &launches
}

func offBoxInstanceWithSurvivors(rc *remoteAgentClient) *Instance {
	inst := newOffBoxInstance(rc)
	inst.Tabs = append(inst.Tabs,
		&Tab{ID: "t-shell", Name: "shell", Kind: TabKindShell, gen: "g1", sandboxTmux: "srv__shell"},
		&Tab{ID: "t-dead", Name: "build", Kind: TabKindProcess, Command: "make", gen: "g2", sandboxTmux: "srv__build"},
		&Tab{ID: "t-old", Name: "old", Kind: TabKindProcess, Command: "tail -f x", gen: "g3"}, // created before the name was recorded
	)
	return inst
}

func TestRestartAdoptionLaunchHandsTheRosterToAServerThatCanRestoreIt(t *testing.T) {
	rc, launches := restoreWire(t, []string{tabAPIFeature, tabRestoreFeature},
		map[string]any{"ok": true, "tabs_restored": []string{"t-shell"}, "tabs_gone": []string{"t-dead"}})
	inst := offBoxInstanceWithSurvivors(rc)
	rs := inst.AgentServer().(*remoteAgentServer)
	rs.restartAdoption = true
	rs.idNative()

	require.NoError(t, rs.Launch(false))

	require.Len(t, *launches, 1)
	var req struct {
		Tabs []TabData `json:"tabs"`
	}
	require.NoError(t, json.Unmarshal([]byte((*launches)[0]), &req))
	require.Len(t, req.Tabs, 2, "only tabs with a recorded session are sent")
	assert.Equal(t, "srv__shell", req.Tabs[0].TmuxName)
	assert.Equal(t, "g1", req.Tabs[0].Gen)

	byID := map[string]*Tab{}
	for _, tab := range inst.GetTabs() {
		byID[tab.ID] = tab
	}
	assert.Equal(t, TabRestored, byID["t-shell"].restoreState, "restored: usable")
	assert.Equal(t, TabRestoreGone, byID["t-dead"].restoreState, "confirmed gone: the sweep retires it")
	assert.Equal(t, TabRestoreUnknown, byID["t-old"].restoreState, "no recorded session: cannot be handed back, said so")
	assert.Nil(t, byID["t-shell"].tmux, "the daemon fabricates no local binding")
}

// An older agent-server does not know the field: nothing is sent, nothing is
// fabricated, and the daemon says the off-box tabs are unavailable without
// dropping rows whose processes may still be alive.
func TestRestartAdoptionAgainstAnOldServerLeavesTheTabsUnavailableAndHonest(t *testing.T) {
	rc, launches := restoreWire(t, []string{tabAPIFeature}, map[string]any{"ok": true})
	inst := offBoxInstanceWithSurvivors(rc)
	rs := inst.AgentServer().(*remoteAgentServer)
	rs.restartAdoption = true
	rs.idNative()

	require.NoError(t, rs.Launch(false))

	require.Len(t, *launches, 1)
	assert.NotContains(t, (*launches)[0], `"tabs"`, "an old server is never sent the roster")
	for idx, tab := range inst.GetTabs() {
		if idx == 0 {
			continue
		}
		assert.Equal(t, TabRestoreUnknown, tab.restoreState, tab.ID)
		assert.Nil(t, tab.tmux, "no binding is made up")
		assert.False(t, inst.TabAlive(idx), "reported unavailable")
	}

	// The restarted server's roster is empty, yet the rows stay: their processes
	// may be running and nothing proved otherwise.
	inst.mu.Lock()
	rows := inst.offBoxPTYRowsLocked()
	inst.mu.Unlock()
	assert.Empty(t, inst.retireLostOffBoxTabs(rows, map[string]TabData{}))
	assert.Len(t, inst.GetTabs(), 4)

	// An ordinary (non-restart) launch never sends a roster either way.
	rc2, launches2 := restoreWire(t, []string{tabAPIFeature, tabRestoreFeature}, map[string]any{"ok": true})
	inst2 := offBoxInstanceWithSurvivors(rc2)
	rs2 := inst2.AgentServer().(*remoteAgentServer)
	rs2.idNative()
	require.NoError(t, rs2.Launch(false))
	assert.NotContains(t, (*launches2)[0], `"tabs"`)
}

// The daemon restarts too: the production caller is ReattachSandbox over a
// RELOADED instance, so the binding and its row must survive persistence and be
// handed to the restarted server (#6229 review).
func TestSandboxTabBindingSurvivesADaemonRestartAndReachesTheRoster(t *testing.T) {
	rc, _ := restoreWire(t, []string{tabAPIFeature, tabRestoreFeature}, map[string]any{"ok": true})
	before := offBoxInstanceWithSurvivors(rc)
	data := before.ToInstanceData()
	raw, err := json.Marshal(data)
	require.NoError(t, err)
	var reloaded InstanceData
	require.NoError(t, json.Unmarshal(raw, &reloaded))
	reloaded.BackendType = "docker"
	reloaded.Liveness = LiveRunning

	inst, err := FromInstanceData(reloaded)
	require.NoError(t, err)
	require.Empty(t, inst.GetTabs(), "nothing is bound until the reattach reaches the sandbox")

	inst.materializeRestorableTabs()
	rows := inst.restoreRosterForRestart(featurePresent)
	var names []string
	for _, r := range rows {
		names = append(names, r.ID+"="+r.TmuxName)
	}
	assert.ElementsMatch(t, []string{"t-shell=srv__shell", "t-dead=srv__build"}, names,
		"the reloaded roster carries each recorded session, so a restarted server can rebind it")
	for _, tab := range inst.GetTabs() {
		if tab.ID == "t-old" {
			t.Fatal("a row with no recorded session cannot be handed back and is not retained")
		}
	}
	inst.materializeRestorableTabs()
	assert.Len(t, inst.GetTabs(), 3, "agent plus the two restorable rows; a retried reattach adds nothing")
}

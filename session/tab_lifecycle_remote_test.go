package session

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lifecycleSandbox is a stateful stand-in for an in-sandbox agent-server: it
// owns a roster keyed by the daemon's stable id, executes each command at most
// once per id (the id idempotency the daemon leans on), and can lose a create's
// response or refuse to answer a close, the two transport failures the
// lifecycle must survive (#5571; #5952, #5955-#5963).
type lifecycleSandbox struct {
	mu          sync.Mutex
	tabs        map[string]TabData // id → row, as /v1/agent/tabs reports it
	executions  map[string]int     // command → times it was started
	closeCalls  []string
	dropCreate  bool            // execute, then hang up without answering
	closeDown   bool            // hang up on every close
	refuseNew   bool            // refuse a create that would start a new execution (a definitive answer)
	hold        chan struct{}   // when set, a create answers only once it is closed
	stuck       map[string]bool // ids whose tmux teardown fails: the row goes, the command stays (#6022)
	genSeq      int
	rosterCalls int
}

func newLifecycleSandbox(t *testing.T) (*lifecycleSandbox, *Instance) {
	t.Helper()
	sb := &lifecycleSandbox{tabs: map[string]TabData{}, executions: map[string]int{}}
	ws := newTabWireServer(t).route("/v1/health", map[string]any{
		"ok": true, "features": []string{"tab-api", tabIDAddressingFeature}})
	hangUp := func(w http.ResponseWriter) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}
	ws.mux.HandleFunc("/v1/agent/tab-create", func(w http.ResponseWriter, r *http.Request) {
		var req agentTabCreateReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		sb.mu.Lock()
		td, exists := sb.tabs[req.ID]
		if exists && td.Command != req.Command {
			sb.mu.Unlock()
			ws.encode(w, map[string]any{"data": nil, "error": map[string]any{
				"message": "tab id already holds a different command", "code": "conflict"}})
			return
		}
		if !exists && sb.refuseNew {
			sb.mu.Unlock()
			ws.encode(w, map[string]any{"data": nil, "error": map[string]any{
				"message": "refused", "code": "refused"}})
			return
		}
		if !exists {
			sb.genSeq++
			kind, _ := ParseTabKindName(req.Kind)
			if req.Kind == "shell" {
				// The sandbox names a shell against its own roster, which knows
				// none of the daemon's web tabs (#5962).
				req.Name = "shell"
			}
			td = TabData{ID: req.ID, Gen: "gen-" + req.ID + "-" + string(rune('a'+sb.genSeq)),
				Name: req.Name, Kind: kind, Command: req.Command}
			sb.tabs[req.ID] = td
			sb.executions[req.Command]++
		}
		drop := sb.dropCreate
		hold := sb.hold
		sb.mu.Unlock()
		if hold != nil {
			<-hold
		}
		if drop {
			hangUp(w)
			return
		}
		ws.encode(w, map[string]any{"data": map[string]any{"tab": td}, "error": nil})
	})
	ws.mux.HandleFunc("/v1/agent/tab-close", func(w http.ResponseWriter, r *http.Request) {
		var req agentTabCloseReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		sb.mu.Lock()
		down := sb.closeDown
		// The generation is compared HERE, with the removal, the way the real
		// agent-server does it (#5996): a mismatch is tab_gone, never a close.
		td, held := sb.tabs[req.ID]
		if !down && sb.stuck[req.ID] {
			delete(sb.tabs, req.ID)
			sb.mu.Unlock()
			ws.encode(w, map[string]any{"data": nil, "error": map[string]any{
				"message": "teardown unconfirmed", "code": "internal"}})
			return
		}
		gone := !held || (req.Gen != "" && td.Gen != req.Gen)
		if !down && !gone {
			sb.closeCalls = append(sb.closeCalls, req.ID)
			delete(sb.tabs, req.ID)
		}
		sb.mu.Unlock()
		if down {
			hangUp(w)
			return
		}
		if gone {
			ws.encode(w, map[string]any{"data": nil, "error": map[string]any{
				"message": "tab gone", "code": "tab_gone"}})
			return
		}
		ws.encode(w, map[string]any{"data": map[string]any{}, "error": nil})
	})
	ws.mux.HandleFunc("/v1/agent/tabs", func(w http.ResponseWriter, r *http.Request) {
		sb.mu.Lock()
		sb.rosterCalls++
		rows := make([]TabData, 0, len(sb.tabs))
		for _, td := range sb.tabs {
			rows = append(rows, td)
		}
		sb.mu.Unlock()
		ws.encode(w, map[string]any{"data": map[string]any{"tabs": rows}, "error": nil})
	})
	srv := httptest.NewServer(ws.mux)
	t.Cleanup(srv.Close)
	inst := newOffBoxInstance(ws.client(t, srv.URL))
	inst.AgentServer().(*remoteAgentServer).idNative()
	return sb, inst
}

func (sb *lifecycleSandbox) execs(cmd string) int {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.executions[cmd]
}

func (sb *lifecycleSandbox) closed() []string {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return append([]string(nil), sb.closeCalls...)
}

func (sb *lifecycleSandbox) ids() map[string]bool {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	out := map[string]bool{}
	for id := range sb.tabs {
		out[id] = true
	}
	return out
}

func rosterIDs(i *Instance) map[string]bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	out := map[string]bool{}
	for _, t := range i.Tabs[1:] { // [0] is the agent tab, seeded independently
		out[t.ID] = true
	}
	return out
}

// intentAdopted reports whether the uncertain-create intent for command is still
// present and whether the sweep latched it as adopted. The refused-adoption
// tests lean on this: a spawn refused with the session inert or a spawn-block
// fence up must leave the latch clear so a later sweep retries it.
func intentAdopted(i *Instance, command string) (present, adopted bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	for _, in := range i.tabCreateIntents {
		if in.command == command {
			return true, in.adopted
		}
	}
	return false, false
}

func (sb *lifecycleSandbox) setExit(id string, status int) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	td := sb.tabs[id]
	td.Exit = &TabExitData{Status: status, StatusKnown: true}
	sb.tabs[id] = td
}

// The interleavings the remote tab lifecycle must hold (#5571). Each row names
// the finding it closes.
func TestRemoteTabLifecycleInterleavings(t *testing.T) {
	t.Run("concurrent creates get unique names and distinct ids (#5956)", func(t *testing.T) {
		_, inst := newLifecycleSandbox(t)
		var wg sync.WaitGroup
		const n = 8
		tabs := make([]*Tab, n)
		for k := 0; k < n; k++ {
			wg.Add(1)
			go func(k int) {
				defer wg.Done()
				tab, err := inst.AddProcessTabWithID("", "deploy.sh", "")
				assert.NoError(t, err)
				tabs[k] = tab
			}(k)
		}
		wg.Wait()
		seen := map[string]bool{}
		for _, tab := range tabs {
			require.NotNil(t, tab)
			assert.False(t, seen[tab.Name], "canonical name %q minted twice", tab.Name)
			seen[tab.Name] = true
		}
	})

	t.Run("same-id creates execute the command once (#5959)", func(t *testing.T) {
		sb, inst := newLifecycleSandbox(t)
		var wg sync.WaitGroup
		for k := 0; k < 6; k++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := inst.AddProcessTabWithID("one-id", "migrate.sh", "")
				assert.NoError(t, err)
			}()
		}
		wg.Wait()
		assert.Equal(t, 1, sb.execs("migrate.sh"))
		assert.Len(t, rosterIDs(inst), 1)
	})

	t.Run("create versus close converge on one roster (#5961)", func(t *testing.T) {
		sb, inst := newLifecycleSandbox(t)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = inst.AddProcessTabWithID("race-id", "tail -f log", "")
		}()
		go func() {
			defer wg.Done()
			_ = inst.CloseTabByID("race-id")
		}()
		wg.Wait()
		assert.Equal(t, rosterIDs(inst), sb.ids(), "daemon and sandbox rosters must agree")
		assert.Empty(t, inst.PendingRemoteTabClose())
	})

	t.Run("a create whose answer is lost keeps its id; the retry cannot run it twice (#5958)", func(t *testing.T) {
		sb, inst := newLifecycleSandbox(t)
		sb.mu.Lock()
		sb.dropCreate = true
		sb.mu.Unlock()
		_, err := inst.AddProcessTabWithID("", "deploy.sh", "")
		require.Error(t, err, "an unanswered create is reported, not assumed")
		require.Equal(t, 1, sb.execs("deploy.sh"), "the sandbox did start it")
		require.Empty(t, rosterIDs(inst), "nothing is claimed until the outcome is known")

		sb.mu.Lock()
		sb.dropCreate = false
		sb.mu.Unlock()
		tab, err := inst.AddProcessTabWithID("", "deploy.sh", "")
		require.NoError(t, err)
		assert.Equal(t, 1, sb.execs("deploy.sh"), "the retry reused the minted id and joined the one execution")
		assert.Equal(t, sb.ids(), rosterIDs(inst))
		assert.NotEmpty(t, tab.ID)
	})

	t.Run("an uncertain create is adopted by the sweep if nobody retries (#5958)", func(t *testing.T) {
		sb, inst := newLifecycleSandbox(t)
		sb.mu.Lock()
		sb.dropCreate = true
		sb.mu.Unlock()
		_, err := inst.AddProcessTabWithID("", "deploy.sh", "")
		require.Error(t, err)
		inst.ReconcileOffBoxTabs()
		assert.Equal(t, sb.ids(), rosterIDs(inst), "the tab the sandbox started is not left untracked")
		inst.ReconcileOffBoxTabs()
		assert.Len(t, rosterIDs(inst), 1, "adoption is idempotent")
	})

	t.Run("a close the sandbox cannot confirm is durable and retried (#5957)", func(t *testing.T) {
		sb, inst := newLifecycleSandbox(t)
		tab, err := inst.AddProcessTabWithID("", "serve", "")
		require.NoError(t, err)
		sb.mu.Lock()
		sb.closeDown = true
		sb.mu.Unlock()

		require.Error(t, inst.CloseTabByID(tab.ID), "the unconfirmed teardown is reported")
		pending := inst.PendingRemoteTabClose()
		require.Len(t, pending, 1, "the roster dropped the tab; a record must name the command still running")
		assert.Equal(t, tab.ID, pending[0].TabID)
		assert.NotEmpty(t, pending[0].Gen)
		data := inst.ToInstanceData()
		assert.Equal(t, pending, data.PendingRemoteTabClose, "the record rides the persisted projection")

		inst.ReconcileOffBoxTabs()
		assert.Len(t, inst.PendingRemoteTabClose(), 1, "still unreachable: still pending")

		sb.mu.Lock()
		sb.closeDown = false
		sb.mu.Unlock()
		inst.ReconcileOffBoxTabs()
		assert.Empty(t, inst.PendingRemoteTabClose(), "confirmed on reconnect")
		assert.Equal(t, []string{tab.ID}, sb.closed())
		assert.Empty(t, sb.ids())
	})

	t.Run("a pending close whose tab is already gone is retired without a call (#5957)", func(t *testing.T) {
		sb, inst := newLifecycleSandbox(t)
		inst.mu.Lock()
		inst.pendingRemoteClose = []RemoteTabCleanupData{{TabID: "ghost", Gen: "g"}}
		inst.mu.Unlock()
		inst.ReconcileOffBoxTabs()
		assert.Empty(t, inst.PendingRemoteTabClose())
		assert.Empty(t, sb.closed())
	})

	t.Run("a pending close never closes a later incarnation of the id (#5957, #5960)", func(t *testing.T) {
		sb, inst := newLifecycleSandbox(t)
		tab, err := inst.AddProcessTabWithID("reused", "serve", "")
		require.NoError(t, err)
		inst.mu.Lock()
		inst.pendingRemoteClose = []RemoteTabCleanupData{{TabID: "reused", Gen: "an-older-incarnation"}}
		inst.mu.Unlock()
		inst.ReconcileOffBoxTabs()
		assert.Equal(t, map[string]bool{tab.ID: true}, sb.ids(), "the live replacement is untouched")
	})

	t.Run("exit evidence refreshes into the matching incarnation (#5955)", func(t *testing.T) {
		sb, inst := newLifecycleSandbox(t)
		tab, err := inst.AddProcessTabWithID("", "build", "")
		require.NoError(t, err)
		require.Nil(t, tab.Exit)
		sb.setExit(tab.ID, 3)
		inst.ReconcileOffBoxTabs()
		var got *TabExit
		for _, td := range inst.ToInstanceData().Tabs {
			if td.ID == tab.ID {
				got = tabExitFromData(td.Exit)
			}
		}
		require.NotNil(t, got, "a completed remote command no longer reads as running")
		assert.Equal(t, 3, got.Status)
	})

	t.Run("a stale exit is never stamped onto the replacement (#5960)", func(t *testing.T) {
		sb, inst := newLifecycleSandbox(t)
		first, err := inst.AddProcessTabWithID("slot", "job-a", "")
		require.NoError(t, err)
		require.NoError(t, inst.CloseTabByID("slot"))
		second, err := inst.AddProcessTabWithID("slot", "job-b", "")
		require.NoError(t, err)
		require.NotEqual(t, first.gen, second.gen, "each incarnation has its own generation")

		// The sandbox reports the OLD incarnation's ending under the reused id.
		sb.mu.Lock()
		td := sb.tabs["slot"]
		td.Gen = first.gen
		td.Exit = &TabExitData{Status: 1, StatusKnown: true}
		sb.tabs["slot"] = td
		sb.mu.Unlock()
		inst.ReconcileOffBoxTabs()
		for _, row := range inst.ToInstanceData().Tabs {
			if row.ID == "slot" {
				assert.Nil(t, row.Exit, "the live replacement must not report the old command's exit")
			}
		}
	})

	t.Run("a closed id is terminal for subscriptions until it is re-created (#5963)", func(t *testing.T) {
		_, inst := newLifecycleSandbox(t)
		rs := inst.AgentServer().(*remoteAgentServer)
		tab, err := inst.AddProcessTabWithID("term", "serve", "")
		require.NoError(t, err)
		br, err := rs.ensureBrokerByID("term")
		require.NoError(t, err)
		require.NotNil(t, br)

		require.NoError(t, inst.CloseTabByID("term"))
		_, err = rs.ensureBrokerByID(tab.ID)
		require.ErrorIs(t, err, ErrTabGone, "nothing resurrects a closed tab's broker")
		_, err = rs.SubscribeTab(tab.ID, 0)
		require.ErrorIs(t, err, ErrTabGone, "a subscriber gets a definitive gone and stops")

		// A mark with no roster row (the close had begun) is refused the same way.
		_, err = rs.ensureBrokerByID("never-existed")
		require.ErrorIs(t, err, ErrTabGone)

		_, err = inst.AddProcessTabWithID("term", "serve", "")
		require.NoError(t, err)
		_, err = rs.ensureBrokerByID("term")
		assert.NoError(t, err, "re-creating the id lifts the mark")
	})

	t.Run("an adoption refused while the session is inert is retried once started (#5996)", func(t *testing.T) {
		sb, inst := newLifecycleSandbox(t)
		sb.mu.Lock()
		sb.dropCreate = true
		sb.mu.Unlock()
		_, err := inst.AddProcessTabWithID("", "deploy.sh", "")
		require.Error(t, err)
		require.Equal(t, 1, sb.execs("deploy.sh"), "the sandbox did start it")
		require.Empty(t, rosterIDs(inst), "nothing is claimed while the outcome is unknown")

		// A sweep while the session is inert: the sandbox roster carries the tab,
		// but the spawn is refused, so neither the roster row nor the latch moves.
		inst.mu.Lock()
		inst.started = false
		inst.mu.Unlock()
		require.False(t, inst.ReconcileOffBoxTabs(), "an inert session adopts nothing")
		require.Empty(t, rosterIDs(inst), "the sandbox tab is not spliced into an inert session")
		present, adopted := intentAdopted(inst, "deploy.sh")
		require.True(t, present, "the intent still pins the minted id for a retry")
		require.False(t, adopted, "a refused adoption must not latch the intent")

		// The session comes back; the next sweep adopts the tab the sandbox
		// started, instead of skipping a latched intent forever.
		inst.mu.Lock()
		inst.started = true
		inst.mu.Unlock()
		require.True(t, inst.ReconcileOffBoxTabs(), "the un-latched intent is adopted once started")
		assert.Equal(t, sb.ids(), rosterIDs(inst), "daemon and sandbox rosters agree")
		inst.ReconcileOffBoxTabs()
		assert.Len(t, rosterIDs(inst), 1, "adoption is idempotent")
	})

	t.Run("an adoption refused under a spawn-block fence is retried once the fence lifts (#5996)", func(t *testing.T) {
		sb, inst := newLifecycleSandbox(t)
		sb.mu.Lock()
		sb.dropCreate = true
		sb.mu.Unlock()
		_, err := inst.AddProcessTabWithID("", "deploy.sh", "")
		require.Error(t, err)
		require.Equal(t, 1, sb.execs("deploy.sh"), "the sandbox did start it")
		require.Empty(t, rosterIDs(inst), "nothing is claimed while the outcome is unknown")

		// An archive in flight fences off the spawn while started stays true —
		// the very case the started-flag guard cannot see. The sweep must refuse
		// and leave the intent un-latched.
		inst.SetInFlightOpForTest(OpArchiving)
		require.False(t, inst.ReconcileOffBoxTabs(), "a fenced session adopts nothing")
		require.Empty(t, rosterIDs(inst), "the sandbox tab is not spliced under a spawn-block fence")
		present, adopted := intentAdopted(inst, "deploy.sh")
		require.True(t, present, "the intent still pins the minted id for a retry")
		require.False(t, adopted, "a fenced spawn must not latch the intent")

		// The archive aborts; the fence lifts and the next sweep adopts the tab.
		inst.SetInFlightOpForTest(OpNone)
		require.True(t, inst.ReconcileOffBoxTabs(), "the un-latched intent is adopted once the fence lifts")
		assert.Equal(t, sb.ids(), rosterIDs(inst), "daemon and sandbox rosters agree")
		assert.Len(t, rosterIDs(inst), 1)
	})
}

// A closing tab's tmux name stays reserved until teardown is confirmed, so a
// concurrent create cannot take a name that is still live (#5961).
func TestCloseTabStagesCleanupBeforeTeardown(t *testing.T) {
	inst, _ := raceMockInstance(t, "af_close_stage", func() {})
	tab, err := inst.AddProcessTab("worker", "")
	require.NoError(t, err)

	inst.mu.Lock()
	idx, ok := inst.tabIndexByIDLocked(tab.ID)
	require.True(t, ok)
	removed := inst.removeTabLocked(idx)
	handle, _ := inst.stageCleanupLocked(removed)
	require.NotNil(t, handle)
	next := uniqueTabTmuxNameReserving(inst.Tabs, inst.pendingTabCleanup, nil,
		inst.Tabs[0].tmux.SanitizedName(), removed.Name)
	inst.mu.Unlock()
	assert.NotEqual(t, handle.TmuxName, next, "the closing tab's live tmux name must not be re-derived")

	// A confirmed teardown retires the reservation.
	require.NoError(t, inst.settleRemovedTab(removed, handle, nil))
	assert.Empty(t, inst.PendingTabCleanup())
}

// A pending close persisted for a SANDBOX row survives a restart and is retried:
// the restore branch for sandbox backends is not the local one, and the record
// is the only handle on a command still running there (#5996, property 5).
func TestPendingRemoteCloseSurvivesRestartForSandboxBackends(t *testing.T) {
	for _, backend := range []string{"sandbox", "ssh", "docker", "remote"} { // "remote" is the hook backend's persisted type
		t.Run(backend, func(t *testing.T) {
			sb, live := newLifecycleSandbox(t)
			tab, err := live.AddProcessTabWithID("", "serve", "")
			require.NoError(t, err)
			sb.mu.Lock()
			sb.closeDown = true
			sb.mu.Unlock()
			require.Error(t, live.CloseTabByID(tab.ID))
			stored := live.ToInstanceData()
			require.Len(t, stored.PendingRemoteTabClose, 1)

			// A daemon restart: the row is rebuilt from storage as this backend.
			stored.BackendType = backend
			restored, err := FromInstanceData(stored)
			require.NoError(t, err)
			require.Equal(t, stored.PendingRemoteTabClose, restored.PendingRemoteTabClose(),
				"the pending close must be restored for every off-box backend")

			// Reattach hands the row a live client; the sweep then retries it.
			sb.mu.Lock()
			sb.closeDown = false
			sb.mu.Unlock()
			restored.mu.Lock()
			restored.remoteClient = live.remoteClient
			restored.started = true
			restored.mu.Unlock()
			restored.ReconcileOffBoxTabs()
			assert.Empty(t, restored.PendingRemoteTabClose(), "retried and confirmed after the restart")
			assert.Equal(t, []string{tab.ID}, sb.closed())
		})
	}
}

// The generation check runs on the sandbox, atomically with the removal: a
// retried close for an older incarnation finds a newer tab under the id and
// closes nothing (#5996).
func TestCloseTabByIDIfGenFencesTheIncarnation(t *testing.T) {
	inst, _ := raceMockInstance(t, "af_ifgen", func() {})
	tab, err := inst.AddProcessTab("worker", "")
	require.NoError(t, err)
	require.NotEmpty(t, tab.gen)

	err = inst.CloseTabByIDIfGen(tab.ID, "an-older-incarnation")
	require.ErrorIs(t, err, ErrTabGone)
	_, still := inst.TabIndexByID(tab.ID)
	assert.True(t, still, "a mismatched generation must leave the newer tab running")

	require.NoError(t, inst.CloseTabByIDIfGen(tab.ID, tab.gen))
	_, still = inst.TabIndexByID(tab.ID)
	assert.False(t, still)
}

// Property 1 (execute at most once) across every interleaving of a caller's
// retry with the status sweep adopting the tab the first, unanswered attempt
// started (#5996). The intent that holds the minted id must survive the
// adoption, or a late retry mints a new id and runs the command again.
func TestRemoteCreateRetryVersusSweepRunsOnce(t *testing.T) {
	lostCreate := func(t *testing.T) (*lifecycleSandbox, *Instance) {
		sb, inst := newLifecycleSandbox(t)
		sb.mu.Lock()
		sb.dropCreate = true
		sb.mu.Unlock()
		_, err := inst.AddProcessTabWithID("", "deploy.sh", "")
		require.Error(t, err, "the unanswered create is reported")
		require.Equal(t, 1, sb.execs("deploy.sh"))
		sb.mu.Lock()
		sb.dropCreate = false
		sb.mu.Unlock()
		return sb, inst
	}
	settled := func(t *testing.T, sb *lifecycleSandbox, inst *Instance) {
		t.Helper()
		assert.Equal(t, 1, sb.execs("deploy.sh"), "the command ran exactly once")
		assert.Len(t, rosterIDs(inst), 1, "one tab, not a duplicate")
		assert.Equal(t, sb.ids(), rosterIDs(inst), "daemon and sandbox agree on the id")
	}

	t.Run("retry then sweep", func(t *testing.T) {
		sb, inst := lostCreate(t)
		retried, err := inst.AddProcessTabWithID("", "deploy.sh", "")
		require.NoError(t, err)
		inst.ReconcileOffBoxTabs()
		settled(t, sb, inst)
		assert.Contains(t, rosterIDs(inst), retried.ID)
	})

	t.Run("sweep then retry joins the adopted tab", func(t *testing.T) {
		sb, inst := lostCreate(t)
		require.True(t, inst.ReconcileOffBoxTabs(), "the sweep adopts the tab the sandbox started")
		adopted := rosterIDs(inst)
		require.Len(t, adopted, 1)

		retried, err := inst.AddProcessTabWithID("", "deploy.sh", "")
		require.NoError(t, err, "the retry succeeds by joining, not by running again")
		assert.Contains(t, adopted, retried.ID, "the retry resolved to the adopted id")
		settled(t, sb, inst)

		// The retry consumed the intent: a deliberate second request is a new run.
		again, err := inst.AddProcessTabWithID("", "deploy.sh", "")
		require.NoError(t, err)
		assert.NotEqual(t, retried.ID, again.ID)
		assert.Equal(t, 2, sb.execs("deploy.sh"))
	})

	t.Run("concurrent retry and sweep", func(t *testing.T) {
		for round := 0; round < 25; round++ {
			sb, inst := lostCreate(t)
			var wg sync.WaitGroup
			wg.Add(3)
			go func() { defer wg.Done(); inst.ReconcileOffBoxTabs() }()
			go func() { defer wg.Done(); inst.ReconcileOffBoxTabs() }()
			go func() {
				defer wg.Done()
				_, err := inst.AddProcessTabWithID("", "deploy.sh", "")
				assert.NoError(t, err)
			}()
			wg.Wait()
			settled(t, sb, inst)
		}
	})

	t.Run("an expired intent no longer pins a later request", func(t *testing.T) {
		sb, inst := lostCreate(t)
		inst.ReconcileOffBoxTabs()
		orig := tabIntentTTL
		tabIntentTTL = 0
		t.Cleanup(func() { tabIntentTTL = orig })
		time.Sleep(time.Millisecond)
		fresh, err := inst.AddProcessTabWithID("", "deploy.sh", "")
		require.NoError(t, err)
		assert.Equal(t, 2, sb.execs("deploy.sh"), "after the intent expires the request is a new one")
		assert.NotEmpty(t, fresh.ID)
	})
}

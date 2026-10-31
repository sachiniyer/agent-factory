package session

import (
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// planeChannel is a clientlessChannel that, like the remote WS channel, refuses
// input and resize until its capture is open (activeConn: "remote pty stream is
// not open").
type planeChannel struct {
	mu    sync.Mutex
	open  bool
	w     *io.PipeWriter
	sends int
}

func (c *planeChannel) StartCapture() (io.ReadCloser, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, w := io.Pipe()
	c.w, c.open = w, true
	return r, nil
}

func (c *planeChannel) StopCapture() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.open = false
	if c.w != nil {
		_ = c.w.Close()
	}
	return nil
}

func (c *planeChannel) isOpen() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.open
}

func (c *planeChannel) gate() error {
	if !c.isOpen() {
		return fmt.Errorf("remote pty stream is not open")
	}
	return nil
}

func (c *planeChannel) SendRaw([]byte) error {
	if err := c.gate(); err != nil {
		return err
	}
	c.mu.Lock()
	c.sends++
	c.mu.Unlock()
	return nil
}

func (c *planeChannel) sent() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sends
}

func (c *planeChannel) Resize(uint16, uint16) error     { return c.gate() }
func (c *planeChannel) Snapshot() (PaneSnapshot, error) { return PaneSnapshot{}, nil }
func (c *planeChannel) PaneDead() bool                  { return false }

// planeHarness is a remote agent-server whose /v1/health probe can be made to
// fail (an UNKNOWN answer) or to confirm the id plane, over fake channels.
type planeHarness struct {
	mu      sync.Mutex
	modern  bool
	failing bool
	noTabs  bool // a legacy agent-server: no tab API, so /v1/agent/tabs is a 404
	s       *remoteAgentServer
	ord     map[string]*planeChannel
	id      map[string]*planeChannel
	ws      *tabWireServer
}

func newPlaneHarness(t *testing.T, tabIDs ...string) *planeHarness {
	t.Helper()
	h := &planeHarness{ord: map[string]*planeChannel{}, id: map[string]*planeChannel{}}
	ws := newTabWireServer(t)
	h.ws = ws
	ws.route("/v1/agent/preview", map[string]any{"content": "screen"})
	// The SANDBOX's own roster: agent tab first, then the given tabs in the order
	// the sandbox holds them. A local reorder on the daemon never changes it.
	ws.mux.HandleFunc("/v1/agent/tabs", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		noTabs := h.noTabs
		h.mu.Unlock()
		if noTabs {
			http.NotFound(w, r)
			return
		}
		rows := []TabData{{ID: "sandbox-agent", Kind: TabKindAgent, Name: agentTabName}}
		for _, id := range tabIDs {
			rows = append(rows, TabData{ID: id, Kind: TabKindShell, Name: id})
		}
		ws.encode(w, map[string]any{"data": map[string]any{"tabs": rows}, "error": nil})
	})
	ws.mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		failing, modern := h.failing, h.modern
		h.mu.Unlock()
		if failing {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		features := []string{tabAPIFeature}
		if h.noTabsLocked() {
			features = nil
		}
		if modern {
			features = append(features, tabIDAddressingFeature)
		}
		ws.encode(w, map[string]any{"data": map[string]any{"ok": true, "features": features}, "error": nil})
	})
	srv := httptest.NewServer(ws.mux)
	t.Cleanup(srv.Close)
	tabs := []*Tab{newRemoteAgentTab()}
	for _, id := range tabIDs {
		tabs = append(tabs, &Tab{ID: id, Name: id, Kind: TabKindShell})
	}
	h.s = &remoteAgentServer{rc: ws.client(t, srv.URL), inst: &Instance{Title: "sb", Tabs: tabs}}
	h.s.boundChannel = func(id string) clientlessChannel {
		h.mu.Lock()
		defer h.mu.Unlock()
		c := &planeChannel{}
		h.ord[id] = c
		return c
	}
	h.s.idChannel = func(id string) clientlessChannel {
		h.mu.Lock()
		defer h.mu.Unlock()
		c := &planeChannel{}
		h.id[id] = c
		return c
	}
	t.Cleanup(func() { _ = h.s.Kill(false) })
	return h
}

func (h *planeHarness) noTabsLocked() bool { return h.noTabs }

func (h *planeHarness) setProbe(failing, modern bool) {
	h.mu.Lock()
	h.failing, h.modern = failing, modern
	h.mu.Unlock()
}

// A probe that fails is an unknown, not a verdict: the subscription it left on
// the ordinal plane keeps working when a later probe confirms the id plane.
func TestTransientProbeMissThenSwitchKeepsTheLiveSubscription(t *testing.T) {
	h := newPlaneHarness(t, "t1")
	h.setProbe(true, false) // the first probe misses
	sub, err := h.s.SubscribeTab("t1", 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Close() })

	h.setProbe(false, true) // a later probe confirms the id plane
	require.True(t, h.s.idNative())

	require.NoError(t, h.s.InputTab("t1", []byte("ls\n")), "input keeps flowing on the live subscription")
	require.NoError(t, h.s.ResizeTab("t1", 30, 100))
	h.mu.Lock()
	_, idDialled := h.id["t1"]
	h.mu.Unlock()
	assert.False(t, idDialled, "no second broker is built under a live subscription")

	// Once that subscription ends, the tab moves to the confirmed plane.
	require.NoError(t, sub.Close())
	sub2, err := h.s.SubscribeTab("t1", 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub2.Close() })
	require.NoError(t, h.s.InputTab("t1", []byte("x")))
	h.mu.Lock()
	idChan := h.id["t1"]
	h.mu.Unlock()
	require.NotNil(t, idChan)
	assert.True(t, idChan.isOpen(), "the new subscription rides the id plane")
}

func (h *planeHarness) sendsOf(id string) (n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c := h.ord[id]; c != nil {
		n += c.sent()
	}
	if c := h.id[id]; c != nil {
		n += c.sent()
	}
	return n
}

func (h *planeHarness) totalSends() (n int) {
	h.mu.Lock()
	ids := make([]string, 0, len(h.ord)+len(h.id))
	for id := range h.ord {
		ids = append(ids, id)
	}
	for id := range h.id {
		ids = append(ids, id)
	}
	h.mu.Unlock()
	seen := map[string]bool{}
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			n += h.sendsOf(id)
		}
	}
	return n
}

func openPlaneOnce(h *planeHarness, id string, idx int) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	o, i := h.ord[id], h.id[id]
	switch {
	case i != nil && i.isOpen() && (o == nil || !o.isOpen()):
		return "id"
	case o != nil && o.isOpen() && (i == nil || !i.isOpen()):
		return "ordinal"
	case (o == nil || !o.isOpen()) && (i == nil || !i.isOpen()):
		return "none"
	}
	return "both"
}

// Model: random subscribe / end / input / resize steps interleaved with probe
// outcomes of unknown and confirmed, and a reorder of the roster. While a tab has any live subscription, all
// of them stay on one plane, none is dropped, and input and resize succeed.
func TestPlaneSwitchModel(t *testing.T) {
	const seeds, depth = 200, 24
	tabIDs := []string{"t1", "t2"}
	for seed := int64(1); seed <= seeds; seed++ {
		rng := rand.New(rand.NewSource(seed))
		h := newPlaneHarness(t, tabIDs...)
		h.setProbe(true, false)
		live := map[string][]PTYSubscription{}
		plane := map[string]string{} // tab → "ordinal" | "id" while it has live subs
		var trace []string
		fail := func(format string, args ...any) {
			t.Helper()
			require.FailNowf(t, "invariant broken", "seed %d after %v: %s", seed, trace, fmt.Sprintf(format, args...))
		}
		// A departing subscriber stops its broker's capture on a goroutine, so a
		// moved tab shows both planes open for a moment; the verdict waits it out.
		openPlane := func(id string, idx int) string {
			got := ""
			for try := 0; try < 200; try++ {
				if got = openPlaneOnce(h, id, idx); got != "both" {
					return got
				}
				time.Sleep(5 * time.Millisecond)
			}
			return got
		}
		for step := 0; step < depth; step++ {
			k := rng.Intn(len(tabIDs))
			id, idx := tabIDs[k], k+1
			switch rng.Intn(8) {
			case 7:
				trace = append(trace, "reorder")
				h.s.inst.mu.Lock()
				h.s.inst.Tabs[1], h.s.inst.Tabs[2] = h.s.inst.Tabs[2], h.s.inst.Tabs[1]
				h.s.inst.mu.Unlock()
			case 0:
				trace = append(trace, "probe-unknown")
				h.setProbe(true, false)
			case 1:
				trace = append(trace, "probe-confirms-id-plane")
				h.setProbe(false, true)
				h.s.idNative()
			case 2, 3:
				trace = append(trace, "subscribe("+id+")")
				sub, err := h.s.SubscribeTab(id, 0)
				if err != nil {
					fail("subscribe: %v", err)
				}
				live[id] = append(live[id], sub)
				got := openPlane(id, idx)
				if prev, ok := plane[id]; ok && len(live[id]) > 1 && got != prev {
					fail("a second subscription moved %s from %s to %s under a live one", id, prev, got)
				}
				plane[id] = got
			case 4:
				if len(live[id]) == 0 {
					continue
				}
				trace = append(trace, "end("+id+")")
				_ = live[id][0].Close()
				live[id] = live[id][1:]
				if len(live[id]) == 0 {
					delete(plane, id)
				}
			case 5:
				trace = append(trace, "input("+id+")")
				before := h.totalSends()
				mine := h.sendsOf(id)
				err := h.s.InputTab(id, []byte("x"))
				if len(live[id]) > 0 {
					if err != nil {
						fail("input on a tab with %d live subscriptions: %v", len(live[id]), err)
					}
					if h.sendsOf(id) != mine+1 || h.totalSends() != before+1 {
						fail("input for %s reached another tab's stream", id)
					}
				}
			default:
				trace = append(trace, "resize("+id+")")
				err := h.s.ResizeTab(id, 24, 80)
				if len(live[id]) > 0 && err != nil {
					fail("resize on a tab with %d live subscriptions: %v", len(live[id]), err)
				}
			}
			// The daemon never puts an ordinal from its own roster on the wire: a
			// tab is addressed by id, or by its slot in the SANDBOX's roster.
			for n, tab := range tabIDs {
				tabN, byID, err := h.s.wireTarget(tab)
				switch {
				case err != nil:
					fail("wire target of %s: %v", tab, err)
				case byID != "" && byID != tab:
					fail("%s addressed as id %q", tab, byID)
				case byID == "" && tabN != n+1:
					fail("%s addressed by ordinal %d, the sandbox holds it at %d", tab, tabN, n+1)
				}
			}
			for tab, subs := range live {
				if len(subs) == 0 {
					continue
				}
				if got := openPlane(tab, 0); got != plane[tab] {
					fail("%s live subscriptions are on %s, started on %s", tab, got, plane[tab])
				}
			}
		}
		for _, subs := range live {
			for _, sub := range subs {
				_ = sub.Close()
			}
		}
	}
}

// A repaint or redial of a channel bound to A addresses A in the SANDBOX's
// roster after a local reorder, with the id probe failing or confirmed: the
// daemon's own ordinals never reach the wire (#5952).
func TestSnapshotAfterLocalReorderAddressesTheBoundTab(t *testing.T) {
	for _, probe := range []string{"probe-failed", "legacy", "confirmed"} {
		t.Run(probe, func(t *testing.T) {
			h := newPlaneHarness(t, "t1", "t2")
			switch probe {
			case "probe-failed":
				h.setProbe(true, false)
			case "confirmed":
				h.setProbe(false, true)
			}
			ch := &remoteClientlessChannel{rc: h.s.rc, label: "t1", route: func() (int, string, error) { return h.s.wireTarget("t1") }}

			// The daemon reorders its own roster: t1 now sits in slot 2 there.
			h.s.inst.mu.Lock()
			h.s.inst.Tabs[1], h.s.inst.Tabs[2] = h.s.inst.Tabs[2], h.s.inst.Tabs[1]
			h.s.inst.mu.Unlock()

			_, err := ch.Snapshot()
			require.NoError(t, err)
			h.ws.mu.Lock()
			body := h.ws.bodies["/v1/agent/preview"]
			h.ws.mu.Unlock()
			if probe == "confirmed" {
				assert.Equal(t, "t1", body["tab_id"], "addressed by stable id")
			} else {
				assert.EqualValues(t, 1, body["tab"], "the sandbox holds t1 at slot 1, whatever the daemon's order")
				assert.Nil(t, body["tab_id"])
			}
		})
	}

	t.Run("an id absent from the sandbox's roster is refused", func(t *testing.T) {
		h := newPlaneHarness(t, "t1")
		h.s.inst.mu.Lock()
		h.s.inst.Tabs = append(h.s.inst.Tabs, &Tab{ID: "ghost", Name: "ghost", Kind: TabKindShell})
		h.s.inst.mu.Unlock()
		_, _, err := h.s.wireTarget("ghost")
		require.ErrorIs(t, err, ErrTabGone)
	})
}

// The agent tab is pinned to slot 0 on both rosters and cannot be confused with
// another tab, so it stays subscribable whatever the health probe says — on a
// legacy agent-server with no tab API too (#6082).
func TestLegacyAgentTabStaysSubscribableWhateverTheProbeSays(t *testing.T) {
	const seeds, depth = 100, 16
	for seed := int64(1); seed <= seeds; seed++ {
		rng := rand.New(rand.NewSource(seed))
		h := newPlaneHarness(t)
		h.mu.Lock()
		h.noTabs = true
		h.mu.Unlock()
		agentID := h.s.inst.Tabs[0].ID
		var live []PTYSubscription
		var trace []string
		fail := func(format string, args ...any) {
			t.Helper()
			require.FailNowf(t, "invariant broken", "seed %d after %v: %s", seed, trace, fmt.Sprintf(format, args...))
		}
		h.setProbe(true, false) // the first probe misses
		for step := 0; step < depth; step++ {
			switch rng.Intn(6) {
			case 0:
				trace = append(trace, "probe-fails")
				h.setProbe(true, false)
			case 1:
				trace = append(trace, "probe-answers-legacy")
				h.setProbe(false, false)
				h.s.idNative()
			case 2, 3:
				trace = append(trace, "subscribe-agent")
				sub, err := h.s.SubscribeTab(agentID, 0)
				if err != nil {
					fail("subscribing to the agent tab: %v", err)
				}
				live = append(live, sub)
			case 4:
				trace = append(trace, "input-agent")
				if err := h.s.InputTab(agentID, []byte("x")); len(live) > 0 && err != nil {
					fail("input to a live agent tab: %v", err)
				}
			default:
				if len(live) > 0 {
					trace = append(trace, "end")
					_ = live[0].Close()
					live = live[1:]
				}
			}
			tab, byID, err := h.s.wireTarget(agentID)
			if err != nil || (byID == "" && tab != 0) {
				fail("agent tab wire target = (%d,%q,%v), want slot 0", tab, byID, err)
			}
		}
		for _, sub := range live {
			_ = sub.Close()
		}
	}
}

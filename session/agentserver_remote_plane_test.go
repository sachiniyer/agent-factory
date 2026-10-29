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
	mu   sync.Mutex
	open bool
	w    *io.PipeWriter
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

func (c *planeChannel) SendRaw([]byte) error            { return c.gate() }
func (c *planeChannel) Resize(uint16, uint16) error     { return c.gate() }
func (c *planeChannel) Snapshot() (PaneSnapshot, error) { return PaneSnapshot{}, nil }
func (c *planeChannel) PaneDead() bool                  { return false }

// planeHarness is a remote agent-server whose /v1/health probe can be made to
// fail (an UNKNOWN answer) or to confirm the id plane, over fake channels.
type planeHarness struct {
	mu      sync.Mutex
	modern  bool
	failing bool
	s       *remoteAgentServer
	ord     map[int]*planeChannel
	id      map[string]*planeChannel
}

func newPlaneHarness(t *testing.T, tabIDs ...string) *planeHarness {
	t.Helper()
	h := &planeHarness{ord: map[int]*planeChannel{}, id: map[string]*planeChannel{}}
	ws := newTabWireServer(t)
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
		features := []string{}
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
	h.s.ordinalChannel = func(tab int) clientlessChannel {
		h.mu.Lock()
		defer h.mu.Unlock()
		c := &planeChannel{}
		h.ord[tab] = c
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

func openPlaneOnce(h *planeHarness, id string, idx int) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	o, i := h.ord[idx], h.id[id]
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
// outcomes of unknown and confirmed. While a tab has any live subscription, all
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
			switch rng.Intn(7) {
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
				err := h.s.InputTab(id, []byte("x"))
				if len(live[id]) > 0 && err != nil {
					fail("input on a tab with %d live subscriptions: %v", len(live[id]), err)
				}
			default:
				trace = append(trace, "resize("+id+")")
				err := h.s.ResizeTab(id, 24, 80)
				if len(live[id]) > 0 && err != nil {
					fail("resize on a tab with %d live subscriptions: %v", len(live[id]), err)
				}
			}
			for tab, subs := range live {
				if len(subs) == 0 {
					continue
				}
				if got := openPlane(tab, map[string]int{"t1": 1, "t2": 2}[tab]); got != plane[tab] {
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

package session

import (
	"errors"
	"io"
	"testing"
	"time"
)

// #2136: closing a tab must end THAT tab's PTY stream. Before the fix CloseTab
// only killed the tab's tmux session, leaving its broker open — so a subscriber
// (a third-party client on apiclient.DialStream, or a pane) blocked on a stream
// that could never produce another byte until the 15s WS keepalive dropped it,
// with nothing on the protocol saying the tab had gone.
//
// The three properties pinned here are the ones the fix must not trade against
// each other: the closed tab's subscribers end PROMPTLY and with a distinguishable
// cause, a SIBLING tab's subscribers are untouched (the brokers are per-tab, keyed
// by stable id — a session-wide teardown would be a regression, not a fix), and a
// repeat/no-subscriber close is a no-op rather than a double-close panic.

// newTabStreamInstance builds a started instance with an agent tab and two shell
// tabs whose brokers are injected over in-memory channels, since a probe instance
// has no real tmux pane for ensureBroker to bind. The tabs carry no tmux session,
// so CloseTab's teardown is exercised without a tmux server.
func newTabStreamInstance(t *testing.T) (*Instance, *localAgentServer, map[string]*ptyBroker) {
	t.Helper()
	inst, _ := newProbeInstance(t)
	agent := &Tab{ID: newTabID(), Name: agentTabName, Kind: TabKindAgent}
	first := &Tab{ID: newTabID(), Name: "alpha", Kind: TabKindShell}
	second := &Tab{ID: newTabID(), Name: "beta", Kind: TabKindShell}
	inst.mu.Lock()
	inst.Tabs = []*Tab{agent, first, second}
	inst.mu.Unlock()

	las := inst.AgentServer().(*localAgentServer)
	brokers := map[string]*ptyBroker{
		agent.ID:  newPTYBroker(&fakeClientlessChannel{}),
		first.ID:  newPTYBroker(&fakeClientlessChannel{}),
		second.ID: newPTYBroker(&fakeClientlessChannel{}),
	}
	las.mu.Lock()
	las.brokers = brokers
	las.mu.Unlock()
	return inst, las, brokers
}

// TestCloseTabEndsOnlyThatTabsStream is the #2136 regression: the closed tab's
// subscriber ends with ErrTabClosed straight away, and the sibling tab's
// subscriber keeps streaming.
func TestCloseTabEndsOnlyThatTabsStream(t *testing.T) {
	inst, las, brokers := newTabStreamInstance(t)
	tabs := inst.GetTabs()
	closedID, siblingID := tabs[1].ID, tabs[2].ID

	closedSub, err := brokers[closedID].subscribe(0)
	if err != nil {
		t.Fatalf("subscribe closed tab: %v", err)
	}
	siblingSub, err := brokers[siblingID].subscribe(0)
	if err != nil {
		t.Fatalf("subscribe sibling tab: %v", err)
	}

	if err := inst.CloseTab(1); err != nil {
		t.Fatalf("CloseTab: %v", err)
	}

	// The closed tab's subscriber is woken NOW (the 2s bound stands in for the
	// keepalive it used to wait out) and can tell a tab close from a session end.
	if _, err := nextWithin(t, closedSub, 2*time.Second); !errors.Is(err, ErrTabClosed) {
		t.Fatalf("NextEvent on the closed tab = %v, want ErrTabClosed", err)
	}
	// ...while still reading as end-of-stream to every consumer that only asks
	// io.EOF (the whole point of wrapping it).
	if _, err := nextWithin(t, closedSub, 2*time.Second); !errors.Is(err, io.EOF) {
		t.Fatalf("ErrTabClosed must wrap io.EOF for existing end-of-stream checks; got %v", err)
	}
	// The closed tab's broker is dropped, so nothing retains its ring buffer.
	las.mu.Lock()
	_, stillMapped := las.brokers[closedID]
	las.mu.Unlock()
	if stillMapped {
		t.Error("the closed tab's broker must be dropped from the agent-server")
	}

	// The SIBLING tab is untouched: its stream is still live and still delivering.
	brokers[siblingID].feed([]byte("still-here"))
	for {
		ev, err := nextWithin(t, siblingSub, 2*time.Second)
		if err != nil {
			t.Fatalf("sibling tab's subscriber must stay live, got %v", err)
		}
		if ev.Kind != PTYData { // the fresh subscriber's one-shot repaint
			continue
		}
		if string(ev.Data) != "still-here" {
			t.Fatalf("sibling event = %+v, want data %q", ev, "still-here")
		}
		return
	}
}

// TestCloseTabStreamTeardownIsIdempotent pins the safety properties: closing a
// tab nobody streams, an already-closed broker, and a repeat close must all be
// no-ops rather than panics or double-closes.
func TestCloseTabStreamTeardownIsIdempotent(t *testing.T) {
	inst, las, brokers := newTabStreamInstance(t)
	tabs := inst.GetTabs()
	closedID := tabs[1].ID

	// Already closed by a racing session teardown: closing the tab on top of it
	// must not re-run the teardown or panic.
	brokers[closedID].close()
	if err := inst.CloseTab(1); err != nil {
		t.Fatalf("CloseTab over an already-closed broker: %v", err)
	}

	// A tab nobody ever streamed has no broker at all.
	las.mu.Lock()
	delete(las.brokers, tabs[2].ID)
	las.mu.Unlock()
	if err := inst.CloseTab(1); err != nil { // beta is at index 1 now
		t.Fatalf("CloseTab for a tab with no broker: %v", err)
	}

	// And an agent-server that never built a broker map at all (nothing streamed):
	// no map, no id, no subscribers — still a no-op.
	las.mu.Lock()
	las.brokers = nil
	las.mu.Unlock()
	las.closeTabStream("some-id")
	las.closeTabStream("")

	// Idempotent at the broker itself: repeated teardowns of either flavour on the
	// same broker must not double-close its capture.
	agentBroker := brokers[tabs[0].ID]
	agentBroker.closeTab()
	agentBroker.closeTab()
	agentBroker.close()
}

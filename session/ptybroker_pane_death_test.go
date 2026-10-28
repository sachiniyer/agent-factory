package session

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// A pane that dies while watched keeps its pipe-pane attached (remain-on-exit),
// so the capture never EOFs. The watcher must notice pane_dead on its own and
// release the dead-screen repaint to the attached subscriber (#5618).
func TestPTYBrokerPaneDeathWithoutEOFRepaintsAttachedSubscriber(t *testing.T) {
	ch := &fakeClientlessChannel{
		snapshot:        []byte("1 2 3\nPane is dead"),
		snapshotRows:    24,
		snapshotCols:    80,
		snapshotHasSize: true,
	}
	br := newPTYBroker(ch)
	br.paneDeathPoll = 10 * time.Millisecond
	t.Cleanup(br.close)
	sub, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	// Drain the live-subscribe opening events.
	for {
		if _, err := nextWithin(t, sub, 100*time.Millisecond); err != nil {
			break
		}
	}

	// The pane dies; the capture stays open (no EOF) and pipe-pane now refuses.
	ch.mu.Lock()
	ch.paneDead = true
	ch.startErr = errors.New("pipe-pane: target pane has exited")
	ch.mu.Unlock()

	var got *PTYEvent
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ev, err := nextWithin(t, sub, 200*time.Millisecond)
		if err != nil {
			continue
		}
		if ev.Kind == PTYRepaint {
			got = &ev
			break
		}
	}
	if got == nil {
		t.Fatal("no dead-screen repaint arrived after the watched pane died")
	}
	if want := "Pane is dead"; !strings.Contains(string(got.Data), want) {
		t.Fatalf("repaint %q lacks %q", got.Data, want)
	}
}

// A healthy pane must not be disturbed by the watcher.
func TestPTYBrokerPaneWatcherLeavesLivePaneAlone(t *testing.T) {
	ch := &fakeClientlessChannel{snapshotHasSize: true}
	br := newPTYBroker(ch)
	br.paneDeathPoll = 5 * time.Millisecond
	t.Cleanup(br.close)
	if _, err := br.subscribe(0); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	time.Sleep(60 * time.Millisecond)
	if starts, _ := ch.counts(); starts != 1 {
		t.Fatalf("live pane capture restarted; starts = %d, want 1", starts)
	}
	br.close()
}

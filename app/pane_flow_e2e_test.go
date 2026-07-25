package app

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"

	"github.com/sachiniyer/agent-factory/ui/layout"
)

// TestE2E_PaneFlow drives the real tea.Program through the pane lifecycle:
// s opens a focused pane, the tree walks to another instance and s opens a
// second pane beside it, Tab cycles the ring across both, and x hides the
// focused pane with focus landing on the survivor.
//
// Split out of pane_model_test.go for the file-length limit (#1145); it carries
// the #2559 contention-robustness helpers with it.
func TestE2E_PaneFlow(t *testing.T) {
	eh := newE2EHarness(t)
	eh.addStartedInstance("alpha")
	eh.addStartedInstance("beta")
	eh.home.sidebar.SetSelectedInstance(0)
	eh.start()

	paneState := func() (count int, titles []string, region string) {
		eh.query(func(h *home) {
			count = h.store.NumOpenPanes()
			titles = visibleTitles(h)
			region = h.ring.Active()
		})
		return
	}

	// s opens the selection (alpha) as a focused pane.
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	eh.waitUntil(e2eAsyncTimeout, "s opens alpha's pane focused", func() bool {
		count, titles, region := paneState()
		return count == 1 && len(titles) == 1 && titles[0] == "alpha" && layout.IsPaneRegion(region)
	})

	// The tree walks to beta (j walks alpha → its two tab rows → beta);
	// alpha's pane stays put. Then s opens beta beside it.
	for i := 0; i < 3; i++ {
		eh.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	}
	eh.waitUntil(e2eAsyncTimeout, "tree selection lands on beta", func() bool {
		var selected string
		eh.query(func(h *home) {
			if s := h.store.GetSelectedInstance(); s != nil {
				selected = s.Title
			}
		})
		return selected == "beta"
	})
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	eh.waitUntil(e2eAsyncTimeout, "s opens beta's pane to the right", func() bool {
		count, titles, _ := paneState()
		return count == 2 && len(titles) == 2 && titles[0] == "alpha" && titles[1] == "beta"
	})

	// Both pane headers render side by side.
	var view string
	eh.query(func(h *home) { view = h.View() })
	// Pane 2's tab label is whatever the shared active-tab index resolved to
	// after the tree walk (the index survives instance switches by design),
	// so assert the instance halves only.
	assert.Contains(t, view, "alpha · ◆ Agent", "pane 1 header shows its binding")
	assert.Contains(t, view, "beta · ", "pane 2 header shows its binding")

	// Tab cycles the focus ring; assert only that it MOVES off the current stop.
	// The exact traversal count is deliberately NOT asserted (#2559): the ring's
	// pane entries are recency-ranked (store.VisibleOpenPanes), so a background
	// preview-tick relayout can reorder them between keys, and any fixed Tab count
	// is flake-prone under scheduler contention — a blind "4 Tabs then assert" once
	// landed on a non-pane and hung the whole 30s.
	var beforeTab string
	eh.query(func(h *home) { beforeTab = h.ring.Active() })
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyTab})
	eh.waitUntil(e2eAsyncTimeout, "Tab moves the focus ring", func() bool {
		var now string
		eh.query(func(h *home) { now = h.ring.Active() })
		return now != beforeTab
	})

	// x hides a focused pane, leaving one. Retry-until-effect: the same background
	// relayout can move the ring off the focused pane between the key and its
	// Update, so a single x can no-op under contention (#2559). Re-drive the ring
	// onto a pane and re-send x until a pane is actually hidden — bounded by the
	// same 30s ceiling, never widened. A working x drops the count in the next
	// Update, so the happy path breaks out at once; only a no-op pays a re-check.
	hideDeadline := time.Now().Add(e2eAsyncTimeout)
	for {
		tabRingToPane(eh) // ring rests on a pane (bounded)
		eh.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
		hidden := false
		for confirm := time.Now().Add(500 * time.Millisecond); time.Now().Before(confirm); {
			count, titles, region := paneState()
			if count == 1 && len(titles) == 1 && layout.IsPaneRegion(region) {
				hidden = true
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if hidden {
			break
		}
		if time.Now().After(hideDeadline) {
			eh.t.Fatalf("x never hid a focused pane within %s", e2eAsyncTimeout)
		}
	}
}

// tabRingToPane presses Tab until the focus ring rests on a workspace pane. Each
// Tab is synchronised through the tea goroutine (query() drains the message queue
// up to it), so the ring state is settled when checked and the next Tab acts on a
// known position — closing the #2559 race where a blind burst interleaved with a
// pane-reordering relayout. Tab always advances the ring and panes are always
// ring stops, so this terminates on a pane within the deadline.
func tabRingToPane(eh *e2eHarness) {
	eh.t.Helper()
	deadline := time.Now().Add(e2eAsyncTimeout)
	for {
		eh.tm.Send(tea.KeyMsg{Type: tea.KeyTab})
		var region string
		eh.query(func(h *home) { region = h.ring.Active() })
		if layout.IsPaneRegion(region) {
			return
		}
		if time.Now().After(deadline) {
			eh.t.Fatalf("Tab never cycled the focus ring to a pane; last region=%q", region)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

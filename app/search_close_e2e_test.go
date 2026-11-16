package app

import (
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/ui/layout"
)

// TestE2E_SearchCloseAttachTargetsSearchedSession is the end-to-end guard for
// the search-overlay close focus-ring fix, driven through the REAL
// tea.Program event loop (teatest) with the real WindowSizeMsg/relayout and
// the real attach-transition tick — the path the unit-level
// search_close_focus_ring_test.go exercises at the model level only.
//
// Scenario (the bug report's headline gesture): the user focuses a workspace
// pane bound to session "alpha", opens the session search with `/`, searches
// for "beta", submits, then presses `o` to attach. Before the fix the focus
// ring stayed on the alpha pane across the submit, so `o` routed through
// handleEnterPane and attached the pane's stale COMMITTED binding "alpha"
// while the screen showed "beta". After the fix the submit re-homes the ring
// to the tree, so `o` reads the tree selection and attaches "beta".
//
// The attach byte stream is swapped out (attachOverlayCallbackFn) so the
// test observes the captured target title without a real WS PTY / tmux
// attach; the first-time attach help overlay is pre-seen so the attach runs
// synchronously through the transition. The E2E harness uses activeProjectHome
// + FakeBackend, so no real daemon or real tmux server is required.
func TestE2E_SearchCloseAttachTargetsSearchedSession(t *testing.T) {
	eh := newE2EHarness(t)
	eh.addStartedInstance("alpha")
	eh.addStartedInstance("beta")
	eh.home.sidebar.SetSelectedInstance(0) // cursor on alpha
	// Mark first-time attach help seen so `o` runs the attach transition
	// synchronously rather than deferring behind the help overlay.
	require.NoError(t, eh.home.appState.SetHelpScreensSeen(helpTypeInstanceAttach{}.mask()))

	// Capture the attach target via the swapped seam. The callback runs on
	// the tea goroutine, so guard the title with a mutex and signal via a
	// channel the test goroutine can poll on.
	var mu sync.Mutex
	var attachedTitle string
	attachedCh := make(chan string, 1)
	swapAttachOverlayCallbackFn(t, func(_ *home, target sessionActionTarget, _, _ string, _ func() (chan struct{}, error)) tea.Cmd {
		mu.Lock()
		attachedTitle = target.title
		mu.Unlock()
		select {
		case attachedCh <- target.title:
		default:
		}
		return nil
	})

	eh.start()

	// s opens the selection (alpha) as a focused pane — the ring rests on
	// the alpha pane, exactly the precondition that hid the bug.
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	eh.waitUntil(e2eAsyncTimeout, "s opens alpha's pane and moves the ring onto it", func() bool {
		count := 0
		region := ""
		eh.query(func(h *home) {
			count = h.store.NumOpenPanes()
			region = h.ring.Active()
		})
		return count == 1 && layout.IsPaneRegion(region)
	})

	// Open the session search overlay exactly as the `/` key does, type the
	// query "beta", and submit with Enter. The overlay selects the single
	// matching instance and the close branch re-homes the ring to the tree.
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	eh.waitUntil(e2eAsyncTimeout, "search overlay opens", func() bool {
		return eh.homeState() == stateSearch
	})
	for _, r := range "beta" {
		eh.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyEnter})

	// After the submit the ring must be on the tree (the fix) and the cursor
	// must be on beta. The real tea.WindowSize() the close path returns is
	// processed by the event loop here — so this also proves the re-home
	// survives the post-close relayout through the real program, not just the
	// model-level resizeHome call the unit test uses.
	eh.waitUntil(e2eAsyncTimeout, "submit re-homes the ring to the tree and selects beta", func() bool {
		region := ""
		selected := ""
		eh.query(func(h *home) {
			region = h.ring.Active()
			if s := h.sidebar.GetSelectedInstance(); s != nil {
				selected = s.Title
			}
		})
		return region == layout.RegionTree && selected == "beta"
	})

	// `o` presses the attach verb through the real key dispatch. With the
	// ring on the tree it reads the tree selection (beta); before the fix the
	// ring stayed on the alpha pane and `o` attached "alpha" instead.
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})

	// The attach transition fires beginAttachTransition (20ms tick) then the
	// swapped callback. Wait for the captured title.
	select {
	case got := <-attachedCh:
		require.Equal(t, "beta", got,
			"`o` after a search submit must attach the searched session (beta), "+
				"not the focused pane's stale committed binding (alpha)")
	case <-time.After(e2eAsyncTimeout):
		t.Fatalf("attach callback never fired within %s — `o` did not reach handleAttach", e2eAsyncTimeout)
	}

	// Sanity: the title captured on the tea goroutine agrees with the channel.
	mu.Lock()
	require.Equal(t, "beta", attachedTitle, "captured attach target must be beta")
	mu.Unlock()
}

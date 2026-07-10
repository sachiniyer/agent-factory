package app

import (
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInteractivePollPause_HoldsLeaseForFocusedInteractivePane is the
// embedded-interactive half of #1586: typing into a session through the FOCUSED
// embedded interactive pane (not just full-screen attach) must hold the same
// #1160 pause-poll lease, so the daemon defers automated task deliveries into
// that session instead of pasting into the user's in-progress input. The lease
// is taken on entry, renewed (throttled) while interactive, and released on
// exit.
func TestInteractivePollPause_HoldsLeaseForFocusedInteractivePane(t *testing.T) {
	h, inst := liveTestHome(t)
	stubLiveTermFactory(t)
	h.syncLiveTermPane()
	require.NotNil(t, h.livePane, "the focused eligible pane must bind so it can be entered")

	var mu sync.Mutex
	var paused, resumed []string
	h.pauseStatusPoll = func(title, _ string) error {
		mu.Lock()
		defer mu.Unlock()
		paused = append(paused, title)
		return nil
	}
	h.resumeStatusPoll = func(title, _ string) error {
		mu.Lock()
		defer mu.Unlock()
		resumed = append(resumed, title)
		return nil
	}
	run := func(cmd tea.Cmd) {
		if cmd != nil {
			_ = cmd()
		}
	}
	pausedSnap := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), paused...) }
	resumedSnap := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), resumed...) }

	// Nav mode: no lease held.
	run(h.interactivePollPauseCmd())
	assert.Empty(t, pausedSnap(), "no delivery-defer lease while not interactive")

	// Enter interactive on the focused pane → hold the target's lease.
	h.setInteractive(true)
	run(h.interactivePollPauseCmd())
	assert.Equal(t, []string{inst.Title}, pausedSnap(), "entering an interactive pane holds the target's lease")
	assert.Equal(t, inst.Title, h.interactivePauseTitle)

	// Still interactive within the renew window → throttled, no extra RPC.
	run(h.interactivePollPauseCmd())
	assert.Equal(t, []string{inst.Title}, pausedSnap(), "renew must be throttled to statusPollRenewInterval")

	// Renew window elapsed → the lease renews so it never lapses mid-typing.
	h.interactivePauseAt = time.Now().Add(-2 * statusPollRenewInterval)
	run(h.interactivePollPauseCmd())
	assert.Equal(t, []string{inst.Title, inst.Title}, pausedSnap(), "lease renews after the interval")

	// Exit interactive → release immediately so deliveries resume on detach.
	h.setInteractive(false)
	run(h.interactivePollPauseCmd())
	assert.Equal(t, []string{inst.Title}, resumedSnap(), "leaving interactive releases the lease")
	assert.Empty(t, h.interactivePauseTitle)
}

// TestInteractivePollPause_NoLeaseWhileFullScreenAttached pins that the
// embedded-interactive hold yields to full-screen attach, which runs its own
// pause heartbeat (attachOverlayCallback): while m.attached is set the embedded
// path must not also pause/renew, avoiding two owners of the same lease.
func TestInteractivePollPause_NoLeaseWhileFullScreenAttached(t *testing.T) {
	h, _ := liveTestHome(t)
	stubLiveTermFactory(t)
	h.syncLiveTermPane()

	var mu sync.Mutex
	var paused []string
	h.pauseStatusPoll = func(title, _ string) error {
		mu.Lock()
		defer mu.Unlock()
		paused = append(paused, title)
		return nil
	}
	h.resumeStatusPoll = func(string, string) error { return nil }

	h.setInteractive(true)
	h.attached.Store(true) // full-screen attach owns the lease now
	if cmd := h.interactivePollPauseCmd(); cmd != nil {
		_ = cmd()
	}
	mu.Lock()
	defer mu.Unlock()
	assert.Empty(t, paused, "the embedded path must not pause while full-screen attached")
	assert.Empty(t, h.interactivePauseTitle)
}

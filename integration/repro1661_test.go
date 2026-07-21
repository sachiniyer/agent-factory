package integration_test

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// TestEmbeddedPaneRefreshesFromSendPrompt is the #1661 end-to-end guard: an
// already-connected WS subscriber (the embedded pane) must refresh with output
// delivered out-of-band via `af sessions send-prompt` (daemon DeliverPrompt), not
// only with input it types itself.
//
// It then exercises the exact regression trigger — a full-screen attach+detach
// re-binds the embedded pane, whose reconnect used to race the attach
// subscriber's capture teardown and strand the pane on a disabled pane pipe. The
// reconnected subscriber must still receive a subsequent send-prompt delivery.
//
// Run it in the container fence: make ws-pty-roundtrip-container GOTESTARGS=...
// (a real daemon + real tmux, like TestWSPTYBrokerRoundTrip).
func TestEmbeddedPaneRefreshesFromSendPrompt(t *testing.T) {
	testguard.SkipDarwinPTYStream(t)
	h := newHarness(t)
	h.startDaemon()
	h.createSession("beta")

	// The embedded pane: a live WS subscriber, already connected before the prompt.
	emb := h.dialWS(t, "/v1/sessions/beta/stream")
	defer emb.close()
	time.Sleep(500 * time.Millisecond) // let the initial repaint drain

	// Out-of-band delivery through the daemon (the CLI/send-prompt path), NOT the
	// WS input path. The already-open subscriber must see it.
	h.run("sessions", "--repo", h.repo, "send-prompt", "beta", "echo REFRESH_ONE")
	emb.waitOutput(t, "REFRESH_ONE")

	// Now model the attach+detach cycle that triggers #1661: a full-screen attach
	// opens its own subscriber, the embedded panes are closed, then the attach
	// detaches (last subscriber leaves → capture teardown), and the embedded pane
	// re-binds with a FRESH subscription that races that teardown.
	att := h.dialWS(t, "/v1/sessions/beta/stream")
	emb.close()                                     // attach closes the embedded panes
	att.close()                                     // detach: last subscriber leaves → teardown
	emb2 := h.dialWS(t, "/v1/sessions/beta/stream") // embedded re-binds after detach
	defer emb2.close()

	// The re-bound embedded pane must refresh from a subsequent send-prompt — the
	// teardown must not have disabled the pane pipe out from under it.
	h.run("sessions", "--repo", h.repo, "send-prompt", "beta", "echo REFRESH_TWO")
	emb2.waitOutput(t, "REFRESH_TWO")
}

package app

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/ui"
)

// A home with no fetcher never polls, and a cancelled context never reaches the
// fetcher: Init arms this poll, so a harness that runs Init and tears down must
// not dial — or spawn — a daemon afterwards (Codex P2 on #6394).
func TestWebListenerNoticeCmdNeverDialsWithoutAFetcherOrAfterCancel(t *testing.T) {
	if cmd := webListenerNoticeCmd(context.Background(), nil, 0); cmd != nil {
		t.Fatal("a nil fetcher must not arm a poll")
	}

	calls := 0
	fetch := func() webListenerNoticeMsg {
		calls++
		return webListenerNoticeMsg{notice: "web UI not served"}
	}

	cmd := webListenerNoticeCmd(context.Background(), fetch, time.Millisecond)
	msg, ok := cmd().(webListenerNoticeMsg)
	if !ok || msg.notice != "web UI not served" || calls != 1 {
		t.Fatalf("live poll: msg=%v ok=%v calls=%d", msg, ok, calls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := webListenerNoticeCmd(ctx, fetch, time.Hour)(); got != nil || calls != 1 {
		t.Fatalf("cancelled poll must return nil without fetching: got=%v calls=%d", got, calls)
	}
}

// TestWebUIHintFromHealth pins the health→hint mapping of #7811: the pane may
// print ONLY an address the daemon reported itself bound to, so every state
// that cannot prove one — refused, unbound, retrying, or a responder too old
// to report listeners at all — resolves to the unknown (docs-pointer) hint,
// and a configured-off listener resolves to its own "off" state.
func TestWebUIHintFromHealth(t *testing.T) {
	resp := func(l daemon.DaemonListenerStatus) daemon.PingResponse {
		return daemon.PingResponse{Phase: daemon.DaemonPhaseReady, Listeners: l}
	}

	t.Run("bound listener yields its bound address as a URL", func(t *testing.T) {
		got := webUIHintFrom(resp(daemon.DaemonListenerStatus{
			TCPConfigured: true, TCPBound: true, TCPBoundAddr: "127.0.0.1:8443",
		}))
		require.Equal(t, ui.WebUIHint{URL: "http://127.0.0.1:8443"}, got)
	})

	t.Run("wildcard bound address renders as localhost", func(t *testing.T) {
		for _, addr := range []string{"0.0.0.0:8443", "[::]:8443"} {
			got := webUIHintFrom(resp(daemon.DaemonListenerStatus{
				TCPConfigured: true, TCPBound: true, TCPBoundAddr: addr,
			}))
			require.Equal(t, ui.WebUIHint{URL: "http://localhost:8443"}, got,
				"bound %s is a bind address, not a destination — localhost reaches it", addr)
		}
	})

	t.Run("tailnet bound address renders verbatim", func(t *testing.T) {
		got := webUIHintFrom(resp(daemon.DaemonListenerStatus{
			TCPConfigured: true, TCPBound: true, TCPBoundAddr: "100.64.1.2:8443",
		}))
		require.Equal(t, ui.WebUIHint{URL: "http://100.64.1.2:8443"}, got)
	})

	t.Run("configured-off listener is off, not unknown", func(t *testing.T) {
		got := webUIHintFrom(resp(daemon.DaemonListenerStatus{}))
		require.Equal(t, ui.WebUIHint{Disabled: true}, got)
	})

	t.Run("refused, unbound, or retrying listener is unknown, never a guess", func(t *testing.T) {
		for name, l := range map[string]daemon.DaemonListenerStatus{
			"refused":         {TCPConfigured: true, TCPRefusalReason: "refusing to bind"},
			"bind failed":     {TCPConfigured: true, TCPListenAddr: "127.0.0.1:8443", TCPBindError: "address already in use"},
			"still retrying":  {TCPConfigured: true, TCPListenAddr: "127.0.0.1:8443", BindRetrying: true},
			"bound flag only": {TCPConfigured: true, TCPBound: true},
		} {
			require.Equal(t, ui.WebUIHint{}, webUIHintFrom(resp(l)), name)
		}
	})

	t.Run("empty phase means the responder predates listener reporting", func(t *testing.T) {
		got := webUIHintFrom(daemon.PingResponse{Listeners: daemon.DaemonListenerStatus{
			TCPConfigured: true, TCPBound: true, TCPBoundAddr: "127.0.0.1:8443",
		}})
		require.Equal(t, ui.WebUIHint{}, got,
			"a zero Listeners block must not read as off, nor an unproven one as bound")
	})
}

func TestBrowseableWebAddr(t *testing.T) {
	for _, tc := range []struct{ addr, want string }{
		{"0.0.0.0:8443", "localhost:8443"},
		{"[::]:8443", "localhost:8443"},
		{":8443", "localhost:8443"},
		{"127.0.0.1:8443", "127.0.0.1:8443"},
		{"[::1]:8443", "[::1]:8443"},
		{"100.64.1.2:8443", "100.64.1.2:8443"},
		{"192.168.1.5:9000", "192.168.1.5:9000"},
		{"garbage", "garbage"},
	} {
		require.Equal(t, tc.want, browseableWebAddr(tc.addr), "addr %s", tc.addr)
	}
}

// TestHandleWebListenerNoticeStoresTheHint pins the store-and-read path: a
// landed hint reaches the pane provider, and a read with nothing proven resets
// to unknown instead of keeping a stale address live.
func TestHandleWebListenerNoticeStoresTheHint(t *testing.T) {
	h := newTestHome(t)
	h.webListenerFetcher = func() webListenerNoticeMsg { return webListenerNoticeMsg{} }

	_, _ = h.handleWebListenerNotice(webListenerNoticeMsg{
		hint: ui.WebUIHint{URL: "http://127.0.0.1:8443"},
	})
	require.Equal(t, ui.WebUIHint{URL: "http://127.0.0.1:8443"}, h.currentWebUIHint())

	_, _ = h.handleWebListenerNotice(webListenerNoticeMsg{})
	require.Equal(t, ui.WebUIHint{}, h.currentWebUIHint(),
		"a read with nothing proven must not keep serving the last address")
}

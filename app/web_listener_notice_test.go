package app

import (
	"context"
	"testing"
	"time"
)

// A home with no fetcher never polls, and a cancelled context never reaches the
// fetcher: Init arms this poll, so a harness that runs Init and tears down must
// not dial — or spawn — a daemon afterwards (Codex P2 on #6394).
func TestWebListenerNoticeCmdNeverDialsWithoutAFetcherOrAfterCancel(t *testing.T) {
	if cmd := webListenerNoticeCmd(context.Background(), nil, 0); cmd != nil {
		t.Fatal("a nil fetcher must not arm a poll")
	}

	calls := 0
	fetch := func() string { calls++; return "web UI not served" }

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

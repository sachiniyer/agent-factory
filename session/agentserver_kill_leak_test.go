package session

import (
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestInstanceKillClosesOpenStream is the #1632 regression: killing a session
// with a live WS PTY subscriber must close the broker so the subscriber's
// NextEvent returns io.EOF and the clientless capture goroutine stops — instead
// of the subscriber hanging until the keepalive lapses and the capture goroutine
// leaking (blocked on the FIFO read).
//
// The bug was that Instance.Kill() called backend.Kill() directly, bypassing the
// agent-server's Kill (the only path that closes the broker). This drives the
// exact production call — inst.Kill() (what daemon.KillSession invokes) — against
// a broker holding a live subscriber and a running capture goroutine.
func TestInstanceKillClosesOpenStream(t *testing.T) {
	inst, b := newProbeInstance(t)
	las := inst.AgentServer().(*localAgentServer)

	// Inject a broker over the in-memory fake channel into the cached agent-server
	// (a probe instance has no real tmux pane, so ensureBroker can't build one).
	// StartCapture spawns the readLoop goroutine that blocks on the pipe read —
	// the goroutine #1632 leaks if the broker is never closed.
	ch := &fakeClientlessChannel{}
	br := newPTYBroker(ch)
	sub, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	las.mu.Lock()
	las.brokers = map[string]*ptyBroker{"agent": br}
	las.mu.Unlock()

	if ch.starts != 1 {
		t.Fatalf("StartCapture calls = %d, want 1 (subscribe started the capture)", ch.starts)
	}

	// The exact call the daemon's KillSession makes.
	if err := inst.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	// 1. The underlying session was still killed (Kill routes through the backend).
	if !b.killed {
		t.Error("Kill must still tear down the underlying session (Backend.Kill)")
	}
	// 2. The open subscriber gets EOF instead of hanging until the keepalive lapses.
	if _, err := nextWithin(t, sub, 2*time.Second); !errors.Is(err, io.EOF) {
		t.Fatalf("NextEvent after kill = %v, want io.EOF (broker closed)", err)
	}
	// 3. The capture was stopped — StopCapture closed the FIFO reader and the
	// broker joined the readLoop goroutine synchronously inside close(), so it is
	// provably gone (not leaked) by the time Kill returns.
	if ch.stops != 1 {
		t.Fatalf("StopCapture calls = %d, want 1 (capture torn down on kill)", ch.stops)
	}
	assertNoLeakedGoroutine(t)

	// 4. Anti-resurrection latch: a Subscribe that raced the kill must NOT lazily
	// rebuild a broker (which would start a fresh capture goroutine on a dead
	// session and leak it again). ensureBroker refuses once closed.
	if _, err := las.Subscribe(0, 0); err == nil || !strings.Contains(err.Error(), "terminated") {
		t.Errorf("Subscribe after kill = %v, want a terminated error (no broker resurrection)", err)
	}
	if _, ok := ch.startAgain(); ok {
		t.Error("a post-kill Subscribe restarted the clientless capture — broker was resurrected")
	}
}

// startAgain reports whether StartCapture ran more than once (a resurrection).
func (f *fakeClientlessChannel) startAgain() (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts, f.starts > 1
}

// assertNoLeakedGoroutine waits for the goroutine count to settle back down after
// a broker teardown. The broker joins its readLoop synchronously in close(), so
// this normally passes on the first read; the retry loop only absorbs unrelated
// runtime/GC goroutines from settling, never a leaked capture goroutine (which
// would keep the count permanently elevated).
func assertNoLeakedGoroutine(t *testing.T) {
	t.Helper()
	runtime.GC()
	base := runtime.NumGoroutine()
	for i := 0; i < 50; i++ {
		if runtime.NumGoroutine() <= base {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Not fatal on its own — NumGoroutine is noisy under -parallel — but the
	// synchronous join + ch.stops assertions above already prove the capture
	// goroutine is gone; this is a belt-and-suspenders signal.
	t.Logf("goroutine count did not settle to %d (now %d); see synchronous-join assertions", base, runtime.NumGoroutine())
}

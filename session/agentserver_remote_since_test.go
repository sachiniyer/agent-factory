package session

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/agentproto"
)

// streamProbe is a fake agent-server /v1/sessions/{id}/stream endpoint that
// records each subscription's ?since= and answers with a fixed script of
// frames before holding the socket open.
type streamProbe struct {
	mu     sync.Mutex
	since  []string
	frames []agentproto.Frame // emitted verbatim on each accepted socket
}

func (p *streamProbe) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.since = append(p.since, r.URL.Query().Get("since"))
		p.mu.Unlock()
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		ctx := context.Background()
		for _, f := range p.frames {
			if err := agentproto.WriteFrame(ctx, conn, f); err != nil {
				return
			}
		}
		// Hold the socket until the client hangs up — like a quiet pane.
		<-conn.CloseRead(ctx).Done()
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}
}

func (p *streamProbe) sinces() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.since...)
}

// remoteSeqEventually polls the cursor under the channel's lock until it
// reaches want: the seq update lands just after the pipe write, so a single
// read can lose the lock race to a following StopCapture by a scheduling hair.
func remoteSeqEventually(t *testing.T, ch *remoteClientlessChannel, want uint64, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ch.mu.Lock()
		got := ch.remoteSeq
		ch.mu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: remoteSeq = %d, want %d", msg, got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func newStreamProbeChannel(t *testing.T, probe *streamProbe) *remoteClientlessChannel {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sessions/{id}/stream", probe.handler(t))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	rc, err := newRemoteAgentClient(AgentServerEndpoint{URL: srv.URL, Token: "tok"}, "probe")
	require.NoError(t, err)
	return newRemoteClientlessChannel(rc, 0)
}

// TestRemoteChannelSinceReplay is the W1.2 data-plane contract: the channel
// must learn the remote ring's absolute cursor from the in-band OpHello (its
// seq IS the post-clamp start), advance it per delivered PTY_OUT, and send the
// tracked cursor as ?since= on the next dial — so the sandbox replays exactly
// the bytes produced while the socket was down instead of dropping them.
func TestRemoteChannelSinceReplay(t *testing.T) {
	probe := &streamProbe{frames: []agentproto.Frame{
		agentproto.HelloFrame(1000),
		agentproto.PTYOutFrame([]byte("abcde")),
	}}
	ch := newStreamProbeChannel(t, probe)

	// First capture: no prior cursor, so no ?since at all.
	r1, err := ch.StartCapture()
	require.NoError(t, err)
	buf := make([]byte, 5)
	_, err = io.ReadFull(r1, buf)
	require.NoError(t, err, "read replayed output")
	require.Equal(t, "abcde", string(buf), "the remote's PTY_OUT bytes")
	require.NoError(t, ch.StopCapture())
	remoteSeqEventually(t, ch, 1005, "hello seq 1000 + 5 delivered bytes")

	// Second capture: the tracked cursor rides the wire as ?since=.
	_, err = ch.StartCapture()
	require.NoError(t, err)
	require.NoError(t, ch.StopCapture())

	sinces := probe.sinces()
	require.Len(t, sinces, 2, "server subscriptions")
	require.Empty(t, sinces[0], "first dial must carry no since (live tail)")
	require.Equal(t, "1005", sinces[1], "re-dial must send the tracked cursor")
}

// TestRemoteChannelSinceHonorsClamp is the stale-cursor case: a remote whose
// ring no longer covers ?since answers with a CLAMPED OpHello seq — the
// channel must adopt the reported start, not keep counting from its own
// guess, or every later re-dial asks for bytes counted against the wrong base.
func TestRemoteChannelSinceHonorsClamp(t *testing.T) {
	// Ring starts at 500; a stale cursor of 999999 clamps. The server's
	// authoritative answer is the clamped value.
	probe := &streamProbe{frames: []agentproto.Frame{agentproto.HelloFrame(500)}}
	ch := newStreamProbeChannel(t, probe)
	ch.mu.Lock()
	ch.remoteSeq = 999999 // a cursor from a remote generation that is gone
	ch.mu.Unlock()

	_, err := ch.StartCapture()
	require.NoError(t, err)
	remoteSeqEventually(t, ch, 500, "the clamped hello seq")
	require.NoError(t, ch.StopCapture())
}

// TestRemoteChannelMidStreamReseed is the PTYCursor path: the remote broker
// can fast-forward a subscriber over evicted ring bytes mid-stream, emitting
// the same OpHello as a re-seed. The cursor must JUMP to it — counting on from
// the old base would silently desync every later ?since.
func TestRemoteChannelMidStreamReseed(t *testing.T) {
	probe := &streamProbe{frames: []agentproto.Frame{
		agentproto.HelloFrame(1000),
		agentproto.PTYOutFrame([]byte("ab")),
		agentproto.HelloFrame(5000), // eviction re-seed: bytes 1002..5000 are gone
		agentproto.PTYOutFrame([]byte("cd")),
	}}
	ch := newStreamProbeChannel(t, probe)

	r, err := ch.StartCapture()
	require.NoError(t, err)
	buf := make([]byte, 4)
	_, err = io.ReadFull(r, buf)
	require.NoError(t, err)
	require.Equal(t, "abcd", string(buf))
	require.NoError(t, ch.StopCapture())
	remoteSeqEventually(t, ch, 5002, "re-seed 5000 + 2 delivered bytes — not 1000+4")
}

// TestRemoteChannelReplayContiguityGatesRepaint is the recovery-repaint
// contract (#5571 W1.2 codex): the broker suppresses the recovery repaint only
// when the LIVE capture provably replays contiguous history — the OpHello seq
// matching the requested ?since exactly. A clamped hello (remote ring evicted
// the cursor mid-outage) means the missing window is gone from the stream and
// the repaint is still owed; no live capture or a hello not yet landed is the
// repaint-safe side too.
func TestRemoteChannelReplayContiguityGatesRepaint(t *testing.T) {
	// Honored cursor: dialSeq 1000, hello answers 1000 → contiguous replay.
	probe := &streamProbe{frames: []agentproto.Frame{agentproto.HelloFrame(1000)}}
	ch := newStreamProbeChannel(t, probe)
	ch.mu.Lock()
	ch.remoteSeq = 1000
	ch.mu.Unlock()

	_, err := ch.StartCapture()
	require.NoError(t, err)
	require.Eventually(t, ch.ReplaysHistory, 5*time.Second, 10*time.Millisecond,
		"an exact-honor hello must mark the capture contiguous")
	require.NoError(t, ch.StopCapture())
	require.False(t, ch.ReplaysHistory(), "no live capture → nothing replays")

	// Clamped cursor: dialSeq 999999, hello answers the real base 500 → the
	// outage window is gone; the repaint is still owed.
	ch.mu.Lock()
	ch.remoteSeq = 999999
	ch.mu.Unlock()
	_, err = ch.StartCapture()
	require.NoError(t, err)
	t.Cleanup(func() { _ = ch.StopCapture() })
	require.Eventually(t, func() bool {
		ch.mu.Lock()
		defer ch.mu.Unlock()
		return !ch.helloPending
	}, 5*time.Second, 10*time.Millisecond, "the clamped hello must land")
	require.False(t, ch.ReplaysHistory(),
		"a clamped hello leaves a gap the ring cannot backfill — repaint stays owed")
}

func remoteReadLoopStacks() int {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	n := 0
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "remoteClientlessChannel).readLoop") {
			n++
		}
	}
	return n
}

// A readLoop blocked writing PTY output into a pipe whose reader has gone away
// must be released by StopCapture: neither the context nor the socket close
// reaches a goroutine parked in io.Pipe's write, so without closing the reader
// it would block forever (the package leak guard's find).
func TestRemoteChannelStopCaptureReleasesAReadLoopBlockedOnAnUnreadPipe(t *testing.T) {
	probe := &streamProbe{frames: []agentproto.Frame{
		agentproto.HelloFrame(0),
		agentproto.PTYOutFrame(bytes.Repeat([]byte("x"), 64*1024)),
	}}
	ch := newStreamProbeChannel(t, probe)
	before := remoteReadLoopStacks()

	_, err := ch.StartCapture() // the reader is never read
	require.NoError(t, err)
	// The pipe is unbuffered, so the first PTY_OUT parks the loop in the write.
	require.Eventually(t, func() bool {
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		return strings.Contains(string(buf), "io.(*pipe).write") && remoteReadLoopStacks() > before
	}, 5*time.Second, 10*time.Millisecond, "the read loop never reached the blocked write this test stages")

	require.NoError(t, ch.StopCapture())
	require.Eventually(t, func() bool { return remoteReadLoopStacks() <= before }, 5*time.Second, 10*time.Millisecond,
		"StopCapture left the read loop blocked writing to a reader that is gone")
}

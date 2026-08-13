package apiclient

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/agentproto"
)

// A failed stdout write must END the attach (#3191).
//
// Discarding it left the WS reader goroutine looping — reading frames and
// re-writing them to a destination that had already refused everything — until
// the pane exited on its own. `af attach | head`, a closed terminal emulator, or
// a full pipe all produce that, and the loop then burns every frame the server is
// still paying to send.
//
// This is the same discarded-error class as #3142 in daemon/ws_pty.go, and the
// correct shape already existed one package over: remoteClientlessChannel's
// capture loop does `if _, werr := pw.Write(...); werr != nil { return }`.

// failingWriter refuses every write after the first n bytes have landed, and
// counts the attempts made after it started refusing.
type failingWriter struct {
	mu       sync.Mutex
	allow    int
	refusals int
}

var errWriterGone = errors.New("stdout is gone")

func (w *failingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.allow > 0 {
		w.allow--
		return len(p), nil
	}
	w.refusals++
	return 0, errWriterGone
}

func (w *failingWriter) refusalCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.refusals
}

func TestAttachStream_StdoutWriteFailureEndsTheAttach(t *testing.T) {
	out := &failingWriter{}
	server, done := startDriverWithFailingStdout(t, out)

	// One PTY frame is enough: the very first write refuses.
	require.NoError(t, agentproto.WriteFrame(context.Background(), server,
		agentproto.Frame{Op: agentproto.OpPTYOut, Data: []byte("hello")}))

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the attach kept running after stdout refused a write (#3191): the reader goroutine " +
			"hot-loops against a dead destination until the pane exits, so `af attach | head` burns " +
			"every frame the server sends for the rest of the session")
	}
}

// …and it must stop after the FIRST refusal rather than keeping going and
// failing repeatedly. A loop that exits only once the server happens to close
// would satisfy the test above while still doing the work this fixes.
func TestAttachStream_StopsOnTheFirstRefusedWrite(t *testing.T) {
	out := &failingWriter{}
	server, done := startDriverWithFailingStdout(t, out)

	// Several frames back to back. Only the first may be attempted. A later
	// write may fail outright: the driver stops on the first refusal and tears
	// the conn down while these are still being queued, so the same outcome —
	// the attach ended — can surface here as a transport error (broken pipe)
	// instead of at the client layer. Both spellings are the success condition;
	// the refusalCount assertion below is what carries the claim (#3265).
	for i := 0; i < 5; i++ {
		if err := agentproto.WriteFrame(context.Background(), server,
			agentproto.Frame{Op: agentproto.OpPTYOut, Data: []byte("x")}); err != nil {
			break
		}
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("attach did not end on a refused write")
	}
	require.Equal(t, 1, out.refusalCount(),
		"the loop must return on the first refusal, not keep writing to a destination that said no")
}

// A test that fails an assertion returns WITHOUT waiting for the driver, and
// its Cleanup then restores the attachStdin/attachStdout/attachTermSize seams
// while the driver goroutine may still be reading them (#3265). The pass-path
// ordering — driver reads, close(done), test receives, cleanup writes — only
// exists when the test reaches its <-done, so the harness itself must join the
// driver before any other cleanup runs. These two tests model the early exit
// (start the driver, return immediately) so that join is exercised under -race
// on every run, for each harness.
func TestAttachStream_EarlyTestExitDoesNotRaceTheDriver(t *testing.T) {
	out := &failingWriter{}
	_, _ = startDriverWithFailingStdout(t, out)
}

func TestAttachStream_EarlyTestExitDoesNotRaceTheDriver_SharedHarness(t *testing.T) {
	_, _, _, _ = startDriver(t)
}

// startDriverWithFailingStdout mirrors startDriverWithInput with stdout swapped
// for a writer the test controls. Kept separate rather than parameterising the
// shared helper, so the existing tests' harness is untouched.
func startDriverWithFailingStdout(t *testing.T, out io.Writer) (*websocket.Conn, <-chan struct{}) {
	t.Helper()
	prevDrain := attachDrainTimeout
	attachDrainTimeout = 200 * time.Millisecond
	t.Cleanup(func() { attachDrainTimeout = prevDrain })

	c, connCh := attachWSServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	sc, err := c.DialStream(ctx, "alpha", "", "", 0, 0)
	require.NoError(t, err)
	server := <-connCh

	// A stdin that simply blocks: this test is about the OUTPUT direction, and a
	// pipe that never delivers keeps the input pump parked instead of racing it.
	inR, inW, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = inR.Close()
		_ = inW.Close()
	})

	prevIn, prevOut, prevSize := attachStdin, attachStdout, attachTermSize
	attachStdin, attachStdout, attachTermSize = inR, out, driverTermSize
	t.Cleanup(func() { attachStdin, attachStdout, attachTermSize = prevIn, prevOut, prevSize })

	input, err := newAttachInputReader(inR)
	require.NoError(t, err)
	d := make(chan struct{})
	// The handback writes the neutral terminal restore on release, and it must NOT
	// land on the writer this test COUNTS — otherwise the refusal tally mixes the
	// PTY copy loop's attempts with one teardown write and the assertion below
	// measures the wrong thing. Production shares one stdout; here the point is to
	// isolate the loop.
	handback := beginTerminalHandback(io.Discard, nil)
	go func() { defer close(d); driveAttachStream(sc.Conn, handback, input) }()
	// Join the driver BEFORE any other cleanup touches state it reads (#3265).
	// Registered last so it runs first; CloseNow ends the driver's read loop, so
	// the join terminates even when the test bailed before the driver exited on
	// its own.
	t.Cleanup(func() {
		_ = server.CloseNow()
		<-d
	})
	return server, d
}

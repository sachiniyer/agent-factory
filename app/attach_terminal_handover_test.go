package app

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The #2157 handover: a full-screen attach is a raw stdin proxy, so for its
// duration the Bubble Tea program must stop reading the terminal. These tests
// pin the lifecycle (release before the attach starts, reclaim after it ends,
// on every exit path) and — with a fake tty — the property that lifecycle
// exists for: while the attach runs, nothing else takes bytes off the terminal.

// recordingTerminalHandover wires h's release/restore seams to recorders and
// returns the shared event log. Every event that matters to the ordering — the
// two seam calls and the attach itself — appends to it.
type recordingTerminalHandover struct {
	mu      sync.Mutex
	events  []string
	release func() error
	restore func() error
}

func newRecordingHandover(h *home) *recordingTerminalHandover {
	r := &recordingTerminalHandover{}
	r.release = func() error { r.record("release"); return nil }
	r.restore = func() error { r.record("restore"); return nil }
	h.releaseTerminal = r.release
	h.restoreTerminal = r.restore
	return r
}

func (r *recordingTerminalHandover) record(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recordingTerminalHandover) log() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

// TestAttachOverlayCallback_ReleasesTerminalAroundTheAttach is the #2157
// regression lock. The attach reads the REAL stdin itself; Bubble Tea's input
// reader is blocked on that same descriptor and nothing else ever stops it, so
// unless this callback releases the terminal FIRST the two race for every byte
// the user types or pastes — and the bytes the TUI's reader wins never reach the
// pane. Release must therefore happen before the attach starts, and the reclaim
// after it ends, so the TUI gets its input reader back.
func TestAttachOverlayCallback_ReleasesTerminalAroundTheAttach(t *testing.T) {
	resetDetachWatchdog(t)
	h := newTestHome(t)
	rec := newRecordingHandover(h)

	var out bytes.Buffer
	swapRemoteDetachResetWriter(t, &out)

	ch := make(chan struct{})
	done := make(chan tea.Cmd, 1)
	go func() {
		done <- h.attachOverlayCallback("t1", "test-attach", "", func() (chan struct{}, error) {
			rec.record("attach")
			return ch, nil
		})
	}()
	require.Eventually(t, func() bool { return h.attached.Load() },
		time.Second, time.Millisecond, "attached flag must arm before <-ch blocks")

	// Mid-attach: the terminal is the attach's, and has not been taken back.
	assert.Equal(t, []string{"release", "attach"}, rec.log(),
		"the terminal must be released BEFORE the attach starts reading stdin — "+
			"releasing after it has already begun leaves a window in which both "+
			"readers are live, which is the whole of #2157")

	close(ch)
	select {
	case cmd := <-done:
		require.NotNil(t, cmd)
	case <-time.After(2 * time.Second):
		t.Fatalf("attachOverlayCallback did not return after detach")
	}

	assert.Equal(t, []string{"release", "attach", "restore"}, rec.log(),
		"the terminal must be reclaimed after the attach ends — without it the "+
			"TUI never restarts its input reader and the session is unusable")
	assert.Equal(t, attachAltScreenEnter+remoteDetachTerminalReassert, out.String(),
		"exactly two writes, in this order: the alt screen the attach runs on (a "+
			"released terminal is back on the MAIN screen, and the pane's paint "+
			"must not be left in the user's scrollback), then the mode re-assert "+
			"— RestoreTerminal does not re-enable mouse reporting, so the "+
			"hand-rolled re-assert still has to run after the reclaim (#845)")
	endDetachWatchdog()
}

// TestAttachOverlayCallback_ReclaimsTerminalWhenAttachFails: a failed attach
// must still give the terminal back, MODES INCLUDED. Leaving it released would
// leave the TUI with no input reader and a stopped renderer — a frozen app —
// which is a far worse outcome than the attach error the user actually hit.
//
// The modes half is its own hazard, and attach errors are production-reachable
// (a daemon that is down or restarting, an unresolved socket, a failed WS dial).
// Releasing the terminal disables mouse reporting, and RestoreTerminal does not
// put it back — bubbletea writes it once at startup from WithMouseCellMotion and
// never again. So a reclaim that skips the mode re-assert hands the user a TUI
// whose mouse is dead until the process restarts: the silent mode leak of #1832,
// arrived at from the other side.
func TestAttachOverlayCallback_ReclaimsTerminalWhenAttachFails(t *testing.T) {
	h := newTestHome(t)
	rec := newRecordingHandover(h)

	var out bytes.Buffer
	swapRemoteDetachResetWriter(t, &out)

	cmd := h.attachOverlayCallback("t1", "test-attach", "", func() (chan struct{}, error) {
		rec.record("attach")
		return nil, assert.AnError
	})

	assert.Nil(t, cmd)
	assert.Equal(t, []string{"release", "attach", "restore"}, rec.log(),
		"a failed attach must hand the terminal back, not strand the TUI")
	assert.False(t, h.attached.Load())
	assert.False(t, h.attachTransitioning)
	assert.Equal(t, attachAltScreenEnter+remoteDetachTerminalReassert, out.String(),
		"a released terminal must be reclaimed with its modes: every reclaim, "+
			"clean detach or failed attach, re-asserts what RestoreTerminal does not")
	for _, seq := range []struct{ esc, what string }{
		{"\x1b[?1002h", "cell-motion mouse"},
		{"\x1b[?1006h", "SGR mouse encoding"},
	} {
		assert.Contains(t, out.String(), seq.esc,
			"the release disabled %s and RestoreTerminal does not put it back — "+
				"without this write it stays dead for the rest of the session", seq.what)
	}
}

// TestAttachOverlayCallback_NoReclaimWhenReleaseFails: if the release itself
// failed, the terminal was never handed over, so "restoring" it would re-init an
// input reader and renderer that were never stopped. The attach still proceeds —
// a racy paste is better than refusing to attach — but the reclaim is skipped.
func TestAttachOverlayCallback_NoReclaimWhenReleaseFails(t *testing.T) {
	resetDetachWatchdog(t)
	h := newTestHome(t)
	rec := newRecordingHandover(h)
	h.releaseTerminal = func() error { rec.record("release-failed"); return errors.New("no tty") }

	var out bytes.Buffer
	swapRemoteDetachResetWriter(t, &out)

	cmd := runAttachOverlayCallback(t, h)
	require.NotNil(t, cmd, "a failed release must not abort the attach")
	assert.Equal(t, []string{"release-failed"}, rec.log(),
		"nothing to reclaim from a release that never happened")
	assert.Equal(t, remoteDetachTerminalReassert, out.String(),
		"only the mode re-assert is written: the alt-screen switch belongs to the "+
			"release that did not happen")
	endDetachWatchdog()
}

// TestAttachOverlayCallback_NoHandoverWithoutAProgram: the seams are nil when no
// Bubble Tea program owns the terminal (every test in this package, and any
// future headless caller). The callback must then behave exactly as before.
func TestAttachOverlayCallback_NoHandoverWithoutAProgram(t *testing.T) {
	resetDetachWatchdog(t)
	h := newTestHome(t)
	require.Nil(t, h.releaseTerminal)
	require.Nil(t, h.restoreTerminal)

	var out bytes.Buffer
	swapRemoteDetachResetWriter(t, &out)

	cmd := runAttachOverlayCallback(t, h)
	require.NotNil(t, cmd)
	assert.Equal(t, remoteDetachTerminalReassert, out.String(),
		"with no program to hand the terminal to, only the mode re-assert is written")
	endDetachWatchdog()
}

// --- the property the handover exists for -----------------------------------

var errTTYReaderCancelled = errors.New("tty reader cancelled")

// sharedTTY models the one thing about a terminal that makes #2157 possible:
// readers do NOT each get a copy of what the user types. The bytes sit in one
// queue, whichever reader is served takes them off it, and they are gone from
// every other reader. A second live reader on the terminal is therefore not a
// bystander — it is a thief.
//
// Readers are served in the order they blocked (FIFO), which makes the theft
// deterministic instead of a coin flip: the TUI's reader has been parked on the
// terminal since boot, so it is ahead of an attach pump that keeps going back to
// the queue after forwarding each chunk. Cancelling a reader is what Bubble
// Tea's ReleaseTerminal does to its cancelreader.
type sharedTTY struct {
	mu        sync.Mutex
	cond      *sync.Cond
	buf       []byte
	queue     []int // ids of readers currently blocked, oldest first
	cancelled map[int]bool
}

func newSharedTTY() *sharedTTY {
	tty := &sharedTTY{cancelled: make(map[int]bool)}
	tty.cond = sync.NewCond(&tty.mu)
	return tty
}

// reader returns the io.Reader for reader id.
func (tty *sharedTTY) reader(id int) io.Reader {
	return readerFunc(func(p []byte) (int, error) { return tty.read(id, p) })
}

// typed puts bytes on the terminal, as a keystroke or a paste would.
func (tty *sharedTTY) typed(b []byte) {
	tty.mu.Lock()
	defer tty.mu.Unlock()
	tty.buf = append(tty.buf, b...)
	tty.cond.Broadcast()
}

// cancel ends reader id's read, now and for good.
func (tty *sharedTTY) cancel(id int) {
	tty.mu.Lock()
	defer tty.mu.Unlock()
	tty.cancelled[id] = true
	tty.cond.Broadcast()
}

// blocked reports whether reader id is parked on the terminal.
func (tty *sharedTTY) blocked(id int) bool {
	tty.mu.Lock()
	defer tty.mu.Unlock()
	for _, q := range tty.queue {
		if q == id {
			return true
		}
	}
	return false
}

func (tty *sharedTTY) read(id int, p []byte) (int, error) {
	tty.mu.Lock()
	defer tty.mu.Unlock()
	tty.queue = append(tty.queue, id)
	defer func() {
		for i, q := range tty.queue {
			if q == id {
				tty.queue = append(tty.queue[:i], tty.queue[i+1:]...)
				break
			}
		}
		tty.cond.Broadcast()
	}()
	for {
		if tty.cancelled[id] {
			return 0, errTTYReaderCancelled
		}
		if len(tty.buf) > 0 && tty.queue[0] == id {
			n := copy(p, tty.buf)
			tty.buf = tty.buf[n:]
			return n, nil
		}
		tty.cond.Wait()
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// TestAttachOwnsTheTerminalAlone is #2157 itself, in a unit test. A reader
// modelling Bubble Tea's input loop is parked on the terminal, an attach starts,
// and a 128-byte paste is typed. Every byte must reach the attach.
//
// Without the handover the TUI's reader is still parked and takes the paste off
// the terminal in one 256-byte read — the pane gets a fragment or nothing, which
// is exactly the reported symptom. With it, the release cancels that reader
// before the attach's pump starts, so the pump is the only reader there is.
//
// The pump here is a stand-in for apiclient's (same 32-byte reads, same forward-
// then-read-again loop); what is under test is the app's handover, not the pump.
func TestAttachOwnsTheTerminalAlone(t *testing.T) {
	resetDetachWatchdog(t)
	const tuiReader, attachPump = 1, 2
	paste := make([]byte, 128)
	for i := range paste {
		paste[i] = byte('a' + i%26)
	}

	h := newTestHome(t)
	tty := newSharedTTY()
	swapRemoteDetachResetWriter(t, &bytes.Buffer{})

	// Bubble Tea's input loop: parked on the terminal since boot, 256-byte reads.
	stolen := make(chan []byte, 1)
	go func() {
		var took []byte
		buf := make([]byte, 256)
		for {
			n, err := tty.reader(tuiReader).Read(buf)
			took = append(took, buf[:n]...)
			if err != nil {
				stolen <- took
				return
			}
		}
	}()
	require.Eventually(t, func() bool { return tty.blocked(tuiReader) },
		time.Second, time.Millisecond, "the TUI reader must be on the terminal before the attach")

	// ReleaseTerminal cancels that reader; RestoreTerminal would start a new one.
	h.releaseTerminal = func() error { tty.cancel(tuiReader); return nil }
	h.restoreTerminal = func() error { return nil }

	forwarded := make(chan []byte, 1)
	ch := make(chan struct{})
	// callbackDone is what the test waits on before returning: the callback
	// reads package-level seams (remoteDetachResetWriter) that t.Cleanup restores,
	// so letting it outlive the test is a real data race, not a tidiness point.
	callbackDone := make(chan struct{})
	go func() {
		defer close(callbackDone)
		h.attachOverlayCallback("t1", "test-attach", "", func() (chan struct{}, error) {
			go func() {
				var got []byte
				buf := make([]byte, 32) // the attach pump's read size
				for {
					n, err := tty.reader(attachPump).Read(buf)
					got = append(got, buf[:n]...)
					if err != nil || len(got) >= len(paste) {
						forwarded <- got
						return
					}
				}
			}()
			return ch, nil
		})
	}()
	require.Eventually(t, func() bool { return tty.blocked(attachPump) },
		time.Second, time.Millisecond, "the attach pump must be on the terminal")

	tty.typed(paste)

	select {
	case got := <-forwarded:
		assert.Equal(t, paste, got,
			"every pasted byte must reach the attach: a second reader on the "+
				"terminal does not copy bytes, it takes them (#2157)")
	case <-time.After(3 * time.Second):
		t.Fatalf("the attach never received the paste")
	}
	select {
	case took := <-stolen:
		assert.Empty(t, took,
			"the released reader must take nothing: it was cancelled before the "+
				"attach began, so it can no longer be served")
	case <-time.After(time.Second):
		t.Fatalf("the released TUI reader never returned")
	}

	close(ch) // detach
	select {
	case <-callbackDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("attachOverlayCallback did not return after detach")
	}
	endDetachWatchdog()
}

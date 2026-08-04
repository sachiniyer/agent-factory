package apiclient

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty"
	"github.com/stretchr/testify/require"
	"golang.org/x/term"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// A full-screen attach BORROWS the user's terminal: it puts the real terminal
// into raw mode for the pane's lifetime and owes it back when the attach ends.
// #2784 is what an exit path that skips the hand-back costs — a shell with no
// echo and no line editing, where the recovery (reset, stty sane) is the hardest
// thing there is to type.
//
// These are the tests that can see that. A unit test asserting that a restore
// function was called would pass against a driver that hands back a terminal
// nobody can type into; the assertion that matters is on the termios, so the
// tests below run the production attach against a REAL pty in a REAL child
// process and then prove the terminal works by typing into it and requiring the
// line discipline to echo.
//
// The child process is not incidental. An unrecovered panic is only observable
// as a process that dies, and the fix has to let it die: recovering it would
// swallow the bug the panic is reporting. So the panic test requires BOTH halves
// — the terminal comes back AND the panic still reaches the user.

const (
	// attachHelperEnv selects the child-process behaviour; empty means this is an
	// ordinary suite run and the helper test skips itself.
	attachHelperEnv = "AF_ATTACH_PTY_HELPER"
	// attachHelperReady is printed once the attach owns the terminal in raw mode,
	// so the parent knows when the process is in the state under test.
	attachHelperReady = "AF-ATTACH-HELPER-READY"
	// attachHelperPanic is the panic value injected through attachTermSize, an
	// EXISTING production seam that driveAttachStream calls on its own goroutine
	// (the initial resize). Injecting there needs no test-only hook in the driver
	// and lands the panic exactly where #2784 describes it: after the terminal is
	// raw, before the hand-back.
	attachHelperPanic = "af attach: injected panic from the terminal-size seam"
	// attachHelperSawPrefix leads the line the helper prints when the stub server
	// receives typed bytes through the attach — proof the stream is still live. It
	// carries everything typed so far, so a frame split mid-token resolves as soon
	// as the rest arrives, and it cannot be confused with a local terminal echo of
	// those same bytes.
	attachHelperSawPrefix = "AF-ATTACH-STREAM-SAW:"
)

// TestAttachStream_PanicHandsTheTerminalBack is #2784. The attach driver panics
// with the terminal in raw mode; the user must get a usable terminal back, and
// must still see the panic.
func TestAttachStream_PanicHandsTheTerminalBack(t *testing.T) {
	h := startAttachInPTY(t, "panic")
	err := h.wait(t)

	// The panic SURFACES: the process dies and says why. If this ever starts
	// passing because someone wrapped the driver in recover(), the terminal
	// assertion below would pass too — and a real bug would be silently eaten.
	//
	// Waited for rather than asserted outright: process exit does not mean the
	// terminal has finished handing us what the process wrote, so the tail of the
	// trace can still be in flight when Wait returns.
	require.Error(t, err, "an unrecovered panic must kill the process, not be swallowed")
	h.waitForOutput(t, attachHelperPanic, "the panic must reach the user")
	h.waitForOutput(t, "goroutine", "the panic must print its stack trace")

	requireTerminalUsable(t, h, "after-panic",
		"the attach panicked with the terminal in raw mode and never handed it back: "+
			"the user is left in a shell with no echo and no line editing (#2784)")
}

// TestAttachStream_SignalHandsTheTerminalBack covers the other exit nobody wrote
// a handler for: the attached process is killed from outside. Default signal
// disposition terminates the process without running a single defer, so the
// terminal stays raw for exactly the same reason the panic left it raw.
func TestAttachStream_SignalHandsTheTerminalBack(t *testing.T) {
	h := startAttachInPTY(t, "signal")
	h.waitForOutput(t, attachHelperReady, "the helper should reach raw-mode attach")
	require.NoError(t, h.cmd.Process.Signal(syscall.SIGTERM))
	err := h.wait(t)

	// The signal still kills the process — the hand-back must not turn a
	// terminate into a survivor.
	require.Error(t, err, "SIGTERM must still terminate the attached process")

	requireTerminalUsable(t, h, "after-signal",
		"the attached process was killed by SIGTERM with the terminal in raw mode and "+
			"never handed it back (#2784)")
}

// TestAttachStream_IgnoredSignalDoesNotDisturbTheAttach is the other half of the
// signal contract. A signal the process inherited as IGNORED (nohup, a disowned
// job, a shell trap that ignores HUP — SIG_IGN survives execve) can never kill
// it, so the attach must leave that signal alone.
//
// Watching it anyway is worse than not handling it: os/signal has to take the
// ignore away to deliver the signal, and putting the default disposition back
// restores the same ignore, so the re-raise is discarded — the process lives on
// with a terminal it has already handed back while it is still proxying a raw
// byte stream to it. Nothing here is about dying well; it is about a live attach
// staying live.
//
// The child is started through a shell that ignores SIGHUP and then execs the
// helper, which is the real inheritance path rather than a simulation of it.
func TestAttachStream_IgnoredSignalDoesNotDisturbTheAttach(t *testing.T) {
	h := startAttachInPTY(t, "ignored-hup")
	h.waitForOutput(t, attachHelperReady, "the helper should reach raw-mode attach")
	require.NoError(t, h.cmd.Process.Signal(syscall.SIGHUP))

	// The FIRST round trip is a synchronization point, not an assertion. Signal
	// delivery is asynchronous, so inspecting the terminal immediately after
	// Signal returns would race a handler that has not run yet — and would pass
	// against a build that does handle the ignored signal. A completed round trip
	// (terminal to stdin pump to websocket to server and back out to stdout) puts
	// many scheduling points between the signal and the inspection below.
	h.roundTrip(t, "sync", "the attach must survive a signal it inherited as ignored")
	h.roundTrip(t, "again", "the attach must survive a signal it inherited as ignored")

	// NOW the terminal can be inspected, two independent ways — a handler that
	// wrongly ran would have to beat BOTH. The hand-back writes the neutral
	// restore to the terminal, so its bytes appearing at all mean the attach gave
	// back a terminal it is still proxying to; and a restored terminal is cooked,
	// so it would echo the probe below locally.
	//
	// No hand-back happens on this path (the signal is filtered out before the
	// watch is armed), so the terminal never changes state under the probes and a
	// single strict probe is right here.
	require.NotContains(t, h.output(), tmux.NeutralTerminalRestore,
		"the attach handed the terminal back on a signal that could never kill it")
	arrived, echoed := h.attachProbe(t, "ping", 30*time.Second)
	require.True(t, arrived, "the attach must still be reading the terminal (%s)", h.childState())
	require.False(t, echoed,
		"the terminal echoed locally, so the attach handed back a terminal it is still using")

	// A signal that CAN kill it still hands the terminal back — skipping one
	// signal must not disarm the others.
	require.NoError(t, h.cmd.Process.Signal(syscall.SIGTERM))
	require.Error(t, h.wait(t), "SIGTERM must still terminate the attached process")
	requireTerminalUsable(t, h, "after-ignored-then-term",
		"SIGTERM after an ignored SIGHUP left the terminal unusable (#2784)")
}

// TestAttachStream_UnkillableSignalRetakesTheTerminal is the case the ignored-
// signal filter CANNOT see, and it is the TUI's normal state rather than an
// exotic one.
//
// signal.Ignored only reports an inherited ignore while nothing has called
// Notify for that signal — and Bubble Tea calls Notify for SIGINT/SIGTERM at
// program start (tea.go handleSignals). So in the TUI the filter reports "not
// ignored", the hand-back arms, and signal.Reset then restores the inherited
// SIG_IGN, which discards the re-raise. Same shape when the kernel suppresses a
// default action (pid 1 in a container).
//
// The process therefore survives a signal it handed the terminal back for. That
// must not leave a live attach proxying raw bytes to a cooked terminal — which
// would be WORSE than no handler at all, since the un-patched code ignored these
// signals and left the attach intact. The terminal has to come back.
func TestAttachStream_UnkillableSignalRetakesTheTerminal(t *testing.T) {
	h := startAttachInPTY(t, "unkillable-int")
	h.waitForOutput(t, attachHelperReady, "the helper should reach raw-mode attach")
	// Prove the round trip works BEFORE the signal, while the terminal state is
	// stable. Establishing liveness afterwards instead would race the hand-back
	// window, and a probe lost there would be reported as a dead attach.
	h.roundTrip(t, "presignal", "the attach must be reading the terminal before the signal")

	require.NoError(t, h.cmd.Process.Signal(syscall.SIGINT))

	// Wait for the hand-back to actually LAND before requiring it to be undone,
	// and take a COOKED ECHO as the proof rather than the neutral-restore bytes.
	//
	// Those bytes are written first and the tcsetattr follows them (see
	// terminalHandback.restoreLocked), so a probe typed in between finds a
	// terminal that is still raw and reads as a successful retake — the false PASS
	// this ordering exists to prevent, just through a narrower window. An echo is
	// the first thing that can only happen AFTER the termios actually changed.
	h.requireTerminalCooked(t, "the signal must make the attach hand the terminal back "+
		"in the first place")

	// Then the terminal must come BACK to raw, and the attach must still be
	// reading it. Polled rather than asserted once, for two reasons: the retake
	// deliberately waits out a grace period so a genuinely fatal signal kills the
	// process before anything re-raws a terminal it is about to abandon, and a
	// probe typed while the hand-back is still in effect is not the attach's to
	// receive at all.
	h.requireTerminalRetaken(t, "the terminal never came back to raw under a live attach "+
		"after a signal that could not kill the process: the attach handed it back and "+
		"never took it again")

	// And the terminal it retook still comes back for real when the attach ends.
	require.NoError(t, h.cmd.Process.Signal(syscall.SIGTERM))
	require.Error(t, h.wait(t), "SIGTERM must still terminate the attached process")
	requireTerminalUsable(t, h, "after-retake-then-term",
		"a terminal retaken after an unkillable signal was never handed back (#2784)")
}

// TestAttachStream_FailedAttachLeavesTheTerminalAlone is the last exit in the
// #2784 audit: an attach that never starts (daemon down, socket unresolved) —
// the error path the TUI hits and then re-attaches through. It must hand back a
// working terminal too, which today it does by never taking it: the dial runs
// before MakeRaw, so a failure returns with the termios untouched.
//
// That makes this a guard rather than a fix witness, and it has teeth: move the
// MakeRaw in AttachStream above the DialStream call and it fails, because the
// error return then skips a hand-back it now owes.
func TestAttachStream_FailedAttachLeavesTheTerminalAlone(t *testing.T) {
	h := startAttachInPTY(t, "dial-failure")
	require.NoError(t, h.wait(t), "the helper should report a clean failed attach; output: %s", h.output())

	requireTerminalUsable(t, h, "after-failed-attach",
		"an attach that failed to start left the terminal unusable (#2784)")
}

// TestAttachTerminalHandbackHelper is the child half of every test above: it
// runs a REAL attach (raw mode and all) against a stub WS server, on the pty its
// parent handed it, in the shape the named mode calls for. It is a no-op in an
// ordinary suite run.
func TestAttachTerminalHandbackHelper(t *testing.T) {
	mode := os.Getenv(attachHelperEnv)
	if mode == "" {
		t.Skip("child-process helper for the attach terminal hand-back tests")
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		t.Fatalf("helper stdin is not a terminal, so there is no raw mode to leak")
	}
	switch mode {
	case "panic":
		// driveAttachStream calls this seam itself, on its own goroutine, once the
		// terminal is already raw.
		prev := attachTermSize
		attachTermSize = func() (uint16, uint16, bool) { panic(attachHelperPanic) }
		t.Cleanup(func() { attachTermSize = prev })
	case "unkillable-int":
		// Widen the pause between hand-back and retake so the handed-back window is
		// comfortably observable from the parent, which has to SEE the terminal go
		// cooked before it can require it to come back. Only the pause changes; the
		// sequence under test — restore, re-raise, survive, retake — is untouched.
		// Same seam-swapping the rest of this package's tests use for its timeouts.
		handbackRetakeGrace = 2 * time.Second
		// Stand in for Bubble Tea, which calls Notify for SIGINT/SIGTERM at startup:
		// that is what hides the inherited ignore from signal.Ignored. The parent
		// started this process with SIGINT inherited as ignored, so the re-raise the
		// hand-back performs is discarded and the process lives on.
		bubbleteaLike := make(chan os.Signal, 1)
		signal.Notify(bubbleteaLike, syscall.SIGINT)
		t.Cleanup(func() { signal.Stop(bubbleteaLike) })
	case "dial-failure":
		// Nothing is listening on that socket, so the attach fails before it can
		// own anything. Exiting 0 here is the child reporting a clean failure; the
		// parent then checks what it left behind.
		dead := NewWithSocket(filepath.Join(t.TempDir(), "no-daemon.sock"))
		if _, derr := dead.AttachStream(context.Background(), "alpha", "", "", 0); derr == nil {
			t.Fatalf("attaching over a dead socket must fail")
		}
		fmt.Println(attachHelperReady)
		return
	}

	c, connCh := attachWSServer(t)
	done, err := c.AttachStream(context.Background(), "alpha", "", "", 0)
	if err != nil {
		t.Fatalf("AttachStream: %v", err)
	}
	if mode != "panic" {
		// AttachStream returning only means the driver GOROUTINE was spawned. The
		// parent is about to signal this process, so READY has to mean the driver
		// actually reached the state under test: wait for the initial RESIZE frame,
		// which the driver writes strictly after it arms the terminal hand-back.
		// Without this the test would race the goroutine scheduler.
		server := <-connCh
		waitForInitialResize(t, server)
		if mode == "ignored-hup" || mode == "unkillable-int" {
			// Report typed bytes arriving through the stream, so the parent can prove
			// the attach is still reading the terminal after the ignored signal.
			go reportInputToStdout(server)
		}
	}
	fmt.Println(attachHelperReady)
	<-done
	t.Fatalf("helper: the attach ended on its own; nothing exercised the exit under test")
}

// reportInputToStdout prints everything typed so far, each time an INPUT frame
// carries more of it, and stops when the stream ends. Reporting the accumulation
// rather than the frame keeps the parent's wait exact even if the terminal
// splits a token across reads.
func reportInputToStdout(server *websocket.Conn) {
	var typed strings.Builder
	for {
		msg, err := agentproto.ReadMessage(context.Background(), server)
		if err != nil {
			return
		}
		if msg.Binary && msg.Frame.Op == agentproto.OpInput {
			// A cooked terminal delivers the line with a newline, a raw one delivers
			// the carriage return; neither belongs to the token.
			typed.WriteString(strings.NewReplacer("\r", "", "\n", "").Replace(string(msg.Frame.Data)))
			fmt.Println(attachHelperSawPrefix + typed.String())
		}
	}
}

// waitForInitialResize blocks until the driver sends the RESIZE frame it emits
// on connect — the proof that driveAttachStream is past its setup.
func waitForInitialResize(t *testing.T, server *websocket.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		msg, err := agentproto.ReadMessage(ctx, server)
		if err != nil {
			t.Fatalf("waiting for the attach initial resize: %v", err)
		}
		if msg.Binary && msg.Frame.Op == agentproto.OpResize {
			return
		}
	}
}

// ptyAttach is a child af attach running on a pty the test owns both ends of.
type ptyAttach struct {
	ptmx *os.File
	tty  *os.File
	cmd  *exec.Cmd
	out  *syncBuffer
	// exited closes when the single owning Wait returns; exitErr is written
	// before the close, so reading it after is ordered.
	exited  chan struct{}
	exitErr error
}

// startAttachInPTY allocates a pty, proves it starts out as a working cooked
// terminal, and starts the helper child attached to it.
func startAttachInPTY(t *testing.T, mode string) *ptyAttach {
	t.Helper()
	ptmx, tty, err := pty.Open()
	require.NoError(t, err, "open a pty pair")
	t.Cleanup(func() {
		_ = ptmx.Close()
		_ = tty.Close()
	})

	// Drain the master continuously: a Go panic trace is far larger than the pty
	// buffer, and a child blocked on a full terminal would never reach its exit.
	out := &syncBuffer{}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, rerr := ptmx.Read(buf)
			if n > 0 {
				_, _ = out.Write(buf[:n])
			}
			if rerr != nil {
				return
			}
		}
	}()

	h := &ptyAttach{ptmx: ptmx, tty: tty, out: out}
	// A fresh pty is cooked, exactly like the shell a user runs af from. Proving
	// it HERE is what keeps the post-exit assertion honest: without this a probe
	// that never echoes for some unrelated reason would read as a raw-mode leak,
	// and one that always echoes would make the test vacuous.
	requireTerminalUsable(t, h, "baseline",
		"the harness pty must start as a working cooked terminal")

	// Re-exec this very test binary: the child IS the test, one helper case deep.
	helperArgs := []string{"-test.run=^TestAttachTerminalHandbackHelper$", "-test.timeout=60s"}
	cmd := exec.Command(os.Args[0], helperArgs...)
	if trap := shellTrapFor(mode); trap != "" {
		// Inherit the signal as ignored the way nohup and a disowned job do: SIG_IGN
		// survives execve, so the shell traps it and then execs the helper INTO the
		// same process (same pid).
		cmd = exec.Command("/bin/sh", append([]string{
			"-c", trap + `; exec "$@"`, "sh", os.Args[0],
		}, helperArgs...)...)
	}
	cmd.Env = append(os.Environ(),
		attachHelperEnv+"="+mode,
		// Fence the child off the developer real af home, same rule as testguard.
		"AGENT_FACTORY_HOME="+t.TempDir())
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	// The child gets the pty as its stdio and nothing else — no SysProcAttr.
	//
	// Neither a controlling terminal (Setctty) nor its own session (Setsid) is
	// needed for what these tests measure: the hand-back is a tcsetattr on fd 0
	// plus escape sequences, and the tests signal the child by pid rather than
	// through the terminal. Both were here for realism, and on darwin both are
	// harness poison — two halves of one rule, which is why dropping Setctty
	// alone was not enough:
	//
	//   - a session leader that EXITS revokes its controlling terminal, and that
	//     invalidates every descriptor for the pty in every process, so the
	//     post-exit probe gets EIO instead of an answer;
	//   - a session leader that OPENS a terminal ACQUIRES it as a controlling
	//     terminal. The shell wrapping the ignored-signal child touches the
	//     terminal where a Go binary never does, so it took the ctty straight back
	//     and re-armed the revoke above.
	//
	// Linux does neither, so only the macOS CI leg ever sees this.
	require.NoError(t, cmd.Start(), "start the attach helper on the pty")
	h.cmd = cmd
	// ONE Wait for the child, ever, owned here — so a test that fails before
	// reaping cannot leave a second Wait racing this one.
	h.exited = make(chan struct{})
	go func() {
		h.exitErr = cmd.Wait()
		close(h.exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill() // ErrProcessDone once it has been reaped
		<-h.exited
	})
	return h
}

// shellTrapFor returns the shell trap that gives a mode its inherited-ignored
// signal, or empty for the modes that need none.
func shellTrapFor(mode string) string {
	switch mode {
	case "ignored-hup":
		return `trap "" HUP`
	case "unkillable-int":
		return `trap "" INT`
	}
	return ""
}

// requireTerminalCooked waits until a typed probe is echoed back locally. The
// echo is the only observable that is ordered strictly AFTER the hand-back's
// tcsetattr, which is what makes it the right thing to wait for: the terminal is
// provably cooked from this point, so anything the caller then requires of a
// retake cannot be satisfied by a terminal that was simply never touched.
//
// Arrival at the pane is deliberately not required here. During the hand-back
// the terminal is not the attach's to read, so whether the bytes reach the pane
// says nothing either way.
func (h *ptyAttach) requireTerminalCooked(t *testing.T, why string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for i := 1; time.Now().Before(deadline); i++ {
		token := fmt.Sprintf("cooked%d", i)
		from := len(h.output())
		_, err := h.ptmx.WriteString(token + "\r")
		require.NoError(t, err, "type into the terminal")
		// The echo is produced by the line discipline at write time, so it needs a
		// short wait, not a long one — and a short wait is what lets this poll the
		// window rather than sit through it.
		echoBy := time.Now().Add(200 * time.Millisecond)
		for time.Now().Before(echoBy) {
			if strings.Contains(h.output()[from:], token+"\r\n") {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	t.Fatalf("%s\nthe terminal never went cooked, so the attach never handed it back (%s)\npty output: %q",
		why, h.childState(), h.output())
}

// requireTerminalRetaken polls until a typed probe both reaches the pane and is
// NOT echoed locally — the terminal back in raw mode under a still-running
// attach.
//
// Retried with a fresh token, because a single probe proves less than it looks
// like it does and both ways it can come back short mean "ask again":
//
//   - echoed: the terminal is still cooked. The retake waits out a grace period
//     on purpose, so early probes SHOULD see this;
//   - never arrived: the byte did not reach the pane this time. A single probe
//     with a long timeout would spend the whole budget here and then report it
//     as "the terminal never came back", which is a different defect from the
//     one that happened.
//
// Only the deadline is a failure, and the message separates those two cases by
// counting them, so a future failure names its own mechanism instead of leaving
// the next reader to infer it from a truncated CI log.
func (h *ptyAttach) requireTerminalRetaken(t *testing.T, why string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	probes, reached, lastEchoed := 0, 0, false
	for i := 1; time.Now().Before(deadline); i++ {
		arrived, echoed := h.attachProbe(t, fmt.Sprintf("probe%d", i), time.Second)
		probes++
		if arrived {
			reached++
		}
		lastEchoed = echoed
		if arrived && !echoed {
			return
		}
	}
	diagnosis := "the terminal stayed COOKED: the attach handed it back and never took it again"
	if reached == 0 {
		diagnosis = "no probe ever reached the pane: the attach stopped reading the terminal " +
			"altogether, which is a live attach gone deaf rather than a terminal left raw — " +
			"treat this as a PRODUCT failure, not a flaky test"
	}
	t.Fatalf("%s\n%s\n%d probes typed, %d reached the pane, last one echoed locally: %v (%s)\npty output: %q",
		why, diagnosis, probes, reached, lastEchoed, h.childState(), h.output())
}

func (h *ptyAttach) output() string { return h.out.String() }

// attachProbe types a token into the terminal and reports two things: whether
// the helper saw it arrive through the PTY stream, and whether the terminal
// echoed it locally. Arrival means the attach is reading the terminal; a local
// echo means the terminal is cooked.
//
// A probe can legitimately go unanswered, which is why this reports rather than
// asserts. While a hand-back is in effect the terminal belongs to the shell and
// not to the pane, so bytes typed in that window are not the attach's to
// receive. The caller decides whether an unanswered probe is a failure or a
// reason to type again.
func (h *ptyAttach) attachProbe(t *testing.T, token string, within time.Duration) (arrived, echoed bool) {
	t.Helper()
	from := len(h.output())
	_, err := h.ptmx.WriteString(token + "\r")
	require.NoError(t, err, "type into the attached terminal")
	// Match the token ALONE inside a report line, never an accumulation of every
	// token typed so far. The helper reports what the stream DELIVERED, which is
	// not always what the parent typed — anything queued on the terminal before
	// the attach started arrives too, and a single byte that never arrives
	// desynchronizes an accumulated expectation permanently, so every later probe
	// waits out its full timeout for a string that can no longer appear. Unique
	// tokens need no accumulation to be unambiguous.
	//
	// Anchoring to the report line still keeps a cooked terminal honest: the local
	// echo of the token carries no report prefix, so it cannot satisfy this.
	want := regexp.MustCompile(regexp.QuoteMeta(attachHelperSawPrefix) + `[^\r\n]*` + regexp.QuoteMeta(token))
	deadline := time.Now().Add(within)
	for {
		if want.MatchString(h.output()) {
			return true, strings.Contains(h.output()[from:], token+"\r\n")
		}
		if !time.Now().Before(deadline) {
			return false, strings.Contains(h.output()[from:], token+"\r\n")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// roundTrip is attachProbe where an unanswered probe IS the failure — use it
// only when the terminal state is stable, never across a hand-back.
func (h *ptyAttach) roundTrip(t *testing.T, token, why string) {
	t.Helper()
	if arrived, _ := h.attachProbe(t, token, 30*time.Second); !arrived {
		t.Fatalf("%s\ntyped %q and it never reached the pane through the attach (%s)\npty output: %q",
			why, token, h.childState(), h.output())
	}
}

// childState describes the helper process for a failure message. A test that
// timed out waiting on the pane should say whether the process it was waiting
// for is even alive — otherwise the next reader has to guess, as I did.
func (h *ptyAttach) childState() string {
	select {
	case <-h.exited:
		return fmt.Sprintf("helper process exited: %v", h.exitErr)
	default:
		return "helper process still running"
	}
}

// wait blocks until the child exits and returns its exit error (nil only if it
// exited 0).
func (h *ptyAttach) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-h.exited:
		return h.exitErr
	case <-time.After(30 * time.Second):
		t.Fatalf("the attach helper never exited; pty output: %s", h.output())
		return nil
	}
}

// waitForOutput blocks until want shows up on the pty.
func (h *ptyAttach) waitForOutput(t *testing.T, want, why string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(h.output(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s: never saw %q on the pty (%s)\npty output: %q", why, want, h.childState(), h.output())
}

// requireTerminalUsable types a line into the terminal and requires the line
// discipline to echo it back — the assertion #2784 is actually about. A raw
// terminal echoes nothing (MakeRaw clears ECHO), and a cooked one echoes the
// line with the carriage return translated (ICRNL then ONLCR), so the expected
// bytes prove both halves of the termios came back.
func requireTerminalUsable(t *testing.T, h *ptyAttach, tag, why string) {
	t.Helper()
	typed := "af-terminal-" + tag
	from := len(h.output())
	_, err := h.ptmx.WriteString(typed + "\r")
	// A write that fails outright is the harness losing the pty, not the attach
	// leaving it raw. Say so, so the two never get confused in a CI log.
	require.NoError(t, err, "%s\ncould not type into the pty at all, so it was torn "+
		"down rather than left in raw mode — this is a harness failure, not the bug under test", why)

	want := typed + "\r\n"
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(h.output()[from:], want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s\ntyped %q into the terminal and the line discipline never echoed %q back "+
		"(terminal still in raw mode)\npty output after typing: %q",
		why, typed, want, h.output()[from:])
}

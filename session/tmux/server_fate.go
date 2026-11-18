package tmux

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// A session that dies moments after tmux confirmed it leaves one question af
// used to answer with a guess: did its PANE die, or the SERVER it was on? The two
// point at different causes — a program that exited or was killed, against a
// server that crashed, was killed, or lost its socket — and before #6581 every
// such failure read "the pane program likely exited immediately", whichever it
// was.
//
// serverFate answers from the two facts af can still check at that moment:
// which server, if any, answers on the session's socket now, and whether the
// server pid the session was created on still names a live tmux process. tmux
// keeps no record of a closed session's exit status (measured: a pane-exited
// hook fires with an empty status and signal once its session is gone), so this
// is the most a vanished session can tell.
//
// It also means af can never say WHY a session ended. With its server still
// answering, a pane program that exited, a kill-session from another client
// and a signal to the pane all read the same, so the verdict is only that the
// session ended while its server kept running. And a server that exited may
// have taken the session with it, or may have retired because this was its
// only session and it ended (exit-empty), whether or not another server has
// since bound the socket. A verdict naming an exited server says both.
//
// The socket alone never proves the server died. A server whose socket was
// removed keeps running with its sessions (#2875), and another server can bind
// the freed path while it does (see ServerPIDOnSocket). So an exit is claimed
// only when the creation-time pid no longer names a live tmux process; where
// that is unknown, the clause says what the socket showed and stops there.
type serverFate int

const (
	// serverFateUnknown: no creation-time server pid to compare, or the probe
	// got no clear answer. Nothing is claimed.
	serverFateUnknown serverFate = iota
	// serverFateSameServer: the server the session was created on still
	// answers. The session ended while that server kept running.
	serverFateSameServer
	// serverFateNoServer: the socket is there and nothing listens on it
	// (ECONNREFUSED).
	serverFateNoServer
	// serverFateUnreachable: the socket file is missing (ENOENT).
	serverFateUnreachable
	// serverFateReplaced: a DIFFERENT server answers on the socket.
	serverFateReplaced
)

// serverProcessState is what the creation-time server pid names now.
type serverProcessState int

const (
	serverProcessUnknown serverProcessState = iota
	// serverProcessExited: the pid is gone, or now names a program that is not
	// tmux, so the server it named has exited.
	serverProcessExited
	// serverProcessRunning: the pid still names a live tmux process. A pid
	// recycled to another tmux process reads the same, which only ever errs
	// toward NOT claiming an exit.
	serverProcessRunning
)

// serverProcessStateOf reads what pid names now. A package var so tests can
// stage each answer without a live server behind a fake pid.
var serverProcessStateOf = func(pid string) serverProcessState {
	n, err := strconv.Atoi(pid)
	if err != nil || n <= 0 {
		return serverProcessUnknown
	}
	if _, err := proctree.Lookup(n); err != nil {
		if errors.Is(err, proctree.ErrProcessExited) || errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			return serverProcessExited
		}
		return serverProcessUnknown
	}
	switch proctree.TmuxIdentityOf(n) {
	case proctree.Tmux:
		return serverProcessRunning
	case proctree.NotTmux:
		return serverProcessExited
	default:
		return serverProcessUnknown
	}
}

// serverFateReport is a serverFate with the facts its sentence names.
type serverFateReport struct {
	fate serverFate
	// createdPID is the server pid the session was created on.
	createdPID string
	// original is what createdPID names now; read for every fate but
	// serverFateSameServer, where the answering pid already is the answer.
	original serverProcessState
	// nowPID is the pid of the server answering now, for serverFateReplaced.
	nowPID string
	// diagnostic is tmux's own words for serverFateNoServer and
	// serverFateUnreachable.
	diagnostic string
}

// originalServerExited reports whether the server the session was created on
// is independently shown to have exited.
func (r serverFateReport) originalServerExited() bool {
	return r.original == serverProcessExited
}

// serverExitHedge is what an exited server's verdict says about the order of
// events: the evidence fits the server dying first, and it fits the session
// ending first and the server retiring with it.
const serverExitHedge = "the server died, or this was its only session and tmux retired it when the session ended"

// clause renders the report as the clause an error carries, or "" when there
// is nothing to say.
func (r serverFateReport) clause() string {
	switch r.fate {
	case serverFateSameServer:
		return fmt.Sprintf("the session ended while the tmux server it was created on (pid %s) kept running", r.createdPID)
	case serverFateNoServer:
		switch r.original {
		case serverProcessExited:
			return fmt.Sprintf("the tmux server it was created on (pid %s) has exited, and nothing answers on its socket (%s): %s", r.createdPID, r.diagnostic, serverExitHedge)
		case serverProcessRunning:
			return fmt.Sprintf("nothing answers on its socket (%s), though the tmux server it was created on (pid %s) is still a live tmux process", r.diagnostic, r.createdPID)
		default:
			return fmt.Sprintf("nothing answers on the socket of the tmux server it was created on (pid %s) (%s)", r.createdPID, r.diagnostic)
		}
	case serverFateUnreachable:
		switch r.original {
		case serverProcessExited:
			return fmt.Sprintf("the tmux server it was created on (pid %s) has exited, and its socket is missing (%s)", r.createdPID, r.diagnostic)
		case serverProcessRunning:
			return fmt.Sprintf("the tmux server it was created on (pid %s) is still a live tmux process, but its socket is missing (%s): the socket was removed while the server keeps running", r.createdPID, r.diagnostic)
		default:
			return fmt.Sprintf("the socket of the tmux server it was created on (pid %s) is missing (%s), so that server cannot be reached", r.createdPID, r.diagnostic)
		}
	case serverFateReplaced:
		switch r.original {
		case serverProcessExited:
			return fmt.Sprintf("the tmux server it was created on (pid %s) has exited, and a different tmux server (pid %s) now answers on its socket: %s", r.createdPID, r.nowPID, serverExitHedge)
		case serverProcessRunning:
			return fmt.Sprintf("a different tmux server (pid %s) now answers on its socket, though the tmux server it was created on (pid %s) is still a live tmux process: the session is out of reach, not proven gone", r.nowPID, r.createdPID)
		default:
			return fmt.Sprintf("a different tmux server (pid %s) now answers on the socket of the tmux server it was created on (pid %s)", r.nowPID, r.createdPID)
		}
	default:
		return ""
	}
}

// serverFateSince reports which server answers on this session's socket now,
// against createdPID, the server pid the session was created on, and what that
// pid names now. It reads ONE bounded display-message and the process table,
// and runs on failure paths only.
//
// It deliberately classifies tmux's no-server answers itself instead of going
// through NoServerRunning: on a missing socket that would run the socket-absent
// SIGUSR1 probe against every tmux server this uid runs, which is not a cost a
// diagnostic may impose.
func (t *TmuxSession) serverFateSince(createdPID string) serverFateReport {
	report := serverFateReport{createdPID: createdPID}
	if createdPID == "" {
		return report
	}
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	out, err := t.outputTmuxBounded(ctx, "display-message", "-p", "#{pid}")
	if err != nil {
		if ctx.Err() != nil {
			return report
		}
		diagnostic, exitOne := tmuxExitOneDiagnostic(err)
		if !exitOne {
			return report
		}
		switch classifyNoServerDiagnostic(diagnostic) {
		case serverRefusedConnection:
			report.fate = serverFateNoServer
		case socketAbsent:
			report.fate = serverFateUnreachable
		default:
			return report
		}
		report.diagnostic = diagnostic
		report.original = serverProcessStateOf(createdPID)
		return report
	}
	nowPID := strings.TrimSpace(string(out))
	if nowPID == "" || strings.ContainsAny(nowPID, " \t\r\n") {
		return report
	}
	if nowPID == createdPID {
		report.fate = serverFateSameServer
		return report
	}
	report.fate = serverFateReplaced
	report.nowPID = nowPID
	report.original = serverProcessStateOf(createdPID)
	return report
}

// boundServerPID is the creation-time server pid for a gone-probe: the
// generation the caller polled, else the one this session's monitor was bound
// to when it last answered live (Start's attach binds it). "" when neither is
// known.
func (t *TmuxSession) boundServerPID(gen *tmuxGeneration) string {
	t.monitorMu.Lock()
	defer t.monitorMu.Unlock()
	if gen != nil && gen.serverPID != "" {
		return gen.serverPID
	}
	if t.monitor != nil && t.monitor.generation != nil {
		return t.monitor.generation.serverPID
	}
	return ""
}

// withServerFate appends what the server's state says about a death to cause,
// or returns cause unchanged when there is nothing to add.
func (t *TmuxSession) withServerFate(cause error, gen *tmuxGeneration) error {
	clause := t.serverFateSince(t.boundServerPID(gen)).clause()
	if clause == "" {
		return cause
	}
	return fmt.Errorf("%w; %s", cause, clause)
}

// vanishedBeforeAttachError is Start's report for a session the existence poll
// saw and the attach then found gone. report is serverFateSince the server pid
// read when the poll confirmed the session; an unknown fate (that read, or the
// later one, did not answer) keeps the wording of the guess af made before
// #6581.
//
// Wherever the pane exiting is one of the explanations, the error says what to
// check if it did, and never that it did. The one verdict that leaves the pane
// out is a different server on the socket while the original is still alive or
// unknown: the session is out of reach there, not shown to have ended.
func (t *TmuxSession) vanishedBeforeAttachError(report serverFateReport, err error) error {
	program := t.programCmd()
	switch {
	case report.fate == serverFateSameServer, report.fate == serverFateNoServer, report.fate == serverFateUnreachable,
		report.fate == serverFateReplaced && report.originalServerExited():
		return fmt.Errorf("tmux session %s vanished before attach; %s — if the pane exited, check that its program runs and accepts its flags (program: %q): %w",
			t.sanitizedName, report.clause(), program, err)
	case report.fate == serverFateReplaced:
		return fmt.Errorf("tmux session %s vanished before attach; %s (program: %q): %w",
			t.sanitizedName, report.clause(), program, err)
	default:
		return fmt.Errorf("tmux session %s vanished before attach; the pane program likely exited immediately after launch — check that it runs and accepts its flags (program: %q): %w",
			t.sanitizedName, program, err)
	}
}

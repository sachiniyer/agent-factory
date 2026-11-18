package tmux

import (
	"fmt"
	"strings"
)

// A session that dies moments after tmux confirmed it leaves one question af
// used to answer with a guess: did its PANE die, or the SERVER it was on? The two
// point at different causes — a program that exited or was killed, against a
// server that crashed, was killed, or was replaced — and before #6581 every such
// failure read "the pane program likely exited immediately", whichever it was.
//
// serverFate answers from the one fact af can still check at that moment: which
// server, if any, answers on the session's socket now, compared with the server
// pid the session was created on. tmux keeps no record of a closed session's
// exit status (measured: a pane-exited hook fires with an empty status and
// signal once its session is gone), so this is the most a vanished session can
// tell.
type serverFate int

const (
	// serverFateUnknown: no creation-time server pid to compare, or the probe
	// got no clear answer. Nothing is claimed.
	serverFateUnknown serverFate = iota
	// serverFateSameServer: the server the session was created on still
	// answers, so only the session's own pane exited.
	serverFateSameServer
	// serverFateNoServer: the socket is there and nothing listens on it
	// (ECONNREFUSED). The server exited — or the session was its only one, and
	// tmux retired it (exit-empty) when the pane's exit closed that session.
	// tmux cannot tell those apart afterwards.
	serverFateNoServer
	// serverFateUnreachable: the socket file is missing (ENOENT). That does NOT
	// show the server exited: a live server whose socket was removed keeps its
	// sessions while every client gets ENOENT (#2875). Proving which needs the
	// SIGUSR1 probe NoServerRunning runs, which a diagnostic must not, so this
	// reports the server unreachable and names both possibilities.
	serverFateUnreachable
	// serverFateReplaced: a DIFFERENT server answers, so the one the session was
	// created on exited and another took its socket.
	serverFateReplaced
)

// serverFateReport is a serverFate with the facts its sentence names.
type serverFateReport struct {
	fate serverFate
	// createdPID is the server pid the session was created on.
	createdPID string
	// nowPID is the pid of the server answering now, for serverFateReplaced.
	nowPID string
	// diagnostic is tmux's own words for serverFateNoServer and
	// serverFateUnreachable.
	diagnostic string
}

// clause renders the report as the clause an error carries, or "" when there
// is nothing to say.
func (r serverFateReport) clause() string {
	switch r.fate {
	case serverFateSameServer:
		return fmt.Sprintf("the tmux server it was created on (pid %s) is still running, so only the session's own pane exited", r.createdPID)
	case serverFateNoServer:
		return fmt.Sprintf("the tmux server it was created on (pid %s) no longer answers (%s): the server exited, or this was its only session and tmux retired it when the pane exited", r.createdPID, r.diagnostic)
	case serverFateUnreachable:
		return fmt.Sprintf("the tmux server it was created on (pid %s) is unreachable: its socket is missing (%s), so the server exited, or its socket was removed while it keeps running", r.createdPID, r.diagnostic)
	case serverFateReplaced:
		return fmt.Sprintf("the tmux server it was created on (pid %s) is gone: a different tmux server (pid %s) now answers on its socket", r.createdPID, r.nowPID)
	default:
		return ""
	}
}

// serverFateSince reports which server answers on this session's socket now,
// against createdPID, the server pid the session was created on. It reads ONE
// bounded display-message and runs on failure paths only.
//
// It deliberately classifies tmux's no-server answers itself instead of going
// through NoServerRunning: on a missing socket that would run the socket-absent
// SIGUSR1 probe against every tmux server this uid runs, which is not a cost a
// diagnostic may impose. A missing socket is reported as unreachable, never as
// an exit it cannot prove.
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
		return report
	}
	nowPID := strings.TrimSpace(string(out))
	if nowPID == "" || strings.ContainsAny(nowPID, " \t\r\n") {
		return report
	}
	if nowPID == createdPID {
		report.fate = serverFateSameServer
	} else {
		report.fate = serverFateReplaced
		report.nowPID = nowPID
	}
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
func (t *TmuxSession) vanishedBeforeAttachError(report serverFateReport, err error) error {
	program := t.programCmd()
	switch report.fate {
	case serverFateSameServer:
		return fmt.Errorf("tmux session %s vanished before attach; %s: the pane program exited immediately after launch — check that it runs and accepts its flags (program: %q): %w",
			t.sanitizedName, report.clause(), program, err)
	case serverFateNoServer, serverFateUnreachable:
		return fmt.Errorf("tmux session %s vanished before attach; %s — if the pane exited, check that its program runs and accepts its flags (program: %q): %w",
			t.sanitizedName, report.clause(), program, err)
	case serverFateReplaced:
		return fmt.Errorf("tmux session %s vanished before attach; %s, so the session died with the server that held it (program: %q): %w",
			t.sanitizedName, report.clause(), program, err)
	default:
		return fmt.Errorf("tmux session %s vanished before attach; the pane program likely exited immediately after launch — check that it runs and accepts its flags (program: %q): %w",
			t.sanitizedName, program, err)
	}
}

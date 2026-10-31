package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/log"
)

// probeKind is the question a has-session caller is asking. It is stated at
// every call site, never inferred, because the SAME tmux answer — ENOENT on the
// socket — means different things to each (#4135, #5766):
//
//   - probeNewName: pre-spawn of a NEW session name (Start's gate). A new server
//     binds a fresh socket file, so an old server holding an unlinked inode
//     cannot collide: socket-ENOENT is plain absence. Answering unknown here
//     fails session creation on a fresh host.
//   - probeReadOnly: a read-only existence check that conflates every non-
//     timeout failure with absence (ExistsOrUnknown). Unchanged legacy.
//   - probeLiveness: is an EXISTING session's agent still alive? A socket a /tmp
//     cleaner unlinked while its server runs is unknown, not gone.
//   - probeRestore: respawn of an EXISTING session. It would REPLACE the session,
//     so "socket gone" read as dead starts a second agent beside a still-running
//     one. It keeps the uncertainty and the caller refuses.
type probeKind int

const (
	probeNewName probeKind = iota
	probeReadOnly
	probeLiveness
	probeRestore
)

// keepsSocketGoneUncertain reports whether this kind must treat a missing socket
// that a live server may still hold as unknown.
func (k probeKind) keepsSocketGoneUncertain() bool { return k == probeLiveness || k == probeRestore }

// ErrSessionSocketUnverified marks a restore refused because the session's tmux
// socket is gone but a live server may still be running its agent.
var ErrSessionSocketUnverified = errors.New("tmux socket unverified")

// socketClaimProofAvailable reports whether this platform can tell "a live
// server still holds the unlinked socket" from "none does". NoServerRunning's
// ENOENT disambiguation walks the process table (/proc) and signals servers, so
// it is only conclusive on linux. A package var so tests can pin the darwin shape.
var socketClaimProofAvailable = func() bool { return runtime.GOOS == "linux" }

// probeAnswer is a has-session result. socketGone marks the one unknown that is
// about the socket rather than a deadline, so a restore caller can refuse with a
// specific message instead of the timeout's.
type probeAnswer struct {
	exists, known, socketGone bool
}

// ProbeSessionLiveness is ProbeSession for a caller asking "is the agent behind
// this EXISTING session still alive?" (probeLiveness).
func (t *TmuxSession) ProbeSessionLiveness() (exists bool, known bool) {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	a := probeSessionAs(ctx, t.cmdExec, t.sanitizedName, probeLiveness)
	return a.exists, a.known
}

// ProbeSessionForRestore is ProbeSession for a caller deciding the fate of an
// EXISTING session during a restore pass (probeRestore): an unlinked socket under
// a possibly-live server is unknown, never absent. It is the probe every restore
// pass read must use, so no tab is classified dead with pre-spawn semantics while
// RestoreWithResult would refuse the same session (#6254).
func (t *TmuxSession) ProbeSessionForRestore() (exists bool, known bool) {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	a := probeSessionAs(ctx, t.cmdExec, t.sanitizedName, probeRestore)
	return a.exists, a.known
}

// probeSessionAs is the has-session probe for an explicit caller kind.
func probeSessionAs(ctx context.Context, cmdExec cmd.Executor, name string, kind probeKind) probeAnswer {
	// Liveness falls back to the legacy answer where the platform cannot prove a
	// socket unclaimed (darwin), so it is not held unknown forever. Restore does
	// NOT: where the live-server check is inconclusive it refuses, because the
	// alternative is a second agent beside a running one.
	if !kind.keepsSocketGoneUncertain() || (kind == probeLiveness && !socketClaimProofAvailable()) {
		exists, known := probeSessionContext(ctx, cmdExec, name)
		return probeAnswer{exists: exists, known: known}
	}
	// stderr is the only place tmux's reason lives; a Run-based probe otherwise
	// sees a bare "exit status 1".
	c := boundedTmuxCommand(ctx, "has-session", fmt.Sprintf("-t=%s", name))
	var stderr bytes.Buffer
	if c.Stderr == nil {
		c.Stderr = &stderr
	}
	err := cmdExec.Run(c)
	reapTmuxGroup(c)
	err = normalizeWaitDelay(err)
	if err == nil {
		return probeAnswer{exists: true, known: true}
	}
	if ctx.Err() != nil {
		return probeAnswer{}
	}
	if exitErr := (*exec.ExitError)(nil); errors.As(err, &exitErr) && len(exitErr.Stderr) == 0 && stderr.Len() > 0 {
		exitErr.Stderr = stderr.Bytes()
	}
	if diagnostic, exitOne := tmuxExitOneDiagnostic(err); exitOne &&
		classifyNoServerDiagnostic(diagnostic) == socketAbsent && !NoServerRunning(err) {
		return probeAnswer{socketGone: true}
	}
	return probeAnswer{known: true}
}

// restoreExistence is restoreWithResult's existence read, as probeRestore. It
// returns the (existsOrUnknown, answered) pair sessionExistsReportingAnswer
// does, plus an error when the socket is gone under a possibly-live server:
// restore must not respawn beside it.
func (t *TmuxSession) restoreExistence() (existsOrUnknown, answered bool, err error) {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	a := probeSessionAs(ctx, t.cmdExec, t.sanitizedName, probeRestore)
	if a.socketGone {
		return true, false, fmt.Errorf("restore deferred: %w — not respawning session %q beside a possibly-running agent; "+
			"the session is kept as it was. Find the tmux server (tmux ls, or af doctor) and retry "+
			"once its socket is reachable or the server is stopped",
			ErrSessionSocketUnverified, t.sanitizedName)
	}
	if !a.known {
		log.WarningLog.Printf("tmux has-session for %s timed out after %s; the server is wedged, so "+
			"reporting the session as still present rather than risk a false teardown", t.sanitizedName, tmuxCommandTimeout)
		return true, false, nil
	}
	return a.exists, true, nil
}

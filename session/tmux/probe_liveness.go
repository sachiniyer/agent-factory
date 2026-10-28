package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"

	"github.com/sachiniyer/agent-factory/cmd"
)

// socketClaimProofAvailable reports whether this platform can tell "a live
// server still holds the unlinked socket" from "none does". NoServerRunning's
// ENOENT disambiguation walks the process table (/proc) and signals servers, so
// it is only conclusive on linux; on darwin it can never prove a socket
// unclaimed, which would make every missing-socket answer "unknown". A package
// var so tests can pin the darwin shape.
var socketClaimProofAvailable = func() bool { return runtime.GOOS == "linux" }

// ProbeSessionLiveness is ProbeSession for a caller asking "is the agent behind
// this EXISTING session still alive?" (#4135).
//
// It differs from ProbeSession in exactly one answer. A socket file unlinked by a
// /tmp cleaner while its server and panes keep running makes every client get
// ENOENT; for a liveness read that is "unknown", not "gone", because reading it
// as gone has the daemon respawn a second agent into the same worktree. The
// PRE-SPAWN existence probe (Start) must NOT take this form: it asks whether a
// name is taken, and a new server binds a fresh socket file, so an old server
// holding an unlinked inode cannot collide — there ENOENT is plain absence, and
// an unknown there fails session creation on a fresh host.
//
// Where the platform cannot prove a socket unclaimed (see
// socketClaimProofAvailable) this falls back to ProbeSession's answer.
func (t *TmuxSession) ProbeSessionLiveness() (exists bool, known bool) {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	return probeSessionLivenessContext(ctx, t.cmdExec, t.sanitizedName)
}

func probeSessionLivenessContext(ctx context.Context, cmdExec cmd.Executor, name string) (exists bool, known bool) {
	if !socketClaimProofAvailable() {
		return probeSessionContext(ctx, cmdExec, name)
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
		return true, true
	}
	if ctx.Err() != nil {
		return false, false
	}
	if exitErr := (*exec.ExitError)(nil); errors.As(err, &exitErr) && len(exitErr.Stderr) == 0 && stderr.Len() > 0 {
		exitErr.Stderr = stderr.Bytes()
	}
	if diagnostic, exitOne := tmuxExitOneDiagnostic(err); exitOne &&
		classifyNoServerDiagnostic(diagnostic) == socketAbsent && !NoServerRunning(err) {
		return false, false
	}
	return false, true
}

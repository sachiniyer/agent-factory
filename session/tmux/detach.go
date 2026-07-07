package tmux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// waitForAttachDrain waits for the attach goroutines (io.Copy +
// monitorWindowSize x2) to finish, falling back to SIGKILLing the
// attach-session child if the wait exceeds wgWaitSigkillDeadline. The
// fallback exists because closing the PTY master (t.ptmx) does not wake a
// blocking Read on a character device — that read only returns when the
// slave end closes, which happens when the tmux client child exits, which
// requires a round-trip through a potentially contended tmux server (#598).
//
// Three-stage bound to guarantee the user-visible detach is finite even
// when our escape hatches fail (#598 follow-up regression at 00:05:14
// 2026-05-20 — a 51s hang because killAttach was nil and the post-SIGKILL
// wait was unbounded):
//
//  1. wg.Wait returns within wgWaitSigkillDeadline (the happy path).
//  2. Otherwise: try the recorded killAttach closure if present, then a
//     pgrep-based "find the tmux attach-session for this name and kill it"
//     as last-resort. Then wait at most wgWaitAbandonDeadline for the
//     stuck goroutine to drain.
//  3. Otherwise: log ERROR, return, and let the goroutine leak. The
//     kernel will eventually drain the PTY and the goroutine will exit on
//     its own — a leaked goroutine is strictly better than freezing the
//     TUI.
//
// Returns the elapsed wait so callers that surface diagnostics about a slow
// wg.Wait (Detach) can do so without re-measuring. On the abandon path
// returns wgWaitAbandonDeadline (not the literal elapsed) so the caller's
// slowDetachWgWaitThreshold check still fires cleanly.
// proactiveDetachDrain SIGTERMs the attach-session child and waits up to
// proactiveGraceDeadline for the attach goroutines (io.Copy + monitorWindowSize)
// to drain. It returns the elapsed wait and whether the drain completed within
// the grace.
//
// This is the #1157 fix. The old detach path closed the PTY master and then
// waited for the child to notice and exit on its own — an exit that round-trips
// through the shared tmux server. When the daemon's 1s capture-pane poll had
// that server busy (~32% of the time), the round-trip lost the race and wg.Wait
// ran the full wgWaitSigkillDeadline until the SIGKILL fallback fired. SIGTERM
// goes straight to the child process, no server round-trip, so a well-behaved
// client detaches and exits within a scheduler tick regardless of server load —
// and, unlike an immediate SIGKILL, lets the client tear its terminal state
// down cleanly first. The session survives a client death (Detach re-attaches
// via Restore), so this is always safe.
//
// drained=false (SIGTERM ignored, child never started, or already gone) falls
// through to waitForAttachDrain's unchanged SIGKILL → pgrep → abandon backstop,
// so every existing #598/#601/#602 escape hatch stays behind this proactive
// signal exactly as before.
func (t *TmuxSession) proactiveDetachDrain() (time.Duration, bool) {
	wg := t.wg
	if wg == nil {
		// Nothing was attached; there's nothing to drain.
		return 0, true
	}
	start := time.Now()
	if t.termAttach == nil {
		// No recorded child to signal — let the backstop handle it.
		return 0, false
	}
	if _, err := t.termAttach(); err != nil {
		// Child never started or already exited; nothing to unblock.
		return 0, false
	}
	waitDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
		return time.Since(start), true
	case <-time.After(proactiveGraceDeadline):
		// SIGTERM didn't unblock io.Copy in time. The wg.Wait goroutine
		// above stays parked until the SIGKILL backstop drains wg — a
		// transient, self-completing goroutine, not the abandon-path leak.
		return time.Since(start), false
	}
}

func (t *TmuxSession) waitForAttachDrain() time.Duration {
	// Capture the WaitGroup pointer locally so the helper goroutine below
	// doesn't race against the Detach/Close defer that nils t.wg after
	// we return. The abandon path leaks the goroutine on purpose; capture
	// here means the leaked goroutine reads its own local, not a field
	// concurrent goroutines may have mutated.
	wg := t.wg
	if wg == nil {
		return 0
	}
	waitStart := time.Now()
	waitDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
		return time.Since(waitStart)
	case <-time.After(wgWaitSigkillDeadline):
		// Primary fallback: SIGKILL the recorded attach process.
		if t.killAttach != nil {
			pid, killErr := t.killAttach()
			log.WarningLog.Printf("tmux: wg.Wait exceeded %v; SIGKILLing tmux attach-session pid=%d to unblock io.Copy",
				wgWaitSigkillDeadline, pid)
			if killErr != nil {
				log.WarningLog.Printf("tmux: SIGKILL attempt failed: %v", killErr)
			}
		} else {
			// Last-resort fallback: locate the attach client via pgrep -f
			// and SIGKILL by pid. We get here when the pairing invariant
			// between t.ptmx and t.killAttach was violated (a bug
			// elsewhere) — the Problem A fix should prevent this, but
			// the safety net protects against any future regression.
			log.WarningLog.Printf("tmux: wg.Wait exceeded %v but no attach process recorded; attempting pgrep-based fallback",
				wgWaitSigkillDeadline)
			if killed, err := killTmuxAttachByName(t.sanitizedName); err != nil {
				log.WarningLog.Printf("tmux: pgrep fallback failed: %v", err)
			} else if killed > 0 {
				log.WarningLog.Printf("tmux: pgrep fallback killed %d attach-session process(es) for %s",
					killed, t.sanitizedName)
			} else {
				log.WarningLog.Printf("tmux: pgrep fallback found no matching attach-session process for %s",
					t.sanitizedName)
			}
		}
		// Secondary bound: even if the SIGKILL/pgrep attempts above
		// failed (or there was nothing to kill), do not block the TUI
		// indefinitely waiting for the io.Copy goroutine to drain. If
		// it's still stuck after wgWaitAbandonDeadline more, abandon
		// the wait, leak the goroutine, and return. See the package
		// doc on wgWaitAbandonDeadline.
		select {
		case <-waitDone:
			return time.Since(waitStart)
		case <-time.After(wgWaitAbandonDeadline):
			log.ErrorLog.Printf("tmux: wg.Wait exceeded %v even after SIGKILL/pgrep fallback; "+
				"abandoning wg.Wait. The io.Copy goroutine may leak until the kernel drains the PTY. "+
				"This is preferable to freezing the TUI but indicates a deeper PTY/tmux-server issue.",
				wgWaitAbandonDeadline)
			return wgWaitSigkillDeadline + wgWaitAbandonDeadline
		}
	}
}

// pgrepRunner abstracts the "pgrep -f <pattern>" call so tests can stub
// process discovery without actually shelling out. Returns the matched
// pids (one per line of pgrep stdout) or an error if pgrep fails for a
// reason other than "no matches" (which pgrep signals with exit code 1
// and the runner reports as len(pids) == 0, nil).
type pgrepRunner func(pattern string) (pids []int, err error)

// killByPid abstracts SIGKILL-by-pid so tests can record calls without
// actually killing real processes.
type killByPidFn func(pid int) error

// pgrepRunnerVar / killByPidVar are package-level hooks tests can swap.
// Production uses defaultPgrepRunner + defaultKillByPid via exec/syscall.
var (
	pgrepRunnerVar pgrepRunner = defaultPgrepRunner
	killByPidVar   killByPidFn = defaultKillByPid
)

// killTmuxAttachByName locates tmux attach-session client(s) bound to the
// given sanitized session name via `pgrep -f` and SIGKILLs each match.
// Returns the number of processes signalled and any error encountered.
//
// The pgrep pattern is anchored to the literal `tmux attach-session ... -t
// =<name>:` invocation we run in Restore(), so the worst that can happen is
// missing a kill (graceful) — not killing the wrong process. A bare name
// match could collide with other tmux invocations (e.g. `tmux kill-session
// -t =<name>:`), which we explicitly do NOT want to interrupt mid-flight.
// The `=<name>:` exact-match target must mirror exactTarget() / the
// attach-session target in Restore() (#1006).
//
// Exit code 1 from pgrep means "no matches" and is treated as success
// with 0 kills; any other exit code is surfaced as an error.
func killTmuxAttachByName(sanitizedName string) (int, error) {
	pattern := fmt.Sprintf(`tmux attach-session -t =%s:$`, regexp.QuoteMeta(sanitizedName))
	pids, err := pgrepRunnerVar(pattern)
	if err != nil {
		return 0, fmt.Errorf("pgrep -f %q: %w", pattern, err)
	}
	killed := 0
	for _, pid := range pids {
		if killErr := killByPidVar(pid); killErr != nil {
			log.WarningLog.Printf("tmux: SIGKILL pid=%d (pgrep fallback) failed: %v", pid, killErr)
			continue
		}
		killed++
	}
	return killed, nil
}

// defaultPgrepRunner shells out to `pgrep -f <pattern>` and parses the
// pid list. Exit status 1 = "no matches" returns (nil, nil); any other
// non-zero exit is an error.
func defaultPgrepRunner(pattern string) ([]int, error) {
	out, err := exec.Command("pgrep", "-f", pattern).Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}
	var pids []int
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pid, parseErr := strconv.Atoi(line)
		if parseErr != nil {
			return nil, fmt.Errorf("parsing pgrep pid %q: %w", line, parseErr)
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

// defaultKillByPid sends SIGKILL to the given pid. ESRCH (process already
// exited) is silently ignored — the goal is "no longer a blocker", which
// an already-dead process satisfies.
func defaultKillByPid(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := proc.Signal(syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return err
	}
	return nil
}

func (t *TmuxSession) Attach() (chan struct{}, error) {
	// Detach clears t.ptmx after closing it so a Restore failure in the
	// detach path can't leave a stale closed handle behind (issue #464).
	// Refuse to attach without a live PTY rather than binding goroutines
	// to a nil or closed file and hanging.
	if t.ptmx == nil {
		return nil, fmt.Errorf("cannot attach: no PTY available, call Restore first")
	}

	t.attachCh = make(chan struct{})

	t.wg = &sync.WaitGroup{}
	t.wg.Add(1)
	t.ctx, t.cancel = context.WithCancel(context.Background())

	// The first goroutine should terminate when the ptmx is closed. We use the
	// waitgroup to wait for it to finish.
	// The 2nd one returns when you press escape to Detach. It doesn't need to be
	// in the waitgroup because is the goroutine doing the Detaching; it waits for
	// all the other ones.
	go func() {
		defer t.wg.Done()
		_, _ = io.Copy(os.Stdout, t.ptmx)
		// When io.Copy returns, it means the connection was closed
		// This could be due to normal detach or Ctrl-D
		// Check if the context is done to determine if it was a normal detach
		select {
		case <-t.ctx.Done():
			// Normal detach, do nothing
		default:
			// If context is not done, it was likely an abnormal termination (Ctrl-D)
			// Print warning message
			fmt.Fprintf(os.Stderr, "\n\033[31mError: Session terminated without detaching. Use %s to properly detach from tmux sessions.\033[0m\n", DetachKeyDisplay)
		}
	}()

	go func() {
		// Close the channel after 50ms
		timeoutCh := make(chan struct{})
		go func() {
			time.Sleep(50 * time.Millisecond)
			close(timeoutCh)
		}()

		// Read input from stdin and check for Ctrl+q
		buf := make([]byte, 32)
		for {
			nr, err := os.Stdin.Read(buf)
			if err != nil {
				if err == io.EOF {
					break
				}
				continue
			}

			// Nuke the first bytes of stdin, up to 64, to prevent tmux from reading it.
			// When we attach, there tends to be terminal control sequences like ?[?62c0;95;0c or
			// ]10;rgb:f8f8f8. The control sequences depend on the terminal (warp vs iterm). We should use regex ideally
			// but this works well for now. Log this for debugging.
			//
			// There seems to always be control characters, but I think it's possible for there not to be. The heuristic
			// here can be: if there's characters within 50ms, then assume they are control characters and nuke them.
			select {
			case <-timeoutCh:
			default:
				log.InfoLog.Printf("nuked first stdin: %s", buf[:nr])
				continue
			}

			// Check for detach key. stdin.Read can batch the detach key with
			// preceding bytes in a single read (buffered terminal input), so
			// check the last byte rather than requiring it to be the sole byte
			// — otherwise the detach key is forwarded to tmux and the detach is
			// silently missed (#975). Forward any preceding bytes first so they
			// still reach the session, matching the surrounding (best-effort)
			// write-error handling.
			if nr > 0 && buf[nr-1] == DetachKeyByte {
				if nr > 1 {
					_, _ = t.ptmx.Write(buf[:nr-1])
				}
				// Closest point to "user pressed detach" we can observe —
				// the elapsed in this trace is whatever Detach() itself
				// took, which matches what blocks the app-side <-ch.
				detachTracef("tmux-stdin-reader-saw-detach-key name=%s", t.sanitizedName)
				// Detach from the session
				t.Detach()
				return
			}

			// Forward other input to tmux
			_, _ = t.ptmx.Write(buf[:nr])
		}
	}()

	t.monitorWindowSize()
	return t.attachCh, nil
}

// detachTraceEnabled reports whether the opt-in [detach-trace] step markers
// on the Detach hot path should be emitted. Off by default so a routine
// detach produces ZERO WARN lines; set AF_DETACH_TRACE=1 to restore the
// step-level breakdown that localized the #598 stall to the wg.Wait step.
// This completes the #788/#790 gating, which moved the app-layer sibling
// markers (app/detach_trace.go) behind the same env var but left this
// tmux-layer set — added by #599 to catch the #598 hang — logging
// unconditionally at WARNING, where it grew to 93% of all WARN volume (#1157).
func detachTraceEnabled() bool {
	return os.Getenv("AF_DETACH_TRACE") == "1"
}

// detachTracef emits a [detach-trace] step marker, and only when
// AF_DETACH_TRACE=1. It logs at INFO rather than WARNING so that even the
// opt-in traces stay out of the WARN stream that log triage and af
// bug-report (#1048) rely on — the only detach event worth a WARN is the
// SIGKILL backstop actually firing (see waitForAttachDrain).
func detachTracef(format string, args ...any) {
	if !detachTraceEnabled() {
		return
	}
	log.InfoLog.Printf("[detach-trace] "+format, args...)
}

// Detach disconnects from the current tmux session. Logs errors instead of panicking
// so the application can attempt graceful recovery.
func (t *TmuxSession) Detach() {
	detachStart := time.Now()
	detachTracef("tmux.Detach-entry name=%s", t.sanitizedName)
	defer func() {
		detachTracef("tmux.Detach-exit name=%s total=%v", t.sanitizedName, time.Since(detachStart))
		close(t.attachCh)
		t.attachCh = nil
		t.cancel = nil
		t.ctx = nil
		t.wg = nil
		// NOTE: t.killAttach is deliberately NOT cleared here. The Restore()
		// call below sets a fresh killAttach paired with the new t.ptmx; if
		// we cleared it here we'd leave the next Attach lifecycle in a
		// ptmx-valid / killAttach-nil state, which is exactly the
		// invariant break that caused the 51s detach hang in the #598
		// follow-up regression. killAttach is now paired with t.ptmx
		// inline below — set/cleared together. See the in-line clear next
		// to "t.ptmx = nil".
	}()

	// Cancel context FIRST so monitorWindowSize goroutines exit promptly and
	// the io.Copy goroutine in Attach() sees a normal detach rather than an
	// abnormal termination. Without this, closing the PTY can wake the
	// io.Copy goroutine before cancel() runs, causing a spurious
	// "Session terminated without detaching" warning.
	stepStart := time.Now()
	t.cancel()
	detachTracef("tmux.Detach-cancel-done name=%s elapsed=%v", t.sanitizedName, time.Since(stepStart))

	// Close the attached pty session so the io.Copy goroutine returns.
	stepStart = time.Now()
	closeErr := t.ptmx.Close()
	detachTracef("tmux.Detach-ptmx.Close-done name=%s elapsed=%v", t.sanitizedName, time.Since(stepStart))

	// Wait for the attach goroutines (io.Copy + monitorWindowSize x2) to
	// finish before mutating t.ptmx. monitorWindowSize reads t.ptmx via
	// updateWindowSize, so clearing the field before wg.Wait races against
	// those reads (#512). waitForAttachDrain bounds the wait by SIGKILLing
	// the attach-session child if wg.Wait exceeds wgWaitSigkillDeadline —
	// see #598 follow-up for the diagnosis.
	// Proactively SIGTERM the attach child so io.Copy unblocks without a
	// tmux-server round-trip (#1157). On a healthy client this drains within
	// the grace and we never touch the SIGKILL path; a client that ignores
	// SIGTERM falls through to waitForAttachDrain's unchanged
	// SIGKILL → pgrep → abandon backstop.
	waitElapsed, drained := t.proactiveDetachDrain()
	if !drained {
		waitElapsed = t.waitForAttachDrain()
	}
	detachTracef("tmux.Detach-wg.Wait-done name=%s elapsed=%v", t.sanitizedName, waitElapsed)
	// Defense-in-depth: if wg.Wait still exceeded the slow threshold after
	// the SIGKILL fallback ran, that means killAttach didn't unstick the
	// goroutine — a deeper bug than what this fix targets. Keep the loud
	// log so we hear about it.
	if waitElapsed > slowDetachWgWaitThreshold {
		log.ErrorLog.Printf("tmux.Detach: wg.Wait took %v — likely tmux server "+
			"contention from background capture-pane operations. Sessions paused "+
			"while attached should have prevented this; bug?", waitElapsed)
	}

	// Now safe to clear t.ptmx. Clearing unconditionally before Restore
	// means a Restore failure (or a Close failure) can't leave the closed
	// handle dangling on the struct — a subsequent Attach would otherwise
	// silently bind goroutines to a closed file and hang (#464).
	// Pair the clear with t.killAttach AND t.termAttach: both closures
	// reference the dying attachCmd whose process is being torn down, so
	// neither must survive past this point. Restore() below reassigns all
	// three fields together for the next attach lifecycle; this is the
	// invariant the #598 follow-up regression broke — clearing here (before
	// Restore), never in the defer (which runs after Restore), is what keeps
	// the fresh closures alive (#602).
	t.ptmx = nil
	t.killAttach = nil
	t.termAttach = nil

	if closeErr != nil {
		log.ErrorLog.Printf("error closing attach pty session: %v", closeErr)
		return
	}

	// Call t.Restore to set a new t.ptmx. The session is alive (we just
	// detached from it), so pass empty workDir — a missing session here is a
	// real problem and should surface, not silently re-spawn and lose history.
	stepStart = time.Now()
	if err := t.Restore(""); err != nil {
		log.ErrorLog.Printf("error restoring pty after detach: %v", err)
	}
	detachTracef("tmux.Detach-Restore-done name=%s elapsed=%v", t.sanitizedName, time.Since(stepStart))
}

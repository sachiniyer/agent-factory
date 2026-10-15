package tmux

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// markedSessionSurvivors attributes a live session's marked processes when the
// kernel-side keys are gone — a dead pane tmux reports no pid for (tmux ≥ 3.8,
// #5319). With the pid goes the pane's kernel session id, which is what the
// detached-survivor lookup above is keyed on, so what remains is the launch
// provenance af stamps into every pane it spawns (#1104).
//
// Every read below is bound to the session incarnation the row's session_id
// recorded at list-panes time: the name can be reused by a replacement between
// the pane listing and these reads, and reading a replacement's markers would
// either misattribute an old generation's survivor or pretend there are none.
// The identity probe runs once before the marker reads and once after, so a
// swap mid-window is caught rather than laundered into a conclusive answer.
//
// The provenance bar is read from the SESSION's own environment, not the
// invoker's: af doctor maps every visible session on the server, including
// ones another agent-factory home owns, so a candidate must carry this
// session's AF_SESSION name, this session's AF_HOME, and — when the session
// reports a generation — this session's AF_SESSION_GEN. A process carrying
// all three provably came from this session: the same bar the vanished-session
// sweep stands on. A same-named process of a different generation is a
// leftover of an earlier incarnation, never this session's to claim.
//
// The process table is re-read AFTER the marker lookups, so a survivor that
// forks a marked helper during the reads is still seen. A process that has
// since exited leaves the scan; a LIVE same-uid process whose environment
// cannot be read is not absence of markers — an `env -i` launch looks exactly
// like a withheld one. But unreadable alone cannot refuse either: daemons
// like ssh-agent or (sd-pam) answer that shape on every Linux host, and on
// macOS every SIP/code-signing-restricted same-uid process does. What makes
// one suspicious is positive evidence of possible pane membership: it still
// wears a kernel session whose leader is gone, inside the session's own
// lifetime. Everything else unreadable is cleared — a survivor that setsids
// itself out of any observable membership escapes both this scan and the
// pre-3.8 SID path alike.
//
// A session that carries no launch markers at all predates the contract, so a
// scan that finds nothing there proves nothing about its survivors: that is an
// incomplete capture — reported as an error — not an empty one.
func markedSessionSurvivors(cmdExec cmd.Executor, sanitizedName string, row paneRow, verifiedOnly bool) ([]proctree.Process, error) {
	if !strings.HasPrefix(row.sessionID, "$") {
		return nil, fmt.Errorf("tmux session %s: list-panes reported no usable session id, so a dead pane's survivors cannot be attributed",
			sanitizedName)
	}
	// identity re-asks the incarnation's server pid and creation stamp: both
	// answering means the session id still resolves, and matching answers on
	// both sides of the marker reads proves nothing replaced it mid-window.
	identity := func() (answer string, created time.Time, err error) {
		ctx, cancel := tmuxTimeoutContext()
		defer cancel()
		out, probeErr := outputTmuxBoundedWith(ctx, cmdExec, "display-message", "-p",
			"-t", row.sessionID, "#{pid} #{session_created}")
		if probeErr != nil {
			return "", time.Time{}, probeErr
		}
		fields := strings.Fields(strings.TrimSpace(string(out)))
		if len(fields) < 2 {
			return "", time.Time{}, fmt.Errorf("incarnation %s is gone", row.sessionID)
		}
		sec, parseErr := strconv.ParseInt(fields[1], 10, 64)
		if parseErr != nil {
			return "", time.Time{}, fmt.Errorf("incarnation %s reported unparseable creation %q", row.sessionID, fields[1])
		}
		return fields[0] + " " + fields[1], time.Unix(sec, 0), nil
	}
	before, sessionCreated, err := identity()
	if err != nil {
		return nil, fmt.Errorf("cannot bind tmux session %s (id %s) for survivor attribution: %w",
			sanitizedName, row.sessionID, err)
	}
	sessionMarker, sessionMarked, err := sessionEnvMarkerAt(cmdExec, row.sessionID, EnvMarkerSession)
	if err != nil {
		return nil, err
	} else if !sessionMarked {
		return nil, fmt.Errorf("tmux session %s carries no %s launch marker, so a dead pane's survivors cannot be attributed",
			sanitizedName, EnvMarkerSession)
	}
	// The recorded marker — not the listed name — is what descendants carry:
	// a session renamed since launch still stamps its original AF_SESSION on
	// everything it spawned, so matching the current name would read those
	// survivors as foreign and call the capture empty while they still live.
	markerName := sessionMarker
	sessionHome, homePresent, err := sessionEnvMarkerAt(cmdExec, row.sessionID, EnvMarkerHome)
	if err != nil {
		return nil, err
	}
	if !homePresent {
		return nil, fmt.Errorf("tmux session %s carries no %s launch marker, so a dead pane's survivors cannot be attributed",
			sanitizedName, EnvMarkerHome)
	}
	sessionGeneration, _, err := sessionEnvMarkerAt(cmdExec, row.sessionID, EnvMarkerGeneration)
	if err != nil {
		return nil, err
	}
	after, _, err := identity()
	if err != nil {
		return nil, fmt.Errorf("cannot revalidate tmux session %s (id %s) after marker reads: %w",
			sanitizedName, row.sessionID, err)
	}
	if after != before {
		return nil, fmt.Errorf("tmux session %s (id %s) was replaced during marker reads, so a dead pane's survivors cannot be attributed",
			sanitizedName, row.sessionID)
	}
	// The identity answer leads with the server's own pid: a marked process
	// still parented directly by it is the pane's live root, not an escapee.
	serverPID, _ := strconv.Atoi(strings.Fields(before)[0])
	fresh, err := proctree.Snapshot()
	if err != nil {
		return nil, fmt.Errorf("cannot re-snapshot the process table for a dead pane's survivors: %w", err)
	}
	cleanHome := filepath.Clean(sessionHome)
	selfUID, selfUIDKnown := proctree.UID(os.Getpid())
	if !selfUIDKnown {
		// A scanner that cannot name its own uid cannot bound the same-uid
		// requirement anywhere below: every judgement degrades to "maybe".
		return nil, fmt.Errorf("tmux session %s: cannot determine this process's uid, so a dead pane's survivors cannot be attributed",
			sanitizedName)
	}
	// The scanning process and its ancestors are excluded from candidacy: a
	// daemon (or af CLI) spawned from inside a pane of this session inherits
	// the pane's launch markers, so it marker-matches itself, and the reap
	// this capture feeds would signal the control path answering the teardown
	// RPC. Marker scrubbing at daemon spawn keeps the common case from ever
	// reaching this exclusion (startDaemonChild); the exclusion covers every
	// other inherited-marker control process.
	selfChain := selfAndAncestorProcesses(fresh)
	var procs []proctree.Process
	var capturedPID = map[int]bool{}
	addSurvivor := func(p proctree.Process) {
		if !capturedPID[p.PID] {
			capturedPID[p.PID] = true
			procs = append(procs, p)
		}
	}
	// fullMatch reports whether an environment carries the complete launch
	// provenance of THIS session: the recorded AF_SESSION name, the session's
	// own AF_HOME, and — when the session reports one — its AF_SESSION_GEN.
	fullMatch := func(environ []string) bool {
		session, hasSession := processEnvValue(environ, EnvMarkerSession)
		processHome, hasHome := processEnvValue(environ, EnvMarkerHome)
		generation, hasGeneration := processEnvValue(environ, EnvMarkerGeneration)
		return hasSession && session == markerName && hasHome &&
			filepath.Clean(processHome) == cleanHome &&
			(sessionGeneration == "" || (hasGeneration && generation == sessionGeneration))
	}
	// attribute returns the process record a full marker match actually
	// describes. The snapshot predates the environment read, so the pid may
	// have been recycled in between: the markers then belong to the pid's
	// CURRENT occupant, and appending the stale record would fail the reap's
	// AliveSame check — reading as "nothing left" while a live marked process
	// stands. The current occupant must re-prove the full match itself; a
	// failure anywhere is an incomplete capture, not a silent drop.
	attribute := func(p proctree.Process, environ []string) (proctree.Process, error) {
		same, err := proctree.SameIdentity(p)
		if err != nil {
			return proctree.Process{}, fmt.Errorf("tmux session %s: cannot revalidate marked pid %d after its environment read: %w",
				sanitizedName, p.PID, err)
		}
		if same {
			return p, nil
		}
		cur, err := proctree.Lookup(p.PID)
		if err != nil {
			// ErrProcessExited answers a zombie still awaiting collection;
			// a missing /proc entry answers one already reaped — Linux
			// returns the raw os.ErrNotExist. Both are the same definitive
			// absence: ordinary churn, not an incomplete capture.
			if errors.Is(err, proctree.ErrProcessExited) || errors.Is(err, os.ErrNotExist) {
				return proctree.Process{}, nil
			}
			return proctree.Process{}, fmt.Errorf("tmux session %s: marked pid %d changed identity during capture and its current occupant cannot be read: %w",
				sanitizedName, p.PID, err)
		}
		if uid, known := proctree.UID(cur.PID); !known {
			return proctree.Process{}, fmt.Errorf("tmux session %s: marked pid %d changed identity during capture and its occupant's ownership cannot be determined",
				sanitizedName, p.PID)
		} else if uid != selfUID {
			return proctree.Process{}, nil
		}
		curEnv, err := proctree.Environ(cur.PID)
		if err != nil {
			return proctree.Process{}, fmt.Errorf("tmux session %s: marked pid %d changed identity during capture and its occupant's environment cannot be read: %w",
				sanitizedName, p.PID, err)
		}
		if !fullMatch(curEnv) {
			return proctree.Process{}, fmt.Errorf("tmux session %s: marked pid %d changed identity during capture and its occupant no longer proves membership",
				sanitizedName, p.PID)
		}
		return cur, nil
	}
	for _, process := range fresh {
		if selfChain[process.PID] {
			continue
		}
		environ, envErr := proctree.Environ(process.PID)
		if envErr != nil {
			// Revalidate identity first: a snapshot entry that has since
			// exited is no survivor at all. What remains needs POSITIVE
			// evidence of possible pane membership before it can make the
			// capture incomplete — on macOS, SIP/code-signing-restricted
			// processes answer unreadable for the same uid, so "unreadable
			// and young" would wedge every teardown on a busy host. The
			// evidence that survives is kernel-session orphanage: a process
			// still wearing a session id whose leader is gone (or whose
			// leader pid has plainly been recycled — a leader cannot start
			// after its member) could be a detached `env -i` survivor. One
			// started before the session existed provably cannot be, and a
			// foreign uid's unreadable environment cannot be either. An
			// unknown start time — StartedAt is zero when the platform
			// cannot derive it — cannot clear by age either.
			same, identityErr := proctree.SameIdentity(process)
			switch {
			case identityErr != nil:
				return procs, fmt.Errorf("tmux session %s: cannot revalidate pid %d while attributing a dead pane's survivors: %w",
					sanitizedName, process.PID, identityErr)
			case same && (process.StartedAt.IsZero() || !process.StartedAt.Before(sessionCreated)):
				// Non-positive session ids are not membership evidence at
				// all: kernel threads carry 0 and darwin records an
				// unreadable session as -1, both excluded by
				// proctree.SessionMembers for the same reason. A FOREIGN
				// uid does not clear the shape either: a detached
				// `sudo -u` child keeps the pane's kernel session while
				// making its environment unreadable to us, so a foreign
				// owner is exactly what such a survivor looks like.
				leader, leaderListed := fresh[process.SID]
				orphanMember := process.SID > 0 && process.SID != process.PID &&
					(!leaderListed || leader.StartedAt.After(process.StartedAt))
				if orphanMember {
					return procs, fmt.Errorf("tmux session %s: live pid %d's environment cannot be read and it wears an orphaned kernel session, so a dead pane's survivors cannot be attributed",
						sanitizedName, process.PID)
				}
			}
			continue
		}
		session, hasSession := processEnvValue(environ, EnvMarkerSession)
		processHome, hasHome := processEnvValue(environ, EnvMarkerHome)
		generation, hasGeneration := processEnvValue(environ, EnvMarkerGeneration)
		if !hasSession && !hasHome && !hasGeneration {
			// No af provenance at all: nothing here distinguishes a pane
			// child that stripped every marker from an unrelated process —
			// the same blind spot the SID-based path has for a survivor
			// that setsids itself. Children of captured survivors are
			// handled separately below, where parentage supplies the
			// evidence markers lack.
			continue
		}
		// A marker that CONTRADICTS the session is positive foreign
		// evidence: a different session name, a different home, or — when
		// the session reports a generation — a different or unexpected
		// generation all prove the process is not this session's.
		contradicts := (hasSession && session != markerName) ||
			(hasHome && filepath.Clean(processHome) != cleanHome) ||
			(sessionGeneration != "" && hasGeneration && generation != sessionGeneration) ||
			(sessionGeneration == "" && hasGeneration)
		if contradicts {
			continue
		}
		// The surviving markers are consistent with this session, but only
		// the FULL set proves membership: a detached `env -u AF_SESSION
		// -u AF_HOME` survivor keeps a readable environment whose remaining
		// markers (here, the generation stamp) neither match nor contradict.
		// Consistent-but-incomplete cannot prove absence — like the sweep's
		// "has no AF_SESSION marker" refusal, this is an incomplete capture,
		// not an empty one.
		full := fullMatch(environ)
		uid, uidKnown := proctree.UID(process.PID)
		switch {
		case full:
			if !uidKnown {
				return procs, fmt.Errorf("tmux session %s: live pid %d carries this session's launch markers but its ownership cannot be determined, so a dead pane's survivors cannot be attributed",
					sanitizedName, process.PID)
			}
			if uid == selfUID {
				member, attrErr := attribute(process, environ)
				if attrErr != nil {
					return procs, attrErr
				}
				if member.PID != 0 && (!verifiedOnly || member.PPID == serverPID) {
					// A marker-matched process NOT parented by this server
					// escaped its pane's tree — exactly what the verified
					// variant exists to leave out. A marker-matched DIRECT
					// child is the pane's still-live root: positively
					// verifiable, so it counts as a member.
					addSurvivor(member)
				}
			}
		case !uidKnown || uid == selfUID:
			return procs, fmt.Errorf("tmux session %s: live pid %d carries only a partial set of launch markers, so it cannot be ruled out as a dead pane's survivor",
				sanitizedName, process.PID)
		}
	}
	if verifiedOnly {
		// Everything below a verified root is a verified member, by the
		// same rule ordinary panes use: PPID descendants plus surviving
		// kernel-session members — a child whose intermediate parent
		// exited is reparented to init but keeps the root's session id.
		for _, root := range procs {
			for _, descendant := range proctree.TreeOf(fresh, root.PID) {
				if !capturedPID[descendant.PID] && !selfChain[descendant.PID] {
					addSurvivor(descendant)
				}
			}
			for _, member := range proctree.SessionMembers(fresh, root.SID) {
				if !capturedPID[member.PID] && !selfChain[member.PID] {
					addSurvivor(member)
				}
			}
		}
		return procs, nil
	}
	// Marker-less descendants of a captured survivor: a detached child that
	// stripped its markers is invisible to the scan above, but while its
	// marked parent still lives, parentage supplies the attribution env
	// cannot — the same evidence the sweep's partial-capture rule stands on.
	// A descendant whose markers say nothing (or whose env cannot be read)
	// is a captured candidate that cannot be cleared: incomplete, not empty.
	queue := append([]proctree.Process(nil), procs...)
	for i := 0; i < len(queue); i++ {
		for _, descendant := range proctree.TreeOf(fresh, queue[i].PID) {
			if capturedPID[descendant.PID] || selfChain[descendant.PID] {
				continue
			}
			environ, envErr := proctree.Environ(descendant.PID)
			if envErr != nil {
				// Revalidate before refusing: a snapshot descendant that
				// has since exited is churn, not a survivor — the same
				// treatment the top-level scan gives its entries.
				same, identityErr := proctree.SameIdentity(descendant)
				switch {
				case identityErr != nil:
					return procs, fmt.Errorf("tmux session %s: cannot revalidate descendant pid %d while attributing a dead pane's survivors: %w",
						sanitizedName, descendant.PID, identityErr)
				case same:
					return procs, fmt.Errorf("tmux session %s: live pid %d descends from an attributed survivor but its environment cannot be read, so a dead pane's survivors cannot be attributed",
						sanitizedName, descendant.PID)
				}
				continue
			}
			if fullMatch(environ) {
				uid, uidKnown := proctree.UID(descendant.PID)
				if !uidKnown {
					return procs, fmt.Errorf("tmux session %s: live pid %d carries this session's launch markers but its ownership cannot be determined, so a dead pane's survivors cannot be attributed",
						sanitizedName, descendant.PID)
				}
				if uid == selfUID {
					member, attrErr := attribute(descendant, environ)
					if attrErr != nil {
						return procs, attrErr
					}
					if member.PID != 0 {
						addSurvivor(member)
						queue = append(queue, member)
					}
				}
				continue
			}
			return procs, fmt.Errorf("tmux session %s: live pid %d descends from an attributed survivor but has no matching %s marker, so a dead pane's survivors cannot be attributed",
				sanitizedName, descendant.PID, EnvMarkerSession)
		}
	}
	return procs, nil
}

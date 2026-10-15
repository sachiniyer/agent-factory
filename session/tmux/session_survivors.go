package tmux

import (
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
// like a withheld one. It is cleared only by a positive reason it cannot be
// the pane's — a start time older than the session itself — because daemons
// like ssh-agent or (sd-pam) answer the uninspectable shape on every desktop
// host and refusing on them would wedge every teardown. One started inside
// the session's lifetime could be a detached env-less survivor, leading its
// own session or not, so that one makes the capture incomplete.
//
// A session that carries no launch markers at all predates the contract, so a
// scan that finds nothing there proves nothing about its survivors: that is an
// incomplete capture — reported as an error — not an empty one.
func markedSessionSurvivors(cmdExec cmd.Executor, sanitizedName string, row paneRow) ([]proctree.Process, error) {
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
	if _, sessionMarked, err := sessionEnvMarkerAt(cmdExec, row.sessionID, EnvMarkerSession); err != nil {
		return nil, err
	} else if !sessionMarked {
		return nil, fmt.Errorf("tmux session %s carries no %s launch marker, so a dead pane's survivors cannot be attributed",
			sanitizedName, EnvMarkerSession)
	}
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
	for _, process := range fresh {
		if selfChain[process.PID] {
			continue
		}
		environ, envErr := proctree.Environ(process.PID)
		if envErr != nil {
			// Revalidate identity first: a snapshot entry that has since
			// exited is no survivor at all. A live entry we cannot inspect
			// and cannot clear by age — it started inside the session's
			// lifetime — could be a detached `env -i` survivor, so the
			// capture is incomplete rather than clean. A foreign uid's
			// unreadable environment cannot be this session's either.
			same, identityErr := proctree.SameIdentity(process)
			switch {
			case identityErr != nil:
				return nil, fmt.Errorf("tmux session %s: cannot revalidate pid %d while attributing a dead pane's survivors: %w",
					sanitizedName, process.PID, identityErr)
			case same && !process.StartedAt.Before(sessionCreated):
				if uid, known := proctree.UID(process.PID); !known || uid == selfUID {
					return nil, fmt.Errorf("tmux session %s: live pid %d's environment cannot be read and it started inside the session's lifetime, so a dead pane's survivors cannot be attributed",
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
		contradicts := (hasSession && session != sanitizedName) ||
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
		full := hasSession && hasHome && (sessionGeneration == "" || hasGeneration)
		uid, uidKnown := proctree.UID(process.PID)
		switch {
		case full:
			if !uidKnown {
				return nil, fmt.Errorf("tmux session %s: live pid %d carries this session's launch markers but its ownership cannot be determined, so a dead pane's survivors cannot be attributed",
					sanitizedName, process.PID)
			}
			if uid == selfUID {
				addSurvivor(process)
			}
		case !uidKnown || uid == selfUID:
			return nil, fmt.Errorf("tmux session %s: live pid %d carries only a partial set of launch markers, so it cannot be ruled out as a dead pane's survivor",
				sanitizedName, process.PID)
		}
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
				return nil, fmt.Errorf("tmux session %s: live pid %d descends from an attributed survivor but its environment cannot be read, so a dead pane's survivors cannot be attributed",
					sanitizedName, descendant.PID)
			}
			session, hasSession := processEnvValue(environ, EnvMarkerSession)
			processHome, hasHome := processEnvValue(environ, EnvMarkerHome)
			generation, hasGeneration := processEnvValue(environ, EnvMarkerGeneration)
			full := hasSession && session == sanitizedName && hasHome &&
				filepath.Clean(processHome) == cleanHome &&
				(sessionGeneration == "" || (hasGeneration && generation == sessionGeneration))
			if full {
				uid, uidKnown := proctree.UID(descendant.PID)
				if !uidKnown {
					return nil, fmt.Errorf("tmux session %s: live pid %d carries this session's launch markers but its ownership cannot be determined, so a dead pane's survivors cannot be attributed",
						sanitizedName, descendant.PID)
				}
				if uid == selfUID {
					addSurvivor(descendant)
					queue = append(queue, descendant)
				}
				continue
			}
			return nil, fmt.Errorf("tmux session %s: live pid %d descends from an attributed survivor but has no matching %s marker, so a dead pane's survivors cannot be attributed",
				sanitizedName, descendant.PID, EnvMarkerSession)
		}
	}
	return procs, nil
}

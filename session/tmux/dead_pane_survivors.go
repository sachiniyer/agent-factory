package tmux

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// markedDeadPaneSurvivors is the pid-less dead pane's survivor attribution:
// the vanished-session sweep's bounded-candidate discovery and ownership
// rules, fed with the evidence the row's own session supplies.
//
// The generation cohort comes from two places: the session's captured members
// (orphanGenerations), AND the session's own AF_SESSION_GEN, read against the
// incarnation id the row carries so a same-named replacement can never lend
// its stamp (#5319). The second read is what lets a session holding ONLY dead
// panes — no members left to seed the cohort — still attribute its detached
// survivors. A session with no generation marker falls back to the sweep's
// legacy cohort, matching only marker-less-generation candidates.
//
// The ownership home is likewise the SESSION's stamped AF_HOME when it
// carries one — af doctor runs this capture against sessions another home
// owns, and matching survivors by the invoker's home would silently drop
// exactly the processes the check exists to measure. Only an unmarked
// session falls back to the invoking home.
//
// admitRequesters is true because this capture feeds reapSessionProcesses
// directly: a marked survivor that is itself the teardown requester must stay
// in the set so SignalUnlessTracked can exempt it per signal tier and reap it
// once the exemption lapses — dropping it at discovery would leak it forever.
func markedDeadPaneSurvivors(cmdExec cmd.Executor, snap map[int]proctree.Process,
	sanitizedName string, row paneRow, captured []proctree.Process, verifiedOnly bool,
) ([]proctree.Process, error) {
	target := row.sessionID
	if target == "" {
		target = exactTarget(sanitizedName)
	}
	// The env reads below must answer for the incarnation list-panes observed,
	// not whatever the id — a per-server counter — resolves to after a server
	// restart. The row carries the full (session_id, server pid, created)
	// tuple; check it before AND after so a replacement can never lend its
	// markers to this scan.
	if same, err := paneRowIncarnationMatches(cmdExec, row); err != nil || !same {
		return nil, fmt.Errorf("cannot confirm the session incarnation owning a pid-less dead pane: same=%t err=%v",
			same, err)
	}
	// AF_SESSION is required, not optional: a session that does not carry its
	// own name marker predates the marker contract, so a scan finding nothing
	// proves nothing about survivors — that is an incomplete capture, not an
	// empty one. The stamped value is also the match key, so a session whose
	// tmux name drifted from its launch name still attributes its own.
	markerName, nameFound, nameErr := sessionEnvMarkerAt(cmdExec, target, EnvMarkerSession)
	switch {
	case nameErr != nil:
		return nil, fmt.Errorf("cannot read the %s marker of the session owning a pid-less dead pane: %w",
			EnvMarkerSession, nameErr)
	case !nameFound || markerName == "":
		// A PRESENT-but-empty value (`AF_SESSION=` after set-environment
		// clears it) is worse than absent: it is a key that matches nothing
		// while every survivor still carries its nonempty launch stamp — an
		// empty successful scan hiding live processes (#5319 review).
		return nil, fmt.Errorf("session %s carries no usable %s marker (present=%t value=%q), so a pid-less dead pane's survivors cannot be attributed",
			sanitizedName, EnvMarkerSession, nameFound, markerName)
	}
	generations := orphanGenerations(captured, markerName)
	generation, genFound, genErr := sessionEnvMarkerAt(cmdExec, target, EnvMarkerGeneration)
	switch {
	case genErr != nil:
		return nil, fmt.Errorf("cannot read the generation of the session owning a pid-less dead pane: %w", genErr)
	case genFound && generation == "":
		// A present-but-empty generation seeds a cohort that matches no
		// launch stamp: survivors keep their nonempty generation and the
		// discovery rejects every one of them — another empty-successful-scan
		// shape, malformed rather than empty (#5319 review).
		return nil, fmt.Errorf("session %s carries an empty %s marker, so a pid-less dead pane's survivors cannot be attributed",
			sanitizedName, EnvMarkerGeneration)
	case genFound:
		generations.values[generation] = true
	default:
		generations.legacy = true
	}
	scanHome, homeFound, homeErr := sessionEnvMarkerAt(cmdExec, target, EnvMarkerHome)
	switch {
	case homeErr != nil:
		return nil, fmt.Errorf("cannot read the home of the session owning a pid-less dead pane: %w", homeErr)
	case homeFound && scanHome == "":
		// A present-but-empty home (`AF_HOME=` after set-environment clears
		// it) normalizes to "." and marks every survivor carrying the
		// session's absolute launch home as foreign — the third
		// empty-successful-scan shape, malformed rather than empty (#5319
		// review).
		return nil, fmt.Errorf("session %s carries an empty %s marker, so a pid-less dead pane's survivors cannot be attributed",
			sanitizedName, EnvMarkerHome)
	case !homeFound:
		var ownErr error
		scanHome, ownErr = afHomeDir()
		if ownErr != nil {
			return nil, fmt.Errorf("cannot resolve this agent-factory home for a dead pane's survivor attribution: %w", ownErr)
		}
	}
	if same, err := paneRowIncarnationMatches(cmdExec, row); err != nil || !same {
		return nil, fmt.Errorf("the session incarnation changed while its markers were read: same=%t err=%v",
			same, err)
	}
	// The server pid serves both the self-chain check inside the scan — a
	// marked process directly parented to it is a kernel-verifiable pane root
	// even when it sits on the teardown's own ancestry — and the verified-mode
	// root filter below. A probe failure cannot be read as "no server": it
	// leaves the scan conservative (0 → no self-chain exemptions) but must be
	// reported, so it joins the scan errors.
	serverPID, serverErr := sessionServerPID(cmdExec, target)
	if serverErr != nil {
		serverPID = 0
	}
	candidates := append([]proctree.Process(nil), captured...)
	refreshed, refreshErr := refreshOrphanCandidates(candidates, markerName, &generations, true)
	// Members the pane tree captured moments ago are already proven; a marked
	// one that lands on the teardown's own ancestry (a daemon auto-started by a
	// client inside the pane) must not fail the scan it was never meant to
	// need — the self-chain refusal only guards marker-only matches.
	treeVerified := make(map[int]bool, len(captured))
	for _, member := range captured {
		treeVerified[member.PID] = true
	}
	marked, inspectErr := markedOrphanProcesses(refreshed, markerName, scanHome, generations, treeVerified, serverPID)
	scanErr := errors.Join(refreshErr, inspectErr)
	if serverErr != nil {
		scanErr = errors.Join(scanErr, fmt.Errorf(
			"cannot name the server owning a dead pane for self-chain verification: %w", serverErr))
	}
	if !verifiedOnly {
		return marked, scanErr
	}
	if serverErr != nil {
		return nil, scanErr
	}
	seen := map[int]bool{}
	var verified []proctree.Process
	add := func(p proctree.Process) {
		if !seen[p.PID] {
			seen[p.PID] = true
			verified = append(verified, p)
		}
	}
	// Marker-validated processes that escaped the pane's tree stay OUT — they
	// are the escaped-process finding — but a marker-matched process still
	// parented by this session's tmux server is the pane's live root (tmux
	// marks the pane dead when its terminal closes, before the root exits),
	// positively verifiable, so it and its kernel family count as members by
	// the same rule ordinary panes use.
	for _, process := range marked {
		if process.PPID != serverPID {
			continue
		}
		add(process)
		for _, descendant := range proctree.TreeOf(snap, process.PID) {
			add(descendant)
		}
		for _, member := range proctree.SessionMembers(snap, process.SID) {
			add(member)
		}
	}
	return verified, scanErr
}

// paneRowIncarnationMatches re-asks the session id a pane row carried for the
// server-pid/creation tuple list-panes observed alongside it — the
// (session_id, pid, session_created) triple io.go's confirmedGeneration uses,
// because the id alone is a per-server counter a replacement server reissues.
// A missing target, a blank answer, or a changed tuple means the session the
// row described is gone.
func paneRowIncarnationMatches(cmdExec cmd.Executor, row paneRow) (bool, error) {
	if row.sessionID == "" || row.serverPID == "" || row.sessionCreated == "" {
		return false, fmt.Errorf("pane row carries no session incarnation to revalidate")
	}
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	out, err := outputTmuxBoundedWith(ctx, cmdExec, "display-message", "-p",
		"-t", row.sessionID, "#{session_id} #{pid} #{session_created}")
	if err != nil {
		if ctx.Err() != nil {
			return false, fmt.Errorf("%w: session incarnation probe after %s", ErrTmuxTimeout, tmuxCommandTimeout)
		}
		return false, err
	}
	f := strings.Fields(strings.TrimSpace(string(out)))
	if len(f) < 3 {
		return false, fmt.Errorf("session id %s no longer resolves to a session", row.sessionID)
	}
	return f[0] == row.sessionID && f[1] == row.serverPID && f[2] == row.sessionCreated, nil
}

// sessionServerPID names the tmux server process that owns the session a pane
// row was listed under, via the session incarnation id the row carries.
func sessionServerPID(cmdExec cmd.Executor, target string) (int, error) {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	out, err := outputTmuxBoundedWith(ctx, cmdExec, "display-message", "-p",
		"-t", target, "#{pid}")
	if err != nil {
		if ctx.Err() != nil {
			return 0, fmt.Errorf("%w: session server pid probe after %s", ErrTmuxTimeout, tmuxCommandTimeout)
		}
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("unparseable server pid %q", strings.TrimSpace(string(out)))
	}
	return pid, nil
}

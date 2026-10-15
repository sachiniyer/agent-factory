package tmux

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

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
// survivors.
//
// The ownership home is likewise the SESSION's stamped AF_HOME when it
// carries one — af doctor runs this capture against sessions another home
// owns, and matching survivors by the invoker's home would silently drop
// exactly the processes the check exists to measure. Only an unmarked
// session falls back to the invoking home.
//
// A session with NO generation marker gets no legacy fallback here: unlike
// the sweep — which still holds the vanished session's captured members to
// compare against — this path may attribute a whole cohort to nothing. A
// detached child that stripped AF_SESSION is then unselectable by name and
// by cohort, so an empty successful scan would still authorize the delete
// (#5319 review).
//
// deadPaneSurvivorScan is the residue one marker attribution leaves behind:
// the validated survivors plus every input the post-kill pass needs to repeat
// the discovery without asking tmux — the session's own markers are gone by
// then, so the cohort, name, home and server pid ride along (#5319 review).
type deadPaneSurvivorScan struct {
	procs        []proctree.Process
	candidates   []proctree.Process
	markerName   string
	generations  orphanGenerationSet
	scanHome     string
	serverPID    int
	treeVerified map[int]bool
}

// secondPass repeats discovery after kill-session, while the marked parents
// are still alive to be expanded from: a child a marker-attributed survivor
// forked between the last capture refresh and the kill — during validation,
// the server-child audit, or the kill itself — lands here while its PPID link
// still exists. It mirrors the vanished sweep's second refresh/validate pass;
// the caller merges the additions into the reap set and joins any refusal
// into the capture error (#5319 review).
func (s *deadPaneSurvivorScan) secondPass(name string) ([]proctree.Process, error) {
	observed, observeErr := observeOrphanAncestryObserved(s.candidates, name, reapGraceWait)
	refreshed, refreshErr := refreshOrphanCandidates(observed, s.markerName, &s.generations, true)
	marked, inspectErr := markedOrphanProcesses(refreshed, s.markerName, s.scanHome, s.generations, s.treeVerified, s.serverPID)
	return marked, errors.Join(observeErr, refreshErr, inspectErr)
}

// admitRequesters is true because this capture feeds reapSessionProcesses
// directly: a marked survivor that is itself the teardown requester must stay
// in the set so SignalUnlessTracked can exempt it per signal tier and reap it
// once the exemption lapses — dropping it at discovery would leak it forever.
func markedDeadPaneSurvivors(cmdExec cmd.Executor, snap map[int]proctree.Process,
	sanitizedName string, row paneRow, captured []proctree.Process, verifiedOnly bool,
) (*deadPaneSurvivorScan, error) {
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
		// A session launched before AF_SESSION_GEN existed has no second
		// attribution key: a detached child that execs with AF_SESSION
		// stripped is then unselectable by name AND by cohort — reparented
		// away from the pane root it is invisible to every check here, so an
		// empty successful scan would still authorize the delete (#5319
		// review).
		return nil, fmt.Errorf("session %s carries no %s marker, so a pid-less dead pane's survivors cannot be attributed",
			sanitizedName, EnvMarkerGeneration)
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
	scanErr := refreshErr
	if !verifiedOnly {
		// A marker-attributed survivor ignores SIGHUP and keeps running — and
		// can keep FORKING between this capture and the reap that consumes it,
		// so the static set is never the whole story. Observe its ancestry
		// through the bounded grace the vanished sweep uses, then refresh once
		// more so a child forked mid-window or a marker that appeared late
		// still reaches validation before the capture is called conclusive
		// (#5319 review).
		var observed []proctree.Process
		var observeErr error
		observed, observeErr = observeOrphanAncestryObserved(refreshed, sanitizedName, reapGraceWait)
		refreshed, refreshErr = refreshOrphanCandidates(observed, markerName, &generations, true)
		scanErr = errors.Join(scanErr, observeErr, refreshErr)
	}
	// Members the pane tree captured moments ago are already proven; a marked
	// one that lands on the teardown's own ancestry (a daemon auto-started by
	// a client inside the pane) must not fail the scan it was never meant to
	// need — the self-chain refusal only guards marker-only matches.
	treeVerified := make(map[int]bool, len(captured))
	for _, member := range captured {
		treeVerified[member.PID] = true
	}
	marked, inspectErr := markedOrphanProcesses(refreshed, markerName, scanHome, generations, treeVerified, serverPID)
	scanErr = errors.Join(scanErr, inspectErr)
	// A pane root still parented to this session's server but running under a
	// different uid is invisible to the marker scan: its environment cannot be
	// read, so refreshOrphanCandidates skips it and validation never sees it.
	// With the server pid known, every direct child of that server is
	// enumerable; one the marker scan cannot explain — unreadable, or readable
	// but carrying nothing that proves another owner — is unprovable, never
	// absent (#5319 review).
	if serverErr == nil && serverPID > 0 {
		auditErr := auditServerChildren(cmdExec, refreshed, serverPID, markerName, scanHome)
		scanErr = errors.Join(scanErr, auditErr)
	}
	if serverErr != nil {
		scanErr = errors.Join(scanErr, fmt.Errorf(
			"cannot name the server owning a dead pane for self-chain verification: %w", serverErr))
	}
	scan := &deadPaneSurvivorScan{
		candidates: refreshed, markerName: markerName, generations: generations,
		scanHome: scanHome, serverPID: serverPID, treeVerified: treeVerified,
	}
	if !verifiedOnly {
		scan.procs = marked
		return scan, scanErr
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
	scan.procs = verified
	return scan, scanErr
}

// auditServerChildren accounts for every direct child of this session's tmux
// server that the marker candidate set does not already cover. A child that is
// a LIVE pane root of some session — this one or another af/plain session on
// the shared default socket — is explained by list-panes and skipped. The rest
// are detached processes still parented to the server: if their environment
// cannot be read, or can be read but carries nothing proving another owner,
// they are unattributable and the capture is incomplete. A child that
// definitively exited between the snapshot and the probe is churn, not
// evidence (#5319 review).
func auditServerChildren(cmdExec cmd.Executor, refreshed []proctree.Process, serverPID int,
	markerName, scanHome string) error {
	fresh, snapErr := proctree.Snapshot()
	if snapErr != nil {
		return fmt.Errorf("cannot enumerate children of tmux server %d for a pid-less dead pane: %w",
			serverPID, snapErr)
	}
	var auditErr error
	liveRoots, rootsErr := serverPaneRoots(cmdExec)
	if rootsErr != nil {
		auditErr = errors.Join(auditErr, fmt.Errorf(
			"cannot list live pane roots on tmux server %d: %w", serverPID, rootsErr))
	}
	inSet := make(map[int]proctree.Process, len(refreshed))
	for _, candidate := range refreshed {
		inSet[candidate.PID] = candidate
	}
	for _, process := range fresh {
		if process.PPID != serverPID {
			continue
		}
		if liveRoots[process.PID] {
			continue
		}
		if known, exists := inSet[process.PID]; exists && known.StartID == process.StartID {
			continue
		}
		environ, envErr := proctree.Environ(process.PID)
		if envErr != nil {
			// Ordinary pane-root churn on a shared server exits between the
			// snapshot and the read; suppress only a definitive exit (#5319).
			if _, lookupErr := proctree.Lookup(process.PID); errors.Is(lookupErr, proctree.ErrProcessExited) ||
				errors.Is(lookupErr, os.ErrNotExist) || errors.Is(lookupErr, syscall.ESRCH) {
				continue
			}
			auditErr = errors.Join(auditErr, fmt.Errorf(
				"direct child pid %d of tmux server %d has an unreadable environment, so a pid-less dead pane's survivors cannot be fully attributed: %w",
				process.PID, serverPID, envErr))
			continue
		}
		if serverChildProvenForeign(environ, markerName, scanHome) {
			continue
		}
		auditErr = errors.Join(auditErr, fmt.Errorf(
			"direct child pid %d of tmux server %d is not a live pane root and its environment proves no other owner, so a pid-less dead pane's survivors cannot be fully attributed",
			process.PID, serverPID))
	}
	return auditErr
}

// serverPaneRoots lists the live pane roots across every session on the tmux
// server — pane_pid is the first field of this codebase's row format, so the
// same read serves a bare -F '#{pane_pid}' answer and a full row alike.
func serverPaneRoots(cmdExec cmd.Executor) (map[int]bool, error) {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	out, err := outputTmuxBoundedWith(ctx, cmdExec, "list-panes", "-a", "-F", "#{pane_pid}")
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: list-panes -a after %s", ErrTmuxTimeout, tmuxCommandTimeout)
		}
		return nil, err
	}
	roots := map[int]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		field, _, _ := strings.Cut(line, "|")
		if pid, err := strconv.Atoi(strings.TrimSpace(field)); err == nil && pid > 0 {
			roots[pid] = true
		}
	}
	return roots, nil
}

// serverChildProvenForeign reports whether a server's detached child's
// environment actively attributes it to someone else: a different session
// name, or a different nonempty ownership home. Anything weaker — no markers,
// partial markers, or this session's own name — proves nothing (#5319 review).
func serverChildProvenForeign(environ []string, markerName, scanHome string) bool {
	if session, ok := processEnvValue(environ, EnvMarkerSession); ok && session != "" && session != markerName {
		return true
	}
	if home, ok := processEnvValue(environ, EnvMarkerHome); ok && home != "" &&
		filepath.Clean(home) != filepath.Clean(scanHome) {
		return true
	}
	return false
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

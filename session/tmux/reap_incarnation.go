package tmux

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/log"
)

// IncarnationProof is the durable, pre-spawn evidence a caller holds about a
// session it believes it created and wants to stop (#6258). Every field must be
// set: an empty one proves nothing, so the session is left alone.
type IncarnationProof struct {
	// SessionName is the sanitized tmux session name (AF_SESSION).
	SessionName string
	// Home is the agent-factory home that owns the session (AF_HOME).
	Home string
	// InstanceID is the instance id of the create attempt, recorded before the
	// spawn and stamped into the session at new-session (AF_INSTANCE_ID).
	InstanceID string
}

// IncarnationReap is what ReapProvenIncarnation established.
type IncarnationReap struct {
	// Stopped: the proven incarnation was killed, or its id was already gone by
	// the time the kill ran. Nothing of it remains to write into a workspace.
	Stopped bool
	// Why, when !Stopped, names the condition that kept the session alone:
	// unproven, replaced, unreadable, or unanswered. Never empty then.
	Why string
}

// ReapProvenIncarnation stops the tmux session answering at proof.SessionName
// ONLY if that exact incarnation proves it was spawned by the attempt proof
// names, and kills it by its tmux session id.
//
// A name and a clock cannot prove creation: any af session in this install can
// carry the same name, and a later creation time is exactly what a concurrent
// create has. The proof here is a value the attempt recorded durably BEFORE it
// spawned, which only that attempt's launch options carry into new-session:
//
//  1. Resolve the incarnation once: its "$N" id, server pid and creation stamp.
//  2. Read AF_INSTANCE_ID, AF_HOME and AF_SESSION from THAT id's environment,
//     not from the name — the name can be re-created between two commands.
//  3. Re-check the id still resolves to the same server and creation stamp, so a
//     replacement server that reissued "$N" is not mistaken for it.
//  4. Kill it with ONE server-side command that re-checks the server pid and
//     creation stamp and kills only if they still match (killVerifiedIncarnation).
//     The re-check in step 3 is a client-side read; between it and the kill the
//     server can be replaced and reissue the same "$N", so the check and the act
//     must be one command the server runs atomically.
//
// Every uncertainty leaves the session alone: no marker (a session from before
// the marker existed, or tmux < 3.2 which cannot stamp one), an unreadable
// environment, a probe that did not answer, a mismatch, a replaced incarnation.
// "Unknown" is never "ours".
func ReapProvenIncarnation(cmdExec cmd.Executor, program string, proof IncarnationProof) IncarnationReap {
	if proof.SessionName == "" || proof.Home == "" || proof.InstanceID == "" {
		return IncarnationReap{Why: "the fire recorded no instance id before its spawn, so no session can be proven to be its own"}
	}
	t := NewTmuxSessionFromSanitizedName(proof.SessionName, program)
	t.cmdExec = cmdExec

	gen, answered := t.confirmedGeneration()
	if !answered {
		return IncarnationReap{Why: fmt.Sprintf("tmux did not answer while resolving session %q", proof.SessionName)}
	}
	if gen == nil {
		// Resolved to nothing provable: the session vanished after it was listed,
		// or tmux answered with a failure that is not "absent". Either way there is
		// no incarnation to prove, and nothing may be killed by name instead.
		return IncarnationReap{Why: fmt.Sprintf("session %q did not resolve to a tmux session id", proof.SessionName)}
	}

	for _, want := range []struct{ marker, value string }{
		{EnvMarkerInstance, proof.InstanceID},
		{EnvMarkerHome, proof.Home},
		{EnvMarkerSession, proof.SessionName},
	} {
		got, present, err := sessionEnvMarkerAt(cmdExec, gen.sessionID, proof.SessionName, want.marker)
		switch {
		case err != nil:
			if same, known, gerr := t.confirmIncarnation(gen); gerr == nil && known && !same {
				return IncarnationReap{Why: fmt.Sprintf("session %q was replaced after it was probed", proof.SessionName)}
			}
			return IncarnationReap{Why: fmt.Sprintf("could not read %s of session %q: %v", want.marker, proof.SessionName, err)}
		case !present:
			return IncarnationReap{Why: fmt.Sprintf("session %q carries no %s, so it cannot be proven to be this fire's", proof.SessionName, want.marker)}
		case got != want.value:
			return IncarnationReap{Why: fmt.Sprintf("session %q carries a different %s, so it was not created by this fire", proof.SessionName, want.marker)}
		}
	}

	same, known, err := t.confirmIncarnation(gen)
	if err != nil || !known {
		return IncarnationReap{Why: fmt.Sprintf("could not re-confirm session %q before stopping it: %v", proof.SessionName, err)}
	}
	if !same {
		return IncarnationReap{Why: fmt.Sprintf("session %q was replaced after it was probed", proof.SessionName)}
	}

	return t.killVerifiedIncarnation(gen)
}

// Seams for tests: the pane-row parse needs a real tmux server's process tree, and
// the reap signals real processes. Production uses the shared implementations.
var (
	incarnationPaneProcesses = paneRowsToProcesses
	incarnationReapProcesses = reapSessionProcesses
)

// Markers the guarded kill prints, so the client can tell which branch the server
// executed. Only reapFiredMarker counts as a kill.
const (
	reapFiredMarker   = "af-reap-fired"
	reapSkippedMarker = "af-reap-skipped"
)

func digitsOnly(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// guardedKillArgs builds the one tmux command that kills the incarnation iff, when
// the SERVER runs it, #{pid} and #{session_created} still equal the verified
// values. if-shell -F evaluates its condition as a format in the server, then runs
// exactly one branch; the true branch prints reapFiredMarker before kill-session
// (a failing kill-session fails the whole command, so exit 0 plus the marker means
// the kill ran), the false branch prints reapSkippedMarker and kills nothing. The
// id is single-quoted inside the branches because tmux would otherwise expand
// "$N" as an environment variable.
func guardedKillArgs(gen *tmuxGeneration) []string {
	id := gen.sessionID
	// #{session_id} is compared FIRST: a target tmux did not resolve to $N itself
	// (display-message on a vanished id succeeds with empty output rather than
	// failing, so a conditional could be evaluated against some other session)
	// can never satisfy the condition.
	cond := fmt.Sprintf("#{&&:#{==:#{session_id},%s},#{&&:#{==:#{pid},%s},#{==:#{session_created},%s}}}", id, gen.serverPID, gen.created)
	return []string{"if-shell", "-F", "-t", id, cond,
		fmt.Sprintf("display-message -p -t '%s' %s ; kill-session -t '%s'", id, reapFiredMarker, id),
		fmt.Sprintf("display-message -p -t '%s' %s", id, reapSkippedMarker)}
}

// guardedPaneListingFormat prefixes each list-panes row with the session id, server
// pid and session_created the SERVER evaluated while answering. A row whose prefix is not
// the verified pair did not come from the verified incarnation, so the pids reaped
// later are only ever ones captured from it.
func guardedPaneListingFormat() string {
	return "#{session_id}" + paneFieldSeparator + "#{pid}" + paneFieldSeparator + "#{session_created}" + paneFieldSeparator + paneRowFormat
}

// killVerifiedIncarnation stops the verified incarnation, atomically, and reaps
// the processes captured from it only after the kill is confirmed to have fired.
//
// Every uncertainty leaves the session alone and reaps nothing: a pinned id or
// prefix that is not numeric, an unanswered or failing command that is not proved
// "gone", a skipped branch, a replaced server.
func (t *TmuxSession) killVerifiedIncarnation(gen *tmuxGeneration) IncarnationReap {
	name := t.sanitizedName
	if !digitsOnly(gen.serverPID) || !digitsOnly(gen.created) || !strings.HasPrefix(gen.sessionID, "$") || !digitsOnly(strings.TrimPrefix(gen.sessionID, "$")) {
		return IncarnationReap{Why: fmt.Sprintf("session %q resolved to an identity that cannot be pinned (%s %s %s)", name, gen.sessionID, gen.serverPID, gen.created)}
	}
	r := &closeRun{t: t}

	// Capture the pane process trees BEFORE the kill (afterwards survivors are
	// reparented and the ancestry is gone), from the verified incarnation only.
	leaked, captureNote := t.captureVerifiedTrees(r, gen)

	out, err := r.output(guardedKillArgs(gen)...)
	if err != nil {
		if r.unknown {
			return IncarnationReap{Why: fmt.Sprintf("tmux did not answer while stopping session %q; nothing was reaped: %v", name, err)}
		}
		if sessionIDProvedAbsent(t.cmdExec, err, gen.sessionID) {
			return IncarnationReap{Why: fmt.Sprintf("session %q vanished before it could be stopped; nothing was reaped", name)}
		}
		return IncarnationReap{Why: fmt.Sprintf("stopping session %q failed and its state is unknown; nothing was reaped: %v", name, err)}
	}
	switch strings.TrimSpace(string(out)) {
	case reapFiredMarker:
	case reapSkippedMarker:
		return IncarnationReap{Why: fmt.Sprintf("session %q was replaced after it was verified (server pid or creation stamp changed); nothing was killed or reaped", name)}
	default:
		return IncarnationReap{Why: fmt.Sprintf("tmux answered the guarded kill of session %q with %q, which does not confirm the kill; nothing was reaped", name, strings.TrimSpace(string(out)))}
	}

	// The kill fired on the verified incarnation: the pids captured from it are its
	// own and may now be reaped, exactly as an ordinary Close does.
	if captureNote != "" {
		log.WarningLog.Printf("tmux session %s: stopped; %s", name, captureNote)
	}
	if len(leaked) > 0 {
		// Synchronous: the boot reconcile that calls this is not latency-sensitive,
		// and a caller told "stopped" should find the pane's processes stopped too.
		incarnationReapProcesses(reapOnRequest, name, leaked, reapGraceWait, reapTermWait)
	}
	return IncarnationReap{Stopped: true}
}

// captureVerifiedTrees captures the pane process trees of the incarnation, keeping
// only rows the server itself stamped with the verified pid and creation stamp.
// A listing it cannot verify, or any failure, yields no processes and a note: the
// kill that follows is independently guarded, and nothing unverified is reaped.
func (t *TmuxSession) captureVerifiedTrees(r *closeRun, gen *tmuxGeneration) ([]proctree.Process, string) {
	out, err := r.output("list-panes", "-s", "-t", gen.sessionID, "-F", guardedPaneListingFormat())
	if err != nil {
		return nil, fmt.Sprintf("its panes could not be listed (%v), so no processes will be reaped", err)
	}
	prefix := gen.sessionID + paneFieldSeparator + gen.serverPID + paneFieldSeparator + gen.created + paneFieldSeparator
	var rows []string
	for _, field := range strings.Fields(string(out)) {
		row, ok := strings.CutPrefix(field, prefix)
		if !ok {
			return nil, "a pane row was not stamped with the verified server pid and creation stamp, so no processes will be reaped"
		}
		rows = append(rows, row)
	}
	procs, err := incarnationPaneProcesses(strings.Join(rows, " "), t.sanitizedName)
	if err != nil {
		return procs, fmt.Sprintf("the pane process capture was incomplete (%v)", err)
	}
	return procs, ""
}

// confirmIncarnation re-reads the incarnation's identity by its id and demands the
// answer prove it was evaluated against that id: the reply carries #{session_id}
// and it must equal gen.sessionID exactly. tmux's display-message does not fail for
// a vanished id — it exits 0 with empty output (measured, CI tmux 3.4) — so an
// empty, unparseable or differently-identified answer is UNKNOWN, never a match.
// same is true only for the verified id, server pid and creation stamp together.
func (t *TmuxSession) confirmIncarnation(gen *tmuxGeneration) (same bool, known bool, err error) {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	out, runErr := t.outputTmuxBounded(ctx, "display-message", "-p", "-t", gen.sessionID,
		"#{session_id} #{pid} #{session_created}")
	if runErr != nil {
		if ctx.Err() != nil {
			return false, false, fmt.Errorf("%w: display-message after %s", ErrTmuxTimeout, tmuxCommandTimeout)
		}
		return false, false, fmt.Errorf("identity probe for %s did not answer%s: %w", gen.sessionID, tmuxDiagnosticSuffix(runErr), runErr)
	}
	f := strings.Fields(strings.TrimSpace(string(out)))
	if len(f) != 3 {
		return false, false, fmt.Errorf("identity probe for %s returned %q, which does not prove it was evaluated against that session", gen.sessionID, strings.TrimSpace(string(out)))
	}
	if f[0] != gen.sessionID {
		return false, false, fmt.Errorf("identity probe for %s answered for %s", gen.sessionID, f[0])
	}
	return f[1] == gen.serverPID && f[2] == gen.created, true, nil
}

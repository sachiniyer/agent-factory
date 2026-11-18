package tmux

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/cmd"
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
	return reapProvenSession(cmdExec, program, proof.SessionName, func(t *TmuxSession, gen *tmuxGeneration) string {
		for _, want := range []struct{ marker, value string }{
			{EnvMarkerInstance, proof.InstanceID},
			{EnvMarkerHome, proof.Home},
			{EnvMarkerSession, proof.SessionName},
		} {
			got, present, err := sessionEnvMarkerAt(cmdExec, gen.sessionID, proof.SessionName, want.marker, "")
			switch {
			case err != nil:
				return markerReadRefusal(t, gen, proof.SessionName, want.marker, err)
			case !present:
				return fmt.Sprintf("session %q carries no %s, so it cannot be proven to be this fire's", proof.SessionName, want.marker)
			case got != want.value:
				return fmt.Sprintf("session %q carries a different %s, so it was not created by this fire", proof.SessionName, want.marker)
			}
		}
		return ""
	})
}

// reapProvenSession is the guarded-kill spine both proven-session reapers
// share: resolve the exact incarnation answering at sanitizedName once (its
// "$N" id, server pid and creation stamp), let check prove that incarnation's
// heritage from ITS OWN session environment, re-confirm the same incarnation
// is still answering, then kill it through the atomic server-side re-check
// (killVerifiedIncarnation). check returns "" when the incarnation is proven
// and the refusal to record otherwise.
func reapProvenSession(cmdExec cmd.Executor, program, sanitizedName string, check func(t *TmuxSession, gen *tmuxGeneration) string) IncarnationReap {
	t := NewTmuxSessionFromSanitizedName(sanitizedName, program)
	t.cmdExec = cmdExec

	gen, answered := t.confirmedGeneration()
	if !answered {
		return IncarnationReap{Why: fmt.Sprintf("tmux did not answer while resolving session %q", sanitizedName)}
	}
	if gen == nil {
		// Resolved to nothing provable: the session vanished after it was listed,
		// or tmux answered with a failure that is not "absent". Either way there is
		// no incarnation to prove, and nothing may be killed by name instead.
		return IncarnationReap{Why: fmt.Sprintf("session %q did not resolve to a tmux session id", sanitizedName)}
	}

	if why := check(t, gen); why != "" {
		return IncarnationReap{Why: why}
	}

	same, known, err := t.confirmIncarnation(gen)
	if err != nil || !known {
		return IncarnationReap{Why: fmt.Sprintf("could not re-confirm session %q before stopping it: %v", sanitizedName, err)}
	}
	if !same {
		return IncarnationReap{Why: fmt.Sprintf("session %q was replaced after it was probed", sanitizedName)}
	}

	return t.killVerifiedIncarnation(gen)
}

// markerReadRefusal names the condition that left a marker read unanswered:
// the incarnation's provable replacement, when the session the id resolved to
// is already gone, or the bare read failure otherwise.
func markerReadRefusal(t *TmuxSession, gen *tmuxGeneration, sanitizedName, marker string, err error) string {
	if same, known, gerr := t.confirmIncarnation(gen); gerr == nil && known && !same {
		return fmt.Sprintf("session %q was replaced after it was probed", sanitizedName)
	}
	return fmt.Sprintf("could not read %s of session %q: %v", marker, sanitizedName, err)
}

// BareSessionProof is the durable ownership evidence for a daemon-owned bare
// tmux session — a config-agent or account-login pane, spawned with no
// instance id and so carrying no instance row (#7428). Every field must be
// set: an empty one proves nothing, so the session is left alone.
type BareSessionProof struct {
	// SessionName is the sanitized tmux session name (AF_SESSION).
	SessionName string
	// Home is the agent-factory home that owns the session (AF_HOME).
	Home string
}

// ReapProvenBareSession stops the tmux session answering at proof.SessionName
// ONLY if that exact incarnation proves to be a bare pane this home's daemon
// spawned — and kills it by its tmux session id, through the same
// reapProvenSession spine ReapProvenIncarnation uses.
//
// Where an incarnation's heritage is a journaled instance id, a bare pane's is
// the marker set itself: a bare pane is an af session with no instance
// identity BY CONSTRUCTION — sessionEnvFlags stamps AF_INSTANCE_ID only when
// the launch carries a minted id, which only journaled creates do
// (envmarker.go). The set a bare pane provably carries is therefore:
//
//   - AF_SESSION == the name (the same proof an incarnation's marker loop
//     applies);
//   - AF_HOME == this home — classifyHomeDir: exact literal equality, or both paths
//     stat to the same file. A path that only Cleans to the home must NOT be
//     accepted: filepath.Clean resolves ".." lexically, but the filesystem
//     resolves it against a real directory, so a foreign home whose path
//     merely cleans to this one was admitted when the compare was lexical
//     (#7433 review). A marker that does not stat is unproven;
//   - AF_SESSION_GEN present — heritage from a post-marker binary, so a
//     same-named session from before the marker set existed is excluded;
//   - AF_INSTANCE_ID ABSENT — the bare-pane discriminator: a same-shaped
//     session carrying one is an instance spawn, the pending-creates sweep's
//     class, not this one's.
//
// The reads come from the resolved incarnation's SESSION environment only —
// `show-environment -t $N`, the same scope `new-session -e` stamped. Never the
// global environment (`-g`): tmux copies the first starter's environment into
// the server's global environment, so an unrelated session on a shared server
// can APPEAR to carry this home's markers when the read is not session-scoped
// (#6846 review). Never a pane process's inherited environ either — the same
// untrusted channel.
//
// Every uncertainty leaves the session alone: no markers (a session from
// before the marker set, or tmux < 3.2 which cannot stamp one), an unreadable
// environment, a probe that did not answer, a mismatch, a carried
// AF_INSTANCE_ID, a replaced incarnation. "Unknown" is never "ours".
func ReapProvenBareSession(cmdExec cmd.Executor, program string, proof BareSessionProof) IncarnationReap {
	if proof.SessionName == "" || proof.Home == "" {
		return IncarnationReap{Why: "the proof carries no name or home, so no session can be proven to be a daemon-owned bare pane"}
	}
	return reapProvenSession(cmdExec, program, proof.SessionName, func(t *TmuxSession, gen *tmuxGeneration) string {
		read := func(marker string) (got string, present bool, refusal string) {
			got, present, err := sessionEnvMarkerAt(cmdExec, gen.sessionID, proof.SessionName, marker, "")
			if err != nil {
				return "", false, markerReadRefusal(t, gen, proof.SessionName, marker, err)
			}
			return got, present, ""
		}
		got, present, refusal := read(EnvMarkerSession)
		switch {
		case refusal != "":
			return refusal
		case !present:
			return fmt.Sprintf("session %q carries no %s, so it cannot be proven to be this daemon's bare pane", proof.SessionName, EnvMarkerSession)
		case got != proof.SessionName:
			return fmt.Sprintf("session %q carries a different %s, so it was not spawned under this name", proof.SessionName, EnvMarkerSession)
		}
		got, present, refusal = read(EnvMarkerHome)
		switch {
		case refusal != "":
			return refusal
		case !present:
			return fmt.Sprintf("session %q carries no %s, so its owning home cannot be proven (pre-marker build or tmux <3.2)", proof.SessionName, EnvMarkerHome)
		case classifyHomeDir(got, proof.Home) == homeForeign:
			return fmt.Sprintf("session %q belongs to a different agent-factory home (%s)", proof.SessionName, got)
		case classifyHomeDir(got, proof.Home) != homeSame:
			return fmt.Sprintf("session %q carries a home marker (%s) whose ownership cannot be proven", proof.SessionName, got)
		}
		if _, present, refusal := read(EnvMarkerGeneration); refusal != "" {
			return refusal
		} else if !present {
			return fmt.Sprintf("session %q carries no %s, so it predates the marker set and cannot be proven a daemon-owned bare pane", proof.SessionName, EnvMarkerGeneration)
		}
		if _, present, refusal := read(EnvMarkerInstance); refusal != "" {
			return refusal
		} else if present {
			return fmt.Sprintf("session %q carries an %s, so it is an instance create, not a bare pane", proof.SessionName, EnvMarkerInstance)
		}
		return ""
	})
}

// homeMatch is what comparing a carried home marker against this daemon's
// home could prove: the same directory, a different one, or — the third
// answer — nothing at all. homeUnproven is deliberately the zero value so an
// answer never filled in can never pass for same or foreign (#7435).
type homeMatch int

const (
	homeUnproven homeMatch = iota
	homeForeign
	homeSame
)

// classifyHomeDir classifies the directory a names against the home b:
// homeSame for an exact literal match or when both stat and os.SameFile finds
// them the same file; homeForeign for two paths that both stat to different
// files; homeUnproven for everything else — a relative marker, a path that
// does not exist, a path that cannot be stat'd. Unproven is never "same" on a
// kill path (#7433 review): filepath.Clean is NOT a substitute, because ".."
// in a marker resolves lexically while the filesystem resolves it against a
// real directory, so a path that only Cleans to the home can be a different
// home.
//
// Both sides must be absolute BEFORE either comparison runs (#7433 Codex):
// a relative path is anchored to the resolving process's working directory —
// afHomeDir stamps AGENT_FACTORY_HOME unchanged, so two daemons launched from
// different directories with AF_HOME=.af carry markers that match literally
// while naming different installs, and the marker's own cwd is unrecoverable.
// doctor/tmux_sessions.go classifies the same shape as unproven ownership.
func classifyHomeDir(a, b string) homeMatch {
	if !filepath.IsAbs(a) || !filepath.IsAbs(b) {
		return homeUnproven
	}
	if a == b {
		return homeSame
	}
	ai, err := os.Stat(a)
	if err != nil {
		return homeUnproven
	}
	bi, err := os.Stat(b)
	if err != nil {
		return homeUnproven
	}
	if os.SameFile(ai, bi) {
		return homeSame
	}
	return homeForeign
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
	// Protection is computed from the SAME snapshot the capture used, at the
	// same pre-kill point: a pane shell or CLI dying on the kill reparents a
	// daemon spawned inside this pane out from under the server, and protection
	// decided from a later table is decided from the wrong one (Codex on #7433,
	// #7435 item 3).
	captured, captureNote := t.captureVerifiedTrees(r, gen)
	protected := reapProtectedSet(captured.Snapshot, captured.Procs, nil)

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
	if len(captured.Procs) > 0 {
		// Synchronous: the boot reconcile that calls this is not latency-sensitive,
		// and a caller told "stopped" should find the pane's processes stopped too.
		incarnationReapProcesses(reapOnRequest, name, captured.Procs, reapGraceWait, reapTermWait, protected, nil)
	}
	return IncarnationReap{Stopped: true}
}

// captureVerifiedTrees captures the pane process trees of the incarnation, keeping
// only rows the server itself stamped with the verified pid and creation stamp.
// A listing it cannot verify, or any failure, yields no processes and a note: the
// kill that follows is independently guarded, and nothing unverified is reaped.
// The whole paneCapture is returned so the caller computes protection from the
// same snapshot the process list came from (#7435 item 3).
func (t *TmuxSession) captureVerifiedTrees(r *closeRun, gen *tmuxGeneration) (paneCapture, string) {
	out, err := r.output("list-panes", "-s", "-t", gen.sessionID, "-F", guardedPaneListingFormat())
	if err != nil {
		return paneCapture{}, fmt.Sprintf("its panes could not be listed (%v), so no processes will be reaped", err)
	}
	prefix := gen.sessionID + paneFieldSeparator + gen.serverPID + paneFieldSeparator + gen.created + paneFieldSeparator
	var rows []string
	for _, field := range splitPaneRows(string(out)) {
		row, ok := strings.CutPrefix(field, prefix)
		if !ok {
			return paneCapture{}, "a pane row was not stamped with the verified server pid and creation stamp, so no processes will be reaped"
		}
		rows = append(rows, row)
	}
	captured, err := incarnationPaneProcesses(strings.Join(rows, "\n"), t.sanitizedName)
	if err != nil {
		return captured, fmt.Sprintf("the pane process capture was incomplete (%v)", err)
	}
	return captured, ""
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

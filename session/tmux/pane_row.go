package tmux

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// paneFieldSeparator keeps pane fields positional. tmux prints an unset field as
// nothing, so a whitespace split would shift later fields left. Every field is
// numeric, a pty name, or a predicate digit — none can contain the separator.
const paneFieldSeparator = "|"

// paneRowFormat is what the teardown asks list-panes and display-message for.
// The third field concatenates every value tmux reports only after reaping the
// root, so it is non-empty exactly when the root is known to be gone. The
// fourth carries the pane's @af_pane_pid identity stamp: tmux ≥3.8 empties
// pane_pid once the root exits (#5319), and the stamp is how a held dead pane
// still names its root. The last three fields are the stamp's freshness proof
// — the deaths counter, the live pty name, and the command pin — because a
// pane option outlives the pane incarnation that wrote it (see paneStampPID).
//
// A reply without the separator is a bare pane_pid, which is what a tmux that
// cannot expand the later fields, or a test executor, returns. It reads as a
// live pane, the behaviour from before #4479.
//
// The last two fields are the SERVER identity #7469 persists — server pid and
// session id — repeated identically on every pane row of a session, so any
// row stamps it. The SOCKET PATH is deliberately NOT a row field: it is
// free-form text (a TMUX_TMPDIR can hold spaces, commas, or the separator
// itself), and a free-form field in a listing's output can change its row
// count — a newline in the path prints raw and invents a pane that never
// existed (R5). Every site that needs the socket reads it once, as a single
// value, corroborated to the answering server pid (serverSocketPID).
const paneRowFormat = "#{pane_pid}" + paneFieldSeparator + "#{pane_dead}" + paneFieldSeparator +
	"#{pane_dead_status}#{pane_dead_signal}#{pane_dead_time}" + paneFieldSeparator + "#{" + PanePidMarker + "}" +
	paneFieldSeparator + "#{" + paneDeathsMarker + "}" + paneFieldSeparator + "#{pane_tty}" +
	paneFieldSeparator + paneCommandMatchField +
	paneFieldSeparator + "#{pid}" + paneFieldSeparator + "#{session_id}"

// paneCommandMatchField asks the server — not the row — whether the pane's
// recorded start command is still the exact command the stamp pinned: one
// digit, answered by == equality rather than a substring match, so a foreign
// respawn whose argv merely contains a launch marker still answers 0. The
// comparison runs server-side because the recorded command is free-form text
// that can contain both spaces and the field separator.
const paneCommandMatchField = "#{==:#{" + paneCommandMarker + "},#{pane_start_command}}"

// splitPaneRows cuts tmux's pane listing into its rows. Rows are newline-
// delimited, and a row is never whitespace-split first: a whitespace split
// would shift fields left (the stamp proof can carry spaces) and leave a
// fragment to parse as its own row (Codex on #7469). Empty lines are not
// rows.
func splitPaneRows(output string) []string {
	var rows []string
	for line := range strings.Lines(output) {
		if line = strings.TrimRight(line, "\r\n"); line != "" {
			rows = append(rows, line)
		}
	}
	return rows
}

// paneRow is one pane of a list-panes or display-message reply. server is the
// tmux server identity the row carried (#7469): pid, session id and socket
// ride the same answer, identical on every row of a session, so any row
// stamps it. A row that cannot fully answer them carries an empty identity —
// undecidable, never a wrong one.
type paneRow struct {
	pid int
	// dead: tmux holds the pane after its terminal closed.
	dead bool
	// reaped: tmux collected the root, so the pid no longer names it.
	reaped bool
	// server: the tmux server this pane's session belongs to.
	server ServerIdentity
}

func parsePaneRow(field string) (paneRow, error) {
	parts := strings.Split(strings.TrimSpace(field), paneFieldSeparator)
	row := paneRow{}
	if len(parts) > 1 {
		row.dead = strings.TrimSpace(parts[1]) == "1"
	}
	if len(parts) > 2 {
		row.reaped = row.dead && strings.TrimSpace(parts[2]) != ""
	}
	// The stamp is a fallback authority only, consulted when tmux itself can
	// no longer answer: a held dead pane on tmux ≥3.8 reports the pane_pid
	// FIELD EMPTY. A populated pane_pid is tmux's own maintained value and
	// always wins — after a respawn-pane the new root has a different pid
	// while the old stamp is simply stale — so the two are never compared
	// against each other, only substituted. A malformed pane_pid stays
	// malformed; only an empty one is the tmux ≥3.8 shape the stamp exists
	// to cover. And the stamp counts only while it still names the pane's
	// current incarnation — pane options outlive respawn-pane, so paneStampPID
	// proves the stamp was written by the run that most recently died.
	stampPID, verdict := 0, stampUnusable
	if len(parts) > 3 {
		var deaths, tty, cmdMatch string
		if len(parts) > 4 {
			deaths = strings.TrimSpace(parts[4])
		}
		if len(parts) > 5 {
			tty = strings.TrimSpace(parts[5])
		}
		if len(parts) > 6 {
			cmdMatch = strings.TrimSpace(parts[6])
		}
		stampPID, verdict = paneStampPID(strings.TrimSpace(parts[3]), deaths, tty, cmdMatch)
	}
	panePIDRaw := strings.TrimSpace(parts[0])
	pid, err := strconv.Atoi(panePIDRaw)
	switch {
	case err == nil:
		// pane_pid answered; the stamp is never consulted.
	case panePIDRaw == "" && verdict == stampFresh:
		pid = stampPID
	case panePIDRaw == "" && row.dead && verdict == stampPending:
		return paneRow{}, fmt.Errorf("dead pane carries a %s stamp for root %d whose incarnation proof is one counter short — tmux reports pane_dead before its queued pane-died hook increments the deaths counter, so the hook has not counted the death yet and the process tree cannot be proven — the workspace is left intact; the recorded kill is retried automatically once the session is gone from tmux, and `af sessions kill` re-run drives the same teardown to completion: %w", PanePidMarker, stampPID, errPaneStampPending)
	case panePIDRaw == "" && row.dead && verdict == stampStale:
		return paneRow{}, fmt.Errorf("dead pane carries a %s stamp for root %d from an earlier incarnation — the pane was respawned or its stamp write failed, so its process tree cannot be proven — the workspace is left intact; the recorded kill is retried automatically once the session is gone from tmux, and `af sessions kill` re-run drives the same teardown to completion", PanePidMarker, stampPID)
	case panePIDRaw == "" && row.dead && stampPID > 0:
		return paneRow{}, fmt.Errorf("dead pane carries a %s stamp for root %d that predates the incarnation proof, so its process tree cannot be proven — the workspace is left intact; the recorded kill is retried automatically once the session is gone from tmux, and `af sessions kill` re-run drives the same teardown to completion", PanePidMarker, stampPID)
	case panePIDRaw == "" && row.dead:
		return paneRow{}, fmt.Errorf("dead pane reports no pane_pid and no %s stamp; it predates pid stamping or was not created by af, so its process tree cannot be proven — the workspace is left intact; the recorded kill is retried automatically once the session is gone from tmux, and `af sessions kill` re-run drives the same teardown to completion", PanePidMarker)
	default:
		return paneRow{}, fmt.Errorf("invalid pane pid %q", field)
	}
	row.pid = pid
	row.server = parsePaneServerFields(parts)
	return row, nil
}

// parsePaneServerFields reads the server identity a pane row's tail carries
// (#7469): the server pid (field 7) and the session's $N id (field 8). The
// socket path is never a row field — free-form text has no place inside a
// listing's framing (R5) — so the record leaves here with Socket empty and
// the caller fills it from the server's own corroborated single-value
// answer (serverSocketPID). A tail that does not fully parse is an absent
// record, never a partial one.
func parsePaneServerFields(parts []string) ServerIdentity {
	if len(parts) < 9 {
		return ServerIdentity{}
	}
	serverPID, err := strconv.Atoi(strings.TrimSpace(parts[7]))
	if err != nil || serverPID <= 0 {
		return ServerIdentity{}
	}
	return ServerIdentity{
		PID:       serverPID,
		SessionID: strings.TrimSpace(parts[8]),
	}
}

// paneStampVerdict is paneStampPID's ruling on the stamp read from one row:
// how far the incarnation proof got, and specifically whether its only
// shortfall is a counter the queued pane-died hook has not run yet (#5335).
type paneStampVerdict int

const (
	// stampUnusable means the stamp names nobody: unset, malformed, or a
	// non-positive pid — the pane reads exactly like an unstamped one.
	stampUnusable paneStampVerdict = iota
	// stampLegacy means a readable stamp carrying no incarnation bundle at
	// all — a bare pid written before the proof fields existed. It cannot
	// name the current incarnation, but unlike a stale bundle nothing in it
	// positively contradicts the current run either. Evidence that does not
	// come from the stamp — tmux's own collected status — may stand beside
	// it; the stamped pid itself stays unusable, exactly like an unstamped
	// pane's.
	stampLegacy
	// stampStale means a readable stamp that cannot name the pane's current
	// incarnation: a deaths gap that is not 0-or-1, a moved pty, or a
	// replaced start command — a respawn, or a stamp write that never
	// landed.
	stampStale
	// stampPending means every freshness input matches except the deaths
	// counter, whose gap is exactly 0 — the pane-died hook has not counted
	// this death yet, not a stale record. tmux reports pane_dead before the
	// queued hook runs (#5335), so a reader that can wait re-reads the row
	// until the hook lands; one that cannot must treat the stamp as still
	// unproven, never as proven-false.
	stampPending
	// stampFresh means the stamp names the run that most recently died:
	// deaths gap exactly one, pty and command pins intact.
	stampFresh
)

// errPaneStampPending marks a dead-pane row whose stamp is provable on every
// input but the deaths counter — the pane-died hook has not run yet (#5335).
// panePID re-reads on this error instead of refusing; by the time it escapes,
// the wait deadline has passed and the message stands as the refusal.
var errPaneStampPending = errors.New("the pane-died hook has not counted the death")

// paneStampPID extracts the pane-root pid a @af_pane_pid stamp recorded —
// "<pid>:<deaths>:<pane_tty>" — and rules on whether it still names the
// pane's current incarnation. The pane option outlives the run that wrote it
// (respawn-pane keeps the pane struct and its options), so three proofs must
// all hold before the pid is trusted:
//
//   - deathsNow == stamped deaths + 1: the pane-died hook bumps the counter
//     on every natural root exit, so a stamp from an earlier run shows a
//     wider gap. A gap of exactly 0 is the one case that is NOT stale: the
//     hook runs on tmux's command queue and pane_dead is observable first
//     (#5335), so a fresh pane's own death can still be uncounted — that is
//     stampPending, and it resolves when the queued hook runs.
//   - stamp tty == the pane's reported tty: every respawn allocates a fresh
//     pty. The name is only corroborating evidence — `respawn-pane -k` frees
//     the devpts index before allocating the replacement and Linux can
//     reissue the same pathname to the new allocation (#5328 review), so a
//     matching name alone proves nothing. What normally makes the stale
//     record unreachable for a re-run is the shim's restamp-or-die
//     (sessionenv.stampPaneRootPid): a respawn re-executes the shim, which
//     either lands a fresh bundle or, when the option provably still names
//     an earlier run, exits without running the command. The accepted
//     residual is the shim that can neither write nor read the option at
//     all — manual `respawn-pane -e` sabotage of the pane env — which
//     launches anyway because a first launch must never die over
//     bookkeeping; a stale record that survives these pins is bounded to
//     the stamped session's own earlier run.
//   - cmdMatch == "1": the pane's recorded start command still byte-equals
//     the command the stamp pinned, so a replacement respawn — even one
//     whose argv happens to contain a launch marker — fails it, while an
//     identical commandless re-run re-executed the shim and restamped.
//
// A stamp written before the incarnation proof existed — a bare pid with no
// second colon — is stampLegacy: it fails every proof it was never given
// rather than contradicting one, so readers choose how much that absence
// costs. A bundle that did set out to carry the proof and is malformed still
// fails closed as stale. A deaths field left empty at stamp time is the
// counter's unset state, i.e. zero.
func paneStampPID(stamp, deathsNow, ttyNow, cmdMatch string) (int, paneStampVerdict) {
	pidRaw, rest, hasRest := strings.Cut(stamp, ":")
	pid, err := strconv.Atoi(pidRaw)
	if err != nil || pid <= 0 {
		return 0, stampUnusable
	}
	if !hasRest {
		// A bare pid is a stamp written before the incarnation proof
		// existed: readable, but unprovable — it cannot name the current
		// incarnation, only fail the proofs it was never given. That is
		// weaker than a stale bundle, which DID carry the proof and
		// contradicted it.
		return pid, stampLegacy
	}
	deathsRaw, ttyRaw, ok := strings.Cut(rest, ":")
	if !ok {
		return pid, stampStale
	}
	stampedDeaths := 0
	if deathsRaw != "" {
		stampedDeaths, err = strconv.Atoi(deathsRaw)
		if err != nil || stampedDeaths < 0 {
			return pid, stampStale
		}
	}
	current := 0
	if deathsNow != "" {
		current, err = strconv.Atoi(deathsNow)
		if err != nil || current < 0 {
			return pid, stampStale
		}
	}
	pins := ttyRaw == ttyNow && cmdMatch == "1"
	switch gap := current - stampedDeaths; {
	case gap == 1 && pins:
		return pid, stampFresh
	case gap == 0 && pins:
		return pid, stampPending
	default:
		return pid, stampStale
	}
}

// serverFromPaneRows returns the server identity a pane listing carried —
// identical on every answered row (#7469), so the first answered one speaks
// for the session; a listing without server fields yields an empty identity
// and noteServerIdentity keeps whatever stands. The rows carry pid and
// session id only — the socket is never a row field (R5) — so the caller
// merges the corroborated single-value socket read before the record can
// stamp.
func serverFromPaneRows(fields []string) ServerIdentity {
	for _, field := range fields {
		if row, err := parsePaneRow(field); err == nil && row.server.PID > 0 {
			return row.server
		}
	}
	return ServerIdentity{}
}

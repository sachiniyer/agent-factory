package tmux

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

type statusMonitor struct {
	// Store hashes to save memory.
	prevOutputHash []byte
	// baselinePending makes the first successful capture after reattaching to an
	// existing process establish state without claiming the pane changed. Fresh
	// starts and respawns leave it false: their first output is genuinely new.
	baselinePending bool
	// dead is set once a capture-pane failure has been confirmed by
	// ExistsOrUnknown() reporting the tmux session is gone. While true,
	// HasUpdated short-circuits and emits no further logs so a stale
	// instance can't flood agent-factory.log (#489). A successful Start or
	// Restore replaces the monitor with a fresh one, which naturally clears
	// this state on respawn.
	dead bool
	// generation is the concrete tmux session this monitor polls, resolved
	// when a live session answered during restore — see tmuxGeneration. The
	// reused NAME cannot bind a generation: a poll that straddles a
	// same-object restart and lands on the replacement would consult this
	// monitor's teardown attribution against the new session's death, and a
	// $id alone cannot either, because tmux ids are per-server counters a
	// replacement server reissues (#4473 review). nil — or an id-less
	// generation — means unbound: the pre-binding name target, no worse
	// than before.
	// Written before the monitor is installed; read under monitorMu.
	generation *tmuxGeneration
}

// tmuxGeneration is one concrete tmux session as tmux can prove it: the
// session id ($N) plus the SERVER's process id and the session's creation
// stamp. The pair beyond the id matters because tmux ids are per-server
// counters — tearing down the last session exits the server and the next
// server reissues $0 — so a captured $id can name a foreign session after a
// server restart. serverPID and created together distinguish that reuse.
//
// The teardown mark lives HERE, not on the monitor: every monitor bound to
// the same generation shares af's attribution for ITS death. A pure rebind
// installs a fresh monitor for the same live generation, and a close()
// during an in-flight poll on the swapped-out monitor must still classify
// the shared generation's teardown as af-initiated (#4473 review).
// Fields are guarded by monitorMu.
type tmuxGeneration struct {
	sessionID string
	serverPID string
	created   string
	// teardownInitiated records that af itself asked for this generation's
	// teardown — set by close() before kill-session runs so the
	// ErrSessionGone branch can tell "af asked" (INFO) from "vanished on its
	// own" (ERROR) (#4472). It clears only on tmux ANSWERING that the SAME
	// generation is live — a live name is not proof, since the name may
	// already belong to a replacement — see the clear sites in close(),
	// Start, and ClosedConclusivelyAndStillAbsent — except on an unanswered
	// rebind, where the generation object itself is carried to the
	// replacement monitor because a wedged probe is no evidence the request
	// resolved.
	teardownInitiated bool
	// teardownSettledAt is when the asking close() RETURNED; zero while the
	// request is still running or when there is none. A successful capture
	// retires the mark only if the capture began after this stamp — liveness
	// proven before the request finished says nothing about its outcome, so
	// a poll that straddled the close cannot retire the mark just before the
	// kill lands (Codex on #4473).
	teardownSettledAt time.Time
}

// sameAs reports whether two resolved generations are the same concrete tmux
// session. An id-less generation (one created only to carry a mark on an
// unbound monitor) never matches: it describes no provable session.
func (g *tmuxGeneration) sameAs(o *tmuxGeneration) bool {
	return g != nil && o != nil && g.sessionID != "" &&
		g.sessionID == o.sessionID && g.serverPID == o.serverPID && g.created == o.created
}

func newStatusMonitor() *statusMonitor {
	return &statusMonitor{}
}

func newReattachStatusMonitor() *statusMonitor {
	return &statusMonitor{baselinePending: true}
}

// markTeardownInitiated records on the CURRENT monitor's generation that af
// asked for this session's teardown and returns the monitor it marked, so the
// asking close() can settle the same one. Marking must reach the generation
// the polls are reading — not a flag on the session object — or a mid-close
// monitor swap would leave the old generation's in-flight polls reading a
// cleared shared flag (Codex on #4473). Because the mark lives on the
// generation, a monitor installed later for the SAME live session shares it
// rather than restarting attribution from zero. A nil monitor means nothing
// is polling: there is no one to attribute a disappearance to. An unbound
// monitor gets an id-less generation to carry the mark — the pre-binding
// name-scoped behavior.
//
// A BOUND monitor is marked only while the name still resolves to its
// generation: kill-session targets the NAME, so when the name already answers
// for a different generation this request is aimed at the replacement, not at
// the bound session that vanished on its own — and marking the bound
// generation would launder that unrequested death into af's request (Codex on
// #4473). A name that does not resolve proves nothing either way, so the mark
// still lands: a server wedged enough to refuse the probe will fail the kill
// the same way, and the settled-mark path retires a teardown that did not
// take. The resolution cannot run under monitorMu — it is a tmux command with
// a deadline, exactly what the lock ordering forbids holding it across.
func (t *TmuxSession) markTeardownInitiated() *statusMonitor {
	t.monitorMu.Lock()
	mon := t.monitor
	t.monitorMu.Unlock()
	if mon == nil {
		return nil
	}
	if g := mon.generation; g != nil && g.sessionID != "" {
		if live := t.confirmedGeneration(); live != nil && !g.sameAs(live) {
			return nil
		}
	}
	t.monitorMu.Lock()
	defer t.monitorMu.Unlock()
	// A monitor swapped in while the resolution ran binds the generation its
	// own restore confirmed at the name — newer evidence than the answer
	// above — so the mark lands on whatever monitor is current now.
	mon = t.monitor
	if mon == nil {
		return nil
	}
	if mon.generation == nil {
		mon.generation = &tmuxGeneration{}
	}
	mon.generation.teardownInitiated = true
	mon.generation.teardownSettledAt = time.Time{}
	return mon
}

// settleTeardown stamps that the asking close() returned on the generation it
// marked — what lets a later post-settle successful poll retire a mark whose
// teardown demonstrably did not take. It lands on the marked generation
// itself, so a monitor swap mid-close cannot carry the settle onto the
// replacement.
func (t *TmuxSession) settleTeardown(mon *statusMonitor) {
	if mon == nil {
		return
	}
	t.monitorMu.Lock()
	if mon.generation != nil {
		mon.generation.teardownSettledAt = time.Now()
	}
	t.monitorMu.Unlock()
}

// hash hashes the string.
func (m *statusMonitor) hash(s string) []byte {
	h := sha256.New()
	h.Write([]byte(s))
	return h.Sum(nil)
}

// TapEnter injects a bare Enter into the pane via a clientless `send-keys`
// command (#1592 Phase 2 PR7). It replaces the old write of CR to the attach
// PTY: the tmux-server-mediated attach client is gone, so input now lands
// through a command exactly like the interactive multi-writer path. Used by
// the trust-prompt dismissal in CheckAndHandleTrustPrompt. A missing session
// surfaces ErrSessionGone so
// callers degrade gracefully instead of logging at ERROR (#510).
//
// Bounded by tmuxCommandTimeout (#2105): this runs on the daemon's unsupervised
// per-second poll, which walks instances SEQUENTIALLY, so an unbounded stall here
// freezes every later instance's status exactly like the capture it accompanies.
// On a tripped deadline it returns ErrTmuxTimeout without probing ExistsOrUnknown
// — see tmuxTimeoutContext.
func (t *TmuxSession) TapEnter() error {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	if err := t.runTmuxBounded(ctx, "send-keys", "-t", exactTarget(t.sanitizedName), "Enter"); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%w: send-keys Enter after %s", ErrTmuxTimeout, tmuxCommandTimeout)
		}
		if !t.ExistsOrUnknown() {
			return fmt.Errorf("%w: send-keys Enter", ErrSessionGone)
		}
		return fmt.Errorf("error sending enter keystroke: %w", err)
	}
	return nil
}

// TapDAndEnter injects 'D' then Enter (the non-Claude trust/doc-dialog
// dismissal) via a clientless `send-keys` command. "D" is the literal glyph and
// "Enter" the named key — semantically identical to the old ptmx write of
// {0x44, 0x0D}. Bounded by tmuxCommandTimeout for the same reason as TapEnter
// (#2105): it runs on the same unsupervised daemon poll.
func (t *TmuxSession) TapDAndEnter() error {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	if err := t.runTmuxBounded(ctx, "send-keys", "-t", exactTarget(t.sanitizedName), "D", "Enter"); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%w: send-keys D Enter after %s", ErrTmuxTimeout, tmuxCommandTimeout)
		}
		if !t.ExistsOrUnknown() {
			return fmt.Errorf("%w: send-keys D Enter", ErrSessionGone)
		}
		return fmt.Errorf("error sending D+enter keystroke: %w", err)
	}
	return nil
}

// HasUpdated checks if the tmux pane content has changed since the last tick. It also returns true if the tmux
// pane has a prompt for aider or claude code, plus the raw captured content so the daemon's usage-limit detector (#1146) can inspect it without a second capture ("" on early return).
func (t *TmuxSession) HasUpdated() (updated bool, hasPrompt bool, content string) {
	updated, hasPrompt, content, _ = t.HasUpdatedWithBaseline()
	return updated, hasPrompt, content
}

// HasUpdatedWithBaseline also reports the first successful capture after a
// reattach. That capture seeds comparison state: it proves neither pane churn
// nor idleness, so daemon observations must preserve the distinction.
func (t *TmuxSession) HasUpdatedWithBaseline() (updated bool, hasPrompt bool, content string, baseline bool) {
	// A nil monitor means Restore never ran for this session: a persisted Dead
	// instance is loaded with started=true but LocalBackend.Start returns before
	// Restore (which is the only place monitor is initialized) so the corpse is
	// not re-spawned (#970). The daemon's refreshInstanceStatus still polls every
	// started instance, so HasUpdated must treat "no live monitor" as
	// nothing-to-report rather than panic on a nil deref and kill the refresh
	// goroutine, zombifying the daemon (#999).
	//
	// Once the underlying tmux session has been confirmed gone, stay silent
	// instead of relogging capture-pane failures every daemon tick (#489).
	//
	// monitorMu guards the monitor pointer (swapped by Restore) and its
	// dead/prevOutputHash fields (mutated here) against the data race #1528
	// fixes. The lock is deliberately NOT held across CapturePaneContent: that
	// runs a `tmux capture-pane` which — even bounded by tmuxCommandTimeout since
	// #2105 — can still take that full deadline against a wedged server, and
	// blocking Restore's setMonitor on it would freeze detach/restore for as long
	// as the bound. So snapshot the live monitor under the lock, release it, run the
	// tmux command lock-free, then re-acquire only to update the monitor's
	// fields. Field writes land on the snapshotted monitor: if Restore swaps in
	// a fresh one meanwhile, the stale monitor is discarded and updating it is a
	// harmless no-op.
	t.monitorMu.Lock()
	mon := t.monitor
	alive := mon != nil && !mon.dead
	var gen *tmuxGeneration
	target := exactTarget(t.sanitizedName)
	if alive {
		gen = mon.generation
		if gen != nil && gen.sessionID != "" {
			target = gen.sessionID
		}
	}
	t.monitorMu.Unlock()
	if !alive {
		return false, false, "", false
	}

	captureStartedAt := time.Now()
	var err error
	// An id-bound monitor owes its liveness question to the GENERATION, not
	// to whatever session currently answers at the id: tmux ids are
	// per-server counters, so a replacement server can reissue the same $N
	// for an unrelated session — and capture-pane would then read that
	// session forever without ever noticing the polled generation died.
	// The identity probe asks the id's owner for the server pid and creation
	// stamp resolved at bind time; a mismatch IS the bound generation's
	// death. An unanswered probe means a wedged server — unknown, so the
	// poll ends on the probe's own error rather than doubling the budget on
	// a capture the server is just as unlikely to answer.
	if gen != nil && gen.sessionID != "" {
		switch same, known, probeErr := t.generationMatches(gen); {
		case known && !same:
			// Route through the canonical constructor: a hand-built
			// ErrSessionGone would report a bare disappearance even when af
			// answered a trust dialog moments before the pane vanished —
			// the context the error exists to carry (Codex on #4473).
			err = t.sessionGoneError("identity probe", fmt.Errorf("tmux session id %s no longer resolves to the bound generation", gen.sessionID))
		case !known:
			// The probe already spent this poll's tmux command budget
			// without an answer; capture-pane against the same server would
			// spend a SECOND one, and the daemon's sequential status loop
			// cannot afford 2×tmuxCommandTimeout per wedged session (Codex
			// on #4473). The probe's own error stands in for the capture's:
			// generationMatches never reports it as ErrSessionGone, so the
			// monitor stays retryable and the dead latch stays unset.
			err = probeErr
		}
	}
	if err == nil {
		content, err = t.capturePaneContentTarget(context.Background(), target, gen)
	}
	if err != nil {
		// If the tmux session no longer exists, log once and latch the
		// monitor as dead so the daemon's per-second poll doesn't spam
		// the log (#489). Transient capture-pane failures while the
		// session is still alive are rare and still surface every tick.
		// CapturePaneContent has already probed ExistsOrUnknown on the
		// error path, so use the wrapped sentinel rather than re-probing.
		if errors.Is(err, ErrSessionGone) {
			// af-initiated teardown (kill, archive, task completion, handoff
			// swap, root reap) routes through close(), which marks the
			// GENERATION before kill-session runs — that disappearance is the
			// request completing, not an anomaly, and at ~5,800 lines per log
			// rotation it buried the errors that matter (#4472). The mark is
			// read off the snapshotted monitor's generation: a poll in flight
			// across a same-object restart still attributes the OLD session's
			// death to the teardown af asked for (Codex on #4473), and a
			// vanish af never asked for stays at ERROR.
			t.monitorMu.Lock()
			initiated := mon.generation != nil && mon.generation.teardownInitiated
			mon.dead = true
			t.monitorMu.Unlock()
			if initiated {
				log.InfoLog.Printf("tmux session %s is gone; status monitor going silent (capture-pane error: %v)", t.sanitizedName, err)
			} else {
				log.ErrorLog.Printf("tmux session %s is gone; status monitor going silent (capture-pane error: %v)", t.sanitizedName, err)
			}
			return false, false, "", false
		}
		log.ErrorLog.Printf("error capturing pane content in status monitor: %v", err)
		return false, false, "", false
	}

	// A capture that BEGAN after the asking close() returned and still
	// succeeded proves the session outlived the request — the mark is stale,
	// so retire it before a later unrelated vanish is misattributed to a
	// teardown that never happened (Codex on #4473). The capture-start gate
	// is the precise half of the check: a poll that straddled the close —
	// captured pre-kill, resumed post-settle — proves nothing about the
	// request's outcome and must leave the mark for the kill that is about
	// to land.
	t.monitorMu.Lock()
	if gen := mon.generation; gen != nil && !gen.teardownSettledAt.IsZero() && captureStartedAt.After(gen.teardownSettledAt) {
		gen.teardownInitiated = false
		gen.teardownSettledAt = time.Time{}
	}
	t.monitorMu.Unlock()

	// Only set hasPrompt for agents with a known confirmation dialog, keyed
	// off the agent actually running in the pane (a non-agent override or a
	// substring-matching path must not get an agent's prompt heuristic).
	//
	// opencode has no case because it showed no confirmation dialog to detect:
	// on 0.0.0-main-202604230742 its default permission config auto-approves, and
	// it ran a destructive `rm README.md` with no prompt at all. Inventing a
	// matcher for a dialog we have never observed would be the gemini "╰"
	// TODO(#714) mistake — an unverified best guess that reads as verified.
	switch DetectAgentFromCommand(t.programCmd()) {
	case ProgramClaude:
		hasPrompt = strings.Contains(content, "No, and tell Claude what to do differently")
	case ProgramAider:
		hasPrompt = strings.Contains(content, "(Y)es/(N)o/(D)on't ask again")
	case ProgramGemini:
		hasPrompt = strings.Contains(content, "Yes, allow once")
	}

	// hash() is pure (no shared state), so compute it lock-free; only the
	// compare-and-store against prevOutputHash needs the lock.
	newHash := mon.hash(content)
	t.monitorMu.Lock()
	baseline = mon.baselinePending
	changed := !baseline && !bytes.Equal(newHash, mon.prevOutputHash)
	if baseline || changed {
		mon.prevOutputHash = newHash
		mon.baselinePending = false
	}
	t.monitorMu.Unlock()
	return changed, hasPrompt, content, baseline
}

// CapturePaneContent captures the content of the tmux pane. When the capture
// fails and !ExistsOrUnknown reports the session is definitively gone, the
// returned error wraps ErrSessionGone so non-daemon callers can degrade
// gracefully instead of logging at ERROR (#496). Classifying on the lossy bool
// is the SAFE direction here (#1962): a wedged→"exists" keeps the failure a
// generic error, so a merely-slow server is never misread as ErrSessionGone —
// the same asymmetry every send-keys/capture site in this package relies on.
//
// It passes context.Background(), so before #2105 it had no deadline whatsoever.
// This is the daemon's per-second status-poll capture (via HasUpdated), and the
// daemon polls instances SEQUENTIALLY with no watchdog, so a single wedged server
// silently froze the status of every instance behind it — liveness tracking,
// trust-prompt dismissal, and the usage-limit detector (#1146) all stop.
// CapturePaneContentContext now applies tmuxCommandTimeout regardless of the ctx
// it is handed, which is what bounds this path; a tripped deadline surfaces as
// ErrTmuxTimeout, never ErrSessionGone.
func (t *TmuxSession) CapturePaneContent() (string, error) {
	return t.CapturePaneContentContext(context.Background())
}

// CapturePaneContentContext is CapturePaneContent bound to ctx AND to the
// package's own tmuxCommandTimeout, whichever fires first.
//
// The caller's ctx is honored as before: the readiness poll (task.WaitForReady)
// passes a cancellable one so an abandoned create tears down its in-flight
// capture, and cancellation returns ctx.Err() directly, skipping the
// ExistsOrUnknown probe (which would spawn another tmux subprocess) since the
// failure cause is the cancel, not a dead session.
//
// #2105 added the second, independent bound and moved the command onto
// boundedTmuxCommand. Both halves were needed:
//
//   - A caller-supplied ctx is not a bound at all when the caller supplies
//     context.Background(), which is exactly what CapturePaneContent does on the
//     daemon's per-second status poll. Deriving our own timeout from ctx means
//     that path is bounded no matter what the caller passes, while a caller with
//     a SHORTER deadline still wins.
//   - Plain exec.CommandContext sets no WaitDelay and kills only the direct
//     process, so a child holding the inherited stdout pipe keeps Output()
//     blocked on pipe EOF long after tmux is SIGKILLed — the deadline fires and
//     buys nothing. boundedTmuxCommand's process-group kill plus tmuxWaitDelay is
//     what actually makes a deadline bite.
//
// The two failures are reported distinctly and that distinction matters: a caller
// cancel is a normal abandon (ctx.Err()), while a tripped internal deadline means
// the SERVER is wedged and the session's state is UNKNOWN (ErrTmuxTimeout, never
// ErrSessionGone — callers tear sessions down on "gone"). The parent is checked
// first so a cancel racing the deadline is attributed to the caller.
func (t *TmuxSession) CapturePaneContentContext(ctx context.Context) (string, error) {
	return t.capturePaneContentTarget(ctx, exactTarget(t.sanitizedName), nil)
}

// capturePaneContentTarget is CapturePaneContentContext addressed to an
// explicit tmux target. The status poll passes the monitor's confirmed
// session id ($N): a poll that straddles a same-object restart must not land
// on the replacement that reused the name and consult the old generation's
// teardown mark against ITS death — the name alone cannot tell them apart
// (#4473 review). The gone-probe is bound to the same generation for the
// same reason: neither the name answering "live" nor the bare id answering
// proves the POLLED generation stands behind it, because tmux reissues ids
// per server lifetime — so a bound generation's probe verifies the server
// pid and creation stamp, not just the id.
func (t *TmuxSession) capturePaneContentTarget(ctx context.Context, target string, gen *tmuxGeneration) (string, error) {
	// Add -e flag to preserve escape sequences (ANSI color codes). `=` forces
	// an exact session match: without it tmux would prefix-match a surviving
	// sibling session (e.g. the `__shell` tab) when the agent session has
	// died, capturing the wrong pane and masking the dead agent (#1006).
	bctx, cancel := context.WithTimeout(ctx, tmuxCommandTimeout)
	defer cancel()
	output, err := t.outputTmuxBounded(bctx, "capture-pane", "-p", "-e", "-J", "-t", target)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if bctx.Err() != nil {
			return "", fmt.Errorf("%w: capture-pane after %s", ErrTmuxTimeout, tmuxCommandTimeout)
		}
		if !t.captureTargetAliveOrUnknown(gen) {
			return "", t.sessionGoneError("capture-pane", err)
		}
		return "", fmt.Errorf("error capturing pane content: %v", err)
	}
	return string(output), nil
}

// captureTargetAliveOrUnknown is the gone-probe for a possibly id-bound
// capture: a bound generation needs the identity answer for THAT generation,
// not the reused name, or a replacement's liveness — whether it took the name
// or reissued the id on a new server — would mask the polled generation's
// death. Same conservative timeout lie as ExistsOrUnknown.
func (t *TmuxSession) captureTargetAliveOrUnknown(gen *tmuxGeneration) bool {
	if gen == nil || gen.sessionID == "" {
		return t.ExistsOrUnknown()
	}
	same, known, _ := t.generationMatches(gen)
	if !known {
		return true
	}
	return same
}

// confirmedGeneration resolves the concrete tmux session currently standing
// behind the session name — the identity a reused name cannot provide. The
// tuple beyond the id matters: #{session_id} is a per-server counter a
// replacement server reissues, while #{pid} and #{session_created} pin the
// server lifetime and the session within it. nil on any failure or malformed
// answer: callers fall back to the name target, the pre-binding behavior.
func (t *TmuxSession) confirmedGeneration() *tmuxGeneration {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	out, err := t.outputTmuxBounded(ctx, "display-message", "-p",
		"-t", exactTarget(t.sanitizedName),
		"#{session_id} #{pid} #{session_created}")
	if err != nil {
		return nil
	}
	f := strings.Fields(strings.TrimSpace(string(out)))
	if len(f) < 3 || !strings.HasPrefix(f[0], "$") {
		return nil
	}
	return &tmuxGeneration{sessionID: f[0], serverPID: f[1], created: f[2]}
}

// generationMatches reports whether the session answering at gen's id is
// still gen itself: the id is asked for the server pid and creation stamp
// resolved when the generation was confirmed, and any mismatch means the id
// was reissued — the bound generation is gone even though a session answers.
//
// Three answers, like the name probes, with the error carried so the caller
// can spend its budget instead of a second command: (false, false, err) when
// the probe never got an answer — timeout, exec-level failure, or any error
// that is not tmux's own absence report — because a read that did not happen
// is no evidence of the generation's fate (Codex on #4473). Only two outcomes
// are determinate GONE: tmux PROVING the id resolves to nothing
// (provedGenerationAbsent, which includes a definitively dead server), and a
// successful answer with no session_created. display-message -t resolves a
// missing target to an EMPTY session context rather than erroring — server
// fields still print while session fields come back blank (measured on tmux
// 3.4: a dead $id and an empty live server both answer "975304 " for this
// format) — so a short field list IS tmux's "no session there" for this
// probe, not a parse hiccup.
func (t *TmuxSession) generationMatches(gen *tmuxGeneration) (same bool, known bool, err error) {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	out, runErr := t.outputTmuxBounded(ctx, "display-message", "-p",
		"-t", gen.sessionID,
		"#{pid} #{session_created}")
	if runErr != nil {
		if ctx.Err() != nil {
			return false, false, fmt.Errorf("%w: display-message after %s", ErrTmuxTimeout, tmuxCommandTimeout)
		}
		if t.provedGenerationAbsent(gen, runErr) {
			return false, true, nil
		}
		return false, false, fmt.Errorf("generation probe for %s did not return a usable answer%s: %w",
			gen.sessionID, tmuxDiagnosticSuffix(runErr), runErr)
	}
	f := strings.Fields(strings.TrimSpace(string(out)))
	if len(f) < 2 {
		return false, true, nil
	}
	return f[0] == gen.serverPID && f[1] == gen.created, true, nil
}

// provedGenerationAbsent is tmuxProvedSessionAbsent's contract applied to an
// ID target: a failed identity probe counts as determinate death only when
// tmux itself answered "can't find session: $id" or proved the server gone —
// and, for an exit-1 diagnostic that names neither, when the server's own
// session-id listing corroborates the id is unregistered. A listing that
// still contains the id — or cannot answer — leaves the failure unknown and
// retryable: the bound generation may be alive behind a probe that merely
// could not run (Codex on #4473).
func (t *TmuxSession) provedGenerationAbsent(gen *tmuxGeneration, runErr error) bool {
	if missingTmuxSession(runErr, gen.sessionID) {
		return true
	}
	if _, exitOne := tmuxExitOneDiagnostic(runErr); !exitOne {
		return false
	}
	ids, listErr := listSessionIDs(t.cmdExec)
	if listErr != nil {
		return false
	}
	return !slices.Contains(ids, gen.sessionID)
}

// CaptureVisiblePaneGrid captures the visible pane as a GRID — one output line per
// PHYSICAL pane row (capture-pane WITHOUT -J), escapes preserved (-e). Unlike
// CapturePaneContent, which -J-joins wrapped logical lines (right for prompt
// detection / preview, where the logical text matters), this keeps the row
// structure so output line index i is exactly pane row i. The WS-broker repaint
// relies on that 1:1 mapping to place each row at its true grid position and land
// the restored cursor (a 0-based pane cursor_y) on the correct line REGARDLESS of
// the client emulator's width (#1688): -J-joined lines re-wrap by the client's
// width, so a client wider or narrower than the pane shifts the rows out from under
// the absolute cursor move. Wraps ErrSessionGone when the session has vanished,
// mirroring CapturePaneContent (#496, #1006).
//
// Bounded by tmuxCommandTimeout (#1787): this runs on the WS subscribe path
// BEFORE the 101 upgrade, so an unbounded stall here means the client never
// receives a socket at all. On a tripped deadline it returns ErrTmuxTimeout
// without probing ExistsOrUnknown — see tmuxTimeoutContext.
func (t *TmuxSession) CaptureVisiblePaneGrid() (string, error) {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	output, err := t.outputTmuxBounded(ctx, "capture-pane", "-p", "-e", "-t", exactTarget(t.sanitizedName))
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("%w: capture-pane grid after %s", ErrTmuxTimeout, tmuxCommandTimeout)
		}
		if !t.ExistsOrUnknown() {
			return "", t.sessionGoneError("capture-pane", err)
		}
		return "", fmt.Errorf("error capturing pane grid: %v", err)
	}
	return string(output), nil
}

type paneCursorState struct {
	Row     int
	Col     int
	Visible bool
}

const paneCursorStateFormat = "#{cursor_y} #{cursor_x} #{?cursor_flag,1,0}"

// readPaneCursorState reports cursor position and whether the pane application
// exposes that cursor. Keeping this focused three-field query separate from the
// terminal-mode snapshot means a diagnostic cannot silently change the shared
// snapshot wire shape (#2225 review).
func (t *TmuxSession) readPaneCursorState() (paneCursorState, error) {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	output, err := t.outputTmuxBounded(ctx, "display-message", "-p", "-t", exactTarget(t.sanitizedName), paneCursorStateFormat)
	if err != nil {
		if ctx.Err() != nil {
			return paneCursorState{}, fmt.Errorf("%w: display-message after %s", ErrTmuxTimeout, tmuxCommandTimeout)
		}
		return paneCursorState{}, fmt.Errorf("failed to read tmux cursor state: %v", err)
	}
	var state paneCursorState
	var visible int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(output)), "%d %d %d", &state.Row, &state.Col, &visible); err != nil {
		return paneCursorState{}, fmt.Errorf("failed to parse tmux cursor state %q: %v", string(output), err)
	}
	state.Visible = visible != 0
	return state, nil
}

// CursorPosition reports the pane cursor position as 0-based (row, col) via
// display-message (`#{cursor_y} #{cursor_x}`, both 0-based in tmux). The broker's
// repaint uses it to restore the emulator cursor to where the pane program's
// cursor actually is, so a fresh subscriber's redraw does not orphan a stale copy
// of the line at the top (the duplicated-prompt artifact). `=` forces an exact
// session match, mirroring CapturePaneContent (#1006).
//
// Bounded by tmuxCommandTimeout (#1787): like CaptureVisiblePaneGrid it runs on
// the WS subscribe path before the 101 upgrade. The broker already treats a
// cursor-read failure as best-effort (degrading to a screen-only repaint), so a
// wedged server costs the cursor restore rather than the whole subscription.
func (t *TmuxSession) CursorPosition() (row, col int, err error) {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	output, err := t.outputTmuxBounded(ctx, "display-message", "-p", "-t", exactTarget(t.sanitizedName), "#{cursor_y} #{cursor_x}")
	if err != nil {
		if ctx.Err() != nil {
			return 0, 0, fmt.Errorf("%w: display-message after %s", ErrTmuxTimeout, tmuxCommandTimeout)
		}
		return 0, 0, fmt.Errorf("failed to read tmux cursor position: %v", err)
	}
	if _, err := fmt.Sscanf(strings.TrimSpace(string(output)), "%d %d", &row, &col); err != nil {
		return 0, 0, fmt.Errorf("failed to parse tmux cursor position %q: %v", string(output), err)
	}
	return row, col, nil
}

// CapturePaneContentWithOptions captures the pane content with additional options
// start and end specify the starting and ending line numbers (use "-" for the start/end of history).
// Wraps ErrSessionGone when the session has vanished, mirroring CapturePaneContent.
//
// Bounded by tmuxCommandTimeout (#2099): it was the last capture-pane read in the
// package running on bare exec.Command. On a tripped deadline it returns
// ErrTmuxTimeout without probing ExistsOrUnknown — see tmuxTimeoutContext.
func (t *TmuxSession) CapturePaneContentWithOptions(start, end string) (string, error) {
	// Add -e flag to preserve escape sequences (ANSI color codes). `=` forces
	// an exact session match, mirroring CapturePaneContent (#1006).
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	output, err := t.outputTmuxBounded(ctx, "capture-pane", "-p", "-e", "-J", "-S", start, "-E", end, "-t", exactTarget(t.sanitizedName))
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("%w: capture-pane options after %s", ErrTmuxTimeout, tmuxCommandTimeout)
		}
		if !t.ExistsOrUnknown() {
			return "", t.sessionGoneError("capture-pane", err)
		}
		return "", fmt.Errorf("failed to capture tmux pane content with options: %v", err)
	}
	return string(output), nil
}

// CaptureVisibleWithScrollback captures the visible screen AND the scrollback size
// in ONE tmux invocation, so the two describe the same instant (#3169).
//
// Atomicity is the whole point, and two separate commands could not provide it.
// A pre-read/post-read bracket around the capture still reports an incomplete
// capture as complete: zero at the pre-read, history gained through output or a
// shorter resize before capture-pane, then lost through clear-history or a taller
// resize before the post-read — both endpoints read zero while the returned content
// really did omit lines. Narrowing a check-then-act window is not closing it, and
// this is the one place where a wrong answer means an operator trusts a partial
// capture (#3169 review).
//
// tmux runs both commands in ONE command queue, so no client can interleave a
// clear-history between them. The count is asked for FIRST and is a single line of
// digits, so the split is unambiguous even though pane content is arbitrary — a
// delimiter after arbitrary content would not be.
//
// Verified against real tmux while designing this: a 10-row pane holding 61 lines
// answered "51" followed by the visible screen, and a separate full capture returned
// 61 lines — 51 + 10 = 61, so history_size is exactly the lines above the region
// this returns.
//
// The capture flags match CapturePaneContent exactly (-p -e -J), so the content is
// byte-identical to what the non-atomic path returned.
// ErrScrollbackCaptureUnparseable marks an answer this cannot split — a producer
// that does not implement the combined command shape. It is deliberately NOT the
// same class as ErrSessionGone or ErrTmuxTimeout: those are real failures a caller
// must surface, while this one means "no count available here", which a caller may
// degrade to a plain capture with an unknown count (#3169 review).
var ErrScrollbackCaptureUnparseable = errors.New("tmux answer carries no capture after the history size")

func (t *TmuxSession) CaptureVisibleWithScrollback() (string, int, error) {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	target := exactTarget(t.sanitizedName)
	output, err := t.outputTmuxBounded(ctx, "display-message", "-p", "-t", target, "#{history_size}",
		";", "capture-pane", "-p", "-e", "-J", "-t", target)
	if err != nil {
		if ctx.Err() != nil {
			return "", 0, fmt.Errorf("%w: capture-pane with scrollback after %s", ErrTmuxTimeout, tmuxCommandTimeout)
		}
		if !t.ExistsOrUnknown() {
			return "", 0, t.sessionGoneError("capture-pane with scrollback", err)
		}
		return "", 0, fmt.Errorf("failed to capture tmux pane with scrollback: %v", err)
	}
	// The first line is the count; everything after it is the capture. A missing
	// newline means tmux answered the count and nothing else, which is not a capture
	// this can split — reported rather than guessed at.
	head, content, found := strings.Cut(string(output), "\n")
	if !found {
		return "", 0, fmt.Errorf("%w: %q", ErrScrollbackCaptureUnparseable, string(output))
	}
	size, convErr := strconv.Atoi(strings.TrimSpace(head))
	if convErr != nil {
		return "", 0, fmt.Errorf("%w: history size %q: %v", ErrScrollbackCaptureUnparseable, head, convErr)
	}
	return content, size, nil
}

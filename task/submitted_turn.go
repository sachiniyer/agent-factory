package task

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The post-submit observation (#4429): after a send reports sent-unverified,
// watch briefly for the agent's own in-turn chrome, which can upgrade the
// verdict to delivered. Split from runner.go, which owns readiness and the send.

var (
	// runningTimer matches the elapsed-seconds fragment claude, devin and
	// codex print on the SAME row as their interrupt hint
	// ("(12s · esc to interrupt)"). It is a shape test, NOT proof of a live
	// turn: prose can hold both fragments on one row ("after 12s press esc to
	// interrupt"), and this repository's own source quotes the chrome
	// verbatim, so an agent printing that file would render a matching row.
	// What separates chrome from text is that chrome TICKS — see
	// submittedTurnVisible.
	runningTimer = regexp.MustCompile(`\d+s`)
	// elapsedTimer reads the elapsed time off a timed status row — "12s", and
	// claude's longer "1m 30s" / "1h 2m 3s" forms — so two captures of the SAME
	// row can be compared: a live row's value grows between them.
	elapsedTimer = regexp.MustCompile(`(?:(\d+)h\s*)?(?:(\d+)m\s*)?(\d+)s`)
	// statusRowNumber masks every other number on a timed row (token counts,
	// "1.2k") when deriving the row's identity, since those repaint with the
	// timer and would otherwise make one row read as two.
	statusRowNumber = regexp.MustCompile(`\d+(?:\.\d+)?`)
	// postSubmitDeliveredBudget bounds the post-Enter observation that may
	// upgrade a sent-unverified verdict to delivered (#4429), and
	// postSubmitDeliveredPoll is its re-check interval. The send path proved the
	// pane readable at submit, so a brief watch for the agent's own mid-turn
	// chrome is cheap; the poll exits on the first positive frame, well under
	// the cap. Package vars so tests can compress them.
	postSubmitDeliveredBudget = 3 * time.Second
	postSubmitDeliveredPoll   = 200 * time.Millisecond
)

// submittedTurnVisible is the post-submit positive observation for
// WaitForReadyAndSendPromptWithStatus (#4429). A sent-unverified verdict means
// every capture succeeded but none rendered prompt-specific proof; this poll
// adds the one signal that can still settle it — the agent's own in-turn
// indicator. The send runs only after readiness proved the composer idle, so
// mid-turn chrome appearing inside the window is this submission's work
// starting: positive delivery evidence, not a readability guess. A miss claims
// nothing — an agent with no working signature, or chrome that has not painted
// yet, keeps the ambiguous verdict.
func submittedTurnVisible(ctx context.Context, target ReadinessTarget) bool {
	agent := target.ResolvedAgent()
	if agent == "" {
		return false
	}
	// Claude, devin and codex print an elapsed timer on their in-turn row, and
	// that is the only part of this pane a running turn is guaranteed to
	// rewrite. Claude's row is scoped to the status slot above the live
	// composer but shares no row with its interrupt hint (that moved to the
	// footer, #6590). Devin's still carries both fragments on one unscopeable
	// line, and codex 0.163.0's "• Working (Ns • esc to interrupt)" sits above
	// its composer the same way (#6879). For the unscoped rows, presence alone
	// would accept prose that merely holds the same two fragments, including a
	// verbatim quote of the chrome or a stale line a failed paste scrolled
	// into view. So the proof for all three is the timer ADVANCING on one row:
	// the same status row, found once in each of two consecutive captures,
	// with a larger elapsed time in the later one. Chrome ticks; text stands
	// still, and a row that merely scrolls into view has no earlier self to
	// have ticked from (timedRowTicked).
	//
	// Codex's older chrome generation — a bare "esc to interrupt" row below
	// the composer with no timer — has nothing to tick, but its position below
	// the live composer already scopes it: presence there is the proof on its
	// own, same as amp's and opencode's frame-scoped indicators.
	timed := agent == tmux.ProgramClaude || agent == tmux.ProgramDevin || agent == tmux.ProgramCodex
	var prev map[string]timedRow
	deadline := time.Now().Add(postSubmitDeliveredBudget)
	for {
		if ctx.Err() != nil {
			return false
		}
		content, err := target.PreviewContent(ctx)
		switch {
		case err != nil:
		case !timed:
			if submittedTurnContent(content, agent) {
				return true
			}
		default:
			cur := timedRowsFor(content, agent)
			if timedRowTicked(prev, cur) ||
				(agent == tmux.ProgramCodex && codexInterruptHintBelowComposer(content)) {
				return true
			}
			prev = cur
		}
		if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(postSubmitDeliveredPoll):
		}
	}
}

// submittedTurnContent reports whether the captured pane shows the resolved
// agent mid-turn — its own "I am busy" chrome, never an inference from the
// pane changing. Per-arm scoping is the contract: each agent's indicator is
// matched where the agent actually draws it, so transcript prose quoting the
// hint cannot pass. The timed-row agents (devin, codex) are the exception the
// promotion paths close: their row shape is prose-holdable on one line, so
// this function is only the busy/eligibility answer for them, and the ticking
// requirement lives in submittedTurnVisible and TurnWatch (timedRowTicked).
func submittedTurnContent(content, agent string) bool {
	switch agent {
	case tmux.ProgramAmp, tmux.ProgramOpencode:
		return IsWorkingContent(content, agent)
	case tmux.ProgramCodex:
		// Codex's in-turn chrome comes in two generations (#6879). Older codex
		// drew a bare "esc to interrupt" hint row BELOW its composer; 0.163.0
		// draws "• Working (Ns • esc to interrupt)" ABOVE it — the same
		// hint-and-timer-on-one-row shape devin has. Either form showing is
		// in-turn chrome for the busy question this function answers.
		return codexInterruptHintBelowComposer(content) || len(timedTurnRows(content)) > 0
	case tmux.ProgramClaude:
		// Claude draws its timed status row in the slot above the live
		// composer and its interrupt hint on a separate footer below it —
		// no row carries both fragments any more (#6590). The scoped
		// spinner row, or a footer hint on its own, is the busy signal;
		// submittedTurnVisible is what requires the row to tick.
		return isClaudeWorkingContent(content)
	case tmux.ProgramDevin:
		// Devin still embeds the hint in a timed status row —
		// "Thinking · 3s (esc twice to interrupt)" on current builds (the
		// wording drifted in 3000.11.x, #6879). The shape alone is not proof
		// that a turn is running; submittedTurnVisible is what requires the
		// row to tick.
		return len(timedTurnRows(content)) > 0
	}
	return false
}

// codexInterruptHintBelowComposer reports whether codex's older in-turn
// chrome — the bare "esc to interrupt" hint row — sits below its line-leading
// composer (the daemon/configagent_delivery_test.go pane shape). The composer
// glyph is "›" or, under the Ultra reasoning tier, "»" —
// tmux.IsCodexComposerLine owns the set (#5383). Anchoring on the composer
// keeps a transcript that quotes the hint from qualifying.
func codexInterruptHintBelowComposer(content string) bool {
	lines := strings.Split(paneAnsiEscape.ReplaceAllString(content, ""), "\n")
	composer := -1
	for i, line := range lines {
		if tmux.IsCodexComposerLine(line) {
			composer = i
		}
	}
	if composer < 0 {
		return false
	}
	for _, line := range lines[composer+1:] {
		if escToInterruptHint.MatchString(line) {
			return true
		}
	}
	return false
}

// timedTurnRows returns the rows shaped like devin's or codex's in-turn status
// row (and claude's older one): the interrupt hint and an elapsed timer on the
// SAME row.
//
// Shape only. Unlike amp and opencode — whose indicators this file scopes to a
// live frame or to the status bar below the composer — claude, devin and codex
// draw their timed row above the composer, with the transcript above that and
// no boundary between the two that a capture can see. So a row of prose that
// happens to hold both fragments matches, and one can arrive without the agent
// writing it: a paste that fails to submit pushes the view up and can scroll an
// older line into frame. The caller separates the two by watching one row's
// timer ADVANCE — see timedRowTicked.
func timedTurnRows(content string) []string {
	var rows []string
	for _, line := range strings.Split(paneAnsiEscape.ReplaceAllString(content, ""), "\n") {
		if escToInterruptHint.MatchString(line) && runningTimer.MatchString(line) {
			// Trimmed: only the row's own repaint should read as a change, never
			// the pane's right-hand padding moving under a resize.
			rows = append(rows, strings.TrimSpace(line))
		}
	}
	return rows
}

// timedRow is one timed status row as timedRowTicked compares it: how many rows
// in the capture share its identity, and where and with what elapsed time the
// first of them was drawn.
type timedRow struct {
	count int
	// fromBottom is the row's line offset from the bottom of the capture. The
	// live row is anchored above the composer; transcript text scrolls.
	fromBottom int
	elapsed    time.Duration
}

// timedRowsFor returns a capture's timed status rows under the resolved
// agent's own pane contract. Claude moved its interrupt hint off the status
// row and onto a footer below the composer (#6590), so its timed row is the
// spinner row in the slot the composer scopes. Devin's and codex's rows still
// carry hint and timer on one line — devin's "Thinking · Ns (esc twice to
// interrupt)", codex 0.163.0's "• Working (Ns • esc to interrupt)" — so they
// share the whole-pane identity walk.
func timedRowsFor(content, agent string) map[string]timedRow {
	if agent == tmux.ProgramClaude {
		return claudeTimedRows(content)
	}
	return timedTurnRowsByIdentity(content)
}

// claudeTimedRows reads the row in claude's live-composer status slot. When it
// holds a spinner row with an elapsed timer, that row is the turn's timed
// status row — produced as the singleton timedRowTicked can watch advance.
// Anything else in the slot (a done summary, transcript text, a pane with no
// composer) yields no row: those never tick.
func claudeTimedRows(content string) map[string]timedRow {
	lines := strings.Split(strings.TrimRight(paneAnsiEscape.ReplaceAllString(content, ""), " \t\r\n"), "\n")
	row, idx, ok := claudeStatusRow(lines)
	if !ok || !claudeSpinnerLine.MatchString(row) {
		return nil
	}
	elapsed, ok := rowElapsed(row)
	if !ok {
		return nil
	}
	trimmed := strings.TrimSpace(row)
	id := strings.TrimLeftFunc(trimmed, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	id = statusRowNumber.ReplaceAllString(id, "#")
	return map[string]timedRow{
		id: {count: 1, fromBottom: len(lines) - 1 - idx, elapsed: elapsed},
	}
}

// timedTurnRowsByIdentity groups a capture's timed status rows by identity —
// the row with its spinner glyph dropped and every number masked — so the same
// row can be found again in the next capture after its timer, token count and
// spinner frame have repainted.
func timedTurnRowsByIdentity(content string) map[string]timedRow {
	lines := strings.Split(strings.TrimRight(paneAnsiEscape.ReplaceAllString(content, ""), " \t\r\n"), "\n")
	var byID map[string]timedRow
	for idx, line := range lines {
		if !escToInterruptHint.MatchString(line) || !runningTimer.MatchString(line) {
			continue
		}
		row := strings.TrimSpace(line)
		elapsed, ok := rowElapsed(row)
		if !ok {
			continue
		}
		// The spinner frame is the only part of a live row that repaints
		// without meaning anything, so drop it along with the numbers.
		id := strings.TrimLeftFunc(row, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
		id = statusRowNumber.ReplaceAllString(id, "#")
		// Everything the row draws after the hint repaints with the turn —
		// devin's context count "(NNc · ctrl+o for details)", codex's
		// "· N background terminal running · /ps to view" tail — so the
		// identity ends where the hint does, or a flickering suffix would
		// split one live row's frames into different rows (#6879).
		if m := escToInterruptHint.FindStringIndex(id); m != nil {
			id = id[:m[1]]
		}
		if byID == nil {
			byID = make(map[string]timedRow)
		}
		entry := byID[id]
		if entry.count == 0 {
			entry.fromBottom = len(lines) - 1 - idx
			entry.elapsed = elapsed
		}
		entry.count++
		byID[id] = entry
	}
	return byID
}

// rowElapsed reads the elapsed time off a timed status row.
func rowElapsed(row string) (time.Duration, bool) {
	m := elapsedTimer.FindStringSubmatch(row)
	if m == nil {
		return 0, false
	}
	var total time.Duration
	for i, unit := range []time.Duration{time.Hour, time.Minute, time.Second} {
		if m[i+1] == "" {
			continue
		}
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return 0, false
		}
		total += time.Duration(n) * unit
	}
	return total, true
}

// timedRowTicked reports whether some status row in cur is a row from prev whose
// timer advanced — the one thing a running claude or devin turn does that
// transcript text cannot.
//
// Each half of the condition closes a way static text could pass:
//
//   - the row must already be in prev. A row that is merely NEW is not a tick: a
//     paste that fails to submit pushes the view up and can scroll an old status
//     line into frame, and that line has no earlier self to have ticked from.
//   - it must be the only row with its identity in BOTH captures, at the same
//     distance from the bottom. With two matching rows there is no telling
//     which one moved; and a scroll that carries one quoted copy out while
//     another comes in shows one row per capture, but not in the same place.
//     The live row stays put above the composer while it ticks.
//   - the elapsed time must grow. Equal is a row that stood still; smaller is a
//     different row, never the same timer.
func timedRowTicked(prev, cur map[string]timedRow) bool {
	_, ok := tickingTimedRow(prev, cur)
	return ok
}

// TurnWatch accumulates one session's consecutive pane captures and reports
// when the agent's own in-turn chrome is demonstrated — the signal that
// separates a live turn from boot or prompt-echo noise (#5219). It is the
// poll-cadence sibling of submittedTurnVisible: same vocabulary, fed one
// capture per status tick instead of inside the submit window. A task run's
// idle edge may only spend the session once this fires (or the quieter
// fallback qualifies), because nothing else the pane renders proves the agent
// — rather than its own boot output — began the turn.
type TurnWatch struct {
	agent string
	// boundary is the prompt attempt this watch answers for. Only a turn that
	// demonstrably BEGAN after it counts — a ticking row alone can belong to
	// unrelated work already in flight when a redelivery reset the boundary
	// (#5221 review): elapsed is such a row's own clock, and an indicator that
	// never leaves the pane across the boundary proves nothing about the new
	// prompt.
	boundary time.Time
	prev     map[string]timedRow
	armed    bool
}

// NewTurnWatch returns the chrome watcher for the agent the pane actually runs
// — pass the resolved (runtime) agent so a handoff or program override picks
// the right signature, and the prompt boundary the watch answers for. An agent
// with no in-turn signature never reports; callers cover it with the quieter
// fallback evidence instead.
func NewTurnWatch(agent string, boundary time.Time) *TurnWatch {
	return &TurnWatch{agent: agent, boundary: boundary}
}

// Observe folds one captured frame into the watch and reports whether it
// proves a turn began after the boundary. `at` must be the frame's CAPTURE
// (or request-start) instant, not the caller's apply time: for a remote
// session the pane is captured inside the sandbox and processed after
// transport, and measuring the boundary's age at apply time would let that
// unbounded latency inflate it until a pre-boundary turn's row fits (#5221
// review). For the timed-row agents (claude, devin) a row's presence is not
// enough — scrollback can hold a stale one — so the row's elapsed timer must
// have advanced against the previous capture, the same contract
// submittedTurnVisible applies inside the submit window; and the ticking
// row's own elapsed must fit inside the boundary's age at capture, or a turn
// already running when the prompt went out gets attributed to it. The other
// agents' indicators are scoped to their live frame but carry no clock, so
// they must be seen absent once before a present frame can count — the start
// edge the elapsed check expresses for timed rows.
func (w *TurnWatch) Observe(content string, at time.Time) bool {
	switch w.agent {
	case tmux.ProgramClaude, tmux.ProgramDevin:
		cur := timedRowsFor(content, w.agent)
		row, ticked := tickingTimedRow(w.prev, cur)
		w.prev = cur
		// rowElapsed reads a whole-second display, so its truncation leaves a
		// sub-second gap: a turn started a fraction of a second before the
		// boundary could otherwise satisfy elapsed <= age. The margin is the
		// display's own resolution — real elapsed is strictly less than
		// displayed+1s, so demanding that fit inside the boundary's age makes a
		// strictly-pre-boundary turn unrepresentable (#5221 review).
		return ticked && row.elapsed+time.Second <= at.Sub(w.boundary)
	case tmux.ProgramCodex:
		// Codex's chrome comes in two generations (#6879). 0.163.0's
		// "• Working (Ns • esc to interrupt)" above the composer is a timed
		// row, so the same tick proof as devin's applies — its own elapsed
		// clock bounds the turn's start against the boundary. The older bare
		// "esc to interrupt" row below the composer carries no clock, so it
		// keeps the clockless contract: seen absent once before a present
		// frame counts.
		cur := timedRowsFor(content, w.agent)
		row, ticked := tickingTimedRow(w.prev, cur)
		w.prev = cur
		if ticked && row.elapsed+time.Second <= at.Sub(w.boundary) {
			return true
		}
		if !codexInterruptHintBelowComposer(content) {
			w.armed = true
			return false
		}
		return w.armed
	default:
		if !submittedTurnContent(content, w.agent) {
			w.armed = true
			return false
		}
		return w.armed
	}
}

// tickingTimedRow is timedRowTicked lifted to name the row that moved, so the
// caller can check the ticking row's own elapsed against the prompt boundary.
//
// Identity stays authoritative for unscoped rows: a scrolling transcript that
// carries one quoted timed row out of the viewport while a different quote
// enters the same slot must never read as a tick (#6881 review). The cost of
// that strictness is at most one poll — when devin swaps its verb mid-turn
// ("Thinking" → "Running tools") the transition frame's identity changes too,
// and the row re-proves itself on the next tick under its new text.
func tickingTimedRow(prev, cur map[string]timedRow) (timedRow, bool) {
	for id, now := range cur {
		before, ok := prev[id]
		if ok && before.count == 1 && now.count == 1 &&
			before.fromBottom == now.fromBottom && now.elapsed > before.elapsed {
			return now, true
		}
	}
	return timedRow{}, false
}

// turnSignaturedAgent reports whether the resolved agent draws in-turn chrome
// submittedTurnVisible can find (#5221 review): scoped indicators for amp and
// opencode; timed status rows for claude, devin and codex, plus codex's older
// scoped hint. A signatureless agent's pane has nothing the watch can
// attribute — running it would only burn the post-submit budget.
func turnSignaturedAgent(agent string) bool {
	switch agent {
	case tmux.ProgramClaude, tmux.ProgramDevin, tmux.ProgramCodex, tmux.ProgramAmp, tmux.ProgramOpencode:
		return true
	}
	return false
}

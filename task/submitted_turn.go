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
	// runningTimer matches the elapsed-seconds fragment claude and devin print on
	// the SAME row as their interrupt hint ("(12s · esc to interrupt)"). It is a
	// shape test, NOT proof of a live turn: prose can hold both fragments on one
	// row ("after 12s press esc to interrupt"), and this repository's own source
	// quotes the chrome verbatim, so an agent printing that file would render a
	// matching row. What separates chrome from text is that chrome TICKS — see
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
	// Claude and devin print an elapsed timer on their in-turn row, and that is
	// the only part of this pane a running turn is guaranteed to rewrite. Their
	// row is unscopeable — it sits above the composer with the transcript above
	// it — so presence alone would accept prose that merely holds the same two
	// fragments, including a verbatim quote of the chrome or a stale line a
	// failed paste scrolled into view. So the proof is the timer ADVANCING on
	// one row: the same status row, found once in each of two consecutive
	// captures, with a larger elapsed time in the later one. Chrome ticks; text
	// stands still, and a row that merely scrolls into view has no earlier self
	// to have ticked from (timedRowTicked). The other agents' indicators are
	// already scoped to their live frame, so for them the row's presence there
	// is the proof.
	timed := agent == tmux.ProgramClaude || agent == tmux.ProgramDevin
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
			cur := timedTurnRowsByIdentity(content)
			if timedRowTicked(prev, cur) {
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
// pane changing. Per-arm scoping is the whole contract: each agent's indicator
// is matched where the agent actually draws it, so transcript prose quoting
// the hint cannot pass.
func submittedTurnContent(content, agent string) bool {
	switch agent {
	case tmux.ProgramAmp, tmux.ProgramOpencode:
		return IsWorkingContent(content, agent)
	case tmux.ProgramCodex:
		// Codex draws "esc to interrupt" as a bare hint row BELOW its
		// line-leading "›" composer (the daemon/configagent_delivery_test.go
		// pane shape). Anchoring on the composer keeps a transcript that quotes
		// the hint from qualifying.
		lines := strings.Split(paneAnsiEscape.ReplaceAllString(content, ""), "\n")
		composer := -1
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "›") {
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
	case tmux.ProgramClaude, tmux.ProgramDevin:
		// Claude and devin embed the hint in a timed status row —
		// "✻ … (12s · esc to interrupt)", "Thinking · 3s (esc to interrupt)".
		// The shape alone is not proof that a turn is running; submittedTurnVisible
		// is what requires the row to tick.
		return len(timedTurnRows(content)) > 0
	}
	return false
}

// timedTurnRows returns the rows shaped like claude's or devin's in-turn status
// row: the interrupt hint and an elapsed timer on the SAME row.
//
// Shape only. Unlike codex, amp and opencode — whose indicators this file scopes
// to a live frame or to the status bar below the composer — claude and devin draw
// their row above the composer, with the transcript above that and no boundary
// between the two that a capture can see. So a row of prose that happens to hold
// both fragments matches, and one can arrive without the agent writing it: a
// paste that fails to submit pushes the view up and can scroll an older line into
// frame. The caller separates the two by watching one row's timer ADVANCE —
// see timedRowTicked.
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
// proves a turn began after the boundary. For the timed-row agents (claude,
// devin) a row's presence is not enough — scrollback can hold a stale one —
// so the row's elapsed timer must have advanced against the previous capture,
// the same contract submittedTurnVisible applies inside the submit window;
// and the ticking row's own elapsed must fit inside the boundary's age, or a
// turn already running when the prompt went out gets attributed to it. The
// other agents' indicators are scoped to their live frame but carry no clock,
// so they must be seen absent once before a present frame can count — the
// start edge the elapsed check expresses for timed rows.
func (w *TurnWatch) Observe(content string) bool {
	switch w.agent {
	case tmux.ProgramClaude, tmux.ProgramDevin:
		cur := timedTurnRowsByIdentity(content)
		row, ticked := tickingTimedRow(w.prev, cur)
		w.prev = cur
		return ticked && row.elapsed <= time.Since(w.boundary)
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

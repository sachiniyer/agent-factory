package task

import (
	"testing"
	"time"
)

// TurnWatch feeds the task-run turn boundary (#5219): the daemon folds each
// status-poll capture through it, and only a firing watch — the agent's own
// in-turn chrome — may release a held idle edge. Everything a still-booting
// pane can produce must therefore NOT fire it. And since a redelivery rekeys
// the watch, only a turn that BEGAN after its boundary may fire it (#5221
// review) — two post-boundary frames of a pre-boundary turn prove nothing.

// claudeFrame composes the pane claude really draws: a status-slot row above
// the composer rules, then the composer pair and an optional footer line. Its
// elapsed timer rides on that status row while "esc to interrupt" lives on a
// separate footer below the composer — no row ever carries both fragments
// (#6590).
func claudeFrame(statusRow, composer, footer string) string {
	return "❯ earlier turn\n" + statusRow +
		"────────────────────────────────\n" +
		composer +
		"────────────────────────────────\n" + footer
}

// claudeWorkingPane renders a mid-turn claude frame the way the fixtures draw
// it (claude_working_tool.ansi): a spinner-glyph row with the elapsed timer in
// the status slot, an idle composer, and the interrupt hint on the footer.
func claudeWorkingPane(elapsed string) string {
	return claudeFrame("* Bloviating… ("+elapsed+" · ↓ 108 tokens)\n", "❯ \n",
		"  ⏸ manual mode on · esc to interrupt · ← 2 agents\n")
}

// claudeIdlePane renders a settled claude frame: the finished turn's done
// summary stays in the transcript ("✻ Cogitated for 26s · done 11:09 PM") —
// transcript text, not a status row.
func claudeIdlePane() string {
	return claudeFrame("✻ Cogitated for 26s · done 11:09 PM\n", "❯ \n", "")
}

// The review's shape at pane level (the MHW-852 trace): the prompt's own echo
// plus ACP-init and skill-discovery lines render seconds after Enter as
// ordinary Updated churn. None of it is the agent mid-turn, and none of it may
// release the hold. Only the elapsed-timer row TICKING may.
func TestTurnWatch_DevinBootChurnNeverFires(t *testing.T) {
	w := NewTurnWatch("devin", time.Now())
	boot := []string{
		// Composer idle at the send's baseline.
		"❭ ",
		// The submitted prompt echoing into the transcript.
		"❭ run the nightly lint\n⠀⡆ Connecting…\n",
		// ACP init output.
		"❭ run the nightly lint\n⠀⡆ Connecting…\nACP session ready\n",
		// Skill discovery churn.
		"❭ run the nightly lint\nACP session ready\nDiscovered 4 skills\n❭ ",
	}
	for i, frame := range boot {
		if w.Observe(frame, time.Now()) {
			t.Fatalf("frame %d is boot/echo output, not a running turn:\n%s", i, frame)
		}
	}
}

// A status row that APPEARS without ever having ticked is text, not chrome —
// the same contract submittedTurnVisible applies inside the submit window.
func TestTurnWatch_DevinNewRowNeverFires(t *testing.T) {
	w := NewTurnWatch("devin", time.Now())
	frames := []string{
		"❭ ",
		// The row is new in this capture: nothing for it to have ticked from.
		"⠀⡆ Thinking · 3s (esc to interrupt)\n❭ [Pasted text]",
		// It then stands still.
		"⠀⡆ Thinking · 3s (esc to interrupt)\n❭ [Pasted text]",
	}
	for i, frame := range frames {
		if w.Observe(frame, time.Now()) {
			t.Fatalf("frame %d: a row with no earlier self that ticked is transcript text", i)
		}
	}
}

// The same row found in consecutive captures, timer advancing, is the turn —
// the proof a booting pane cannot fake. Its elapsed fits inside the boundary's
// age, so the turn began after the prompt went out.
func TestTurnWatch_DevinTickingRowFires(t *testing.T) {
	w := NewTurnWatch("devin", time.Now().Add(-time.Minute))
	if w.Observe("⠀⡆ Thinking · 3s (esc to interrupt)\n❭ ", time.Now()) {
		t.Fatal("the first capture establishes the row; it cannot already be a tick")
	}
	if w.Observe("⠀⡆ Thinking · 3s (esc to interrupt)\n❭ ", time.Now()) {
		t.Fatal("a row that stood still has not ticked")
	}
	if !w.Observe("⠀⡆ Thinking · 4s (esc to interrupt)\n❭ ", time.Now()) {
		t.Fatal("the same row with a growing timer is the agent's own in-turn proof")
	}
}

// Devin 3000.11.234 reworded its in-turn hint to "esc twice to interrupt"
// (#6879): the same timed row must still fire the watch. The frames below are
// real captures: the verb varies ("Thinking", "Running tools"), a context
// count rides the row's tail, and the spinner glyph cycles between captures —
// all of it is the same row.
func TestTurnWatch_DevinTickingRowFiresTwiceWording(t *testing.T) {
	w := NewTurnWatch("devin", time.Now().Add(-time.Minute))
	if w.Observe("⡎⢱ Thinking · 3s (esc twice to interrupt) · (174c)\n❭ ", time.Now()) {
		t.Fatal("the first capture establishes the row; it cannot already be a tick")
	}
	if w.Observe("⡜⢣ Thinking · 3s (esc twice to interrupt)\n❭ ", time.Now()) {
		t.Fatal("a row that stood still has not ticked")
	}
	if !w.Observe("⣄⠀ Running tools · 4s (esc twice to interrupt) · (190c)\n❭ ", time.Now()) {
		t.Fatal("the current devin status row ticking is the turn's own chrome")
	}
}

// Codex 0.163.0 draws "• Working (Ns • esc to interrupt)" ABOVE the composer —
// the same hint-and-timer-on-one-row shape devin has (#6879). It fires on a
// tick inside the boundary's age, not on presence: a quoted copy in the
// transcript can hold the shape but can never advance its timer.
func TestTurnWatch_CodexWorkingRowTickingFires(t *testing.T) {
	w := NewTurnWatch("codex", time.Now().Add(-time.Minute))
	working := func(elapsed string) string {
		return "› earlier prompt\n\n• Working (" + elapsed + " • esc to interrupt)\n\n› Ask Codex to do anything\n"
	}
	if w.Observe("› Ask Codex to do anything\n", time.Now()) {
		t.Fatal("an idle composer is not a turn")
	}
	if w.Observe(working("2s"), time.Now()) {
		t.Fatal("the row's first sighting establishes it; it cannot already be a tick")
	}
	if !w.Observe(working("3s"), time.Now()) {
		t.Fatal("codex's Working row ticking inside the boundary's age is its in-turn chrome")
	}
}

// A Working row already running when the boundary was set is an earlier turn:
// its own elapsed clock outdates the boundary, so it must never fire (#5221's
// contract, now that codex's row carries the same clock devin's does).
func TestTurnWatch_CodexWorkingRowPreBoundaryNeverFires(t *testing.T) {
	boundary := time.Now()
	w := NewTurnWatch("codex", boundary)
	for i, elapsed := range []string{"9m 1s", "9m 2s", "9m 3s"} {
		if w.Observe("• Working ("+elapsed+" • esc to interrupt)\n\n› Ask Codex to do anything\n",
			boundary.Add(time.Duration(i+1)*time.Second)) {
			t.Fatalf("elapsed %s exceeds the boundary's age: a pre-boundary turn is not the prompt's", elapsed)
		}
	}
}

// A transcript line quoting the Working row's shape can hold both fragments
// but can never tick — presence of the shape above the composer is not chrome.
func TestTurnWatch_CodexQuotedWorkingRowNeverFires(t *testing.T) {
	w := NewTurnWatch("codex", time.Now())
	frames := []string{
		"› Ask Codex to do anything\n",
		"the agent printed • Working (12s • esc to interrupt) in a log line\n› Ask Codex to do anything\n",
		"the agent printed • Working (12s • esc to interrupt) in a log line\n› Ask Codex to do anything\n",
	}
	for i, frame := range frames {
		if w.Observe(frame, time.Now()) {
			t.Fatalf("frame %d: a static quoted row is transcript text, not chrome", i)
		}
	}
}

func TestTurnWatch_ClaudeTickingRowFires(t *testing.T) {
	w := NewTurnWatch("claude", time.Now().Add(-time.Minute))
	w.Observe(claudeWorkingPane("1s"), time.Now())
	if !w.Observe(claudeWorkingPane("2s"), time.Now()) {
		t.Fatal("claude's advancing elapsed timer is its in-turn chrome")
	}
}

// The pane claude really draws splits the contract the same-row vocabulary
// assumed: the timer rides on the composer-scoped status row and the
// interrupt hint sits on a separate footer. A heartbeat-length turn must
// still fire — its scoped row ticking is the same evidence (#6590).
func TestTurnWatch_ClaudeScopedStatusRowFires(t *testing.T) {
	boundary := time.Now().Add(-time.Minute)
	w := NewTurnWatch("claude", boundary)
	w.Observe(claudeIdlePane(), boundary.Add(30*time.Second))
	w.Observe(claudeWorkingPane("1s"), boundary.Add(45*time.Second))
	if !w.Observe(claudeWorkingPane("2s"), boundary.Add(46*time.Second)) {
		t.Fatal("claude's scoped status row ticking inside the boundary's age is its in-turn chrome")
	}
}

// A turn running BEFORE the boundary keeps ticking on the same scoped row —
// but its own elapsed clock outdates the send, so it can never fire, real
// pane shape or not.
func TestTurnWatch_ClaudeScopedPreBoundaryTurnNeverFires(t *testing.T) {
	boundary := time.Now()
	w := NewTurnWatch("claude", boundary)
	for i, elapsed := range []string{"9m 1s", "9m 2s", "9m 3s"} {
		if w.Observe(claudeWorkingPane(elapsed), boundary.Add(time.Duration(i+1)*time.Second)) {
			t.Fatalf("elapsed %s exceeds the boundary's age: a pre-boundary turn is not the prompt's", elapsed)
		}
	}
}

// The finished turn's done summary is transcript text: it must never fire the
// watch, and a turn that already ended cannot launder a later one either.
func TestTurnWatch_ClaudeDoneRowNeverFires(t *testing.T) {
	w := NewTurnWatch("claude", time.Now().Add(-time.Minute))
	for i := 0; i < 3; i++ {
		if w.Observe(claudeIdlePane(), time.Now()) {
			t.Fatalf("capture %d: a done summary in the transcript is not a running turn", i)
		}
	}
}

// A turn already RUNNING when the boundary was set cannot count as the new
// prompt's turn: its row ticks, but its own elapsed says the turn predates the
// boundary — the case a rebuilt watch must not launder (#5221 review). Once
// that row's turn ends and a NEW one starts post-boundary, its elapsed is
// young enough to prove it.
func TestTurnWatch_PreBoundaryTurnNeverFires(t *testing.T) {
	// The unrelated turn began minutes before the prompt boundary.
	w := NewTurnWatch("devin", time.Now().Add(-30*time.Second))
	for _, elapsed := range []string{"4m12s", "4m13s", "4m14s"} {
		if w.Observe("⠀⡆ Thinking · "+elapsed+" (esc to interrupt)\n❭ ", time.Now()) {
			t.Fatalf("elapsed %s exceeds the boundary's age: a pre-boundary turn is not the prompt's", elapsed)
		}
	}
	// That turn ends; the redelivered prompt's own turn starts and ticks.
	if w.Observe("work so far\n❭ ", time.Now()) {
		t.Fatal("the pane going quiet is not a turn")
	}
	w.Observe("work so far\n⠀⡆ Thinking · 1s (esc to interrupt)\n❭ ", time.Now())
	if !w.Observe("work so far\n⠀⡆ Thinking · 2s (esc to interrupt)\n❭ ", time.Now()) {
		t.Fatal("a turn younger than the boundary's age is the prompt's own")
	}
}

// The scoped-indicator agents keep presence as their proof — their working
// frame is anchored to the live region, so it cannot be transcript text. But
// presence carries no clock: an indicator already up when the watch begins
// could be a pre-boundary turn, so it must be seen absent once first.
func TestTurnWatch_ScopedAgentsFireOnPresence(t *testing.T) {
	w := NewTurnWatch("codex", time.Now())
	if w.Observe("\x1b[2J\x1b[H› \r\n\r\n", time.Now()) {
		t.Fatal("a quiet frame is not a turn; it only arms the watch")
	}
	if !w.Observe("\x1b[2J\x1b[H› [Pasted Content]\r\n\r\n  esc to interrupt\r\n", time.Now()) {
		t.Fatal("codex draws its hint below the composer, where prose cannot reach")
	}
}

// The same watch built over a frame where the indicator is ALREADY up cannot
// attribute the turn to the boundary — wait for it to leave and return.
func TestTurnWatch_ScopedIndicatorPresentAtBoundaryDoesNotFire(t *testing.T) {
	w := NewTurnWatch("codex", time.Now())
	if w.Observe("\x1b[2J\x1b[H› [Pasted Content]\r\n\r\n  esc to interrupt\r\n", time.Now()) {
		t.Fatal("an indicator that never left the pane may predate the boundary")
	}
	if w.Observe("\x1b[2J\x1b[H› \r\n\r\n", time.Now()) {
		t.Fatal("the indicator leaving is the pre-boundary turn's end, not a fire")
	}
	if !w.Observe("\x1b[2J\x1b[H› [Pasted Content]\r\n\r\n  esc to interrupt\r\n", time.Now()) {
		t.Fatal("a fresh appearance after absence is a post-boundary start")
	}
}

// A signature-less agent has no pane proof; the watch never fires and the
// completion gate falls back to sustained quiet.
func TestTurnWatch_SignaturelessAgentNeverFires(t *testing.T) {
	w := NewTurnWatch("aider", time.Now())
	if w.Observe("some output\n✻ Whirring… (2s · esc to interrupt)\n> ", time.Now()) {
		t.Fatal("aider has no in-turn signature — even a matching row is unattributable")
	}
	if w.Observe("more output\n> ", time.Now()) {
		t.Fatal("no signature, no fire")
	}
}

// The boundary's age is measured at CAPTURE time, not apply time (#5221
// review): for a remote session the frame is captured inside the sandbox and
// processed after an unbounded transport delay — measuring at receipt would
// inflate the boundary's age until a pre-boundary turn's row fits inside it.
func TestTurnWatch_PreBoundaryTurnRejectsAtCaptureTime(t *testing.T) {
	boundary := time.Now()
	w := NewTurnWatch("devin", boundary)
	// A turn that began just before the boundary. Its row displays whole
	// seconds; at capture T+2s the boundary is only 2s old, so a row showing
	// 3s elapsed cannot have begun after it — even if the daemon only applies
	// the frame seconds later.
	w.Observe("⠀⡆ Thinking · 2s (esc to interrupt)\n❭ ", boundary.Add(1500*time.Millisecond))
	if w.Observe("⠀⡆ Thinking · 3s (esc to interrupt)\n❭ ", boundary.Add(2*time.Second)) {
		t.Fatal("a row older than the boundary at capture must not fire, whatever the apply delay")
	}
	// After the unrelated turn ends, a young row is the prompt's own turn.
	w.Observe("work so far\n❭ ", boundary.Add(4*time.Second))
	w.Observe("work so far\n⠀⡆ Thinking · 1s (esc to interrupt)\n❭ ", boundary.Add(5*time.Second))
	if !w.Observe("work so far\n⠀⡆ Thinking · 2s (esc to interrupt)\n❭ ", boundary.Add(6*time.Second)) {
		t.Fatal("a turn younger than the boundary's capture-age still fires")
	}
}

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
		if w.Observe(frame, time.Now(), time.Now()) {
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
		if w.Observe(frame, time.Now(), time.Now()) {
			t.Fatalf("frame %d: a row with no earlier self that ticked is transcript text", i)
		}
	}
}

// The same row found in consecutive captures, timer advancing, is the turn —
// the proof a booting pane cannot fake. Its elapsed fits inside the boundary's
// age, so the turn began after the prompt went out.
func TestTurnWatch_DevinTickingRowFires(t *testing.T) {
	w := NewTurnWatch("devin", time.Now().Add(-time.Minute))
	if w.Observe("⠀⡆ Thinking · 3s (esc to interrupt)\n❭ ", time.Now(), time.Now()) {
		t.Fatal("the first capture establishes the row; it cannot already be a tick")
	}
	if w.Observe("⠀⡆ Thinking · 3s (esc to interrupt)\n❭ ", time.Now(), time.Now()) {
		t.Fatal("a row that stood still has not ticked")
	}
	if !w.Observe("⠀⡆ Thinking · 4s (esc to interrupt)\n❭ ", time.Now(), time.Now()) {
		t.Fatal("the same row with a growing timer is the agent's own in-turn proof")
	}
}

func TestTurnWatch_ClaudeTickingRowFires(t *testing.T) {
	w := NewTurnWatch("claude", time.Now().Add(-time.Minute))
	w.Observe("transcript\n✻ Whirring… (1s · esc to interrupt)\n❯ ", time.Now(), time.Now())
	if !w.Observe("transcript\n✻ Whirring… (2s · esc to interrupt)\n❯ ", time.Now(), time.Now()) {
		t.Fatal("claude's advancing elapsed timer is its in-turn chrome")
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
		if w.Observe("⠀⡆ Thinking · "+elapsed+" (esc to interrupt)\n❭ ", time.Now(), time.Now()) {
			t.Fatalf("elapsed %s exceeds the boundary's age: a pre-boundary turn is not the prompt's", elapsed)
		}
	}
	// That turn ends; the redelivered prompt's own turn starts and ticks.
	if w.Observe("work so far\n❭ ", time.Now(), time.Now()) {
		t.Fatal("the pane going quiet is not a turn")
	}
	w.Observe("work so far\n⠀⡆ Thinking · 1s (esc to interrupt)\n❭ ", time.Now(), time.Now())
	if !w.Observe("work so far\n⠀⡆ Thinking · 2s (esc to interrupt)\n❭ ", time.Now(), time.Now()) {
		t.Fatal("a turn younger than the boundary's age is the prompt's own")
	}
}

// A proven-idle boundary stands the elapsed fit down (#5221 review): the
// pane provably held no turn at the prompt, so a ticking row can only be the
// new turn — including one whose own clock started a fraction of a second
// after the boundary, which the fit could never prove: its elapsed is always
// the boundary's age minus the sub-second offset.
func TestTurnWatch_PrimedTimedRowSkipsElapsedFit(t *testing.T) {
	boundary := time.Now()
	// A turn starting ~200ms post-boundary shows a row whose elapsed trails
	// the boundary's age by that offset — 2s elapsed against 2.2s of age
	// fails the +1s margin, on every tick, forever.
	frame := func(elapsed string) string {
		return "transcript\n✻ Whirring… (" + elapsed + " · esc to interrupt)\n❯ "
	}
	unprimed := NewTurnWatch("claude", boundary)
	unprimed.Observe(frame("1s"), boundary.Add(1200*time.Millisecond), boundary.Add(1200*time.Millisecond))
	if unprimed.Observe(frame("2s"), boundary.Add(2200*time.Millisecond), boundary.Add(2200*time.Millisecond)) {
		t.Fatal("unprimed: a sub-second-post-boundary turn cannot satisfy the elapsed fit")
	}
	primed := NewTurnWatch("claude", boundary)
	primed.PrimeProvenIdle()
	primed.Observe(frame("1s"), boundary.Add(1200*time.Millisecond), boundary.Add(1200*time.Millisecond))
	if !primed.Observe(frame("2s"), boundary.Add(2200*time.Millisecond), boundary.Add(2200*time.Millisecond)) {
		t.Fatal("a ticking row after a proven-idle boundary is the turn — no fit needed")
	}
}

// The scoped-indicator agents keep presence as their proof — their working
// frame is anchored to the live region, so it cannot be transcript text. But
// presence carries no clock: an indicator already up when the watch begins
// could be a pre-boundary turn, so it must be seen absent — for the arm
// grace, not a lone frame — before a present frame can count.
func TestTurnWatch_ScopedAgentsFireOnPresence(t *testing.T) {
	w := NewTurnWatch("codex", time.Now())
	t0 := time.Now()
	if w.Observe("\x1b[2J\x1b[H› \r\n\r\n", t0, t0) {
		t.Fatal("a quiet frame is not a turn; it only opens the absence")
	}
	if w.Observe("\x1b[2J\x1b[H› \r\n\r\n", t0.Add(turnIndicatorAbsentGrace), t0.Add(turnIndicatorAbsentGrace)) {
		t.Fatal("a persisted absence arms the watch but is still not a turn")
	}
	if !w.Observe("\x1b[2J\x1b[H› [Pasted Content]\r\n\r\n  esc to interrupt\r\n", t0.Add(turnIndicatorAbsentGrace+time.Second), t0.Add(turnIndicatorAbsentGrace+time.Second)) {
		t.Fatal("codex draws its hint below the composer, where prose cannot reach")
	}
}

// The same watch built over a frame where the indicator is ALREADY up cannot
// attribute the turn to the boundary — wait for it to leave, stay gone, and
// return.
func TestTurnWatch_ScopedIndicatorPresentAtBoundaryDoesNotFire(t *testing.T) {
	w := NewTurnWatch("codex", time.Now())
	t0 := time.Now()
	if w.Observe("\x1b[2J\x1b[H› [Pasted Content]\r\n\r\n  esc to interrupt\r\n", t0, t0) {
		t.Fatal("an indicator that never left the pane may predate the boundary")
	}
	if w.Observe("\x1b[2J\x1b[H› \r\n\r\n", t0.Add(time.Second), t0.Add(time.Second)) {
		t.Fatal("the indicator leaving is not yet the pre-boundary turn's end")
	}
	if w.Observe("\x1b[2J\x1b[H› \r\n\r\n", t0.Add(time.Second+turnIndicatorAbsentGrace), t0.Add(time.Second+turnIndicatorAbsentGrace)) {
		t.Fatal("the absence persisting is the pre-boundary turn's end, not a fire")
	}
	if !w.Observe("\x1b[2J\x1b[H› [Pasted Content]\r\n\r\n  esc to interrupt\r\n", t0.Add(2*time.Second+turnIndicatorAbsentGrace), t0.Add(2*time.Second+turnIndicatorAbsentGrace)) {
		t.Fatal("a fresh appearance after sustained absence is a post-boundary start")
	}
}

// The arm must survive a REPAINT gap (#5221 review): an indicator present at
// the boundary belongs to an in-flight pre-boundary turn, and a repaint that
// drops it for a frame or two is not that turn's end. Only an absence that
// persists the whole grace arms the watch — the old indicator returning
// inside the window is the old turn continuing, not the new prompt's.
func TestTurnWatch_RepaintGapDoesNotArm(t *testing.T) {
	w := NewTurnWatch("codex", time.Now())
	t0 := time.Now()
	// The pre-boundary turn is still up when the watch begins.
	w.Observe("\x1b[2J\x1b[H› [Pasted Content]\r\n\r\n  esc to interrupt\r\n", t0, t0)
	// A repaint drops the indicator for one frame.
	if w.Observe("\x1b[2J\x1b[H› \r\n\r\n", t0.Add(time.Second), t0.Add(time.Second)) {
		t.Fatal("a lone absent frame inside a running turn is a repaint gap")
	}
	// The SAME old turn's indicator redraws — it must not pass as the new
	// prompt's turn.
	if w.Observe("\x1b[2J\x1b[H› [Pasted Content]\r\n\r\n  esc to interrupt\r\n", t0.Add(2*time.Second), t0.Add(2*time.Second)) {
		t.Fatal("the pre-boundary turn's redrawn indicator is not the new prompt's turn")
	}
	// The old turn then genuinely ends — the absence persists past the grace.
	w.Observe("\x1b[2J\x1b[H› \r\n\r\n", t0.Add(3*time.Second), t0.Add(3*time.Second))
	w.Observe("\x1b[2J\x1b[H› \r\n\r\n", t0.Add(3*time.Second+turnIndicatorAbsentGrace), t0.Add(3*time.Second+turnIndicatorAbsentGrace))
	// The redelivered prompt's own turn begins and is seen.
	if !w.Observe("\x1b[2J\x1b[H› [Pasted Content]\r\n\r\n  esc to interrupt\r\n", t0.Add(4*time.Second+turnIndicatorAbsentGrace), t0.Add(4*time.Second+turnIndicatorAbsentGrace)) {
		t.Fatal("an indicator returning after a real turn end is the new prompt's turn")
	}
}

// The absence grace clocks what was OBSERVED, not what was in flight
// (#5221 review): remote transport can stretch a request's start far ahead
// of its capture's landing, so two absent frames whose request-starts span
// the grace but whose completions do not are a repaint gap seen slowly —
// never an arm.
func TestTurnWatch_TransportTimeDoesNotArmAbsence(t *testing.T) {
	w := NewTurnWatch("codex", time.Now())
	t0 := time.Now()
	// Two absent frames a second apart in reality — but the second request
	// left eight seconds before the first's capture landed. Request-start
	// timestamps would call the absence nine seconds old; the clock that
	// matters is the second one.
	w.Observe("\x1b[2J\x1b[H› \r\n\r\n", t0, t0)
	w.Observe("\x1b[2J\x1b[H› \r\n\r\n", t0.Add(8*time.Second), t0.Add(time.Second))
	// The old turn's indicator returns immediately after — it never ended.
	if w.Observe("\x1b[2J\x1b[H› [Pasted Content]\r\n\r\n  esc to interrupt\r\n", t0.Add(9*time.Second), t0.Add(2*time.Second)) {
		t.Fatal("transport latency must not stretch a repaint gap into the arm grace")
	}
	// Observed absence that genuinely persists still arms.
	w.Observe("\x1b[2J\x1b[H› \r\n\r\n", t0.Add(10*time.Second), t0.Add(3*time.Second))
	w.Observe("\x1b[2J\x1b[H› \r\n\r\n", t0.Add(11*time.Second), t0.Add(3*time.Second+turnIndicatorAbsentGrace))
	if !w.Observe("\x1b[2J\x1b[H› [Pasted Content]\r\n\r\n  esc to interrupt\r\n", t0.Add(12*time.Second), t0.Add(4*time.Second+turnIndicatorAbsentGrace)) {
		t.Fatal("an indicator returning after observed-sustained absence is the new prompt's turn")
	}
}

// A signature-less agent has no pane proof; the watch never fires and the
// completion gate falls back to sustained quiet.
func TestTurnWatch_SignaturelessAgentNeverFires(t *testing.T) {
	w := NewTurnWatch("aider", time.Now())
	if w.Observe("some output\n✻ Whirring… (2s · esc to interrupt)\n> ", time.Now(), time.Now()) {
		t.Fatal("aider has no in-turn signature — even a matching row is unattributable")
	}
	if w.Observe("more output\n> ", time.Now(), time.Now()) {
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
	w.Observe("⠀⡆ Thinking · 2s (esc to interrupt)\n❭ ", boundary.Add(1500*time.Millisecond), boundary.Add(1500*time.Millisecond))
	if w.Observe("⠀⡆ Thinking · 3s (esc to interrupt)\n❭ ", boundary.Add(2*time.Second), boundary.Add(2*time.Second)) {
		t.Fatal("a row older than the boundary at capture must not fire, whatever the apply delay")
	}
	// After the unrelated turn ends, a young row is the prompt's own turn.
	w.Observe("work so far\n❭ ", boundary.Add(4*time.Second), boundary.Add(4*time.Second))
	w.Observe("work so far\n⠀⡆ Thinking · 1s (esc to interrupt)\n❭ ", boundary.Add(5*time.Second), boundary.Add(5*time.Second))
	if !w.Observe("work so far\n⠀⡆ Thinking · 2s (esc to interrupt)\n❭ ", boundary.Add(6*time.Second), boundary.Add(6*time.Second)) {
		t.Fatal("a turn younger than the boundary's capture-age still fires")
	}
}

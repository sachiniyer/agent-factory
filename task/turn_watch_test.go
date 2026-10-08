package task

import (
	"testing"
)

// TurnWatch feeds the task-run turn boundary (#5219): the daemon folds each
// status-poll capture through it, and only a firing watch — the agent's own
// in-turn chrome — may release a held idle edge. Everything a still-booting
// pane can produce must therefore NOT fire it.

// The review's shape at pane level (the MHW-852 trace): the prompt's own echo
// plus ACP-init and skill-discovery lines render seconds after Enter as
// ordinary Updated churn. None of it is the agent mid-turn, and none of it may
// release the hold. Only the elapsed-timer row TICKING may.
func TestTurnWatch_DevinBootChurnNeverFires(t *testing.T) {
	w := NewTurnWatch("devin")
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
		if w.Observe(frame) {
			t.Fatalf("frame %d is boot/echo output, not a running turn:\n%s", i, frame)
		}
	}
}

// A status row that APPEARS without ever having ticked is text, not chrome —
// the same contract submittedTurnVisible applies inside the submit window.
func TestTurnWatch_DevinNewRowNeverFires(t *testing.T) {
	w := NewTurnWatch("devin")
	frames := []string{
		"❭ ",
		// The row is new in this capture: nothing for it to have ticked from.
		"⠀⡆ Thinking · 3s (esc to interrupt)\n❭ [Pasted text]",
		// It then stands still.
		"⠀⡆ Thinking · 3s (esc to interrupt)\n❭ [Pasted text]",
	}
	for i, frame := range frames {
		if w.Observe(frame) {
			t.Fatalf("frame %d: a row with no earlier self that ticked is transcript text", i)
		}
	}
}

// The same row found in consecutive captures, timer advancing, is the turn —
// the proof a booting pane cannot fake.
func TestTurnWatch_DevinTickingRowFires(t *testing.T) {
	w := NewTurnWatch("devin")
	if w.Observe("⠀⡆ Thinking · 3s (esc to interrupt)\n❭ ") {
		t.Fatal("the first capture establishes the row; it cannot already be a tick")
	}
	if w.Observe("⠀⡆ Thinking · 3s (esc to interrupt)\n❭ ") {
		t.Fatal("a row that stood still has not ticked")
	}
	if !w.Observe("⠀⡆ Thinking · 4s (esc to interrupt)\n❭ ") {
		t.Fatal("the same row with a growing timer is the agent's own in-turn proof")
	}
}

func TestTurnWatch_ClaudeTickingRowFires(t *testing.T) {
	w := NewTurnWatch("claude")
	w.Observe("transcript\n✻ Whirring… (1s · esc to interrupt)\n❯ ")
	if !w.Observe("transcript\n✻ Whirring… (2s · esc to interrupt)\n❯ ") {
		t.Fatal("claude's advancing elapsed timer is its in-turn chrome")
	}
}

// The scoped-indicator agents keep presence as their proof — their working
// frame is anchored to the live region, so it cannot be transcript text.
func TestTurnWatch_ScopedAgentsFireOnPresence(t *testing.T) {
	w := NewTurnWatch("codex")
	if !w.Observe("\x1b[2J\x1b[H› [Pasted Content]\r\n\r\n  esc to interrupt\r\n") {
		t.Fatal("codex draws its hint below the composer, where prose cannot reach")
	}
}

// A signature-less agent has no pane proof; the watch never fires and the
// completion gate falls back to sustained quiet.
func TestTurnWatch_SignaturelessAgentNeverFires(t *testing.T) {
	w := NewTurnWatch("aider")
	if w.Observe("some output\n✻ Whirring… (2s · esc to interrupt)\n> ") {
		t.Fatal("aider has no in-turn signature — even a matching row is unattributable")
	}
	if w.Observe("more output\n> ") {
		t.Fatal("no signature, no fire")
	}
}

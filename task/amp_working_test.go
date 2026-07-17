package task

import (
	"os"
	"testing"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// TestIsWorkingContentAmpRealCapture pins the amp status-flash fix to the ACTUAL
// bytes an amp pane produces mid-turn. The fixtures are real
// `tmux capture-pane -p -e -J` captures (the exact flags CapturePaneContent uses)
// of amp 0.0.1784160980 driven through a live turn:
//
//   - amp_working_streaming.ansi — mid-turn, bottom rule "╰ ∼ Streaming ────…"
//   - amp_working_thinking.ansi  — mid-turn, bottom rule "╰ ∼ Thinking ────…".
//     This is THE bug capture: it was sampled on a tick whose content was
//     byte-identical to the previous tick, so the daemon poll saw no churn, took
//     its idle branch and painted the green "waiting for you" dot while amp was
//     still thinking. Three consecutive ticks did this in the live repro.
//   - amp_idle_after_turn.ansi   — same session once the turn completed: the
//     status segment is gone and the rule closes bare ("╰────…").
//
// The working/idle discriminator is therefore structural (is there a status
// segment between the corner and the rule?) rather than a list of amp's verbs —
// see ampWorkingIndicator.
func TestIsWorkingContentAmpRealCapture(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		want    bool
	}{
		{"amp streaming mid-turn", "testdata/amp_working_streaming.ansi", true},
		{"amp thinking behind a still pane (the flash capture)", "testdata/amp_working_thinking.ansi", true},
		{"amp idle after the turn completed", "testdata/amp_idle_after_turn.ansi", false},
		// The #1756 readiness fixtures are all genuinely-idle panes: none may
		// report working, or a create would never settle.
		{"amp ready frame", "testdata/amp_ready.ansi", false},
		{"amp ready frame with cost decoration", "testdata/amp_ready_cost.ansi", false},
		{"amp blank loading pane", "testdata/amp_loading.ansi", false},
		{"amp welcome banner, no input box", "testdata/amp_banner_only.ansi", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := os.ReadFile(tc.fixture)
			if err != nil {
				t.Fatalf("read fixture %s: %v", tc.fixture, err)
			}
			if got := IsWorkingContent(string(raw), tmux.ProgramAmp); got != tc.want {
				t.Errorf("IsWorkingContent(%s, amp) = %v, want %v", tc.fixture, got, tc.want)
			}
		})
	}
}

// TestIsWorkingContentAmpReadinessUnaffected guards the #1756 create path: the
// working check is layered ON TOP of readiness, never in place of it, so every
// genuinely-idle amp pane must still read ready. A regression here is an amp
// create spinning the full 60s readiness timeout — the exact failure #1756 fixed.
func TestIsWorkingContentAmpReadinessUnaffected(t *testing.T) {
	for _, fixture := range []string{"testdata/amp_ready.ansi", "testdata/amp_ready_cost.ansi"} {
		raw, err := os.ReadFile(fixture)
		if err != nil {
			t.Fatalf("read fixture %s: %v", fixture, err)
		}
		if !isReadyContent(string(raw), tmux.ProgramAmp) {
			t.Errorf("isReadyContent(%s, amp) = false, want true — amp create would spin the readiness timeout (#1756)", fixture)
		}
	}
}

// TestIsWorkingContentOtherAgents pins the blast radius. Only amp and opencode
// publish an in-progress indicator this can read, so every OTHER agent must answer
// false and keep the poll's pane-churn inference exactly as it was — including the
// "" (non-agent program_overrides, #1131) case.
//
// The agents list is therefore the set with genuinely no case in IsWorkingContent.
// opencode is deliberately absent from it: it grew a case, and the fixture guards
// below assert that positively rather than leaving its removal from this list
// looking like an oversight.
func TestIsWorkingContentOtherAgents(t *testing.T) {
	ampWorking, err := os.ReadFile("testdata/amp_working_streaming.ansi")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	opencodeWorking, err := os.ReadFile("testdata/opencode_working.ansi")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	// Both fixtures must read as working for THEIR OWN agent, or every negative
	// below would pass for the wrong reason — a truncated or stale fixture that
	// shows no indicator at all reads false for everyone, including its owner.
	if !IsWorkingContent(string(ampWorking), tmux.ProgramAmp) {
		t.Fatal("amp working fixture no longer reads as working for amp; the negatives below would prove nothing")
	}
	if !IsWorkingContent(string(opencodeWorking), tmux.ProgramOpencode) {
		t.Fatal("opencode working fixture no longer reads as working for opencode; the negatives below would prove nothing")
	}

	agents := []string{
		tmux.ProgramClaude, tmux.ProgramCodex, tmux.ProgramAider, tmux.ProgramGemini, "",
	}
	for _, agent := range agents {
		name := agent
		if name == "" {
			name = "<no known agent>"
		}
		t.Run(name, func(t *testing.T) {
			// Even handed the working pane of an agent that DOES publish an
			// indicator, a caseless agent must not claim to be working: the
			// indicator is that agent's own UI, and no other agent's pane is being
			// interpreted here.
			if IsWorkingContent(string(ampWorking), agent) {
				t.Errorf("IsWorkingContent(amp working pane, %q) = true, want false", name)
			}
			if IsWorkingContent(string(opencodeWorking), agent) {
				t.Errorf("IsWorkingContent(opencode working pane, %q) = true, want false", name)
			}
			if IsWorkingContent("some busy looking output\n", agent) {
				t.Errorf("IsWorkingContent(arbitrary content, %q) = true, want false", name)
			}
		})
	}
}

// TestIsWorkingContentAmpStaleScrollback covers the mirror-image bug the
// contiguity gate prevents. A finished turn can leave a "╰ ∼ Streaming ─…" rule
// behind in the pane above the CURRENT idle frame; keying off that stale line
// would pin a genuinely idle session at Running forever — a dot that never turns
// green, which is just as wrong as one that flashes.
func TestIsWorkingContentAmpStaleScrollback(t *testing.T) {
	idle, err := os.ReadFile("testdata/amp_idle_after_turn.ansi")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	stale := "╰ ∼ Streaming ──────────────────────────── old/repo (master) ─╯\n" + string(idle)
	if IsWorkingContent(stale, tmux.ProgramAmp) {
		t.Error("IsWorkingContent(stale working rule above a current idle frame, amp) = true, want false — a settled session would never go green again")
	}
}

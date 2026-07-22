package task

import (
	"strings"
	"testing"
)

// The devin panes below are transcribed from driving the real devin 3000.2.17
// binary in an isolated PTY (pyte-rendered) in a throwaway git repo — the same
// states TmuxSession.CapturePaneContent feeds isReadyContent in production. Only
// the load-bearing lines are kept (the composer glyph "❭", the header, the
// working indicator); the box rules are truncated for readability. isReadyContent
// matches the raw "❭" glyph (no ANSI strip, like codex's "›"), so a transcribed
// pane is a faithful input for the substring the detector actually keys on.

// devinReady is devin sitting at its composer, accepting input.
const devinReady = "⠀⣴⣾⣶⡄\n" +
	"⠀⠛⠿⠟⠻⣶⣾⣶⡄  Devin CLI\n" +
	"⠀⣤⣶⣦⣴⠿⢿⠿⠃  v3000.2.17 · Teams · 59% remaining (resets in 3d 11h)\n" +
	"\n" +
	"────────────────────────────────────────\n" +
	"❭ Ask Devin to build features, fix bugs, or work on your code\n" +
	"────────────────────────────────────────\n" +
	"SWE-1.7 Max                 Context: 11k / 262k tokens (4%)\n"

// devinWorking is devin mid-turn: the composer is still present (so the pane is
// past boot and "ready" in the isReadyContent sense — the daemon's churn poll,
// not isReadyContent, decides idle-vs-running among ready panes).
const devinWorking = "❭ Reply with exactly one word: pong\n" +
	"\n" +
	"⠀⡆ Thinking · 3s (esc to interrupt) · (425c · ctrl+o for details)\n" +
	"────────────────────────────────────────\n" +
	"❭ Guide Devin while it works\n" +
	"────────────────────────────────────────\n" +
	"SWE-1.7 Max\n"

// devinBooting is the pane during devin's boot. devin clears the alt-screen and
// paints the whole frame (header + composer) at once, so the pre-composer window
// is genuinely blank rather than a partial splash (observed on 3000.2.17).
const devinBooting = "\n\n\n\n"

func TestIsReadyContentDevin(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"composer visible, accepting input", devinReady, true},
		// Mid-turn still shows the composer, so it reads ready — same as codex,
		// whose "›" also persists while a turn runs.
		{"composer visible while a turn is in flight", devinWorking, true},
		// Pre-composer boot is blank, so not ready.
		{"blank pane, devin still booting", devinBooting, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isReadyContent(tc.content, "devin"); got != tc.want {
				t.Errorf("isReadyContent(devin, %s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestDevinReadyIsStricterThanTheDefaultBranch pins WHY devin has its own
// isReadyContent case rather than falling to the default "any non-blank output"
// branch. devin 3000.2.17 boots atomically (blank → full frame), so on TODAY's
// binary the default branch happens to agree on the panes above. The explicit
// case earns its place on a HYPOTHETICAL that the default cannot survive: a boot
// that prints a line before the composer paints — a connecting/update/MOTD
// notice, entirely plausible for a networked agent. The default branch would read
// that as ready and af would type the first prompt into a composerless TUI (the
// #1131 class opencode's case documents). The "❭" check waits for the composer.
//
// This frame is SYNTHETIC and labeled as such — not a captured devin state — so
// it makes no false claim about what devin prints; it only proves the devin case
// is strictly the composer signal, not "printed anything".
func TestDevinReadyIsStricterThanTheDefaultBranch(t *testing.T) {
	preComposerBanner := "Devin CLI v3000.2.17\nConnecting to Devin…\n"

	if strings.TrimSpace(preComposerBanner) == "" {
		t.Fatal("the hypothetical banner must be non-blank for this guard to mean anything")
	}
	if strings.Contains(preComposerBanner, "❭") {
		t.Fatal("the hypothetical banner must NOT contain the composer glyph")
	}
	// The default (unknown-agent) branch calls any non-blank pane ready.
	if !isReadyContent(preComposerBanner, "some-unknown-agent") {
		t.Fatal("expected the default branch to read a non-blank pre-composer pane as ready; if not, this guard is stale")
	}
	// devin's case does not: no composer glyph, not ready.
	if isReadyContent(preComposerBanner, "devin") {
		t.Error("devin must NOT read as ready before its composer paints — af would type the first prompt into a still-booting TUI")
	}
}

// TestIsWorkingContentDevinFallsToChurn documents that devin gets NO
// IsWorkingContent case. devin repaints its "Thinking · Ns" footer every second
// while a turn runs (an incrementing timer + spinner, observed on 3000.2.17), so
// the daemon's pane-churn inference classifies it as working — exactly as it does
// for codex and claude, which also repaint continuously and have no case here. A
// positive signal is only added for agents that hold their pane STILL mid-turn
// (amp, opencode). If a future devin stops repainting mid-turn, add a case keyed
// on its "esc to interrupt" marker (scoped below the composer, like opencode's).
func TestIsWorkingContentDevinFallsToChurn(t *testing.T) {
	// No case → the default arm returns false; the daemon relies on churn.
	if IsWorkingContent(devinWorking, "devin") {
		t.Error("devin has no IsWorkingContent case by design; it must fall through to false and let churn inference decide")
	}
}

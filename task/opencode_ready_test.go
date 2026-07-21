package task

import (
	"os"
	"strings"
	"testing"
)

// opencodeFixture reads a real opencode pane capture. Every testdata/opencode_*.ansi
// file is an actual `tmux capture-pane -p -e -J` of opencode 0.0.0-main-202604230742
// — the same capture flags (escapes preserved, wrapped lines joined) that
// TmuxSession.CapturePaneContent feeds to isReadyContent/IsWorkingContent in
// production, so these bytes are what the detectors really see.
func opencodeFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(raw)
}

// TestOpencodeIndicatorsAreNotContiguousInRawCapture pins WHY both opencode
// matchers strip ANSI before matching, rather than substring-checking the raw pane.
//
// opencode colorizes its status bar and its composer corner word-by-word and
// glyph-by-glyph, so a truecolor escape sits INSIDE each indicator: the pane
// carries "esc \x1b[38;2;128;128;128minterrupt", never the literal "esc interrupt",
// and "\x1b[38;2;92;156;245m╹\x1b[…m▀▀▀", never the literal "╹▀▀". A
// strings.Contains(content, "esc interrupt") on the captured pane therefore matches
// NOTHING, and a "╹▀▀" match on the raw pane never fires — opencode would never read
// ready and every create would spin the full 60s readiness timeout.
//
// This is not hypothetical: it is exactly how amp's frame regex broke in the wild
// (see paneAnsiEscape's comment). This test fails the moment someone "simplifies"
// either matcher back onto the raw bytes.
func TestOpencodeIndicatorsAreNotContiguousInRawCapture(t *testing.T) {
	working := opencodeFixture(t, "opencode_working.ansi")
	if strings.Contains(working, "esc interrupt") {
		t.Fatal("fixture unexpectedly holds a contiguous \"esc interrupt\"; the ANSI-stripping rationale needs re-verifying against a fresh capture")
	}
	if strings.Contains(working, "╹▀▀") {
		t.Fatal("fixture unexpectedly holds a contiguous \"╹▀▀\"; the ANSI-stripping rationale needs re-verifying against a fresh capture")
	}

	// Stripped, both indicators are present — so the strip is what makes the
	// matchers work at all.
	plain := paneAnsiEscape.ReplaceAllString(working, "")
	if !strings.Contains(plain, "esc interrupt") {
		t.Error("after ANSI stripping the working pane must expose \"esc interrupt\"")
	}
	if !strings.Contains(plain, "╹▀▀") {
		t.Error("after ANSI stripping the working pane must expose the composer bottom rule \"╹▀▀\"")
	}
}

// TestIsOpencodePromptFrameRealCapture pins readiness to the real bytes of every
// opencode boot state, including the two panes a naive check gets wrong.
func TestIsOpencodePromptFrameRealCapture(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		want    bool
	}{
		{"composer visible, accepting input", "opencode_ready.ansi", true},
		// A pane mid-turn and a pane after a finished turn both still show the
		// composer: opencode accepts input in both, so both are ready.
		{"composer visible while a turn is in flight", "opencode_working.ansi", true},
		{"composer visible after a turn finished", "opencode_finished.ansi", true},
		// The first ~2.7s of boot: the pane is genuinely empty.
		{"blank pane, opencode still booting", "opencode_blank.ansi", false},
		// ~2.8-3.2s of boot: ~3.9KB of colour-setup escapes and NOTHING drawn yet.
		// This is the pane that makes this whole case load-bearing — see
		// TestOpencodeReadyDefaultBranchWouldTypeIntoBootingTUI.
		{"escapes-only pane, composer not painted yet", "opencode_booting.ansi", false},
		// opencode's banner is ASCII art built from "█"/"▀" ("█▀▀█ █▀▀█ …"). A bare
		// "▀" readiness check would fire here — while opencode is still starting.
		{"banner art only, no composer", "opencode_banner.ansi", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			content := opencodeFixture(t, tc.fixture)
			if got := isOpencodePromptFrame(content); got != tc.want {
				t.Errorf("isOpencodePromptFrame(%s) = %v, want %v", tc.fixture, got, tc.want)
			}
			// isReadyContent must agree via the opencode branch — that is the seam
			// WaitForReady actually calls.
			if got := isReadyContent(content, "opencode"); got != tc.want {
				t.Errorf("isReadyContent(%s, opencode) = %v, want %v", tc.fixture, got, tc.want)
			}
		})
	}
}

// TestOpencodeBannerDoesNotFalsePositiveOnBlockGlyphs is the explicit guard for the
// banner trap: the banner really does contain the block glyphs a loose frame check
// would key on, yet must not read as ready. Asserting the glyphs are PRESENT is the
// point — otherwise the negative above could pass for the wrong reason (e.g. if a
// future fixture simply lost its banner).
func TestOpencodeBannerDoesNotFalsePositiveOnBlockGlyphs(t *testing.T) {
	banner := paneAnsiEscape.ReplaceAllString(opencodeFixture(t, "opencode_banner.ansi"), "")

	if !strings.Contains(banner, "▀") {
		t.Fatal("banner fixture no longer contains \"▀\" — it can no longer prove the false-positive guard")
	}
	if strings.Contains(banner, "╹") {
		t.Fatal("banner fixture unexpectedly contains \"╹\"; the choice of ╹ as the composer marker needs re-verifying")
	}
	if isOpencodePromptFrame(opencodeFixture(t, "opencode_banner.ansi")) {
		t.Error("opencode's ▀/█ banner art must not read as a composer frame")
	}
}

// TestOpencodeReadyDefaultBranchWouldTypeIntoBootingTUI is the REPRO for the silent
// failure this case exists to prevent, and the reason opencode could not simply be
// added to SupportedPrograms and left to the default branch.
//
// Without a tmux.ProgramOpencode case, isReadyContent falls to default: "the pane
// printed any non-blank output". opencode's boot has a real window (~2.8-3.2s on
// 0.0.0-main-202604230742) where the pane holds ~3.9KB of pure colour-setup escapes
// and nothing else. strings.TrimSpace does not treat ESC as whitespace, so that
// default call returns ready ~0.3s BEFORE the composer paints — and af types the
// first prompt into a TUI that is not listening.
//
// The test asserts both halves: the default branch really does misfire on this pane
// (so the bug is real, not theoretical), and the opencode branch does not (so the
// fix works). Deleting the ProgramOpencode case from isReadyContent makes the second
// assertion fail.
func TestOpencodeReadyDefaultBranchWouldTypeIntoBootingTUI(t *testing.T) {
	booting := opencodeFixture(t, "opencode_booting.ansi")

	if strings.TrimSpace(booting) == "" {
		t.Fatal("booting fixture trims to empty — it no longer reproduces the escapes-only pane")
	}
	if strings.Contains(paneAnsiEscape.ReplaceAllString(booting, ""), "╹") {
		t.Fatal("booting fixture already shows a composer — it no longer captures the pre-composer window")
	}

	// The bug: an agent with no isReadyContent case reads this booting pane as ready.
	if !isReadyContent(booting, "some-unknown-agent") {
		t.Fatal("expected the default branch to misfire on the escapes-only pane; if it no longer does, this repro is stale")
	}
	// The fix: opencode's own case waits for the composer.
	if isReadyContent(booting, "opencode") {
		t.Error("opencode must NOT read as ready while only colour-setup escapes have been emitted — af would type the first prompt into a still-booting TUI")
	}
}

// TestIsOpencodePromptFrameRequiresContiguousFrame pins the contiguity walk. A "╹"
// rule must be attached to the composer's "┃" box interior to count. A bare rule
// glyph sitting in agent output (or a partial redraw) is not a live composer.
func TestIsOpencodePromptFrameRequiresContiguousFrame(t *testing.T) {
	orphan := "some agent output mentioning a rule glyph\n" +
		"╹▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀\n" +
		"more output\n"
	if isOpencodePromptFrame(orphan) {
		t.Error("a bottom rule with no ┃ box interior above it must not read as a live composer")
	}

	live := "┃\n" +
		"┃  Ask anything…\n" +
		"┃  Build · Claude Opus 4.5 (latest) Anthropic\n" +
		"╹▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀\n"
	if !isOpencodePromptFrame(live) {
		t.Error("a contiguous opencode composer must read as ready")
	}
}

// TestIsWorkingContentOpencodeRealCapture pins mid-turn detection to real panes.
// The finished-turn case is the important one: see
// TestOpencodeWorkingIgnoresStaleFinishedTurnLine.
func TestIsWorkingContentOpencodeRealCapture(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		want    bool
	}{
		{"turn in flight: status bar shows esc interrupt", "opencode_working.ansi", true},
		{"idle at a fresh composer", "opencode_ready.ansi", false},
		{"idle after a turn finished", "opencode_finished.ansi", false},
		// No composer at all: booting or dead. The liveness poll owns those; claiming
		// Running here would paper over a dead pane that must read Lost.
		{"blank booting pane", "opencode_blank.ansi", false},
		{"escapes-only booting pane", "opencode_booting.ansi", false},
		{"banner, no composer", "opencode_banner.ansi", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			content := opencodeFixture(t, tc.fixture)
			if got := IsWorkingContent(content, "opencode"); got != tc.want {
				t.Errorf("IsWorkingContent(%s, opencode) = %v, want %v", tc.fixture, got, tc.want)
			}
		})
	}
}

// TestOpencodeWorkingIgnoresStaleFinishedTurnLine pins the stale-scrollback trap —
// the mirror-image bug of a mid-turn green flash, and the harder one to notice
// because it never self-corrects.
//
// A finished opencode turn leaves a static "▣  Build · Claude Opus 4.5 (latest) ·
// 30.2s" line in the transcript permanently, and "Build ·" ALSO appears inside the
// live composer footer. Keying working-detection on "▣" or on "Build ·" would hold
// an idle session at Running forever — the dot would never go green and
// `af sessions watch` would never return.
//
// The fixture is a real capture taken after a real 30.2s turn completed, so the
// stale line is genuinely present rather than hand-written.
func TestOpencodeWorkingIgnoresStaleFinishedTurnLine(t *testing.T) {
	finished := opencodeFixture(t, "opencode_finished.ansi")
	plain := paneAnsiEscape.ReplaceAllString(finished, "")

	// The trap must actually be present, or this test proves nothing.
	if !strings.Contains(plain, "▣") {
		t.Fatal("finished fixture lost its stale \"▣\" turn line — it can no longer prove the trap")
	}
	if strings.Count(plain, "Build ·") < 2 {
		t.Fatalf("finished fixture should carry \"Build ·\" twice (stale transcript line + live composer footer), got %d", strings.Count(plain, "Build ·"))
	}
	if IsWorkingContent(finished, "opencode") {
		t.Error("a finished turn's stale \"▣ … 30.2s\" line must not read as working — the session would sit at Running forever")
	}
}

// TestOpencodeWorkingIgnoresIndicatorTextInTranscript pins why the working match is
// scoped to the status bar BELOW the composer instead of the whole pane.
//
// opencode's transcript is rendered ABOVE the composer. An agent that merely PRINTS
// the words "esc interrupt" — entirely plausible in a repo whose agents discuss af's
// own key hints — must not be read as mid-turn. A whole-pane
// strings.Contains(content, "esc interrupt") fails this test; scoping to the lines
// after the composer's bottom rule passes it.
func TestOpencodeWorkingIgnoresIndicatorTextInTranscript(t *testing.T) {
	pane := "┃\n" +
		"┃  Sure — opencode shows \"esc interrupt\" in its status bar while busy.\n" +
		"┃\n" +
		"┃  Build · Claude Opus 4.5 (latest) Anthropic\n" +
		"╹▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀\n" +
		"                            tab agents  ctrl+p commands\n"

	if !isOpencodePromptFrame(pane) {
		t.Fatal("fixture must show a live composer for this test to be meaningful")
	}
	if IsWorkingContent(pane, "opencode") {
		t.Error("\"esc interrupt\" appearing in the TRANSCRIPT (above the composer) must not read as working — only the status bar below the composer counts")
	}

	// Same pane, indicator in the status bar where opencode really draws it.
	busy := strings.Replace(pane,
		"                            tab agents  ctrl+p commands",
		"   ⬝⬝⬝⬝⬝⬝⬝⬝  esc interrupt                    tab agents  ctrl+p commands",
		1)
	if !IsWorkingContent(busy, "opencode") {
		t.Error("\"esc interrupt\" in the status bar below the composer must read as working")
	}
}

// A rendered composer copied into the transcript is indistinguishable from the
// real composer by glyph shape alone. The current frame is therefore the LAST
// complete frame in the visible pane: only status rendered below that frame may
// decide whether the current turn is running.
func TestOpencodeWorkingUsesBottomMostCompleteComposer(t *testing.T) {
	transcriptFrame := "┃  copied composer in agent output\n" +
		"╹▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀\n"
	liveFrame := "┃  Build · Claude Opus 4.5 (latest) Anthropic\n" +
		"╹▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀\n"

	idle := transcriptFrame +
		"   ⬝⬝⬝⬝⬝⬝⬝⬝  esc interrupt\n" +
		"┃  The text above was an example, not the live status.\n" +
		liveFrame +
		"                            tab agents  ctrl+p commands\n"
	if IsWorkingContent(idle, "opencode") {
		t.Error("a busy-looking transcript frame above an idle live composer must not pin the session at Running")
	}

	busy := transcriptFrame +
		"                            tab agents  ctrl+p commands\n" +
		liveFrame +
		"   ⬝⬝⬝⬝⬝⬝⬝⬝  esc interrupt                    tab agents  ctrl+p commands\n"
	if !IsWorkingContent(busy, "opencode") {
		t.Error("a busy indicator below the bottom-most live composer must still read as working")
	}
}

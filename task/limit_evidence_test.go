package task

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The usage-limit banner counts only when it opens its screen row (#4721). These
// cases are the false positives the issue names — an agent quoting the banner, a
// fixture or diff that contains it, a summary of work on this feature — plus a
// true positive per agent, so a stricter matcher cannot trade them for the silent
// miss of #4718.

const (
	claudeBanner = "Claude usage limit reached. Your limit will reset at 2pm (America/New_York)."
	// Verbatim from #4718: typographic apostrophe, wrapped purchase clause.
	codexBannerLine1 = "You’ve hit your usage limit. Visit https://chatgpt.com/codex/settings/usage to"
	codexBannerLine2 = "purchase more credits or try again at Sep 20th, 2026 9:16 AM."
)

func TestLimitBannerNeedsToOpenItsRow(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	det := NewLimitDetector(nil)

	falsePositives := []struct {
		name, agent, pane string
	}{
		{"claude: agent quotes the banner mid-sentence", tmux.ProgramClaude,
			"● The detector looks for \"" + claudeBanner + "\" anywhere on screen.\n\n❯ "},
		{"claude: agent summary bullet", tmux.ProgramClaude,
			"● " + claudeBanner + " is the wording #4721 is about.\n\n❯ "},
		{"claude: diff added line", tmux.ProgramClaude,
			"  12 +	claudeLimitDetect = `" + claudeBanner + "`\n\n❯ "},
		{"claude: diff removed line", tmux.ProgramClaude,
			"  12 -" + claudeBanner + "\n\n❯ "},
		{"claude: block quote", tmux.ProgramClaude,
			"> " + claudeBanner + "\n\n❯ "},
		{"claude: file listing with a line number gutter", tmux.ProgramClaude,
			"     40→" + claudeBanner + "\n\n❯ "},
		{"claude: table cell", tmux.ProgramClaude,
			"│ " + claudeBanner + " │\n\n❯ "},
		{"codex: agent quotes the banner", tmux.ProgramCodex,
			"• The fixture says \"" + codexBannerLine1 + "\" and the parser reads the next line.\n\n› "},
		{"codex: summary bullet", tmux.ProgramCodex,
			"• " + codexBannerLine1 + "\n  " + codexBannerLine2 + "\n\n› "},
		{"codex: Go test fixture string", tmux.ProgramCodex,
			"   content: \"■ " + codexBannerLine1 + "\\n\" +\n\n› "},
		{"codex: tool output branch", tmux.ProgramCodex,
			"  └ " + codexBannerLine1 + "\n    " + codexBannerLine2 + "\n\n› "},
		{"codex: diff line", tmux.ProgramCodex,
			"+ " + codexBannerLine1 + "\n\n› "},
		{"devin: prose", tmux.ProgramDevin,
			"I read that devin prints Quota exhausted when the plan runs out.\n\n> "},
		{"devin: branch under tool output", tmux.ProgramDevin,
			"  └ Quota exhausted: usage quota has been exhausted\n\n> "},
		{"devin: bullet", tmux.ProgramDevin, "- Quota exhausted handling was added\n\n> "},
		{"devin: quoted", tmux.ProgramDevin, "> \"Quota exhausted\" is the banner\n\n> "},
	}
	for _, tc := range falsePositives {
		t.Run("not a limit/"+tc.name, func(t *testing.T) {
			if hit, _, _ := det.Check(tc.pane, tc.agent, now); hit {
				t.Fatalf("a banner that does not open its row parked the session:\n%s", tc.pane)
			}
		})
	}

	truePositives := []struct {
		name, agent, pane string
		wantReset         time.Time
	}{
		{"claude: error gutter, styled", tmux.ProgramClaude,
			"● Running\n  \x1b[38;5;246m⎿\x1b[39m  \x1b[38;5;211m" + claudeBanner + "\x1b[39m\n\n❯ ",
			time.Date(2026, 9, 19, 18, 0, 0, 0, time.UTC)},
		{"claude: bare line", tmux.ProgramClaude, claudeBanner + "\n\n❯ ",
			time.Date(2026, 9, 19, 18, 0, 0, 0, time.UTC)},
		{"codex: error cell, verbatim wrap", tmux.ProgramCodex,
			"\x1b[31m■\x1b[0m " + codexBannerLine1 + "\n" + codexBannerLine2 + "\n\n› Ask Codex to do anything",
			time.Date(2026, 9, 20, 9, 16, 0, 0, time.UTC)},
		{"devin: line start", tmux.ProgramDevin, "Quota exhausted: usage quota has been exhausted\n\n> ", time.Time{}},
	}
	for _, tc := range truePositives {
		t.Run("limit/"+tc.name, func(t *testing.T) {
			hit, reset, has := det.Check(tc.pane, tc.agent, now)
			if !hit {
				t.Fatalf("a banner that opens its row was missed (the #4718 failure):\n%s", tc.pane)
			}
			if !tc.wantReset.IsZero() && (!has || !reset.Equal(tc.wantReset)) {
				t.Fatalf("reset = %v (parsed %v), want %v", reset, has, tc.wantReset)
			}
		})
	}
}

// A quotation above a real banner must neither hide it nor lend it a reset time.
func TestLimitBannerQuotedAboveARealOneUsesTheRealReset(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	pane := "● Earlier it said \"Claude usage limit reached. Your limit will reset at 7pm (America/New_York).\"\n" +
		"  ⎿  " + claudeBanner + "\n\n❯ "
	hit, reset, has := NewLimitDetector(nil).Check(pane, tmux.ProgramClaude, now)
	if !hit || !has || !reset.Equal(time.Date(2026, 9, 19, 18, 0, 0, 0, time.UTC)) {
		t.Fatalf("hit=%v reset=%v has=%v, want the 2pm banner's reset", hit, reset, has)
	}
}

// The recorded Claude frame is a hit, and the same frame with the banner moved
// into a bullet of the agent's own reply is not.
func TestClaudeFixtureBannerOpensItsRowButItsQuotationDoesNot(t *testing.T) {
	raw, err := os.ReadFile("testdata/claude_usage_limit.ansi")
	if err != nil {
		t.Fatal(err)
	}
	pane := string(raw)
	det := NewLimitDetector(nil)
	if hit, _, _ := det.Check(pane, tmux.ProgramClaude, time.Now()); !hit {
		t.Fatal("the recorded banner frame is no longer a hit")
	}
	quoted := strings.Replace(pane, "⎿  \x1b[38;5;211mClaude usage limit reached.", "● it printed: Claude usage limit reached.", 1)
	if quoted == pane {
		t.Fatal("fixture shape changed; update this test")
	}
	if hit, _, _ := det.Check(quoted, tmux.ProgramClaude, time.Now()); hit {
		t.Fatal("the same words inside the agent's reply parked the session")
	}
}

// limit_patterns is the user's own regex: it keeps matching wherever it matches.
func TestLimitPatternOverrideIsNotLineAnchored(t *testing.T) {
	det := NewLimitDetector(map[string]string{tmux.ProgramClaude: `weekly cap hit`})
	if hit, _, _ := det.Check("note: weekly cap hit again", tmux.ProgramClaude, time.Now()); !hit {
		t.Fatal("a user pattern was line-anchored")
	}
}

// limitParkError is the startup/task-run path to the same decision.
func TestLimitParkErrorIgnoresAQuotedBanner(t *testing.T) {
	det := NewLimitDetector(nil)
	if err := limitParkError(det, "• see \""+claudeBanner+"\"\n❯ ", tmux.ProgramClaude); err != nil {
		t.Fatalf("a quoted banner parked a starting session: %v", err)
	}
	if err := limitParkError(det, "  ⎿  "+claudeBanner+"\n❯ ", tmux.ProgramClaude); err == nil {
		t.Fatal("a real banner did not park a starting session")
	}
}

// TestDevinBannerPrefixesTheEvidenceAdmits pins the INFERRED renderings of
// devin's alert (#6246): bare, the warning glyph, glyph + VS15, and the ASCII
// "[!]" fallback, each also ANSI-styled. None was observed live. The quoted and
// branch shapes (#4721) are pinned as non-matches in TestLimitBannerNeedsToOpenItsRow.
func TestDevinBannerPrefixesTheEvidenceAdmits(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	det := NewLimitDetector(nil)
	for name, lead := range map[string]string{
		"bare":       "",
		"glyph":      "\u26a0 ",
		"glyph+vs15": "\u26a0\ufe0e ",
		"ascii":      "[!] ",
		"indented":   "  \u26a0\ufe0e ",
	} {
		for _, styled := range []bool{false, true} {
			pane := lead + "Quota exhausted: usage quota has been exhausted\n\n> "
			if styled {
				pane = "\x1b[31m" + lead + "\x1b[0m\x1b[1mQuota exhausted\x1b[0m: usage quota has been exhausted\n\n> "
			}
			if hit, _, _ := det.Check(pane, tmux.ProgramDevin, now); !hit {
				t.Errorf("%s (styled=%v) was missed:\n%q", name, styled, pane)
			}
		}
	}
}

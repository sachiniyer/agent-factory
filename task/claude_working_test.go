package task

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The claude_*.ansi fixtures are `capture-pane -p -e` captures of Claude Code
// 2.1.305. The ones that start with the banner come from a scratch session in
// manual permission mode; claude_working_tip, claude_working_long and
// claude_idle_draft are the bottom rows of live af sessions in bypass mode.
func TestIsWorkingContentClaudeRealCapture(t *testing.T) {
	tests := []struct {
		fixture string
		want    bool
	}{
		{"testdata/claude_working_turn_start.ansi", true},
		{"testdata/claude_working_thinking.ansi", true},
		{"testdata/claude_working_tool.ansi", true},
		{"testdata/claude_working_tip.ansi", true},
		{"testdata/claude_working_long.ansi", true},
		{"testdata/claude_idle_fresh.ansi", false},
		{"testdata/claude_idle_after_turn.ansi", false},
		{"testdata/claude_idle_draft.ansi", false},
		// A permission dialog waits for the user, so the session is Ready.
		{"testdata/claude_permission_prompt.ansi", false},
	}
	for _, tc := range tests {
		t.Run(tc.fixture, func(t *testing.T) {
			raw, err := os.ReadFile(tc.fixture)
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			if got := IsWorkingContent(string(raw), tmux.ProgramClaude); got != tc.want {
				t.Errorf("IsWorkingContent(%s, claude) = %v, want %v", tc.fixture, got, tc.want)
			}
		})
	}
}

// These fixtures are claude_working_tool.ansi with the tool row replaced by the
// text Claude Code prints, not live captures. A wrong "true" here holds a
// session Running while it waits for the user, so `af sessions watch` hangs.
func TestIsWorkingContentClaudeWaitingFrames(t *testing.T) {
	tests := []struct {
		fixture string
		want    bool
	}{
		{"testdata/claude_idle_interrupted.ansi", false},
		{"testdata/claude_usage_limit.ansi", false},
		// Claude Code retries a rate-limited request without user input, so the
		// turn is still running.
		{"testdata/claude_working_api_retry.ansi", true},
	}
	for _, tc := range tests {
		t.Run(tc.fixture, func(t *testing.T) {
			raw, err := os.ReadFile(tc.fixture)
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			if got := IsWorkingContent(string(raw), tmux.ProgramClaude); got != tc.want {
				t.Errorf("IsWorkingContent(%s, claude) = %v, want %v", tc.fixture, got, tc.want)
			}
		})
	}
}

func TestClaudeUsageLimitFixtureIsALimitHit(t *testing.T) {
	raw, err := os.ReadFile("testdata/claude_usage_limit.ansi")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if hit, _, _ := NewLimitDetector(nil).Check(string(raw), tmux.ProgramClaude, time.Now()); !hit {
		t.Error("LimitDetector missed the usage-limit banner in claude_usage_limit.ansi")
	}
}

func TestIsWorkingContentClaudeSpinnerWithoutFooterHint(t *testing.T) {
	for _, fixture := range []string{
		"testdata/claude_working_thinking.ansi",
		"testdata/claude_working_tool.ansi",
		"testdata/claude_working_tip.ansi",
		"testdata/claude_working_long.ansi",
	} {
		raw, err := os.ReadFile(fixture)
		if err != nil {
			t.Fatalf("read fixture: %v", err)
		}
		pane := strings.ReplaceAll(string(raw), "esc to interrupt", "")
		if !IsWorkingContent(pane, tmux.ProgramClaude) {
			t.Errorf("%s without the footer hint: IsWorkingContent = false, want true", fixture)
		}
	}
}

const (
	claudeRule   = "────────────────────────────────────────"
	claudeFooter = "  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← 2 agents"
)

func claudePane(above, composer, footer string) string {
	return above + "\n" + claudeRule + "\n" + composer + "\n" + claudeRule + "\n" + footer + "\n"
}

func TestIsWorkingContentClaudeFrames(t *testing.T) {
	tests := []struct {
		name string
		pane string
		want bool
	}{
		{
			"mid-turn",
			claudePane("✻ Orbiting… (2m 14s · ↓ 10.1k tokens)", "❯ ",
				"  ⏵⏵ bypass permissions on (shift+tab to cycle) · esc to interrupt · ← 2 agents"),
			true,
		},
		{
			"mid-turn with a queued message",
			claudePane("· Orbiting… (3m 25s · ↓ 18.8k tokens)", "❯ Press up to edit queued messages",
				"  ⏵⏵ bypass permissions on (shift+tab to cycle) · esc to interrupt · ← 2 agents"),
			true,
		},
		{
			"mid-turn after a large paste, no footer hint",
			claudePane("* Orbiting… (4s · ↓ 200 tokens · thinking)\n                                       ◐ medium · /effort",
				"❯ ", "  paste again to expand"),
			true,
		},
		{
			"mid-turn with a multi-line draft in the composer",
			claudePane("✶ Orbiting… (12s · ↓ 1.1k tokens)", "❯ first line\n  second line", claudeFooter),
			true,
		},
		{
			"mid-turn with a custom multi-word verb",
			claudePane("✢ Reading the tea leaves… (8s)", "❯ ", claudeFooter),
			true,
		},
		{
			"idle after a finished turn",
			claudePane("✻ Baked for 10m 46s · done 10:54 PM", "❯ ", claudeFooter),
			false,
		},
		{
			"idle with a spinner line quoted in the indented transcript",
			claudePane("● The status line looks like this:\n  ✻ Orbiting… (2m 14s · ↓ 10.1k tokens)", "❯ ", claudeFooter),
			false,
		},
		{
			"idle with a stale spinner line above the last message",
			claudePane("✻ Orbiting… (2m 14s · ↓ 10.1k tokens)\n\n● Done.", "❯ ", claudeFooter),
			false,
		},
		{
			"idle with the footer hint quoted above the composer",
			claudePane("● Press esc to interrupt a turn.", "❯ ", claudeFooter),
			false,
		},
		{
			"spinner line with no composer below it",
			"✻ Orbiting… (2m 14s · ↓ 10.1k tokens)\n" + claudeRule + "\n Do you want to proceed?\n ❯ 1. Yes\n",
			false,
		},
		{
			"empty pane",
			"",
			false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsWorkingContent(tc.pane, tmux.ProgramClaude); got != tc.want {
				t.Errorf("IsWorkingContent(claude) = %v, want %v\npane:\n%s", got, tc.want, tc.pane)
			}
		})
	}
}

func TestIsWorkingContentClaudeReadsCurrentComposerOnly(t *testing.T) {
	stale := claudePane("✻ Orbiting… (2m 14s · ↓ 10.1k tokens)", "❯ ",
		"  ⏵⏵ bypass permissions on (shift+tab to cycle) · esc to interrupt · ← 2 agents")
	pane := stale + "\n● Done.\n\n" + claudePane("✻ Baked for 2m 20s · done 10:54 PM", "❯ ", claudeFooter)
	if IsWorkingContent(pane, tmux.ProgramClaude) {
		t.Error("IsWorkingContent read an old composer frame above the current one; a finished session would never go green")
	}
}

func TestIsWorkingContentClaudeDialogBelowStaleComposer(t *testing.T) {
	stale := claudePane("✻ Orbiting… (2m 14s · ↓ 10.1k tokens)", "❯ ",
		"  ⏵⏵ bypass permissions on (shift+tab to cycle) · esc to interrupt · ← 2 agents")
	raw, err := os.ReadFile("testdata/claude_permission_prompt.ansi")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if IsWorkingContent(stale+"\n"+string(raw), tmux.ProgramClaude) {
		t.Error("IsWorkingContent read a stale composer frame above a live permission dialog; the session waits for the user and must read Ready")
	}
}

func TestIsWorkingContentClaudeIgnoresOtherAgentsPanes(t *testing.T) {
	raw, err := os.ReadFile("testdata/claude_working_thinking.ansi")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	for _, agent := range []string{tmux.ProgramCodex, tmux.ProgramAider, tmux.ProgramGemini, tmux.ProgramDevin, ""} {
		if IsWorkingContent(string(raw), agent) {
			t.Errorf("IsWorkingContent(claude working pane, %q) = true, want false", agent)
		}
	}
}

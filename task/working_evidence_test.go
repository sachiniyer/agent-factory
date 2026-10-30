package task

import (
	"os"
	"testing"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

func TestPublishesWorkingContentNamesTheAgentsIsWorkingContentCanAnswerFor(t *testing.T) {
	for agent, want := range map[string]bool{
		tmux.ProgramClaude:   true,
		tmux.ProgramAmp:      true,
		tmux.ProgramOpencode: true,
		tmux.ProgramCodex:    false,
		tmux.ProgramGemini:   false,
		tmux.ProgramAider:    false,
		"":                   false,
	} {
		if got := PublishesWorkingContent(agent); got != want {
			t.Errorf("PublishesWorkingContent(%q) = %v, want %v", agent, got, want)
		}
	}
}

// Typing a draft into Claude's composer changes the captured pane — the daemon's
// churn signal, which moves the session to Running — without the agent showing
// any in-turn indicator. These are two real captures of Claude Code: an empty
// composer, and one holding a typed draft. The frames differ, and neither is
// work; the same agent mid-turn is. So an attention request must be answered by
// the indicator, never by the churn.
func TestTypingADraftChurnsTheClaudePaneButIsNotWorkEvidence(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		raw, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(raw)
	}
	before := read("claude_idle_after_turn.ansi")
	after := read("claude_idle_draft.ansi")
	if before == after {
		t.Fatal("premise: composing a draft must change the pane, or this proves nothing about churn")
	}
	for name, frame := range map[string]string{"empty composer": before, "typed draft": after} {
		if IsWorkingContent(frame, tmux.ProgramClaude) {
			t.Errorf("%s reads as working; typing would answer an attention request", name)
		}
	}
	if !IsWorkingContent(read("claude_working_tool.ansi"), tmux.ProgramClaude) {
		t.Error("control: the same agent mid-turn must read as working")
	}
}

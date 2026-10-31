package task

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Pane text for the first case is the real capture from #6055 (session
// "devpod", 187h 37m 44s into a dead reconnect).
func TestAgentStoppedContentCodex(t *testing.T) {
	reconnectFailed := func(timer string) string {
		return strings.Join([]string{
			"■ Automatic reconnect could not restore this session. Your draft is still",
			"  editable. Copy it before quitting with Ctrl+C, then reconnect…",
			"■ Server connection could not be restored",
			"• Reconnect failed — check the endpoint, then relaunch (" + timer + ")",
			"⚠ weekly limit: 21% left · /status",
			"› Ask Codex to do anything",
		}, "\n")
	}
	workingCodex := strings.Join([]string{
		"• I'll look at the runner first.",
		"",
		"• Working (12s • esc to interrupt)",
		"",
		"› Ask Codex to do anything",
		"  gpt-5 · ~/repo",
	}, "\n")
	idleCodex := strings.Join([]string{
		"• Done. The change is in task/runner.go.",
		"",
		"› Ask Codex to do anything",
		"  gpt-5 · ~/repo",
	}, "\n")
	recovered := reconnectFailed("187h 37m 44s") + strings.Repeat("\n• later output", 10) +
		"\n\n• Working (3s • esc to interrupt)\n\n› Ask Codex to do anything"
	quoted := "• The log said \"Reconnect failed\" and I fixed it.\n\n› Ask Codex to do anything"

	cases := []struct {
		name, content, agent string
		want                 bool
	}{
		{"reconnect failed, timer at 187h 37m 44s", reconnectFailed("187h 37m 44s"), tmux.ProgramCodex, true},
		{"reconnect failed, timer ticked 20s later", reconnectFailed("187h 38m 04s"), tmux.ProgramCodex, true},
		{"working codex with spinner and timer", workingCodex, tmux.ProgramCodex, false},
		{"idle codex prompt", idleCodex, tmux.ProgramCodex, false},
		{"recovered: stale notice in scrollback, working now", recovered, tmux.ProgramCodex, false},
		{"prose quoting the words", quoted, tmux.ProgramCodex, false},
		{"other agents keep the churn inference", reconnectFailed("1h 0m 0s"), tmux.ProgramClaude, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AgentStoppedContent(tc.content, tc.agent); got != tc.want {
				t.Errorf("AgentStoppedContent = %v, want %v", got, tc.want)
			}
		})
	}
}

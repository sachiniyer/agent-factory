package main

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// TestAutoYesClaudeDetection covers the --autoyes Claude detection logic
// (issue #520). The previous substring match falsely classified paths and
// wrapper scripts containing "claude" as Claude itself, appending the Claude
// --permission-mode flag to unrelated programs.
func TestAutoYesClaudeDetection(t *testing.T) {
	tests := []struct {
		name     string
		program  string
		isClaude bool
	}{
		{"bare claude", "claude", true},
		{"absolute path to claude", "/usr/local/bin/claude", true},
		{"claude with args", "claude --model opus", true},
		{"absolute claude with args", "/usr/local/bin/claude --model opus", true},

		// #520 regressions: these must NOT be classified as Claude.
		{"aider under claude-user home", "/home/claude-user/bin/aider", false},
		{"codex under claude-wrapper dir", "/opt/claude-wrapper/bin/codex", false},
		{"hyphen-prefixed wrapper", "claude-code-cli", false},

		// Other agents must remain unmatched.
		{"aider", "aider", false},
		{"codex", "codex", false},
		{"gemini", "gemini", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := session.BaseCommand(tt.program) == tmux.ProgramClaude
			if got != tt.isClaude {
				t.Errorf("BaseCommand(%q)==ProgramClaude = %v, want %v",
					tt.program, got, tt.isClaude)
			}
		})
	}
}

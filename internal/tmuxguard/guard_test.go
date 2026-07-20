package tmuxguard

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestDenialReason(t *testing.T) {
	tests := []struct {
		name    string
		command string
		deny    bool
	}{
		{name: "bare kill-server", command: "tmux kill-server", deny: true},
		{name: "absolute tmux", command: "/usr/bin/tmux kill-server", deny: true},
		{name: "compound command", command: "echo checking && tmux kill-server 2>/dev/null", deny: true},
		{name: "escaped executable", command: `t\mux kill-server`, deny: true},
		{name: "environment wrapper", command: "env FOO=bar tmux kill-server", deny: true},
		{name: "shell wrapper", command: `bash -c 'tmux kill-server'`, deny: true},
		{name: "pkill tmux", command: "pkill tmux", deny: true},
		{name: "anchored pkill pattern", command: `pkill -f '^tmux$'`, deny: true},
		{name: "named socket", command: "tmux -L af-test kill-server"},
		{name: "attached named socket", command: "tmux -Laf-test kill-server"},
		{name: "socket path", command: "tmux -S /tmp/af-test.sock kill-server"},
		{name: "socket option after command is too late", command: "tmux kill-server -L af-test", deny: true},
		{name: "empty socket", command: `tmux -L '' kill-server`, deny: true},
		{name: "other tmux command", command: "tmux list-sessions"},
		{name: "quoted discussion", command: `printf '%s\n' 'tmux kill-server'`},
		{name: "other pkill", command: "pkill test-worker"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DenialReason(tt.command)
			if tt.deny && got == "" {
				t.Fatalf("expected %q to be denied", tt.command)
			}
			if !tt.deny && got != "" {
				t.Fatalf("expected %q to be allowed, got %q", tt.command, got)
			}
		})
	}
}

func TestRunReturnsClaudeDenial(t *testing.T) {
	input := `{"tool_name":"Bash","tool_input":{"command":"tmux kill-server"}}`
	var output bytes.Buffer
	if err := Run(strings.NewReader(input), &output); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var decision hookDecision
	if err := json.Unmarshal(output.Bytes(), &decision); err != nil {
		t.Fatalf("parse decision: %v", err)
	}
	if decision.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("expected deny decision, got: %s", output.String())
	}
}

func TestRunAllowsSocketedKill(t *testing.T) {
	input := `{"tool_name":"Bash","tool_input":{"command":"tmux -S /tmp/af-test.sock kill-server"}}`
	var output bytes.Buffer
	if err := Run(strings.NewReader(input), &output); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if output.Len() != 0 {
		t.Fatalf("safe command should produce no decision, got: %s", output.String())
	}
}

func TestRunFailsClosedOnMalformedInput(t *testing.T) {
	var output bytes.Buffer
	if err := Run(strings.NewReader("not-json"), &output); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(output.String(), `"permissionDecision":"deny"`) {
		t.Fatalf("malformed hook input must be denied, got: %s", output.String())
	}
}

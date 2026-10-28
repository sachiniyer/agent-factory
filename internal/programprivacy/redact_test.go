package programprivacy

import (
	"testing"

	"github.com/sachiniyer/agent-factory/internal/credscrub"
)

func TestRedactKeepsOnlyBoundedAgentIdentity(t *testing.T) {
	tests := map[string]string{
		"": "",
		"/home/private-user/.local/bin/claude --token secret":   "claude",
		"env PROFILE=private /opt/tools/codex --dangerous-flag": "codex",
		"/home/private-user/bin/arbitrary --token secret":       credscrub.RedactedMarker,
	}
	for command, want := range tests {
		if got := Redact(command); got != want {
			t.Errorf("Redact(%q) = %q, want %q", command, got, want)
		}
	}
}

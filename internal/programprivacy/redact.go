// Package programprivacy owns the shared policy for reducing an arbitrary
// resolved command line to a bounded diagnostic value.
package programprivacy

import (
	"github.com/sachiniyer/agent-factory/internal/credscrub"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Redact reduces a resolved program command line to the agent it runs. A
// command with no recognized agent has no safe fragment to retain.
func Redact(program string) string {
	if program == "" {
		return ""
	}
	if agent := tmux.DetectAgentFromCommand(program); agent != "" {
		return agent
	}
	return credscrub.RedactedMarker
}

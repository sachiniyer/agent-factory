package sessionenv

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// An unrelated dollar-quoted or dynamic argument must not change the verdict
// for a selector that is itself unambiguous (#5652). Both shell dialects
// execute the selector as the plain value 1.
func TestCommandEnvironmentFlagStateUnrelatedWordKeepsSelector(t *testing.T) {
	for _, command := range []string{
		`CLAUDE_CODE_USE_BEDROCK=1 claude $'prompt'`,
		`env CLAUDE_CODE_USE_BEDROCK=1 claude $'prompt'`,
		`CLAUDE_CODE_USE_BEDROCK=1 exec claude $'prompt'`,
	} {
		found, enabled := commandEnvironmentFlagState(command, "claude", "CLAUDE_CODE_USE_BEDROCK")
		require.True(t, found, command)
		require.True(t, enabled, command)
	}
}

// The selector itself (or a word that can change the agent's environment)
// being ambiguous must still fail closed, wherever an unrelated word sits.
func TestCommandEnvironmentFlagStateAmbiguousSelectorStillFailsClosed(t *testing.T) {
	for _, command := range []string{
		`CLAUDE_CODE_USE_BEDROCK=$'1' claude $'prompt'`,
		`CLAUDE_CODE_USE_BEDROCK=$"1" claude prompt`,
		`CLAUDE_CODE_USE_BEDROCK=1 FOO=$(x) claude $'prompt'`,
		`env CLAUDE_CODE_USE_BEDROCK=$'1' claude $'prompt'`,
		`env $'-i' CLAUDE_CODE_USE_BEDROCK=1 claude`,
		`CLAUDE_CODE_USE_BEDROCK=$VAR claude $'prompt'`,
	} {
		found, enabled := commandEnvironmentFlagState(command, "claude", "CLAUDE_CODE_USE_BEDROCK")
		require.False(t, enabled, command)
		_ = found
	}
}

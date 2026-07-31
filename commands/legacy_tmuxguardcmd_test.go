package commands

import (
	"bytes"
	"strings"
	"testing"
)

// TestLegacyTmuxGuardHookCommandIsNoop covers the installable Codex side of
// #2608. Its versioned v3.3 hook script survives an af upgrade and invokes this
// removed command. Running sessions must see the removal as "no guard", even
// when their cached hook input names a command the former guard denied.
func TestLegacyTmuxGuardHookCommandIsNoop(t *testing.T) {
	originalVersion := version
	originalRootVersion := rootCmd.Version
	cmd := NewRootCommand(Options{Version: "test"})
	t.Cleanup(func() {
		version = originalVersion
		rootCmd.Version = originalRootVersion
		cmd.SetArgs(nil)
		cmd.SetIn(nil)
		cmd.SetOut(nil)
		cmd.SetErr(nil)
	})

	var output bytes.Buffer
	cmd.SetIn(strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"tmux kill-server"}}`))
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"hook-guard-tmux"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("legacy Codex hook denied Bash after upgrade: %v\n%s", err, output.String())
	}
	if output.Len() != 0 {
		t.Fatalf("compatibility no-op wrote unexpected output: %q", output.String())
	}
}

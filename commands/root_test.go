package commands

import (
	"bytes"
	"strings"
	"testing"
)

// TestVersionFlag covers #1749 item 7: `af --version` prints the version and
// release URL instead of erroring with a usage dump. cobra provides the flag
// (and -v) for free once rootCmd.Version is set in NewRootCommand.
func TestVersionFlag(t *testing.T) {
	origVersion := version
	t.Cleanup(func() {
		version = origVersion
		rootCmd.Version = origVersion
		rootCmd.SetArgs(nil)
	})

	cmd := NewRootCommand(Options{Version: "9.9.9"})
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--version"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("af --version returned error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "agent-factory version 9.9.9") {
		t.Fatalf("--version output missing version string: %q", out)
	}
	if !strings.Contains(out, "releases/tag/v9.9.9") {
		t.Fatalf("--version output missing release URL: %q", out)
	}
}

// TestPersistentPreRunSilencesUsage covers #1749 item 2: after flag parsing has
// succeeded (the point at which PersistentPreRun fires), the root command is set
// to SilenceUsage so a runtime RunE error prints as one calm line instead of a
// usage dump. Flag-PARSE errors fail before this hook runs, so their usage help
// is preserved.
func TestPersistentPreRunSilencesUsage(t *testing.T) {
	origSilence := rootCmd.SilenceUsage
	t.Cleanup(func() { rootCmd.SilenceUsage = origSilence })

	rootCmd.SilenceUsage = false
	if rootCmd.PersistentPreRun == nil {
		t.Fatal("rootCmd.PersistentPreRun must be set to silence usage on runtime errors")
	}
	rootCmd.PersistentPreRun(rootCmd, nil)
	if !rootCmd.SilenceUsage {
		t.Fatal("PersistentPreRun did not set root SilenceUsage")
	}
}

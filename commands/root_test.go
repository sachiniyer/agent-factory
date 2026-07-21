package commands

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
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

func TestAutoYesCLIFlagsAreRemovedWithGuidance(t *testing.T) {
	origVersion := version
	origRootVersion := rootCmd.Version
	cmd := NewRootCommand(Options{Version: "9.9.9"})
	origSilenceErrors := rootCmd.SilenceErrors
	origSilenceUsage := rootCmd.SilenceUsage
	t.Cleanup(func() {
		version = origVersion
		rootCmd.Version = origRootVersion
		cmd.SetArgs(nil)
		cmd.SetOut(nil)
		cmd.SetErr(nil)
		cmd.SilenceErrors = origSilenceErrors
		cmd.SilenceUsage = origSilenceUsage
	})

	tests := []struct {
		name string
		args []string
	}{
		{name: "root long flag", args: []string{"--autoyes", "--version"}},
		{name: "root short flag", args: []string{"-y", "--version"}},
		{name: "agent-server flag", args: []string{"agent-server", "--auto-yes"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			cmd.SilenceErrors = true
			cmd.SetArgs(tc.args)

			err := cmd.Execute()
			if err == nil {
				t.Fatalf("af %s is still accepted", strings.Join(tc.args, " "))
			}
			if !strings.Contains(err.Error(), "was removed") || !strings.Contains(err.Error(), "program_overrides") {
				t.Fatalf("af %s error is not actionable migration guidance: %v", strings.Join(tc.args, " "), err)
			}
		})
	}
}

func TestHelpDoesNotAdvertiseAutoYes(t *testing.T) {
	var checkTree func(*cobra.Command)
	checkTree = func(cmd *cobra.Command) {
		t.Run(strings.ReplaceAll(cmd.CommandPath(), " ", "/"), func(t *testing.T) {
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			t.Cleanup(func() {
				cmd.SetOut(nil)
				cmd.SetErr(nil)
			})

			if err := cmd.Help(); err != nil {
				t.Fatalf("render %s help: %v", cmd.CommandPath(), err)
			}
			lower := strings.ToLower(output.String())
			if strings.Contains(lower, "autoyes") || strings.Contains(lower, "auto-yes") {
				t.Fatalf("%s help still advertises removed auto-yes behavior:\n%s", cmd.CommandPath(), output.String())
			}
		})
		for _, child := range cmd.Commands() {
			checkTree(child)
		}
	}

	checkTree(rootCmd)
}

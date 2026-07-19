package parity

// Tab-name help honesty: does the help describe the thing the code actually
// implements?
//
// The identifier axis (identifier_test.go) pins the RESOLVER: the name is the
// sole handle and the label never resolves (#1986). This file pins the other
// half — what we TELL the user about that name. A resolver that is correct and
// help text that describes a different model is the same #1984 failure with the
// halves swapped: the user reads the help, forms the wrong model, and types the
// wrong string.
//
// Two things are enforced, both derived rather than hardcoded:
//
//  1. The naming rules tab-rename CITES must actually be stated where it points.
//     A dangling cross-reference sends the reader somewhere that does not answer.
//  2. No tab-name-taking verb may call the name a "display" string, because
//     session.TabLabel proves the name and the display differ for agent/shell.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/sachiniyer/agent-factory/commands"
	"github.com/sachiniyer/agent-factory/session"
)

// tabNameCommands returns every command in the real cobra tree that takes a
// tab-addressing --name flag (the `sessions tab-*` verbs and their `tabs *`
// aliases). Derived from the tree, so a new tab verb is covered the day it is
// added rather than when someone remembers to list it here.
func tabNameCommands(t *testing.T) map[string]*cobra.Command {
	t.Helper()
	root := initCobraDefaults(commands.NewRootCommand(commands.Options{Version: "0.0.0-parity"}))
	out := map[string]*cobra.Command{}

	var walk func(c *cobra.Command, path string)
	walk = func(c *cobra.Command, path string) {
		if strings.Contains(path, " tab") {
			hasName := false
			c.LocalFlags().VisitAll(func(f *pflag.Flag) {
				if f.Name == "name" {
					hasName = true
				}
			})
			if hasName && (c.RunE != nil || c.Run != nil) {
				out[path] = c
			}
		}
		for _, sub := range c.Commands() {
			walk(sub, path+" "+sub.Name())
		}
	}
	walk(root, "af")

	if len(out) == 0 {
		t.Fatal("derived no tab commands taking --name; the walk is broken, so every " +
			"assertion below would vacuously pass")
	}
	return out
}

// helpText is everything a user reads for a command: its long description plus
// the flag usage strings, which is where a terse lie most often hides.
func helpText(c *cobra.Command) string {
	var b strings.Builder
	b.WriteString(c.Short)
	b.WriteString("\n")
	b.WriteString(c.Long)
	b.WriteString("\n")
	c.LocalFlags().VisitAll(func(f *pflag.Flag) {
		b.WriteString(f.Usage)
		b.WriteString("\n")
	})
	return b.String()
}

// TestTabNamingRulesAreStatedWhereTheHelpPointsAtThem closes a dangling
// cross-reference: tab-rename tells the reader that --new-name "follows the same
// rules as tab-create's --name", so tab-create's help has to actually define
// those rules. It did not — tab-create documented uniqueness suffixing and said
// nothing about the sanitization the code performs, so a user passing
// --name "my tab" silently got "my-tab" with no help text anywhere admitting it.
//
// Silently rewriting the thing the user explicitly named is the failure mode
// #1957 is filed about, one verb over.
func TestTabNamingRulesAreStatedWhereTheHelpPointsAtThem(t *testing.T) {
	cmds := tabNameCommands(t)

	// Find the verbs that DEFER to another verb's --name for their rules.
	const citation = "same rules as tab-create's --name"
	citing := map[string]bool{}
	for path, c := range cmds {
		if strings.Contains(helpText(c), citation) {
			citing[path] = true
		}
	}
	if len(citing) == 0 {
		t.Skip("no verb cites tab-create's --name for its rules; nothing to keep honest")
	}

	// The cited definition must exist. The sanitization rule is the load-bearing
	// half — uniqueness is visible in the returned name, a rewritten character is
	// not.
	//
	// A command whose help explicitly DELEGATES ("Alias for …, see … --help")
	// satisfies this by reference: the noun-subcommand aliases (#1192) point at
	// the hyphen verb rather than restating it, which is precisely what keeps the
	// two from drifting. Only the verb that actually defines the rules is held to
	// stating them.
	for path, c := range cmds {
		if !strings.Contains(path, "tab-create") {
			continue
		}
		help := helpText(c)
		if strings.Contains(help, "Alias for") {
			continue
		}
		if !strings.Contains(help, "[A-Za-z0-9_-]") {
			t.Errorf("%s does not state the character rule its siblings cite it for (%q).\n\n"+
				"session.sanitizeTabName rewrites anything outside [A-Za-z0-9_-] to \"-\", so "+
				"--name \"my tab\" silently becomes \"my-tab\". %d verb(s) point the reader here "+
				"for that rule and it is not written down.", path, citation, len(citing))
		}
		if !strings.Contains(help, "unique") {
			t.Errorf("%s does not state that the name is made unique, the other half of the "+
				"cited rules.", path)
		}
	}
}

// TestTabNameHelpNeverCallsItADisplayName guards the model the anchor
// established: Name is the handle a user types, session.TabLabel is what they
// read, and for agent/shell tabs the two deliberately differ. Help that calls
// the name a "display name" teaches the conflation #1986 removed from the type —
// and it is the conflation that produced #1984 in the first place.
//
// Covers the narrative CLI guide as well as the built-in help: docs/cli.md is
// hand-maintained (unlike the generated docs/reference/cli.md), so it is the one
// that drifts.
func TestTabNameHelpNeverCallsItADisplayName(t *testing.T) {
	// Premise, derived rather than assumed: the name and the display really do
	// differ. If a future change makes them identical, "display name" stops being
	// wrong and this test should stop asserting it.
	shell := &session.Tab{Name: "shell", Kind: session.TabKindShell}
	if session.TabLabel(shell) == shell.Name {
		t.Skip("TabLabel now equals Name; calling the name a display name is no longer a conflation")
	}

	const banned = "display name"
	for path, c := range tabNameCommands(t) {
		if strings.Contains(strings.ToLower(helpText(c)), banned) {
			t.Errorf("%s calls the tab name a %q. It is the HANDLE a user types; what they "+
				"read is session.TabLabel (%q for a tab named %q). Say \"name\" and, where it "+
				"matters, name the label separately.", path, banned, session.TabLabel(shell), shell.Name)
		}
	}

	guide := filepath.Join("..", "docs", "cli.md")
	raw, err := os.ReadFile(guide)
	if err != nil {
		t.Fatalf("read %s: %v", guide, err)
	}
	for i, line := range strings.Split(string(raw), "\n") {
		lower := strings.ToLower(line)
		if !strings.Contains(lower, banned) {
			continue
		}
		// Only the tab section is in scope — a session's own "display name" is a
		// different concept and not what #1986 split.
		if !strings.Contains(lower, "tab") {
			continue
		}
		t.Errorf("docs/cli.md:%d calls a tab name a %q:\n  %s\n\n"+
			"Name is the handle; session.TabLabel is the display (%q for %q).",
			i+1, banned, strings.TrimSpace(line), session.TabLabel(shell), shell.Name)
	}
}

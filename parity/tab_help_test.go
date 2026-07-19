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
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/sachiniyer/agent-factory/commands"
	"github.com/sachiniyer/agent-factory/session"
)

// allCommands returns every command in the real cobra tree, keyed by its full
// invocation path ("af sessions tab-create").
func allCommands(t *testing.T) map[string]*cobra.Command {
	t.Helper()
	root := initCobraDefaults(commands.NewRootCommand(commands.Options{Version: "0.0.0-parity"}))
	out := map[string]*cobra.Command{}

	var walk func(c *cobra.Command, path string)
	walk = func(c *cobra.Command, path string) {
		out[path] = c
		for _, sub := range c.Commands() {
			walk(sub, path+" "+sub.Name())
		}
	}
	walk(root, "af")
	return out
}

// tabNameCommands narrows the tree to the commands that take a tab-addressing
// --name flag (the `sessions tab-*` verbs and their `tabs *` aliases). Derived,
// so a new tab verb is covered the day it is added rather than when someone
// remembers to list it here.
func tabNameCommands(t *testing.T) map[string]*cobra.Command {
	t.Helper()
	out := map[string]*cobra.Command{}
	for path, c := range allCommands(t) {
		if !strings.Contains(path, " tab") || (c.RunE == nil && c.Run == nil) {
			continue
		}
		hasName := false
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if f.Name == "name" {
				hasName = true
			}
		})
		if hasName {
			out[path] = c
		}
	}
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

// aliasDelegation matches a command whose Long hands the reader to another
// command: `Alias for "sessions tab-create". See "af sessions …" for details.`
// The quotes are load-bearing — flag usages say "Alias for --here" with no
// quotes, and sweeping those in would compare a flag against a command.
var aliasDelegation = regexp.MustCompile(`Alias for "([^"]+)"`)

// TestAliasVerbsMatchTheirTargetShort locks the one-line summary of a
// delegating alias to the verb it delegates to.
//
// The #1192 aliases deliberately do not restate their target's Long — they point
// at it, which is what keeps the two from drifting. But Short is NOT delegated:
// it is written out twice, once per command, and it is what `af sessions tabs
// --help` and the generated command index actually print. So it drifts silently,
// and it did: `tabs create` still described "a process tab or a web tab" long
// after tab-create grew VS Code, so the alias advertised strictly less than the
// verb it claims to be identical to.
//
// The content assertions in this file fold Short into helpText, so a drift like
// that could only ever be caught INCIDENTALLY (and not at all once a delegating
// alias is skipped). This is the dedicated lock.
func TestAliasVerbsMatchTheirTargetShort(t *testing.T) {
	all := allCommands(t)
	checked := 0

	for path, c := range all {
		m := aliasDelegation.FindStringSubmatch(c.Long)
		if m == nil {
			continue
		}
		targetPath := "af " + m[1]
		target, ok := all[targetPath]
		if !ok {
			t.Errorf("%s says it is an %q, but %q is not a command in the tree — the help "+
				"sends the reader somewhere that does not exist.", path, m[0], targetPath)
			continue
		}
		checked++
		if c.Short != target.Short {
			t.Errorf("%s delegates to %s but their one-line summaries disagree:\n"+
				"  alias:  %q\n  target: %q\n\n"+
				"An alias that claims to be identical must SAY the same thing: Short is the "+
				"line `af sessions tabs --help` and the docs index print, and unlike Long it is "+
				"written out twice, so it drifts silently.", path, targetPath, c.Short, target.Short)
		}
	}

	if checked == 0 {
		t.Fatal("found no delegating aliases; the regex or the walk is broken, so this " +
			"assertion proves nothing")
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

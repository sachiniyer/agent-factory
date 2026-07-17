package parity

// Argument-shape parity: the CLI-vs-CLI axis.
//
// The rest of this package asks whether the three surfaces expose the same
// capabilities. This asks a narrower question about one surface: within a noun
// group, does the same CONCEPT take the same SHAPE across sibling verbs?
//
// It is the same failure mode. A user who learned `af sessions create --prompt X`
// cannot predict `af sessions send-prompt <title> <prompt>` — same concept, two
// shapes, two sibling subcommands in one noun group — and the error they get is
// "unknown flag: --prompt", which names the flag as wrong without saying that the
// positional form is what it wants. Learning one verb should let you predict its
// sibling; when it does not, the CLI is internally inconsistent in exactly the way
// the surfaces are externally inconsistent.
//
// Both halves are derived: flags from the cobra tree, positionals from each
// command's Use string ("send-prompt <title> <prompt>"). Nothing is hand-listed
// except the synonym table, which is the one judgment a machine cannot make —
// that `--name` on create and `<title>` on its siblings are the same concept.

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// positionalRe pulls the argument tokens out of a cobra Use line: <title> is
// required, [repo] optional. Both are the same SHAPE for this audit — what
// matters is flag-vs-positional, not required-vs-optional.
var positionalRe = regexp.MustCompile(`[<\[]([a-z][a-z-]*)[>\]]`)

// argForm is how a verb accepts a concept.
type argForm string

const (
	formFlag       argForm = "flag"
	formPositional argForm = "positional"
)

// conceptUse is the set of forms one verb accepts for one concept.
type conceptUse map[argForm]bool

// deriveArgShapes returns, per noun group, per concept, the forms each verb
// accepts: group -> concept -> verb -> forms.
//
// The group is the parent path ("af sessions"); only groups with more than one
// verb can diverge, since a lone verb has no sibling to be inconsistent with.
func deriveArgShapes(t *testing.T, synonyms map[string]string) map[string]map[string]map[string]conceptUse {
	t.Helper()

	// Synonyms are keyed per VERB ("af sessions create name"), not globally.
	// A global name->title mapping is wrong: `af sessions tab-create <title>
	// --name <tabname>` uses --name for the TAB's name, so a global rule would
	// conflate it with the session title and invent a divergence that is not
	// there. Which spellings mean the same thing is verb-local judgment.
	canon := func(verb, s string) string {
		if c, ok := synonyms[verb+" "+s]; ok {
			return c
		}
		return s
	}

	out := map[string]map[string]map[string]conceptUse{}
	for path, v := range deriveCLI(t) {
		if !v.Runnable {
			continue
		}
		// The root `af` is runnable but has no parent group, so it has no
		// siblings to be inconsistent with.
		i := strings.LastIndex(path, " ")
		if i < 0 {
			continue
		}
		group := path[:i]
		if out[group] == nil {
			out[group] = map[string]map[string]conceptUse{}
		}

		add := func(concept string, form argForm) {
			c := canon(path, concept)
			if out[group][c] == nil {
				out[group][c] = map[string]conceptUse{}
			}
			if out[group][c][path] == nil {
				out[group][c][path] = conceptUse{}
			}
			out[group][c][path][form] = true
		}

		for _, f := range v.Flags {
			add(f, formFlag)
		}
		for _, m := range positionalRe.FindAllStringSubmatch(v.Use, -1) {
			add(m[1], formPositional)
		}
	}
	return out
}

// divergentConcepts returns the concepts in a group where no single form works
// across every verb that has the concept.
//
// The test is a non-empty INTERSECTION, not identical sets. A verb that accepts
// BOTH forms is compatible with either neighbour — which is precisely why
// accepting `--prompt` on send-prompt (while keeping the positional) resolves the
// divergence without breaking anyone.
func divergentConcepts(group map[string]map[string]conceptUse) map[string][]string {
	out := map[string][]string{}
	for concept, byVerb := range group {
		if len(byVerb) < 2 {
			continue // one verb has it: nothing to be inconsistent with
		}
		common := conceptUse{formFlag: true, formPositional: true}
		for _, forms := range byVerb {
			for f := range common {
				if !forms[f] {
					delete(common, f)
				}
			}
		}
		if len(common) > 0 {
			continue
		}
		var verbs []string
		for v, forms := range byVerb {
			var fs []string
			for f := range forms {
				fs = append(fs, string(f))
			}
			sort.Strings(fs)
			verbs = append(verbs, v+" ("+strings.Join(fs, "+")+")")
		}
		sort.Strings(verbs)
		out[concept] = verbs
	}
	return out
}

// TestArgumentShapeParity fails when a concept takes irreconcilable shapes across
// sibling verbs in one noun group.
func TestArgumentShapeParity(t *testing.T) {
	inv := loadInventory(t)
	caps := inv.byID()
	shapes := deriveArgShapes(t, inv.ArgumentShapes.Synonyms)

	if len(shapes) == 0 {
		t.Fatal("no noun groups derived — the cobra walk or the Use parser is blind")
	}
	// Not blind: the group everyone knows about must be in view with real verbs.
	if len(shapes["af sessions"]) == 0 {
		t.Fatal("`af sessions` has no derived concepts — the Use/flag derivation is blind")
	}

	for group, concepts := range shapes {
		for concept, verbs := range divergentConcepts(concepts) {
			key := group + " " + concept
			d, declared := inv.ArgumentShapes.Declared[key]
			if !declared {
				t.Errorf("%q takes irreconcilable shapes across sibling verbs:\n    %s\n\n"+
					"A user who learned one of these cannot predict the other. Either make one "+
					"verb accept the other's form (additive: keep both), or declare it in "+
					"argument_shapes.declared in parity/inventory.json with {\"gap\": \"<capability>\"} "+
					"or {\"ok\": \"<reason>\"}.", key, strings.Join(verbs, "\n    "))
				continue
			}
			switch {
			case d.Gap != "" && d.OK != "":
				t.Errorf("%s declares both gap and ok — pick one", key)
			case d.Gap == "" && d.OK == "":
				t.Errorf("%s declares neither gap nor ok", key)
			case d.Gap != "":
				if _, ok := caps[d.Gap]; !ok {
					t.Errorf("%s names unknown capability %q", key, d.Gap)
				}
			}
		}
	}

	// And the declarations cannot outlive the divergence they describe: once a
	// concept is reconciled, its entry must go, or the inventory keeps reporting
	// a wart that was fixed.
	for key := range inv.ArgumentShapes.Declared {
		// The key is "<group> <concept>" where the group itself contains spaces
		// ("af sessions title"), so split on the LAST one.
		i := strings.LastIndex(key, " ")
		if i < 0 {
			t.Errorf("argument_shapes.declared key %q is not `<group> <concept>`", key)
			continue
		}
		group, concept := key[:i], key[i+1:]

		if _, still := divergentConcepts(shapes[group])[concept]; !still {
			t.Errorf("argument_shapes declares %q divergent, but %q is now consistent across "+
				"%s. If it was reconciled, drop the declaration and update the capability.",
				key, concept, group)
		}
	}
}

// TestSendPromptAcceptsPromptFlag pins the fix for the reported trap:
// `af sessions send-prompt --prompt X` used to hard-error with "unknown flag:
// --prompt" while its sibling `create` took exactly that flag.
//
// This is a fixture, not a restatement of the code: it asserts the ACCEPTED
// SHAPES converge, which is the property a user actually relies on.
func TestSendPromptAcceptsPromptFlag(t *testing.T) {
	derived := deriveCLI(t)

	hasFlag := func(path, flag string) bool {
		for _, f := range derived[path].Flags {
			if f == flag {
				return true
			}
		}
		return false
	}

	if !hasFlag("af sessions create", "prompt") {
		t.Fatal("`af sessions create --prompt` is gone — the premise of this fixture changed")
	}
	if !hasFlag("af sessions send-prompt", "prompt") {
		t.Error("`af sessions send-prompt` no longer accepts --prompt. Its sibling `create` " +
			"takes the prompt as a flag, so dropping it re-opens the trap: a user who learned " +
			"one verb gets \"unknown flag: --prompt\" from the other.")
	}
	// The positional form must survive: the flag is additive, not a migration.
	if !strings.Contains(derived["af sessions send-prompt"].Use, "<prompt>") {
		t.Error("`af sessions send-prompt` no longer documents its positional <prompt>. The " +
			"flag was added as an ALIAS; removing the positional would break every existing " +
			"caller and script.")
	}
}

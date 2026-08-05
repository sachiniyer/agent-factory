package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The operator-only row in docs/configuration.md is the hand-written answer to
// "which keys can a checked-in repository config NOT set?" — the question a user
// asks to decide whether adopting a project's config is safe. Its own text
// promises "Setting them in-repo is rejected with an error naming the key".
//
// The real answer lives in the manifest's Sources. Two representations of one
// fact drift, and this one drifted: the `[root_agent]` singleton — the canonical
// successor to `root_agents`, and the key users are being steered toward — was
// rejected in-repo by the code and absent from the doc's list, so the protection
// it does have went undocumented on the newer of the two keys (#2894).
//
// A doc that under-promises a safeguard is the inverse of #2780 and milder, but
// the fix for both is the same: stop maintaining the list by hand.

// repoRootForDocs walks up to the repository root so this test works from the
// package directory. It SKIPS rather than fails when the tree is not there: the
// package must stay testable from a module cache or a vendored copy, where
// docs/ legitimately does not exist, and turning that into a failure would be a
// harness bug rather than a finding.
func repoRootForDocs(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get cwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			if info, err := os.Stat(filepath.Join(dir, "docs")); err == nil && info.IsDir() {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("docs/ is not present in this checkout; the configuration.md drift gate needs the repo tree")
		}
		dir = parent
	}
}

// operatorOnlyDocRow returns the configuration.md table row that enumerates the
// keys rejected in in-repo config. It is found by its own promise text rather
// than by line number, so ordinary edits above it cannot break the gate — and a
// restructure that removes the row fails loudly instead of silently passing.
func operatorOnlyDocRow(t *testing.T) string {
	t.Helper()
	path := filepath.Join(repoRootForDocs(t), "docs", "configuration.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	const marker = "Operator-only. Setting them in-repo is rejected"
	for _, line := range strings.Split(string(body), "\n") {
		if strings.Contains(line, marker) {
			return line
		}
	}
	t.Fatalf("docs/configuration.md no longer has a row containing %q.\n\n"+
		"That row is the documented list of keys a checked-in repo config cannot set. "+
		"If it moved or was reworded, point this gate at its new form — do not delete the gate, "+
		"or the list goes back to being hand-checked (#2894).", marker)
	return ""
}

// TestOperatorOnlyDocRowListsEveryRepoRejectedKey is the drift gate: every key
// the manifest refuses from a checked-in in-repo config must be named in the
// doc row that claims to list them.
func TestOperatorOnlyDocRowListsEveryRepoRejectedKey(t *testing.T) {
	row := operatorOnlyDocRow(t)

	var missing []string
	rejected := 0
	for _, entry := range AllManifest() {
		if entry.Sources.Has(SourceRepoShared) {
			continue
		}
		rejected++
		// Match the backticked key exactly. A bare substring would let
		// `root_agents` satisfy a check for `root_agent`, which is precisely the
		// pair that drifted.
		if !strings.Contains(row, "`"+entry.Key+"`") {
			missing = append(missing, entry.Key)
		}
	}

	if rejected == 0 {
		t.Fatal("no manifest key is rejected in-repo — the gate would pass vacuously, so something is wrong with the manifest or this test")
	}
	if len(missing) > 0 {
		t.Errorf("docs/configuration.md's operator-only row omits %d key(s) the code rejects in in-repo config: %s\n\n"+
			"That row tells a user which keys a cloned repository cannot set. A key missing from it has its "+
			"protection undocumented — add it to the row (#2894).", len(missing), strings.Join(missing, ", "))
	}
}

// TestOperatorOnlyDocRowClaimsNothingTheCodeAllows is the other direction, and
// the dangerous one: a key listed as rejected in-repo that the code actually
// ACCEPTS would be a doc promising protection that does not exist — the #2780
// shape. Nothing is in that state today; this keeps it that way.
func TestOperatorOnlyDocRowClaimsNothingTheCodeAllows(t *testing.T) {
	row := operatorOnlyDocRow(t)

	var falsePromises []string
	for _, entry := range AllManifest() {
		if !entry.Sources.Has(SourceRepoShared) {
			continue
		}
		if strings.Contains(row, "`"+entry.Key+"`") {
			falsePromises = append(falsePromises, entry.Key)
		}
	}

	if len(falsePromises) > 0 {
		t.Errorf("docs/configuration.md's operator-only row lists %s, but the manifest ACCEPTS %s from checked-in in-repo config.\n\n"+
			"This is a doc promising a safeguard that does not exist: a user reading it would believe a cloned "+
			"repository cannot set that key. Either fix the manifest or fix the doc — do not leave them disagreeing (#2894).",
			strings.Join(falsePromises, ", "), plural(len(falsePromises), "it", "them"))
	}
}

// plural picks a word for a count, so the failure above reads as a sentence.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return fmt.Sprintf("%s (%d keys)", many, n)
}

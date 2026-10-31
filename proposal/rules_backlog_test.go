package proposal

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/backlog"
)

// Rule 9 tells an agent how to turn a proposal's PR stack into backlog items.
// This test follows the rule as written — one sequential parent keyed by the
// slug, one child per stack row keyed "pr-<n>" — through the real batch code,
// so what the rule promises (drafts, stack order, a rerun updates rather than
// duplicates) is what af does, not only what the rule says.
func TestRule9BatchDoesWhatTheRuleSays(t *testing.T) {
	r := Rules("p", "/x/p")
	for _, want := range []string{
		`"af backlog add --file -"`, `{"proposal": "p", "items": [...]}`, `key "p" and "sequential": true`,
		`"parent": "p"`, `key "pr-<n>" from the row's "#"`, "the proposal path, /x/p", "Every item is a draft",
		"do not queue, start, drop or delete items", "same keys",
	} {
		if !strings.Contains(r, want) {
			t.Fatalf("rule 9 must say %s:\n%s", want, r)
		}
	}

	batch := func(titles ...string) backlog.Batch {
		items := []map[string]any{{"key": "p", "title": "Proposal p", "sequential": true}}
		for i, title := range titles {
			items = append(items, map[string]any{"key": fmt.Sprintf("pr-%d", i+1), "parent": "p", "title": title, "body": "what it does · /x/p"})
		}
		raw, err := json.Marshal(map[string]any{"proposal": "p", "items": items})
		if err != nil {
			t.Fatal(err)
		}
		var in backlog.Batch
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields() // as af backlog add --file decodes it
		if err := dec.Decode(&in); err != nil {
			t.Fatal(err)
		}
		return in
	}

	b := backlog.New()
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	first, err := b.AddBatch(batch("store", "routes"), now)
	if err != nil {
		t.Fatal(err)
	}
	parent := b.Item(first["p"])
	if parent == nil || !parent.Sequential {
		t.Fatalf("the parent must be sequential: %+v", parent)
	}
	if got, want := b.Children(parent.ID), []int{first["pr-1"], first["pr-2"]}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("children %v, want stack order %v", got, want)
	}
	for _, it := range b.Items {
		if it.Status != backlog.StatusDraft {
			t.Fatalf("%s is %q; every item the rule makes is a draft", backlog.FormatID(it.ID), it.Status)
		}
	}

	// The rule's update path: the same keys again, one title changed and a row added.
	again, err := b.AddBatch(batch("store v2", "routes", "web"), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if again["p"] != first["p"] || again["pr-1"] != first["pr-1"] || again["pr-2"] != first["pr-2"] {
		t.Fatalf("a rerun with the same keys must update, not add: first %v, again %v", first, again)
	}
	if got := b.Item(first["pr-1"]).Title; got != "store v2" {
		t.Fatalf("pr-1 title %q, want the updated one", got)
	}
	if len(b.Items) != 4 {
		t.Fatalf("%d items after the update, want the parent and three children", len(b.Items))
	}
}

// The batch fields rule 9 names are fields af backlog add --file accepts.
func TestRule9NamesOnlyBatchFields(t *testing.T) {
	r := Rules("p", "/x/p")
	rule := r[strings.Index(r, "\n9. "):]
	for _, field := range []string{"proposal", "items", "title", "sequential", "parent", "waits_on"} {
		if !strings.Contains(rule, `"`+field+`"`) {
			t.Fatalf("rule 9 no longer names %q; update this test with it", field)
		}
	}
	var in backlog.Batch
	raw := `{"proposal": "p", "items": [{"key": "k", "title": "t", "body": "b", "parent": "p", "waits_on": ["x"], "sequential": true}]}`
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		t.Fatalf("a field rule 9 names is not a batch field: %v", err)
	}
}

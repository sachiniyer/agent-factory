package proposal

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/backlog"
)

// row is one PR-stack row as rule 9 keys it: by what the PR is, not where it sits.
type row struct{ key, title string }

// ruleBatch is the batch rule 9 tells the agent to write for these stack rows.
func ruleBatch(t *testing.T, slug string, rows ...row) backlog.Batch {
	t.Helper()
	items := []map[string]any{{"key": "proposal", "title": "Proposal " + slug, "sequential": true}}
	for _, r := range rows {
		items = append(items, map[string]any{"key": r.key, "parent": "proposal", "title": r.title, "body": "what it does · /x/" + slug})
	}
	raw, err := json.Marshal(map[string]any{"proposal": slug, "items": items})
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

func titlesOf(b *backlog.Backlog, ids []int) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, b.Item(id).Title)
	}
	return out
}

func TestRule9SaysHowToKeyAndOrderTheItems(t *testing.T) {
	r := Rules("p", "/x/p")
	for _, want := range []string{
		`"af backlog add --file -"`, `{"proposal": "p", "items": [...]}`, `key "proposal" and "sequential": true`,
		`"parent": "proposal"`, `"pr-<name>" that names the PR`, "never its row number", "the proposal path, /x/p",
		"Every item is a draft", "do not queue, start, drop or delete items",
		`"af backlog list --all --json"`, "reuse each item's source key", "even if its row moved or was renamed",
		"does not reorder items that already exist and adds new ones last", `"af backlog move <id> --before <id>"`,
	} {
		if !strings.Contains(r, want) {
			t.Fatalf("rule 9 must say %s:\n%s", want, r)
		}
	}
}

// The rule's batch works for every slug af accepts: the parent's key is fixed
// and the children's keys start "pr-" and name a PR, so no slug can collide
// with either — a slug such as "pr-1" did when the parent was keyed by it.
func TestRule9BatchKeysNeverCollideWithTheSlug(t *testing.T) {
	for _, slug := range []string{"p", "pr-1", "pr-item-store", "proposal"} {
		b := backlog.New()
		in := ruleBatch(t, slug, row{"pr-item-store", "store"}, row{"pr-routes", "routes"})
		if _, err := b.AddBatch(in, time.Now()); err != nil {
			t.Errorf("slug %q: %v", slug, err)
		}
	}
}

// Following rule 9 through the real batch code: drafts, a sequential parent,
// children in stack order; then an update where the stack gained a row ahead
// of the others, one row was renamed and two swapped. Reused keys keep each
// item's identity; the batch appends the new item and leaves the old order;
// the rule's move step is what brings the children to the stack order.
func TestRule9BatchDoesWhatTheRuleSays(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	b := backlog.New()
	first, err := b.AddBatch(ruleBatch(t, "p", row{"pr-store", "store"}, row{"pr-routes", "routes"}, row{"pr-web", "web"}), now)
	if err != nil {
		t.Fatal(err)
	}
	parent := b.Item(first["proposal"])
	if parent == nil || !parent.Sequential {
		t.Fatalf("the parent must be sequential: %+v", parent)
	}
	if got := titlesOf(b, b.Children(parent.ID)); !slices.Equal(got, []string{"store", "routes", "web"}) {
		t.Fatalf("children %v, want stack order", got)
	}
	for _, it := range b.Items {
		if it.Status != backlog.StatusDraft {
			t.Fatalf("%s is %q; every item the rule makes is a draft", backlog.FormatID(it.ID), it.Status)
		}
	}

	// The new stack: schema first, then web and routes swapped, store renamed.
	stack := []row{{"pr-schema", "schema"}, {"pr-store", "store v2"}, {"pr-web", "web"}, {"pr-routes", "routes"}}
	again, err := b.AddBatch(ruleBatch(t, "p", stack...), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"proposal", "pr-store", "pr-routes", "pr-web"} {
		if again[key] != first[key] {
			t.Fatalf("%s: a reused key must update its item, not add one: first %v, again %v", key, first, again)
		}
	}
	if got := b.Item(first["pr-store"]).Title; got != "store v2" {
		t.Fatalf("the renamed row kept title %q", got)
	}
	if len(b.Items) != 5 {
		t.Fatalf("%d items after the update, want the parent and four children", len(b.Items))
	}
	// What the rule warns about: the batch kept the old order and put the new row last.
	if got := titlesOf(b, b.Children(parent.ID)); !slices.Equal(got, []string{"store v2", "routes", "web", "schema"}) {
		t.Fatalf("children after the batch %v; the rule's warning no longer matches the batch", got)
	}

	// The rule's fix: move each child to its stack position, in order.
	for i := 1; i < len(stack); i++ {
		if err := b.Move(again[stack[i].key], backlog.Move{After: again[stack[i-1].key]}, now.Add(2*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Move(again[stack[0].key], backlog.Move{Top: true}, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	want := []string{"schema", "store v2", "web", "routes"}
	if got := titlesOf(b, b.Children(parent.ID)); !slices.Equal(got, want) {
		t.Fatalf("children after the moves %v, want the stack %v", got, want)
	}
}

// The batch fields rule 9 names are fields af backlog add --file accepts.
func TestRule9NamesOnlyBatchFields(t *testing.T) {
	r := Rules("p", "/x/p")
	rule := r[strings.Index(r, "\n9. "):]
	for _, field := range []string{"proposal", "items", "title", "key", "sequential", "parent", "waits_on"} {
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

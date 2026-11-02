package backlog

import (
	"strings"
	"testing"
)

// An item session's agent adds the work it found as a draft (decision (3) on
// #6284): one item with a title and an optional parent, and af notes on its
// thread which item's agent added it.
func TestServiceAddFromAnItemSession(t *testing.T) {
	s, _ := newTestService(t)
	mustAdd(t, s, ItemInput{Key: "epic", Title: "Epic"}, ItemInput{Key: "a", Title: "A", Parent: Ref{Key: "epic"}})
	withRun(t, 2, StatusRunning)

	out, err := s.Add(AddRequest{RepoID: repoID, SessionID: "s1",
		Items: []ItemInput{{Key: "item", Title: "Found work", Parent: Ref{ID: 1}}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 1 {
		t.Fatalf("%+v", out)
	}
	it := out.Items[0]
	if it.Title != "Found work" || it.Parent != 1 || it.Status != StatusDraft {
		t.Fatalf("%+v", it)
	}
	if len(it.Thread) != 1 || it.Thread[0].Kind != KindNote || it.Thread[0].Author != AuthorAF || it.Thread[0].Text != "Added by the agent working on B-2." {
		t.Fatalf("origin note: %+v", it.Thread)
	}

	for name, req := range map[string]AddRequest{
		"a session that is no item's": {SessionID: "s9", Items: []ItemInput{{Key: "item", Title: "x"}}},
		"a body":                      {SessionID: "s1", Items: []ItemInput{{Key: "item", Title: "x", Body: "b"}}},
		"a waits-on":                  {SessionID: "s1", Items: []ItemInput{{Key: "item", Title: "x", WaitsOn: []Ref{{ID: 1}}}}},
		"a program":                   {SessionID: "s1", Items: []ItemInput{{Key: "item", Title: "x", Program: "claude"}}},
		"a placement":                 {SessionID: "s1", Items: []ItemInput{{Key: "item", Title: "x"}}, Before: 1},
		"a proposal batch":            {SessionID: "s1", Proposal: "p", Items: []ItemInput{{Key: "item", Title: "x"}}},
		"two items":                   {SessionID: "s1", Items: []ItemInput{{Key: "i", Title: "x"}, {Key: "j", Title: "y"}}},
	} {
		req.RepoID = repoID
		if _, err := s.Add(req); err == nil {
			t.Errorf("%s: an item session's add was accepted", name)
		} else if name != "a session that is no item's" && !strings.Contains(err.Error(), "adds one draft with a title and an optional parent") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

package backlog

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func orchService(t *testing.T) (*Service, *testHost) {
	t.Helper()
	s, h := newTestService(t)
	h.sessions = map[string]SessionInfo{
		"o1":   {Title: "planner", Program: "claude", State: SessionLive},
		"o2":   {Title: "other", Program: "codex", State: SessionArchived},
		"item": {Title: "worker", Item: 2, State: SessionLive},
		"dead": {Title: "gone", State: SessionGone},
	}
	return s, h
}

func TestSetOrchestratorBindAndClear(t *testing.T) {
	s, h := orchService(t)
	mustAdd(t, s, ItemInput{Key: "g", Title: "G"}, ItemInput{Key: "a", Title: "A", Parent: Ref{Key: "g"}})
	h.reset()
	out, err := s.SetOrchestrator(SetOrchestratorRequest{RepoID: repoID, ID: 1, SessionID: " o1 "})
	if err != nil {
		t.Fatal(err)
	}
	o := out.Orchestrator
	if out.ID != 1 || o == nil || o.SessionID != "o1" || o.Title != "planner" || o.Program != "claude" ||
		o.State != SessionLive || o.Pending != 0 || !o.BoundAt.Equal(s.now()) {
		t.Fatalf("%+v %+v", out, o)
	}
	if got := h.last(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("changed %v", got)
	}
	v, err := s.Get(GetRequest{RepoID: repoID})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := v.Orchestrators[1]; !ok || got.SessionID != "o1" || got.State != SessionLive || len(v.Orchestrators) != 1 {
		t.Fatalf("%+v", v.Orchestrators)
	}

	delete(h.sessions, "o1")
	if v, _ = s.Get(GetRequest{RepoID: repoID}); v.Orchestrators[1].State != SessionGone {
		t.Fatalf("missing session state %q", v.Orchestrators[1].State)
	}

	out, err = s.SetOrchestrator(SetOrchestratorRequest{RepoID: repoID, ID: 1, Clear: true})
	if err != nil || out.Orchestrator != nil {
		t.Fatalf("%+v %v", out, err)
	}
	if v, _ = s.Get(GetRequest{RepoID: repoID}); v.Orchestrators != nil || v.Backlog.Item(1).Orchestrator != nil {
		t.Fatalf("%+v", v.Orchestrators)
	}
}

func TestSetOrchestratorRefusals(t *testing.T) {
	s, _ := orchService(t)
	mustAdd(t, s, ItemInput{Key: "g", Title: "G"}, ItemInput{Key: "a", Title: "A", Parent: Ref{Key: "g"}},
		ItemInput{Key: "h", Title: "H"}, ItemInput{Key: "b", Title: "B", Parent: Ref{Key: "h"}})
	cases := []struct {
		req  SetOrchestratorRequest
		want string
	}{
		{SetOrchestratorRequest{ID: 1}, "session_id names the session"},
		{SetOrchestratorRequest{ID: 1, SessionID: "o1", Clear: true}, "cannot go with session_id"},
		{SetOrchestratorRequest{ID: 1, SessionID: "nope"}, `session "nope" is not a session of this project`},
		{SetOrchestratorRequest{ID: 1, SessionID: "dead"}, `session "dead" is not a session of this project`},
		{SetOrchestratorRequest{ID: 1, SessionID: "item"}, ErrOrchestrateFromItemSession.Error()},
		{SetOrchestratorRequest{ID: 2, SessionID: "o1"}, "B-2 is not a top-level parent; only groups can have an orchestrator"},
	}
	for _, c := range cases {
		c.req.RepoID = repoID
		if _, err := s.SetOrchestrator(c.req); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v", c.req, err)
		}
	}
	if _, err := s.SetOrchestrator(SetOrchestratorRequest{RepoID: repoID, ID: 1, SessionID: "o1"}); err != nil {
		t.Fatal(err)
	}
	_, err := s.SetOrchestrator(SetOrchestratorRequest{RepoID: repoID, ID: 3, SessionID: "o1"})
	if err == nil || err.Error() != `session "o1" already orchestrates B-1; clear it first` {
		t.Fatal(err)
	}
}

func TestSetOrchestratorRefusesARunSession(t *testing.T) {
	s, h := orchService(t)
	mustAdd(t, s, ItemInput{Key: "g", Title: "G"}, ItemInput{Key: "a", Title: "A", Parent: Ref{Key: "g"}})
	if _, err := Update(repoID, func(b *Backlog) error {
		b.Item(2).Runs = []RunRef{{SessionID: "o2", State: RunStarted}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h.sessions["o2"] = SessionInfo{Title: "other", State: SessionLive}
	_, err := s.SetOrchestrator(SetOrchestratorRequest{RepoID: repoID, ID: 1, SessionID: "o2"})
	if !errors.Is(err, ErrOrchestrateFromItemSession) {
		t.Fatal(err)
	}
}

func TestAddBatchOrchestrate(t *testing.T) {
	s, h := orchService(t)
	out, err := s.Add(AddRequest{RepoID: repoID, Proposal: "p", Orchestrator: "o2", Items: []ItemInput{
		{Key: "g", Title: "G"}, {Key: "a", Title: "A", Parent: Ref{Key: "g"}}, {Key: "b", Title: "B", Parent: Ref{Key: "g"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if o := out.Orchestrator; o == nil || o.ID != 1 || o.Orchestrator.SessionID != "o2" || o.Orchestrator.State != SessionArchived ||
		o.Change != OrchestratorBound || o.Previous != "" {
		t.Fatalf("%+v", out.Orchestrator)
	}
	b, _ := Load(repoID)
	if b.Item(1).Orchestrator == nil || b.Item(1).Orchestrator.Title != "other" {
		t.Fatalf("%+v", b.Item(1).Orchestrator)
	}

	out, err = s.Add(AddRequest{RepoID: repoID, Orchestrator: "o2", Items: []ItemInput{
		{Key: "h", Title: "H"}, {Key: "c", Title: "C", Parent: Ref{Key: "h"}}}})
	if err == nil || !strings.Contains(err.Error(), `already orchestrates B-1`) {
		t.Fatalf("%+v %v", out, err)
	}
	if b, _ := Load(repoID); len(b.Items) != 3 {
		t.Fatalf("refused batch was written: %d items", len(b.Items))
	}

	h.reset()
	_, err = s.Add(AddRequest{RepoID: repoID, Orchestrator: "o1", Items: []ItemInput{{Key: "c", Title: "C", Parent: Ref{ID: 1}}}})
	if err == nil || err.Error() != `B-1 is orchestrated by session "o2"; pass --replace to take it over` {
		t.Fatal(err)
	}
	if len(h.changed) != 0 {
		t.Fatalf("refused add changed %v", h.changed)
	}
	out, err = s.Add(AddRequest{RepoID: repoID, Orchestrator: "o1", ReplaceOrchestrator: true,
		Items: []ItemInput{{Key: "c", Title: "C", Parent: Ref{ID: 1}}}})
	if err != nil || out.Orchestrator == nil || out.Orchestrator.ID != 1 || out.Orchestrator.Orchestrator.SessionID != "o1" ||
		out.Orchestrator.Change != OrchestratorReplaced || out.Orchestrator.Previous != "o2" {
		t.Fatalf("%+v %v", out.Orchestrator, err)
	}
	if len(out.Items) != 1 || out.Items[0].ID != 4 {
		t.Fatalf("items %+v", out.Items)
	}
	if got := h.last(); len(got) != 2 || got[0] != 4 || got[1] != 1 {
		t.Fatalf("changed %v", got)
	}
}

func TestAddBatchOrchestrateNeedsOneGroup(t *testing.T) {
	s, _ := orchService(t)
	for _, req := range []AddRequest{
		{Items: []ItemInput{{Key: "a", Title: "A"}}},
		{Items: []ItemInput{{Key: "g", Title: "G"}, {Key: "a", Title: "A", Parent: Ref{Key: "g"}}, {Key: "b", Title: "B"}}},
	} {
		req.RepoID, req.Orchestrator = repoID, "o1"
		if _, err := s.Add(req); err == nil {
			t.Errorf("%+v: no error", req.Items)
		}
	}
	_, err := s.Add(AddRequest{RepoID: repoID, Orchestrator: "o1", Items: []ItemInput{
		{Key: "g", Title: "G"}, {Key: "a", Title: "A", Parent: Ref{Key: "g"}}, {Key: "b", Title: "B"}}})
	if err == nil || !strings.Contains(err.Error(), "under 2 top-level items (B-1, B-3)") {
		t.Fatal(err)
	}
	if b, _ := Load(repoID); len(b.Items) != 0 {
		t.Fatalf("refused batch was written: %d items", len(b.Items))
	}
	if _, err := s.Add(AddRequest{RepoID: repoID, Orchestrator: "item", Items: []ItemInput{{Key: "a", Title: "A"}}}); !errors.Is(err, ErrOrchestrateFromItemSession) {
		t.Fatal(err)
	}
	if _, err := s.Add(AddRequest{RepoID: repoID, Orchestrator: "o1", After: 1, Items: []ItemInput{{Key: "a", Title: "A"}}}); err == nil {
		t.Fatal("orchestrator with after accepted")
	}
}

// TestAddBatchOrchestrateCountsUnkeyedItems: a batch without a proposal may
// leave its items unkeyed, so the group is found from every item the batch
// added or updated, not from its key map (Codex P2 on #6559).
func TestAddBatchOrchestrateCountsUnkeyedItems(t *testing.T) {
	s, _ := orchService(t)
	if _, err := s.Add(AddRequest{RepoID: repoID, Items: []ItemInput{{Key: "g", Title: "G"}, {Key: "a", Title: "A", Parent: Ref{Key: "g"}}}}); err != nil {
		t.Fatal(err)
	}
	// Unkeyed children under the existing group B-1: they are the batch, and B-1 is its group.
	out, err := s.Add(AddRequest{RepoID: repoID, Orchestrator: "o1", Items: []ItemInput{
		{Title: "C", Parent: Ref{ID: 1}}, {Title: "D", Parent: Ref{ID: 1}}}})
	if err != nil || out.Orchestrator == nil || out.Orchestrator.ID != 1 || out.Orchestrator.Orchestrator.SessionID != "o1" {
		t.Fatalf("%+v %v", out, err)
	}
	// A keyed group and an unrelated unkeyed root: two top-level items, refused.
	_, err = s.Add(AddRequest{RepoID: repoID, Orchestrator: "o1", ReplaceOrchestrator: true, Items: []ItemInput{
		{Key: "h", Title: "H"}, {Key: "x", Title: "X", Parent: Ref{Key: "h"}}, {Title: "loose"}}})
	if err == nil || !strings.Contains(err.Error(), "under 2 top-level items") {
		t.Fatalf("a batch under two top-level items must be refused: %v", err)
	}
}

func TestSetOrchestratorHandoff(t *testing.T) {
	s, _ := orchService(t)
	mustAdd(t, s, ItemInput{Key: "g", Title: "G"}, ItemInput{Key: "a", Title: "A", Parent: Ref{Key: "g"}})
	if _, err := s.SetOrchestratorHandoff(SetOrchestratorHandoffRequest{RepoID: repoID, SessionID: "o1", Handoff: "x"}); err == nil ||
		!strings.Contains(err.Error(), "orchestrates no group") {
		t.Fatal(err)
	}
	if _, err := s.SetOrchestrator(SetOrchestratorRequest{RepoID: repoID, ID: 1, SessionID: "o1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetOrchestratorHandoff(SetOrchestratorHandoffRequest{RepoID: repoID, SessionID: "o1",
		Handoff: strings.Repeat("x", MaxHandoffBytes+1)}); err == nil || !strings.Contains(err.Error(), "limit is 8 KB") {
		t.Fatal(err)
	}
	out, err := s.SetOrchestratorHandoff(SetOrchestratorHandoffRequest{RepoID: repoID, SessionID: "o1", Handoff: "  notes\n"})
	if err != nil || out.ID != 1 || out.Orchestrator.Handoff != "notes" {
		t.Fatalf("%+v %v", out, err)
	}
	if _, err := s.SetOrchestrator(SetOrchestratorRequest{RepoID: repoID, ID: 1, SessionID: "o2", Replace: true}); err != nil {
		t.Fatal(err)
	}
	if b, _ := Load(repoID); b.Item(1).Orchestrator.Handoff != "notes" {
		t.Fatal("replacement lost the handoff")
	}
}

func TestSetOrchestratorReplaceAndRepeat(t *testing.T) {
	s, h := orchService(t)
	mustAdd(t, s, ItemInput{Key: "g", Title: "G"}, ItemInput{Key: "a", Title: "A", Parent: Ref{Key: "g"}})
	out, err := s.SetOrchestrator(SetOrchestratorRequest{RepoID: repoID, ID: 1, SessionID: "o1"})
	if err != nil || out.Change != OrchestratorBound {
		t.Fatalf("%+v %v", out, err)
	}
	bound := out.Orchestrator.BoundAt
	s.now = func() time.Time { return bound.Add(time.Hour) }

	h.reset()
	out, err = s.SetOrchestrator(SetOrchestratorRequest{RepoID: repoID, ID: 1, SessionID: "o1"})
	if err != nil || out.Change != "" || !out.Orchestrator.BoundAt.Equal(bound) {
		t.Fatalf("repeat bind: %+v %v", out, err)
	}
	if len(h.changed) != 0 {
		t.Fatalf("repeat bind changed %v", h.changed)
	}

	_, err = s.SetOrchestrator(SetOrchestratorRequest{RepoID: repoID, ID: 1, SessionID: "o2"})
	if err == nil || err.Error() != `B-1 is orchestrated by session "o1"; pass --replace to take it over` {
		t.Fatal(err)
	}
	out, err = s.SetOrchestrator(SetOrchestratorRequest{RepoID: repoID, ID: 1, SessionID: "o2", Replace: true})
	if err != nil || out.Change != OrchestratorReplaced || out.Previous != "o1" || out.Orchestrator.SessionID != "o2" {
		t.Fatalf("%+v %v", out, err)
	}
}

func TestSetOrchestratorWithHandoffIsOneWrite(t *testing.T) {
	s, h := orchService(t)
	mustAdd(t, s, ItemInput{Key: "g", Title: "G"}, ItemInput{Key: "a", Title: "A", Parent: Ref{Key: "g"}})
	h.reset()
	big := strings.Repeat("x", MaxHandoffBytes+1)
	if _, err := s.SetOrchestrator(SetOrchestratorRequest{RepoID: repoID, ID: 1, SessionID: "o1", Handoff: &big}); err == nil {
		t.Fatal("too big handoff accepted")
	}
	if b, _ := Load(repoID); b.Item(1).Orchestrator != nil || len(h.changed) != 0 {
		t.Fatal("refused handoff left the bind")
	}
	notes := "notes"
	out, err := s.SetOrchestrator(SetOrchestratorRequest{RepoID: repoID, ID: 1, SessionID: "o1", Handoff: &notes})
	if err != nil || out.Change != OrchestratorBound || out.Orchestrator.Handoff != "notes" || len(h.changed) != 1 {
		t.Fatalf("%+v %v %v", out, err, h.changed)
	}
	more := "more"
	out, err = s.SetOrchestrator(SetOrchestratorRequest{RepoID: repoID, ID: 1, SessionID: "o1", Handoff: &more})
	if err != nil || out.Change != "" || out.Orchestrator.Handoff != "more" || len(h.changed) != 2 {
		t.Fatalf("%+v %v %v", out, err, h.changed)
	}
}

func TestClearOrchestratorRules(t *testing.T) {
	s, h := orchService(t)
	mustAdd(t, s, ItemInput{Key: "g", Title: "G"}, ItemInput{Key: "a", Title: "A", Parent: Ref{Key: "g"}})
	h.reset()
	_, err := s.SetOrchestrator(SetOrchestratorRequest{RepoID: repoID, ID: 2, Clear: true})
	if err == nil || err.Error() != "B-2 is not a top-level parent; only groups have an orchestrator — clear its group, B-1" {
		t.Fatal(err)
	}
	out, err := s.SetOrchestrator(SetOrchestratorRequest{RepoID: repoID, ID: 1, Clear: true})
	if err != nil || out.Orchestrator != nil || out.Change != "" {
		t.Fatalf("%+v %v", out, err)
	}
	if len(h.changed) != 0 {
		t.Fatalf("clear of an unbound group changed %v", h.changed)
	}
	if _, err := s.SetOrchestrator(SetOrchestratorRequest{RepoID: repoID, ID: 1, Clear: true, Replace: true}); err == nil {
		t.Fatal("clear with replace accepted")
	}
}

func TestAnswerOrchEscalationResumesDelivery(t *testing.T) {
	s, _ := orchService(t)
	mustAdd(t, s, ItemInput{Key: "g", Title: "G"}, ItemInput{Key: "a", Title: "A", Parent: Ref{Key: "g"}})
	if _, err := s.SetOrchestrator(SetOrchestratorRequest{RepoID: repoID, ID: 1, SessionID: "o1"}); err != nil {
		t.Fatal(err)
	}
	now := s.now()
	if _, err := s.Apply(ProjectRef{ID: repoID}, func(b *Backlog) ([]int, error) {
		b.QueueOrchEvent(OrchEvent{Kind: EventReview, Item: 2}, now)
		for range OrchMaxFailures {
			b.OrchDeliveryFailed(1, "o1", "no tmux", now)
		}
		return []int{1}, nil
	}); err != nil {
		t.Fatal(err)
	}
	b, _ := Load(repoID)
	if !b.OrchEscalated(1) {
		t.Fatal("not escalated")
	}

	out, err := s.PostMessage(PostMessageRequest{RepoID: repoID, ID: 1, Kind: KindAnswer, Text: "restarted tmux"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Message.Delivery != "" {
		t.Fatalf("a group answer is delivered: %+v", out.Message)
	}
	b, _ = Load(repoID)
	if b.OrchEscalated(1) || b.Item(1).Orchestrator.Failures != nil || len(b.OrchDeliveries(now)) != 1 {
		t.Fatalf("delivery did not resume: %+v", b.Item(1).Orchestrator)
	}
}

package backlog

import (
	"strings"
	"testing"
)

func TestDismissNote(t *testing.T) {
	b := eventsGroup(t)
	if !b.noteOrchestratorDone(2, "o", t0) {
		t.Fatal("no done note")
	}
	if _, err := b.AddNote(2, AuthorUser, "plain", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := b.DismissNote(2, 2, t0); err == nil || !strings.Contains(err.Error(), "message 2 on B-2 is not a note in the Inbox") {
		t.Fatalf("plain note: %v", err)
	}
	if _, err := b.DismissNote(2, 9, t0); err == nil || !strings.Contains(err.Error(), "B-2 has no message 9") {
		t.Fatalf("missing note: %v", err)
	}
	if _, err := b.DismissNote(99, 1, t0); err == nil {
		t.Fatal("missing item accepted")
	}
	m, err := b.DismissNote(2, 1, t0)
	if err != nil || m.Inbox || m.ID != 1 {
		t.Fatalf("%+v %v", m, err)
	}
	if notes := b.InboxNotes(); len(notes) != 0 {
		t.Fatalf("inbox notes %+v", notes)
	}
	if th := b.Item(2).Thread; len(th) != 2 || th[0].Text != OrchestratorDoneNote("") {
		t.Fatalf("thread %+v", th)
	}
	if _, err := b.DismissNote(2, 1, t0); err == nil || !strings.Contains(err.Error(), "not a note in the Inbox") {
		t.Fatalf("second dismiss: %v", err)
	}
}

func TestServiceDismissNote(t *testing.T) {
	s, h := orchService(t)
	mustAdd(t, s, ItemInput{Key: "g", Title: "G"}, ItemInput{Key: "a", Title: "A", Parent: Ref{Key: "g"}})
	if _, err := s.SetOrchestrator(SetOrchestratorRequest{RepoID: repoID, ID: 1, SessionID: "o1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetItemStatus(SetItemStatusRequest{RepoID: repoID, ID: 2, Status: StatusDone, CallerSessionID: "o1"}); err != nil {
		t.Fatal(err)
	}
	h.reset()
	out, err := s.DismissNote(DismissNoteRequest{RepoID: repoID, ID: 2, NoteID: 1})
	if err != nil || out.ID != 2 || out.Note.ID != 1 || out.Note.Inbox {
		t.Fatalf("%+v %v", out, err)
	}
	if got := h.last(); len(got) != 1 || got[0] != 2 {
		t.Fatalf("changed %v", got)
	}
	b, _ := Load(repoID)
	if notes := b.InboxNotes(); len(notes) != 0 {
		t.Fatalf("inbox notes %+v", notes)
	}
	if th := b.Item(2).Thread; len(th) != 1 || th[0].Inbox {
		t.Fatalf("thread %+v", th)
	}
	if _, err := s.DismissNote(DismissNoteRequest{RepoID: repoID, ID: 9, NoteID: 1}); err == nil || !strings.Contains(err.Error(), "B-9 not found in project") {
		t.Fatalf("missing item: %v", err)
	}
}

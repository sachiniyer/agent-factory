package backlog

import (
	"fmt"
	"time"
)

// DismissNote takes Inbox note noteID on item id out of the Inbox. The note
// stays in the item's thread. It returns the note.
func (b *Backlog) DismissNote(id, noteID int, now time.Time) (*Message, error) {
	it, err := b.get(id)
	if err != nil {
		return nil, err
	}
	for i := range it.Thread {
		m := &it.Thread[i]
		if m.ID != noteID {
			continue
		}
		if m.Kind != KindNote || !m.Inbox {
			return nil, fmt.Errorf("message %d on %s is not a note in the Inbox", noteID, FormatID(id))
		}
		m.Inbox = false
		it.UpdatedAt = now
		out := *m
		return &out, nil
	}
	return nil, fmt.Errorf("%s has no message %d — \"af backlog inbox\" prints each note's id", FormatID(id), noteID)
}

// DismissNoteRequest names a note by its item and its message id, the id
// "af backlog inbox" prints after "note".
type DismissNoteRequest struct {
	RepoID   string `json:"repo_id,omitempty"`
	RepoPath string `json:"repo_path,omitempty"`
	ID       int    `json:"id"`
	NoteID   int    `json:"note_id"`
}

// DismissNoteResponse is the item and the dismissed note.
type DismissNoteResponse struct {
	ID   int     `json:"id"`
	Note Message `json:"note"`
}

// DismissNote takes an Inbox note out of the Inbox; it stays in the item's
// thread.
func (s *Service) DismissNote(req DismissNoteRequest) (*DismissNoteResponse, error) {
	p, err := s.resolve(req.RepoID, req.RepoPath)
	if err != nil {
		return nil, err
	}
	var note Message
	_, err = s.update(p, func(b *Backlog) ([]int, error) {
		m, err := b.DismissNote(req.ID, req.NoteID, s.now())
		if err != nil {
			return nil, err
		}
		note = *m
		return []int{req.ID}, nil
	})
	if err != nil {
		return nil, err
	}
	return &DismissNoteResponse{ID: req.ID, Note: note}, nil
}

package backlog

import (
	"fmt"
	"sort"
	"strings"
)

// MaxHandoffBytes caps a question's handoff notes.
const MaxHandoffBytes = 8 << 10

// Question is what an agent asks: the question, and notes it leaves for a later
// run of the item (Message.Handoff).
type Question struct {
	Text    string
	Handoff string
}

func (q Question) check() error {
	if n := len(strings.TrimSpace(q.Handoff)); n > MaxHandoffBytes {
		return fmt.Errorf("the handoff is %d bytes; the limit is 8 KB (%d bytes)", n, MaxHandoffBytes)
	}
	return nil
}

func (it *Item) openQuestions() []*Message {
	var out []*Message
	for i := range it.Thread {
		if m := &it.Thread[i]; m.Kind == KindQuestion && it.questionOpen(m.ID) {
			out = append(out, m)
		}
	}
	return out
}

// InboxEntry is one open question in the project's inbox.
type InboxEntry struct {
	ID       int     `json:"id"`
	Title    string  `json:"title"`
	Question Message `json:"question"`
}

// Inbox lists every open question of every item, oldest first. Items in an
// archived group are left out.
func (b *Backlog) Inbox() []InboxEntry {
	out := []InboxEntry{}
	for i := range b.Items {
		it := &b.Items[i]
		if b.ArchivedBy(it.ID) != 0 {
			continue
		}
		for _, q := range it.openQuestions() {
			out = append(out, InboxEntry{ID: it.ID, Title: it.Title, Question: *q})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Question.At.Before(out[j].Question.At) })
	return out
}

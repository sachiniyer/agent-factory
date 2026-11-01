package backlog

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// MaxHandoffBytes caps a question's handoff notes.
const MaxHandoffBytes = 8 << 10

// What an asking session does once its run ends (Message.Then).
const (
	// ThenWait keeps the session live until the answer comes.
	ThenWait = "wait"
	// ThenArchive archives the session until the answer comes; the answer
	// restores it, with its conversation, and is delivered there.
	ThenArchive = "archive"
)

// Why af archives an item session.
const (
	// ArchiveDone: the user marked the item done and on_done is archive.
	ArchiveDone = "done"
	// ArchiveAsk: the run ended on open questions all asked with then archive.
	ArchiveAsk = "ask"
)

// Question is what an agent asks: the question, what its session does once
// the run ends (Then: "", ThenWait or ThenArchive; "" waits, as a question
// from before then existed does), and notes it leaves for a later run of the
// item (Message.Handoff).
type Question struct {
	Text    string
	Then    string
	Handoff string
}

func (q Question) check() error {
	switch q.Then {
	case "", ThenWait, ThenArchive:
	default:
		return fmt.Errorf("then %q is not %s or %s", q.Then, ThenWait, ThenArchive)
	}
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

// archiveAsked reports whether the item's open questions ask for an archive:
// there is at least one, and every one was asked with then archive. One asked
// with then wait (or before then existed) keeps the session live.
func (it *Item) archiveAsked() bool {
	open := it.openQuestions()
	for _, q := range open {
		if q.Then != ThenArchive {
			return false
		}
	}
	return len(open) > 0
}

// ArchiveOwed reports whether af still owes an archive of sessionID for
// reason: the session is its item's latest run and the reason still holds.
// An answer on its way to the session cancels an ask archive.
func (b *Backlog) ArchiveOwed(sessionID, reason string) bool {
	it := b.ItemForSession(sessionID)
	if it == nil || it.lastRun().SessionID != sessionID {
		return false
	}
	switch reason {
	case ArchiveDone:
		return it.Status == StatusDone && b.Settings.OnDone == OnDoneArchive
	case ArchiveAsk:
		return it.Status.Open() && it.archiveAsked() && !it.answerPending()
	}
	return false
}

// KeptLiveNote is the note af adds when it keeps a session live that it was
// going to archive.
func KeptLiveNote(why string) string { return "Kept the session live: " + why + "." }

// KeptLive records that af did not archive sessionID, owed for reason,
// because of why: a note on the item and, for an ask archive, the open
// questions' then becomes wait, so a later run end or restart does not try
// again. It returns the item id, or 0 when the archive is no longer owed (an
// answer came first) and nothing changed.
func (b *Backlog) KeptLive(sessionID, reason, why string, now time.Time) (int, error) {
	if !b.ArchiveOwed(sessionID, reason) {
		return 0, nil
	}
	it := b.ItemForSession(sessionID)
	if reason == ArchiveAsk {
		for _, q := range it.openQuestions() {
			q.Then = ThenWait
		}
	}
	if _, err := b.AddNote(it.ID, AuthorAF, KeptLiveNote(why), now); err != nil {
		return 0, err
	}
	return it.ID, nil
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

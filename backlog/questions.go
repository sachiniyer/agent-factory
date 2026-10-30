package backlog

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// AnswerPrompt is what af delivers to the item's session with an answer.
func AnswerPrompt(it *Item, text string) string {
	return fmt.Sprintf("The user answered your question on %s: %s", FormatID(it.ID), text)
}

// GoneError is returned when an answer cannot reach the item's session. The
// answer is still written to the thread.
func GoneError(id int) error {
	return fmt.Errorf("the session for %s is gone; start it again with \"af backlog start %d\" — your answer is kept in the thread", FormatID(id), id)
}

func noOpenQuestion(id int) error {
	return fmt.Errorf("%s has no open question — use \"af backlog note\" to add a note", FormatID(id))
}

func (it *Item) nextMessageID() int {
	next := 1
	for _, m := range it.Thread {
		if m.ID >= next {
			next = m.ID + 1
		}
	}
	return next
}

func (it *Item) appendMessage(m Message, now time.Time) *Message {
	m.ID = it.nextMessageID()
	m.At = now
	it.Thread = append(it.Thread, m)
	it.UpdatedAt = now
	return &it.Thread[len(it.Thread)-1]
}

// Ask appends a question from the agent of sessionID, which must be the item's
// latest run. The item must be open.
func (b *Backlog) Ask(id int, sessionID, text string, now time.Time) (*Message, error) {
	it, err := b.get(id)
	if err != nil {
		return nil, err
	}
	if last := it.lastRun(); last == nil || last.SessionID == "" || last.SessionID != sessionID {
		return nil, fmt.Errorf("session %s is not the current run of %s; only that session can ask on it", sessionID, FormatID(id))
	}
	if !it.Status.Open() {
		return nil, fmt.Errorf("%s is %s; questions go on open items", FormatID(id), it.Status)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("a question needs text")
	}
	return it.appendMessage(Message{Kind: KindQuestion, Author: "agent:" + sessionID, Text: text}, now), nil
}

// Answered is what Answer did.
type Answered struct {
	Answer Message
	// SessionID is the item's latest run, the session the answer goes to; ""
	// when the item never started.
	SessionID string
	// Next is the oldest question still open after the answer, or nil.
	Next *Message
}

// Answer appends the user's answer to question questionID, or to the oldest
// open question when questionID is 0, and closes it. With deliver the item is
// set running and the answer is pending delivery to its session; the status
// it replaced is kept on the answer for AnswerFailed. On error b is unchanged.
func (b *Backlog) Answer(id, questionID int, text string, deliver bool, now time.Time) (Answered, error) {
	it, err := b.get(id)
	if err != nil {
		return Answered{}, err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return Answered{}, errors.New("an answer needs text")
	}
	q := it.OpenQuestion()
	if questionID != 0 {
		q = nil
		for i := range it.Thread {
			if m := &it.Thread[i]; m.ID == questionID && m.Kind == KindQuestion && it.questionOpen(m.ID) {
				q = m
			}
		}
		if q == nil {
			return Answered{}, fmt.Errorf("%s has no open question %d — run \"af backlog show %d\" to see its thread", FormatID(id), questionID, id)
		}
	}
	if q == nil {
		return Answered{}, noOpenQuestion(id)
	}
	q.Closed = ClosedAnswered
	answer := Message{Kind: KindAnswer, Author: AuthorUser, Text: text, Answers: q.ID}
	if deliver && it.Status.Open() {
		// The status to give back if delivery fails is the one before the OLDEST
		// pending answer: a second answer accepted while the first is pending would
		// otherwise save "running" (what the first set) and never get the item back
		// to where it was when both fail.
		prev := it.Status
		for _, x := range it.Thread {
			if x.Kind == KindAnswer && x.Delivery == DeliveryPending && x.PrevStatus.Valid() {
				prev = x.PrevStatus
				break
			}
		}
		answer.Delivery, answer.PrevStatus = DeliveryPending, prev
		it.Status = StatusRunning
	}
	out := Answered{Answer: *it.appendMessage(answer, now)}
	if last := it.lastRun(); last != nil {
		out.SessionID = last.SessionID
	}
	if n := it.OpenQuestion(); n != nil {
		c := *n
		out.Next = &c
	}
	return out, nil
}

func (it *Item) questionOpen(id int) bool {
	for _, m := range it.Thread {
		if m.Kind == KindAnswer && m.Answers == id {
			return false
		}
	}
	for _, m := range it.Thread {
		if m.ID == id {
			return m.Kind == KindQuestion && m.Closed == ""
		}
	}
	return false
}

func (it *Item) message(id int) *Message {
	for i := range it.Thread {
		if it.Thread[i].ID == id {
			return &it.Thread[i]
		}
	}
	return nil
}

// AnswerDelivered records that answer answerID of id reached its session.
func (b *Backlog) AnswerDelivered(id, answerID int, now time.Time) error {
	it, err := b.get(id)
	if err != nil {
		return err
	}
	if m := it.message(answerID); m != nil && m.Kind == KindAnswer {
		m.Delivery = DeliveryDelivered
		it.UpdatedAt = now
	}
	return nil
}

// AnswerFailed records that answer answerID of id could not reach its
// session: the status the answer replaced comes back, unless the item moved
// on since, and a note from af says why. The answer stays in the thread.
func (b *Backlog) AnswerFailed(id, answerID int, reason string, now time.Time) error {
	it, err := b.get(id)
	if err != nil {
		return err
	}
	m := it.message(answerID)
	if m == nil || m.Kind != KindAnswer {
		return fmt.Errorf("%s has no answer %d", FormatID(id), answerID)
	}
	prev := m.PrevStatus
	m.Delivery = DeliveryFailed
	// A newer answer that is pending or was delivered is work the item is
	// running for: the status this answer replaced must not come back over it.
	newerWork := false
	for _, x := range it.Thread {
		if x.Kind == KindAnswer && x.ID > answerID && (x.Delivery == DeliveryPending || x.Delivery == DeliveryDelivered) {
			newerWork = true
		}
	}
	if !newerWork && it.Status == StatusRunning && prev.Valid() && prev != StatusRunning && CheckStatusChange(StatusChange{ID: id,
		From: it.Status, To: prev, By: ActorAF, StartFailed: true, PrevStatus: prev}) == nil {
		it.Status = prev
	}
	_, err = b.AddNote(id, AuthorAF, "The answer was not delivered: "+reason+". It is kept in the thread.", now)
	return err
}

// PendingAnswer is an answer af has not yet delivered.
type PendingAnswer struct {
	ID        int
	AnswerID  int
	SessionID string
	Prompt    string
}

// PendingAnswers lists the answers still pending delivery, oldest first per
// item.
func (b *Backlog) PendingAnswers() []PendingAnswer {
	var out []PendingAnswer
	for i := range b.Items {
		it := &b.Items[i]
		last := it.lastRun()
		for _, m := range it.Thread {
			if m.Kind != KindAnswer || m.Delivery != DeliveryPending {
				continue
			}
			p := PendingAnswer{ID: it.ID, AnswerID: m.ID, Prompt: AnswerPrompt(it, m.Text)}
			if last != nil {
				p.SessionID = last.SessionID
			}
			out = append(out, p)
		}
	}
	return out
}

// AnsweredInSessionText closes the question the user SAW as answered in the
// session's terminal: the
// terminal binding captures what the session's attention showed (the oldest open
// question's text) before forwarding the line, without touching the backlog
// file. It closes the oldest open question only if that is the question with this
// text; a different or newer one is left alone. It returns the item id, or 0.
func (b *Backlog) AnsweredInSessionText(sessionID, text string, now time.Time) int {
	it := b.ItemForSession(sessionID)
	if it == nil || it.lastRun().SessionID != sessionID || text == "" {
		return 0
	}
	open := it.OpenQuestion()
	if open == nil || open.Text != text {
		return 0
	}
	open.Closed = ClosedAnsweredInSession
	it.UpdatedAt = now
	return it.ID
}

// OpenQuestionText is the text of the item's oldest open question, or "".
func (b *Backlog) OpenQuestionText(id int) string {
	if it := b.Item(id); it != nil {
		if q := it.OpenQuestion(); q != nil {
			return q.Text
		}
	}
	return ""
}

// questionSessions maps each item with an open question to its latest run's
// session id.
func (b *Backlog) questionSessions() map[int]string {
	out := map[int]string{}
	for i := range b.Items {
		it := &b.Items[i]
		if it.OpenQuestion() == nil {
			continue
		}
		if last := it.lastRun(); last != nil && last.SessionID != "" {
			out[it.ID] = last.SessionID
		}
	}
	return out
}

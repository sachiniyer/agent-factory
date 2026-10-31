// Package backlog holds the ordered tree of work items for one project and the
// global budget, the share of the Claude plan the runner may use.
package backlog

import (
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"
)

type Status string

const (
	StatusDraft   Status = "draft"
	StatusQueued  Status = "queued"
	StatusRunning Status = "running"
	StatusReview  Status = "review"
	StatusDone    Status = "done"
	StatusDropped Status = "dropped"
)

func (s Status) Valid() bool {
	switch s {
	case StatusDraft, StatusQueued, StatusRunning, StatusReview, StatusDone, StatusDropped:
		return true
	}
	return false
}

func (s Status) Open() bool { return s != StatusDone && s != StatusDropped }

const (
	RunStarting = "starting"
	RunStarted  = "started"
	RunFailed   = "failed"

	StartedByRunner = "runner"
	StartedByUser   = "user"

	KindQuestion = "question"
	KindAnswer   = "answer"
	KindNote     = "note"

	AuthorUser = "user"
	AuthorAF   = "af"

	ClosedAnswered          = "answered"
	ClosedAnsweredInSession = "answered in session"
	ClosedSentToReview      = "sent to review"
	ClosedDone              = "done"
	ClosedDropped           = "dropped"

	DeliveryPending   = "pending"
	DeliveryDelivered = "delivered"
	DeliveryFailed    = "failed"

	OnDoneKeep    = "keep"
	OnDoneArchive = "archive"

	DefaultMaxRunning = 1
	MaxMaxRunning     = 8
)

type Backlog struct {
	SchemaVersion int      `json:"schema_version"`
	NextID        int      `json:"next_id"`
	Settings      Settings `json:"settings"`
	Items         []Item   `json:"items"`
}

type Item struct {
	ID         int    `json:"id"`
	Title      string `json:"title"`
	Body       string `json:"body,omitempty"`
	Status     Status `json:"status"`
	Parent     int    `json:"parent,omitempty"`
	Sequential bool   `json:"sequential,omitempty"`
	// LiveAnswers counts the answers accepted for delivery whose work is still
	// the reason the item is running: +1 on accept, -1 when one fails, 0 when the
	// item leaves running. A failed delivery gives the replaced status back only
	// when it was the last (Codex P2 on #6295).
	LiveAnswers int       `json:"live_answers,omitempty"`
	WaitsOn     []int     `json:"waits_on,omitempty"`
	Program     string    `json:"program,omitempty"`
	Source      *Source   `json:"source,omitempty"`
	PR          string    `json:"pr,omitempty"`
	External    string    `json:"external,omitempty"`
	Runs        []RunRef  `json:"runs,omitempty"`
	Thread      []Message `json:"thread,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type Source struct {
	Proposal string `json:"proposal"`
	Key      string `json:"key"`
}

type RunRef struct {
	SessionID  string           `json:"session_id"`
	Title      string           `json:"title"`
	State      string           `json:"state"`
	PrevStatus Status           `json:"prev_status"`
	StartedBy  string           `json:"started_by"`
	StartedAt  time.Time        `json:"started_at"`
	Meter      map[string]Meter `json:"meter,omitempty"`
	// EndedAt is when a failed start failed.
	EndedAt time.Time `json:"ended_at,omitzero"`
	// CapReset names the 5-hour window the cap per run counts in, by its reset
	// in epoch seconds; CapBase is the run's cost the cap counts from: at the
	// window's start, or when af last stopped the run at the cap.
	CapReset int64   `json:"cap_reset,omitempty"`
	CapBase  float64 `json:"cap_base,omitempty"`
}

type Meter struct {
	Offset int64   `json:"offset"`
	Cost   float64 `json:"cost"`
	// Last is the last message id counted, so a message whose lines straddle
	// two reads is counted once.
	Last string `json:"last,omitempty"`
}

type Message struct {
	ID      int       `json:"id"`
	Kind    string    `json:"kind"`
	Author  string    `json:"author"`
	Text    string    `json:"text"`
	Answers int       `json:"answers,omitempty"`
	Closed  string    `json:"closed,omitempty"`
	At      time.Time `json:"at"`
	// Delivery tracks an answer on its way to the item's session, so a daemon
	// restart can finish it. PrevStatus is the item status the answer replaced,
	// put back if the delivery fails.
	Delivery   string `json:"delivery,omitempty"`
	PrevStatus Status `json:"prev_status,omitempty"`
	// RunCount is how many runs the item had when the answer was accepted: the
	// answer belongs to the latest of them. A rollback applies only while no newer
	// run has begun (zero, from before this field, is not fenced).
	RunCount int `json:"run_count,omitempty"`
}

type Settings struct {
	Runner     bool   `json:"runner"`
	MaxRunning int    `json:"max_running"`
	OnDone     string `json:"on_done"`
}

func DefaultSettings() Settings {
	return Settings{MaxRunning: DefaultMaxRunning, OnDone: OnDoneKeep}
}

// New returns an empty backlog at the current schema version.
func New() *Backlog {
	return &Backlog{SchemaVersion: BacklogSchemaVersion, NextID: 1, Settings: DefaultSettings(), Items: []Item{}}
}

// FormatID renders an item id the way every surface shows it: B-12.
func FormatID(id int) string { return "B-" + strconv.Itoa(id) }

// ParseID accepts "12" or "B-12" (any case for the prefix).
func ParseID(s string) (int, error) {
	t := strings.TrimSpace(s)
	if len(t) > 2 && (t[0] == 'B' || t[0] == 'b') && t[1] == '-' {
		t = t[2:]
	}
	n, err := strconv.Atoi(t)
	if err != nil || n <= 0 || strings.HasPrefix(t, "+") {
		return 0, fmt.Errorf("invalid item id %q — use a number like 12 or B-12", s)
	}
	return n, nil
}

// NotFoundError is returned when an item id is not in the backlog. Project is
// optional; callers that know the project name set it.
type NotFoundError struct {
	ID      int
	Project string
}

func (e *NotFoundError) Error() string {
	if e.Project != "" {
		return fmt.Sprintf("%s not found in project %s — run \"af backlog list --all\"", FormatID(e.ID), e.Project)
	}
	return fmt.Sprintf("%s not found — run \"af backlog list --all\"", FormatID(e.ID))
}

// Item returns the item with id, or nil.
func (b *Backlog) Item(id int) *Item {
	if i := b.index(id); i >= 0 {
		return &b.Items[i]
	}
	return nil
}

func (b *Backlog) index(id int) int {
	for i := range b.Items {
		if b.Items[i].ID == id {
			return i
		}
	}
	return -1
}

func (b *Backlog) get(id int) (*Item, error) {
	if it := b.Item(id); it != nil {
		return it, nil
	}
	return nil, &NotFoundError{ID: id}
}

// OpenQuestion returns the item's oldest question that no answer names and
// that is not closed, or nil.
func (it *Item) OpenQuestion() *Message {
	answered := map[int]bool{}
	for _, m := range it.Thread {
		if m.Kind == KindAnswer && m.Answers != 0 {
			answered[m.Answers] = true
		}
	}
	for i := range it.Thread {
		m := &it.Thread[i]
		if m.Kind == KindQuestion && m.Closed == "" && !answered[m.ID] {
			return m
		}
	}
	return nil
}

// answerPending reports whether an answer to one of the item's questions has not
// been delivered to its session yet. Only an answer of the CURRENT run counts: one
// accepted under an earlier run never reaches this one and must not hold back its
// completion (a legacy answer without a run count counts).
func (it *Item) answerPending() bool {
	for _, m := range it.Thread {
		if m.Kind == KindAnswer && m.Delivery == DeliveryPending && (m.RunCount == 0 || m.RunCount == len(it.Runs)) {
			return true
		}
	}
	return false
}

func (it *Item) closeOpenQuestions(reason string) {
	for q := it.OpenQuestion(); q != nil; q = it.OpenQuestion() {
		q.Closed = reason
	}
}

// closeAFQuestions closes the open questions af asked, such as the cap per
// run question; the agent's own questions stay open.
func (it *Item) closeAFQuestions(reason string) {
	for i := range it.Thread {
		if m := &it.Thread[i]; m.Kind == KindQuestion && m.Author == AuthorAF && it.questionOpen(m.ID) {
			m.Closed = reason
		}
	}
}

func (it *Item) lastRun() *RunRef {
	if len(it.Runs) == 0 {
		return nil
	}
	return &it.Runs[len(it.Runs)-1]
}

func (b *Backlog) clone() *Backlog {
	c := *b
	c.Items = make([]Item, len(b.Items))
	for i, it := range b.Items {
		it.WaitsOn = append([]int(nil), it.WaitsOn...)
		it.Runs = append([]RunRef(nil), it.Runs...)
		for r := range it.Runs {
			it.Runs[r].Meter = maps.Clone(it.Runs[r].Meter)
		}
		it.Thread = append([]Message(nil), it.Thread...)
		if it.Source != nil {
			s := *it.Source
			it.Source = &s
		}
		c.Items[i] = it
	}
	return &c
}

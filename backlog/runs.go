package backlog

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// RunEndNote is the note af adds when a run ends with no done and no question.
const RunEndNote = "The agent stopped without saying it was done. Check the session."

// StartPlan is what a Start does after BeginStart: send "continue" to a live
// session, or create a session for the new starting run.
type StartPlan struct {
	// Continue is the id of a live session of the item. When set, no run was
	// added and the status is not changed yet; see Continued.
	Continue string
	// Run is the new starting run when Continue is empty.
	Run RunRef
}

// BeginStart checks that id can start and, unless the item has a live
// session, sets it running with a new starting run. facts describes the
// item's sessions. On error b is unchanged.
func (b *Backlog) BeginStart(id int, by string, facts Facts, now time.Time) (StartPlan, error) {
	if facts == nil {
		facts = func(string) SessionFacts { return SessionFacts{} }
	}
	it, err := b.get(id)
	if err != nil {
		return StartPlan{}, err
	}
	if b.IsParent(id) {
		return StartPlan{}, fmt.Errorf("%s is a parent; parents run through their children — start a child", FormatID(id))
	}
	if !it.Status.Open() {
		return StartPlan{}, fmt.Errorf("%s is %s; reopen it first with \"af backlog reopen %d\"", FormatID(id), it.Status, id)
	}
	// A group paused (or archived, which pauses it) after a runner pass or an
	// "af backlog run" pass chose this item stops the start here, inside the
	// transaction that records it: a choice made before the lock cannot see a
	// pause that lands in between. A start by hand ignores pauses on purpose.
	if by == StartedByRunner || by == StartedByRunPass {
		if p := b.PausedBy(id); p != 0 {
			return StartPlan{}, fmt.Errorf("%w: %s is under paused %s", ErrNotRunnable, FormatID(id), FormatID(p))
		}
	}
	if last := it.lastRun(); last != nil {
		switch {
		case last.State == RunStarting:
			return StartPlan{}, fmt.Errorf("%s is already starting", FormatID(id))
		case last.State == RunStarted && facts(last.SessionID).Limited:
			return StartPlan{}, fmt.Errorf("%s is at a %s", FormatID(id), facts(last.SessionID).Reason)
		case last.State == RunStarted && facts(last.SessionID).RunActive:
			return StartPlan{}, fmt.Errorf("%s is already working in session %q", FormatID(id), last.Title)
		case last.State == RunStarted && facts(last.SessionID).Live:
			return StartPlan{Continue: last.SessionID}, nil
		}
	}
	if err := CheckStatusChange(StatusChange{ID: id, From: it.Status, To: StatusRunning, By: ActorAF}); err != nil {
		return StartPlan{}, err
	}
	if by != StartedByRunner {
		by = StartedByUser
	}
	run := RunRef{State: RunStarting, PrevStatus: it.Status, StartedBy: by, StartedAt: now}
	it.Status = StatusRunning
	it.resetLiveAnswers() // answers accepted under an earlier run belong to it, not this one
	it.Runs = append(it.Runs, run)
	it.UpdatedAt = now
	return StartPlan{Run: run}, nil
}

// Continued sets id running for a "continue" to its live session, before the
// prompt is sent. If the send fails, ContinueFailed puts the status back.
func (b *Backlog) Continued(id int, sessionID string, now time.Time) error {
	it, err := b.get(id)
	if err != nil {
		return err
	}
	if last := it.lastRun(); last == nil || last.SessionID != sessionID {
		return fmt.Errorf("session %s is no longer the run of %s", sessionID, FormatID(id))
	}
	if err := CheckStatusChange(StatusChange{ID: id, From: it.Status, To: StatusRunning, By: ActorAF}); err != nil {
		return err
	}
	it.Status = StatusRunning
	// The continued work counts like a live answer: the rollback of an answer
	// accepted earlier (held, then failing) must not put its saved status back over
	// it.
	it.LiveAnswers++
	it.UpdatedAt = now
	return nil
}

// ContinueFailed puts back the status a Continued replaced, when the
// "continue" could not be sent, and notes why.
func (b *Backlog) ContinueFailed(id int, prev Status, sendErr error, now time.Time) error {
	it, err := b.get(id)
	if err != nil {
		return err
	}
	if it.LiveAnswers > 0 {
		it.LiveAnswers--
	}
	if it.LiveAnswers == 0 && it.Status == StatusRunning && prev.Valid() && CheckStatusChange(StatusChange{ID: id, From: it.Status, To: prev,
		By: ActorAF, StartFailed: true, PrevStatus: prev}) == nil {
		it.Status = prev
		it.UpdatedAt = now
	}
	_, err = b.AddNote(id, AuthorAF, "Continue could not be sent: "+sendErr.Error(), now)
	return err
}

// LatestRun returns the latest run of id, or nil.
func (b *Backlog) LatestRun(id int) *RunRef {
	if it := b.Item(id); it != nil {
		return it.lastRun()
	}
	return nil
}

// LatestSessions maps the session of each item's latest started run to the
// item id.
func (b *Backlog) LatestSessions() map[string]int {
	out := map[string]int{}
	for i := range b.Items {
		if r := b.Items[i].lastRun(); r != nil && r.State == RunStarted && r.SessionID != "" {
			out[r.SessionID] = b.Items[i].ID
		}
	}
	return out
}

// FinishStart settles the starting run of id that began at startedAt: started
// in sessionID, or, when startErr is set, failed with the status put back and
// a note with the error.
func (b *Backlog) FinishStart(id int, startedAt time.Time, sessionID, title string, startErr error, now time.Time) error {
	it, err := b.get(id)
	if err != nil {
		return err
	}
	var run *RunRef
	for i := range it.Runs {
		if it.Runs[i].State == RunStarting && it.Runs[i].StartedAt.Equal(startedAt) {
			run = &it.Runs[i]
		}
	}
	if run == nil {
		// Already settled by AdoptStarting (the agent reported before this ran):
		// the same run, linked to the same session.
		if startErr == nil {
			for i := range it.Runs {
				if r := it.Runs[i]; r.State == RunStarted && r.StartedAt.Equal(startedAt) && r.SessionID == sessionID && r.Title == title {
					return nil
				}
			}
		}
		return fmt.Errorf("%s has no starting run from %s", FormatID(id), startedAt.Format(time.RFC3339))
	}
	it.UpdatedAt = now
	if startErr == nil {
		run.SessionID, run.Title, run.State = sessionID, title, RunStarted
		return nil
	}
	run.State, run.EndedAt = RunFailed, now
	if it.Status == StatusRunning && it.lastRun() == run {
		if CheckStatusChange(StatusChange{ID: id, From: it.Status, To: run.PrevStatus, By: ActorAF,
			StartFailed: true, PrevStatus: run.PrevStatus}) == nil {
			it.Status = run.PrevStatus
		}
	}
	_, err = b.AddNote(id, AuthorAF, "The session could not start: "+startErr.Error(), now)
	return err
}

// AdoptStarting links the latest run of item id, which is still starting with no
// session, to sessionID created at createdAt: the session reported on its item
// (done, note, ask) before the daemon's own start record was committed. The
// session's own item id and creation time are what vouch for it. It reports
// whether it linked a run.
func (b *Backlog) AdoptStarting(id int, sessionID, title string, createdAt, now time.Time) bool {
	it := b.Item(id)
	if it == nil {
		return false
	}
	r := it.lastRun()
	if r == nil || r.State != RunStarting || r.SessionID != "" || createdAt.Before(r.StartedAt) {
		return false
	}
	r.SessionID, r.Title, r.State = sessionID, title, RunStarted
	it.UpdatedAt = now
	return true
}

// ItemSession is one session the daemon knows was started for an item.
type ItemSession struct {
	ID        string
	Title     string
	Item      int
	CreatedAt time.Time
}

// RecoverStarts settles the runs a daemon restart left starting: those that
// began before the given time, so a start of the current daemon still in
// flight is left alone. A run is matched to the earliest session of its item
// created at or after its start that no other run names; a run with no match
// fails. It returns the ids it changed.
func (b *Backlog) RecoverStarts(sessions []ItemSession, before, now time.Time) []int {
	taken := map[string]bool{}
	for _, it := range b.Items {
		for _, r := range it.Runs {
			if r.SessionID != "" {
				taken[r.SessionID] = true
			}
		}
	}
	sorted := append([]ItemSession(nil), sessions...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].CreatedAt.Before(sorted[j].CreatedAt) })
	var changed []int
	for i := range b.Items {
		it := &b.Items[i]
		for j := range it.Runs {
			r := it.Runs[j]
			if r.State != RunStarting || !r.StartedAt.Before(before) {
				continue
			}
			var match *ItemSession
			for k := range sorted {
				s := &sorted[k]
				if s.Item == it.ID && !taken[s.ID] && !s.CreatedAt.Before(r.StartedAt) {
					match = s
					break
				}
			}
			if match != nil {
				taken[match.ID] = true
				_ = b.FinishStart(it.ID, r.StartedAt, match.ID, match.Title, nil, now)
			} else {
				_ = b.FinishStart(it.ID, r.StartedAt, "", "", errors.New("af restarted before the session was created"), now)
			}
			changed = append(changed, it.ID)
		}
	}
	return changed
}

// ItemForSession returns the item one of whose runs is sessionID, or nil.
func (b *Backlog) ItemForSession(sessionID string) *Item {
	if sessionID == "" {
		return nil
	}
	for i := range b.Items {
		if b.Items[i].hasRun(sessionID) {
			return &b.Items[i]
		}
	}
	return nil
}

// RunEnd is what a run end did to the item.
type RunEnd struct {
	ID int
	// Changed is whether the item was written.
	Changed bool
	// Archive asks the caller to archive the session: the user marked the item
	// done and the project's on_done is archive.
	Archive bool
}

// RunEnded applies the end of a backlog run in sessionID, the item's latest
// run. A running item with no open question goes to review with a note from
// af. It returns a zero RunEnd when sessionID is not an item's latest run.
func (b *Backlog) RunEnded(sessionID string, now time.Time) RunEnd {
	it := b.ItemForSession(sessionID)
	if it == nil || it.lastRun().SessionID != sessionID {
		return RunEnd{}
	}
	out := RunEnd{ID: it.ID}
	switch it.Status {
	case StatusDone:
		out.Archive = b.Settings.OnDone == OnDoneArchive
	case StatusRunning:
		if it.OpenQuestion() != nil {
			return out
		}
		if it.answerPending() {
			// An answer accepted after this run's idle edge is on its way (it may
			// be held behind an attached client): its delivery arms the next run,
			// which ends on its own idle edge. Ending the old one now would put
			// the item in review while the agent is about to work.
			return out
		}
		it.Status = StatusReview
		it.resetLiveAnswers()
		it.UpdatedAt = now
		_, _ = b.AddNote(it.ID, AuthorAF, RunEndNote, now)
		out.Changed = true
	}
	return out
}

// SessionTitleBase is the title base of an item session: "B-12 <title>" with
// "/" removed, so a second run of the item gets a suffix.
func SessionTitleBase(it *Item) string {
	title := strings.Join(strings.Fields(strings.ReplaceAll(it.Title, "/", " ")), " ")
	return strings.TrimSpace(FormatID(it.ID) + " " + title)
}

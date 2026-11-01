package backlog

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/planusage"
)

const RunnerRunning = "running"

// The gates that stop the runner, as RunnerView.Gate names them.
const (
	GateShare       = "share"
	GateStopAt      = "stop_at"
	GateUsageLimit  = "usage_limit"
	GateNoReading   = "no_reading"
	GateUnmeterable = "unmeterable"
	GateUSDLimit    = "usd_limit"
	GateNoAccount   = "no_account"
	GateMetering    = "metering"
	// GatePlanUsageOff closes the runner while [backlog] plan_usage is off: with
	// no plan readings and no metering there is nothing to hold a budget against.
	GatePlanUsageOff = "plan_usage_off"
	// GateMeterUnavailable closes the runner while the meter cannot be read or
	// written: the last good result is stale, and gating on it would let starts
	// continue against spend that can no longer be seen.
	GateMeterUnavailable = "meter_unavailable"
)

// StaleReadingAfter is the reading age past which the runner starts at most
// one item, across all projects, until the new session takes a fresh reading.
const StaleReadingAfter = 15 * time.Minute

// RunnerEvent is the payload of a backlog.runner event.
type RunnerEvent struct {
	RepoID string `json:"repo_id"`
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
	Gate   string `json:"gate,omitempty"`
}

// GateInput is what the gates read: the budget, the meter's usage for the
// account item sessions use, and the usage limit the daemon has seen for it.
type GateInput struct {
	Budget  Budget
	Usage   planusage.Usage
	Limited bool
	// LimitUntil is when the usage limit resets; zero when unknown.
	LimitUntil time.Time
	// LimitUnknown says why af cannot tell whether the account is at its usage
	// limit; the runner then waits.
	LimitUnknown string
	// NoAccount says why af cannot tell which account item sessions use.
	NoAccount string
	Now       time.Time
	// Loc is the zone reset times are shown in.
	Loc *time.Location
}

// Gate is a closed runner gate: its name and the reason the UI shows. The
// zero Gate is open.
type Gate struct {
	Name   string
	Reason string
}

func (g Gate) Open() bool { return g.Name == "" }

type window struct {
	w     planusage.PlanWindow
	label string
	// rolling names the span a window with no reading counts.
	rolling string
}

// CheckGates reports the first closed gate, in the order account, share 0,
// metering, share,
// stop_at, usage limit, no reading, unmeterable, usd_limit.
func CheckGates(in GateInput) Gate {
	b, u := in.Budget, in.Usage
	if in.NoAccount != "" {
		return Gate{GateNoAccount, capitalize(in.NoAccount)}
	}
	if u.Disabled {
		return Gate{GatePlanUsageOff, "Plan usage is off, so the runner cannot stay within a budget · " +
			"turn it on with \"af config set backlog.plan_usage true\", or start items by hand with \"af backlog start\""}
	}
	if u.Unavailable != "" {
		return Gate{GateMeterUnavailable, "Plan metering is unavailable, so the runner waits · " + u.Unavailable}
	}
	if b.Share <= 0 {
		return Gate{GateShare, "Backlog share is 0% · raise it with \"af backlog budget --share 50\""}
	}
	if u.Metering {
		return Gate{GateMetering, "Reading plan usage…"}
	}
	windows := []window{
		{u.Windows.FiveHour, "the 5-hour window", "the last 5 hours"},
		{u.Windows.SevenDay, "the week", "the last 7 days"},
	}
	for _, w := range windows {
		if w.w.Valid() && w.w.Backlog >= b.Share {
			return Gate{GateShare, fmt.Sprintf("Backlog used its %s of %s · resumes %s",
				Percent(b.Share), w.label, ResetLabel(*w.w.ResetsAt, in.Now, in.Loc))}
		}
	}
	for _, w := range windows {
		if w.w.Valid() && w.w.Plan >= b.StopAt {
			return Gate{GateStopAt, fmt.Sprintf("Plan at %s of %s (stop at %s) · resumes %s",
				Percent(w.w.Plan), w.label, Percent(b.StopAt), ResetLabel(*w.w.ResetsAt, in.Now, in.Loc))}
		}
	}
	if in.LimitUnknown != "" {
		return Gate{GateUsageLimit, "Can't tell whether the plan is at its usage limit · " + in.LimitUnknown}
	}
	if in.Limited {
		reason := "Usage limit"
		if !in.LimitUntil.IsZero() {
			reason += " until " + ResetLabel(in.LimitUntil, in.Now, in.Loc)
		}
		return Gate{GateUsageLimit, reason}
	}
	if !u.Windows.FiveHour.Valid() && !u.Windows.SevenDay.Valid() && b.USDLimit <= 0 {
		return Gate{GateNoReading, "No plan usage seen yet · start an item by hand once to take a reading, " +
			"or set \"af backlog budget --usd-limit 10\""}
	}
	if len(u.Unmeterable) > 0 {
		ids := make([]string, 0, len(u.Unmeterable))
		for id := range u.Unmeterable {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		return Gate{GateUnmeterable, capitalize(u.Unmeterable[ids[0]])}
	}
	if b.USDLimit > 0 {
		for _, w := range windows {
			if w.w.BacklogCost < b.USDLimit {
				continue
			}
			reason := fmt.Sprintf("Backlog spent %s of its %s limit in ", USD(w.w.BacklogCost), USD(b.USDLimit))
			if w.w.Valid() {
				reason += w.label + " · resumes " + ResetLabel(*w.w.ResetsAt, in.Now, in.Loc)
			} else {
				reason += w.rolling
			}
			return Gate{GateUSDLimit, reason}
		}
	}
	return Gate{}
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// Percent formats a percent the way every surface shows it: 50%, 12.5%.
func Percent(v float64) string {
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.1f", v), "0"), ".") + "%"
}

// ResetLabel shows a reset time: 14:00 within a day, else Mon 09:00.
func ResetLabel(at, now time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.Local
	}
	at = at.In(loc)
	if at.Sub(now) < 24*time.Hour {
		return at.Format("15:04")
	}
	return at.Format("Mon 15:04")
}

// StaleReading reports whether the newest plan reading is older than
// StaleReadingAfter. No reading is not stale: the gates handle it.
func StaleReading(u planusage.Usage, now time.Time) bool {
	return u.ReadingAt != nil && now.Sub(*u.ReadingAt) > StaleReadingAfter
}

// StartPacer holds the runner, across all projects, to one start while the
// plan reading is stale, until a reading newer than that start arrives.
type StartPacer struct {
	last time.Time
}

// Limit is how many items the runner may start now; -1 means no limit.
func (p *StartPacer) Limit(u planusage.Usage, now time.Time) int {
	switch {
	case !StaleReading(u, now):
		return -1
	case !p.last.IsZero() && !u.ReadingAt.After(p.last):
		return 0
	}
	return 1
}

// Started records a start the runner made.
func (p *StartPacer) Started(at time.Time) {
	if at.After(p.last) {
		p.last = at
	}
}

// heldAfterFailure is whether the runner leaves the item alone: its last start
// failed and nothing changed it since.
func (it *Item) heldAfterFailure() bool {
	r := it.lastRun()
	return r != nil && r.State == RunFailed && !r.EndedAt.IsZero() && !it.UpdatedAt.After(r.EndedAt)
}

// Runnable lists up to n items the runner may start, in tree order: leaves in
// the Queued state whose last start did not fail, with no paused ancestor, and
// that skip does not name.
func (b *Backlog) Runnable(states map[int]Derived, n int, skip func(int) bool) []int {
	var out []int
	for i := range b.Items {
		it := &b.Items[i]
		if len(out) >= n {
			break
		}
		if states[it.ID].State != StateQueued || b.IsParent(it.ID) || it.heldAfterFailure() || b.PausedBy(it.ID) != 0 || (skip != nil && skip(it.ID)) {
			continue
		}
		out = append(out, it.ID)
	}
	return out
}

// ErrNotRunnable refuses an automatic start of an item the runner may no longer
// start because the user changed it since the pass chose it. It is not a failed
// start: the runner neither backs off nor holds the item.
var ErrNotRunnable = errors.New("the item is no longer runnable")

// RunnerEligible reports whether the runner may start item id now: it is a
// queued leaf (not held after a failed start, with no paused ancestor) in the
// given derived states. The admission check for an automatic start, which must
// not override a draft, a new dependency or a pause the user set since the pass
// chose the item.
func (b *Backlog) RunnerEligible(id int, states map[int]Derived) bool {
	it := b.Item(id)
	return it != nil && states[id].State == StateQueued && !b.IsParent(id) && !it.heldAfterFailure() && b.PausedBy(id) == 0
}

// RunnerInput is one runner pass's view of a project.
type RunnerInput struct {
	States map[int]Derived
	// Used counts the slots in use, reservations included.
	Used int
	Gate Gate
	// Limit caps the starts of this pass; -1 is no cap.
	Limit int
	// Skip names items the runner backs off from.
	Skip func(int) bool
}

// RunnerPlan is one runner pass: what the runner shows and which items it
// starts.
type RunnerPlan struct {
	View  RunnerView
	Start []int
}

// PlanRunner decides one pass of the runner for a project.
func (b *Backlog) PlanRunner(in RunnerInput) RunnerPlan {
	v := RunnerView{Used: in.Used, Max: b.Settings.MaxRunning}
	switch {
	case !b.Settings.Runner:
		v.State, v.Reason = RunnerOff, "Items run only when you start them."
		return RunnerPlan{View: v}
	case !in.Gate.Open():
		v.State, v.Reason, v.Gate = RunnerStopped, in.Gate.Reason, in.Gate.Name
		if in.Gate.Name == GateNoReading || in.Gate.Name == GateMetering {
			v.State = RunnerWaiting
		}
		return RunnerPlan{View: v}
	}
	queued := len(b.Runnable(in.States, 1, in.Skip)) > 0
	free := FreeSlots(b.Settings.MaxRunning, in.Used)
	if in.Limit >= 0 {
		free = min(free, in.Limit)
	}
	start := b.Runnable(in.States, free, in.Skip)
	switch {
	case in.Used > 0 || len(start) > 0:
		v.State = RunnerRunning
	case !queued:
		v.State, v.Reason = RunnerIdle, "Nothing queued."
	case in.Limit == 0:
		v.State, v.Reason = RunnerIdle, "Waiting for a fresh plan reading."
	default:
		v.State = RunnerIdle
	}
	return RunnerPlan{View: v, Start: start}
}

// FreeSlots is how many more runs fit under maxRunning.
func FreeSlots(maxRunning, used int) int {
	return max(0, maxRunning-used)
}

// RunNow picks the items a user's "run" pass starts: up to count (default 1)
// Queued leaves in tree order, within the free slots. The runner switch and
// the budget gates do not apply. reason says why nothing was picked.
func (b *Backlog) RunNow(states map[int]Derived, used, count int) ([]int, string) {
	if count <= 0 {
		count = 1
	}
	free := FreeSlots(b.Settings.MaxRunning, used)
	if free == 0 {
		return nil, SlotsFull(b.Settings.MaxRunning)
	}
	pick := b.Runnable(states, min(count, free), nil)
	if len(pick) == 0 {
		return nil, "nothing queued; queue an item with \"af backlog queue <id>\""
	}
	return pick, ""
}

// SlotsFull says that a project's slots are all in use.
func SlotsFull(maxRunning int) string {
	reason := fmt.Sprintf("all %d slots are in use; wait for a run to end", maxRunning)
	if maxRunning < MaxMaxRunning {
		reason += fmt.Sprintf(" or raise them with \"af backlog runner --max-running %d\"", maxRunning+1)
	}
	return reason
}

// RunUse is the % of the 5-hour window a cost stands for: the window's plan %
// times the cost's part of all metered cost in it. ok is false when the window
// has no reading or no metered cost.
func RunUse(cost float64, w planusage.PlanWindow) (float64, bool) {
	if !w.Valid() || w.All <= 0 {
		return 0, false
	}
	return w.Plan * cost / w.All, true
}

// CapQuestion is the question af asks when a run passes the cap per run.
func CapQuestion(used, limit float64) string {
	return fmt.Sprintf("This run used %s of your 5-hour window, over the %s cap per run. "+
		"Answer to continue, or send it to review.", Percent(used), Percent(limit))
}

// CapBase moves the point a run's cap counts from to the start of a new
// 5-hour window.
type CapBase struct {
	ID        int
	SessionID string
	Reset     int64
	Base      float64
}

// OverCap is a run that passed the cap per run.
type OverCap struct {
	ID        int
	SessionID string
	// Cost is the run's cost when it passed the cap; the next cap counts from
	// here.
	Cost  float64
	Reset int64
	Use   float64
}

// CapCheck is what a cap pass found: runs whose window changed, and runs over
// the cap.
type CapCheck struct {
	Rebase []CapBase
	Over   []OverCap
}

// CheckCaps measures each working run's use of its 5-hour window since the
// window started or af last capped it. cost maps session ids to metered cost;
// window gives the 5-hour window of a run's account. maxPerRun 0 turns the cap
// off. A run that started before the window counts from its cost now.
func (b *Backlog) CheckCaps(states map[int]Derived, cost map[string]float64, window func(sessionID string) planusage.PlanWindow, maxPerRun float64) CapCheck {
	var out CapCheck
	if maxPerRun <= 0 {
		return out
	}
	for _, it := range b.Items {
		r := it.lastRun()
		if it.Status != StatusRunning || r == nil || r.State != RunStarted || states[it.ID].State != StateWorking {
			continue
		}
		c, ok := cost[r.SessionID]
		w := window(r.SessionID)
		if !ok || !w.Valid() {
			continue
		}
		reset, base := w.ResetsAt.Unix(), r.CapBase
		if r.CapReset != reset {
			base = c
			if !r.StartedAt.Before(w.ResetsAt.Add(-planusage.FiveHourSpan)) {
				base = 0
			}
			out.Rebase = append(out.Rebase, CapBase{ID: it.ID, SessionID: r.SessionID, Reset: reset, Base: base})
		}
		if use, ok := RunUse(c-base, w); ok && use > maxPerRun {
			out.Over = append(out.Over, OverCap{ID: it.ID, SessionID: r.SessionID, Cost: c, Reset: reset, Use: use})
		}
	}
	return out
}

// SetCapBases stores new cap bases on the runs they name. It returns the ids
// it changed.
func (b *Backlog) SetCapBases(bases []CapBase) []int {
	var ids []int
	for _, cb := range bases {
		it := b.Item(cb.ID)
		if it == nil {
			continue
		}
		if r := it.lastRun(); r != nil && r.SessionID == cb.SessionID {
			r.CapReset, r.CapBase = cb.Reset, cb.Base
			ids = append(ids, cb.ID)
		}
	}
	return ids
}

// AskOverCap adds af's cap question to the item of c and moves the run's cap
// base to its cost now. It refuses when the item moved on since c was found.
func (b *Backlog) AskOverCap(c OverCap, maxPerRun float64, now time.Time) (*Message, error) {
	it, err := b.get(c.ID)
	if err != nil {
		return nil, err
	}
	r := it.lastRun()
	if it.Status != StatusRunning || r == nil || r.SessionID != c.SessionID || it.OpenQuestion() != nil {
		return nil, fmt.Errorf("%s changed since its run passed the cap", FormatID(c.ID))
	}
	r.CapReset, r.CapBase = c.Reset, c.Cost
	return it.appendMessage(Message{Kind: KindQuestion, Author: AuthorAF, Text: CapQuestion(c.Use, maxPerRun)}, now), nil
}

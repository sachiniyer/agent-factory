package planusage

import (
	"math"
	"time"
)

// PlanWindow is one plan window: the plan percent used by everything, the
// part attributed to the backlog, and when the window resets.
type PlanWindow struct {
	Plan    float64 `json:"plan"`
	Backlog float64 `json:"backlog"`
	// All is the metered cost of everything in the window, in US dollars at
	// API prices: a run's % of the window is its window cost / All × Plan.
	All float64 `json:"all_cost"`
	// ResetsAt is nil when there is no valid reading for the window.
	ResetsAt *time.Time `json:"resets_at"`
	// BacklogCost is the item runs' metered cost in the window, in US
	// dollars. A window with no reading counts the span ending now.
	BacklogCost float64 `json:"-"`
}

// Valid reports whether the window has a reading that has not reset yet.
func (w PlanWindow) Valid() bool { return w.ResetsAt != nil }

type Windows struct {
	FiveHour PlanWindow `json:"five_hour"`
	SevenDay PlanWindow `json:"seven_day"`
}

// AccountUsage is one Claude account's plan windows.
type AccountUsage struct {
	Windows Windows `json:"windows"`
	// ReadingAt is when the account's newest plan reading was taken; nil when
	// none.
	ReadingAt *time.Time `json:"reading_at"`
	// Unpriced lists models seen in the account's transcripts that have no
	// price; their messages are not counted.
	Unpriced []string `json:"unpriced,omitempty"`
}

// AccountUsageOf computes an account's windows from its newest reading and
// its meter.
func AccountUsageOf(r *Reading, a *AccountMeter, overrides map[string]Price, now time.Time) AccountUsage {
	var out AccountUsage
	if a != nil {
		for _, m := range a.Unpriced {
			if _, ok := PriceFor(m, overrides); !ok {
				out.Unpriced = append(out.Unpriced, m)
			}
		}
	}
	if r == nil {
		return out
	}
	at := r.ReadAt
	out.ReadingAt = &at
	out.Windows = Attribute(r, a, now)
	return out
}

// Usage is what the meter knows now.
type Usage struct {
	// Disabled is set when plan usage is off ([backlog] plan_usage, the
	// default): af injects no status line tap, reads no transcripts, and has no
	// plan windows, so nothing can be gated on the plan budget.
	Disabled bool `json:"disabled,omitempty"`
	// Unavailable is set (with the reason) when the meter could not be read or
	// written on its last pass: the windows are empty, not stale, so nothing is
	// gated on a result that no longer reflects the spend.
	Unavailable string `json:"unavailable,omitempty"`
	// Metering is set until the first metering pass has finished; the windows
	// are empty until then.
	Metering bool `json:"metering,omitempty"`
	// Account is the af account the top-level windows are for: the one with
	// the newest reading. "default" is the ambient Claude identity.
	Account string  `json:"account,omitempty"`
	Windows Windows `json:"windows"`
	// ReadingAt is when the newest plan reading was taken; nil when none.
	ReadingAt *time.Time `json:"reading_at"`
	// Accounts has the windows of every Claude account af meters, so a run
	// can be judged on its own session's account.
	Accounts map[string]AccountUsage `json:"accounts,omitempty"`
	// Runs is each item session's metered cost in US dollars at API prices.
	Runs map[string]float64 `json:"runs"`
	// RunWindow is each item session's cost, in US dollars, within its
	// account's current 5-hour window; 0 without a valid window.
	RunWindow map[string]float64 `json:"run_window"`
	// Unmeterable says, per item session of a running item, why af cannot
	// meter it.
	Unmeterable map[string]string `json:"unmeterable,omitempty"`
}

// SpendEvent is the payload of a backlog.spend event.
type SpendEvent struct {
	FiveHour  PlanWindow         `json:"five_hour"`
	SevenDay  PlanWindow         `json:"seven_day"`
	ReadingAt *time.Time         `json:"reading_at"`
	Runs      map[string]float64 `json:"runs"`
	RunWindow map[string]float64 `json:"run_window"`
	// Unmeterable is keyed by item session, like Usage.Unmeterable.
	Unmeterable map[string]string `json:"unmeterable"`
}

func (u Usage) SpendEvent() SpendEvent {
	return SpendEvent{FiveHour: u.Windows.FiveHour, SevenDay: u.Windows.SevenDay, ReadingAt: u.ReadingAt,
		Runs: nonNil(u.Runs), RunWindow: nonNil(u.RunWindow), Unmeterable: nonNil(u.Unmeterable)}
}

func nonNil[V any](m map[string]V) map[string]V {
	if m == nil {
		return map[string]V{}
	}
	return m
}

// Attribute computes the plan windows from a reading and the account's meter:
// backlog% = plan% × backlog cost / all cost, over the window's span ending at
// its reset. A window past its reset counts as 0 until a new reading.
func Attribute(r *Reading, a *AccountMeter, now time.Time) Windows {
	if r == nil {
		return Windows{}
	}
	return Windows{
		FiveHour: attribute(r.FiveHour, FiveHourSpan, a, now),
		SevenDay: attribute(r.SevenDay, SevenDaySpan, a, now),
	}
}

func attribute(l *Limit, span time.Duration, a *AccountMeter, now time.Time) PlanWindow {
	if l == nil {
		return PlanWindow{}
	}
	reset := time.Unix(l.ResetsAt, 0).UTC()
	if !now.Before(reset) {
		return PlanWindow{}
	}
	all, backlog := a.costIn(reset.Add(-span), reset)
	w := PlanWindow{Plan: l.UsedPercentage, All: all, ResetsAt: &reset}
	if all > 0 && backlog > 0 {
		w.Backlog = math.Min(w.Plan, w.Plan*backlog/all)
	}
	return w
}

// SetBacklogCosts fills each window's BacklogCost from the account's meter: the
// reading's window when it has one, else the span that ends now.
func SetBacklogCosts(w *Windows, a *AccountMeter, now time.Time) {
	set := func(pw *PlanWindow, span time.Duration) {
		from, to := now.Add(-span), now.Add(BucketSpan)
		if pw.Valid() {
			from, to = pw.ResetsAt.Add(-span), *pw.ResetsAt
		}
		_, pw.BacklogCost = a.costIn(from, to)
	}
	set(&w.FiveHour, FiveHourSpan)
	set(&w.SevenDay, SevenDaySpan)
}

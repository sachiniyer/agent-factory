package task

import (
	"slices"
	"strings"
	"time"

	cron "github.com/robfig/cron/v3"
)

// Schedule health: whether a task is actually firing on its own schedule
// (#3623).
//
// The bug this closes: two enabled hourly cron tasks stopped firing for 18 days
// on a healthy, running daemon, and no surface anywhere could say so. `enabled`
// describes only the current instant — a task toggled off and back on reads
// true and carries no trace. `last_run_status` still said "started", because the
// last run really did start, 18 days ago. And the two places that computed a
// next-fire time recomputed it from the cron expression FOR DISPLAY, so the
// Automations pane cheerfully rendered "next Sep 02 04:20 · last Aug 14 14:20"
// and left the subtraction to the reader. Nothing anywhere compared a task's
// last run against its own schedule.
//
// So the machine does the subtraction now. It is pure derivation from state that
// was already on disk, which is exactly what makes it catch every cause — an
// operator toggle nobody remembers, a daemon that came back without the task, an
// expression that stopped parsing, a cause nobody has thought of yet. Nothing new
// is persisted: a stored "overdue" would be a claim about an instant that has
// already passed by the time anything reads it back.
//
// The premise that makes this load-bearing is #2212's: a box whose owner never
// opens the TUI. A cron task's whole promise is that you stop thinking about it,
// so the machine has to be the one that notices.

// MinSlack is the floor of the lateness window. A schedule shorter than this
// (a per-minute watchdog) would otherwise report overdue on any hiccup in
// firing, delivery, or clock, so the floor buys every schedule at least five
// minutes of grace before it is called late.
const MinSlack = 5 * time.Minute

// MaxMissedOccurrences caps MissedOccurrences, and with it the work one
// derivation may do: the count is produced by stepping the schedule forward one
// occurrence at a time, so a per-minute task dark for a year would otherwise
// walk half a million steps on a read path the TUI hits every 750ms. The count
// SATURATES rather than truncating the verdict — Overdue stays true and
// Saturated says the number is a floor, because "at least 10000 missed" and
// "exactly 526000 missed" call for the same action.
//
// It is also the budget for a WHOLE LOAD, shared across every task in it, and
// that second cap is the load-bearing one. A per-task limit bounds nothing in
// aggregate: the store holds an unbounded number of tasks and the Automations
// rail derives all of them every 750ms, so a hundred long-dark high-frequency
// tasks cost a hundred times the cap — measured at ~2.4s per load on this
// checkout, which is refreshes overlapping continuously and a core burned while
// nothing changes (#3623 review).
//
// Sharing the budget costs only PRECISION, never the verdict: overdue is one
// Next() call and is decided before any walking, so a task that finds the budget
// spent still reports overdue, with a count that says it is a floor. The
// walk exists to turn "late" into "how late", and a floor answers that as well
// as an exact number does.
const MaxMissedOccurrences = 10000

// Arming states. These are the LIVE observation — whether the running daemon's
// scheduler or watcher supervisor actually holds an entry for this task — and
// not a re-derivation from the record, which is the whole point: the number a
// user reads must reflect what is armed, not what the cron expression says
// ought to be.
//
// ArmingUnknown is the zero value and means NOT OBSERVED: no daemon answered, so
// nothing is known either way. It must never be read as "not armed" — that is
// the fabricated negative this repo keeps re-learning, and here it would report
// every task on a box with a stopped daemon as broken.
const (
	ArmingUnknown  = ""
	ArmingArmed    = "armed"
	ArmingNotArmed = "not-armed"
)

// Why the scheduler cannot fire a task. One classification, named here, because
// three surfaces render it and each of them was re-deriving it from ParseCron —
// and each got a different subset wrong: `af tasks show` reported "cron
// expression is invalid" for a record with no expression at all, the rail said
// the same, and the doctor row told the operator to correct an expression that
// did not exist. Renderers differ; the classification must not (#3623 review).
const (
	// ReasonNoTrigger: enabled with neither a cron expression nor a watch
	// command. ValidateTrigger refuses to write one, so it is a hand edit or a
	// legacy row — and it is the emptiest kind of broken, since nothing schedules
	// it and nothing watches it.
	ReasonNoTrigger = "no-trigger"
	// ReasonInvalidExpression: the expression does not parse, so the scheduler
	// skips it with a warning and it is armed by nothing.
	ReasonInvalidExpression = "invalid-expression"
	// ReasonNoOccurrence: it parses and matches no date inside robfig's five-year
	// search horizon — February 31st, or a long-gap expression in the run-up to a
	// skipped leap year. The scheduler consults the same horizon, so it will not
	// fire it either.
	ReasonNoOccurrence = "no-occurrence"
)

// UnschedulableReason names why the scheduler cannot fire this cron task, or ""
// when it can. It is THE classifier: DeriveScheduleHealth sets Unschedulable
// from it, and every surface that words the condition switches on it rather than
// re-deriving, so a renderer cannot disagree with the verdict it is rendering.
//
// Only meaningful for an enabled cron task — a watch task has no schedule and a
// disabled one is not expected to fire — and it answers "" for both, matching
// the derivation's own exclusions.
func UnschedulableReason(t Task, now time.Time) string {
	if !t.Enabled || t.IsWatch() {
		return ""
	}
	if strings.TrimSpace(t.CronExpr) == "" {
		return ReasonNoTrigger
	}
	sched, err := ParseCron(t.CronExpr)
	if err != nil {
		return ReasonInvalidExpression
	}
	if sched.Next(now).IsZero() {
		return ReasonNoOccurrence
	}
	return ""
}

// ScheduleHealth is the derived answer for one task at one instant. It is
// computed at read time and never stored; Task carries the two fields every
// surface shows (Overdue, MissedOccurrences) and this struct carries the rest
// for callers that report on them, such as `af doctor`.
type ScheduleHealth struct {
	// Overdue reports that the task's last run precedes its most recent
	// scheduled occurrence by more than the slack window.
	Overdue bool
	// MissedOccurrences counts the scheduled fires strictly after the reference
	// point (the last run, or creation for a task that has never run) up to and
	// including now. Zero when the task is not overdue: within the slack window
	// a pending occurrence is not a missed one.
	MissedOccurrences int
	// Saturated reports that MissedOccurrences hit MaxMissedOccurrences and is
	// therefore a floor, not an exact count. It rides the task record too
	// (MissedOccurrencesCapped): a consumer that reads "10000" and cannot tell it
	// from an exact count has been handed a number that is quietly wrong.
	Saturated bool
	// Unschedulable reports that THE SCHEDULER CANNOT DERIVE A NEXT FIRE from
	// this expression: it does not parse, or nothing matches within the five-year
	// horizon robfig's Next() searches before giving up.
	//
	// That is deliberately a claim about the scheduler and not about the calendar,
	// because the scheduler is what will or will not run the task, and it consults
	// the same horizon. For February 31st the two agree — it can never fire. For a
	// leap-day expression evaluated in the run-up to a skipped leap year they do
	// not: "0 0 29 2 *" asked in 2096 finds nothing before 2104, so Next() returns
	// zero, the cron entry sits with a zero next-fire time, and the daemon does not
	// fire it — even though the calendar says 2104. Reporting that as healthy would
	// be the false negative; reporting it as "can never fire" would be the false
	// claim. It cannot be scheduled, which is both true and the thing to act on.
	// (Measured against robfig directly: zero from 2096-03-01 through 2098,
	// non-zero either side.)
	//
	// The parse failure and the no-match case are ONE verdict on purpose. They
	// differ in how they got there and in nothing a reader can act on, and
	// splitting them left a gap: while the parse failure was treated as "reported
	// by the arming state", a box whose daemon was down or warming up observed no
	// arming at all and doctor called such a task healthy. A verdict derived from
	// the record alone is available everywhere; one that depends on a live
	// observation is not. `af tasks show` still names the parse error itself,
	// because it has the expression in hand.
	Unschedulable bool
	// Reason is WHICH shape of unschedulable this is — one of the Reason*
	// constants — and is empty whenever Unschedulable is false. The verdict stays
	// one thing (see above); this is only the wording, and it exists so that a
	// surface which cannot call UnschedulableReason reads its answer rather than
	// inventing a fourth classification (#3626).
	Reason string
	// OldestMissedAt is the first occurrence the task did not run — the instant
	// the silence began. Zero when the task is not overdue.
	OldestMissedAt time.Time
	// Unassessable reports that no lateness verdict could be reached. There are
	// two ways in, and the wording every surface uses has to cover both: the
	// record carries no instant to measure from at all, or it carries one the
	// schedule cannot be evaluated against — a reference old enough that the next
	// two occurrences after it fall outside robfig's search horizon, which a
	// leap-day task last run in 2096 and read in 2099 manages. Saying "this record
	// has no timestamps" would be false in the second case (#3623 review).
	//
	// It is a THIRD answer, not a flavour of health, and that is the whole point.
	// The store stamps CreatedAt on every create now, so only a hand-edited or
	// truncated row can land here — but reporting such a row as "on schedule" is
	// the exact lie this feature exists to remove, and it would be told about a
	// task that may never have fired. Consumers treat it as UNKNOWN: visible,
	// never a clean bill, and never an actionable alarm either, which is the same
	// line `af doctor` already draws for everything it could not establish.
	Unassessable bool
	// Slack is the window that was allowed before calling the task late, kept so
	// a report can explain its own verdict.
	Slack time.Duration
}

// DeriveScheduleHealth answers "is this task firing on its schedule?" at now.
//
// Only an ENABLED CRON task can be overdue:
//
//   - A watch task has no schedule to be late against. It fires when its command
//     emits a line, which may legitimately be never, so lateness is not defined
//     for it at all — its supervision state is the signal, and the watcher
//     supervisor already owns that.
//   - A disabled task is not expected to fire. Whether disabling it was
//     INTENDED is exactly what the audit trail answers (see task/audit.go); it
//     is not something this derivation can know.
//   - An expression the scheduler cannot fire — one that does not parse, or with
//     no match inside its search horizon — has no occurrence to be late against,
//     so the answer is Unschedulable rather than a lateness verdict. It is
//     emphatically not health: the task is enabled and is not going to run.
//
// The verdict is one Next() call, not a scan. The task is overdue exactly when
// some scheduled occurrence lands at or before now and more than slack after the
// reference point, and the first such occurrence is Next(ref+slack): if that is
// at or before now the schedule has moved on without the task, and if it is
// after now the task is merely pending. That equivalence is what keeps this
// affordable on a 750ms poll — the walk below runs only for a task already known
// to be overdue.
func DeriveScheduleHealth(t Task, now time.Time) ScheduleHealth {
	budget := MaxMissedOccurrences
	return deriveScheduleHealth(t, now, &budget)
}

// deriveScheduleHealth is DeriveScheduleHealth against a shared walk budget, so
// a whole load costs what one derivation is allowed to.
func deriveScheduleHealth(t Task, now time.Time, budget *int) ScheduleHealth {
	if !t.Enabled || t.IsWatch() {
		return ScheduleHealth{}
	}
	// "Can the scheduler fire this?" is answered BEFORE "how late is it?", because
	// the two need different things and only the second needs a reference point.
	// Asking them the other way round meant a record with no timestamps at all — a
	// hand-edited or legacy row — declined to derive anything and reported an
	// unschedulable expression as healthy, which is the one case where every input
	// needed for the verdict was in hand (#3623 review).
	//
	// Through the shared classifier, which probes from NOW rather than from the
	// reference point: the question is what the scheduler will do next, and that
	// is what it asks too.
	if reason := UnschedulableReason(t, now); reason != "" {
		return ScheduleHealth{Unschedulable: true, Reason: reason}
	}
	sched, err := ParseCron(t.CronExpr)
	if err != nil {
		// Unreachable while the classifier above owns the parse — it returns
		// ReasonInvalidExpression for exactly this. Kept as a belt, and named the
		// same way, so the belt cannot report a verdict with no wording if the two
		// ever come apart.
		return ScheduleHealth{Unschedulable: true, Reason: ReasonInvalidExpression}
	}
	ref, ok := scheduleReference(t)
	if !ok {
		return ScheduleHealth{Unassessable: true}
	}
	// Evaluate in the SCHEDULER's location, not the reference timestamp's.
	//
	// robfig treats a schedule with no explicit timezone as local to the time it
	// is handed ("schedules without a time zone specified (time.Local) are
	// treated as local to the time provided", spec.go), so the location riding
	// the reference decides where the cron lands. That location is not reliably
	// the daemon's: an HTTP client can send created_at as UTC and the store
	// preserves it, and a record read back over gob or JSON carries a
	// fixed-offset zone rather than a named one. Either way the derivation would
	// evaluate a midnight task somewhere the scheduler does not — measured seven
	// hours out on a PDT host — and disagree with the thing that actually fires
	// it (#3623 review).
	//
	// now is the scheduler's own clock (time.Now, and the daemon's cron reads the
	// same), so normalizing to it makes the two agree by construction.
	ref = ref.In(now.Location())
	slack, ok := slackFor(sched, ref)
	if !ok {
		// Also unassessable rather than healthy — see below for why this is not
		// "unschedulable".
		// NOT unschedulable — the probe above already proved a future fire exists,
		// so this is only "no lateness can be measured from THIS reference". The
		// two come apart when the reference is old enough that the schedule's next
		// occurrences after it fall outside the search horizon: a leap-day task
		// last run in 2096 asked in 2099 can be fired (2104 is inside the horizon
		// from now) while nothing can be computed from 2096. Answering
		// "unschedulable" there would be a false claim about a task the scheduler
		// is going to run.
		return ScheduleHealth{Unassessable: true}
	}
	oldest := sched.Next(ref.Add(slack))
	if oldest.IsZero() || oldest.After(now) {
		return ScheduleHealth{Slack: slack}
	}
	// The oldest MISSED occurrence is the first one after the reference point,
	// which is not the same as the first one past the slack window: a task last
	// run at 14:20 and silent since has missed 15:20 even though the window
	// forgave it until 15:20:00. Report the instant the silence began.
	missedFrom := sched.Next(ref)
	count, saturated := countOccurrences(sched, ref, now, budget)
	return ScheduleHealth{
		Overdue:           true,
		MissedOccurrences: count,
		Saturated:         saturated,
		OldestMissedAt:    missedFrom,
		Slack:             slack,
	}
}

// scheduleReference is the instant lateness is measured from: the LATEST of the
// last run, the last time the task was enabled, and — for a task that has never
// run — its creation.
//
// The never-run case is worth getting right on its own: a task created and then
// never armed has no LastRunAt at all, and treating that as "nothing to compare
// against" would make the most broken task the one that reports healthiest.
//
// A RE-ENABLE is a fresh start, exactly as a first run is, and this is where the
// audit trail earns its keep beyond being something a person reads. Without it,
// a task deliberately paused and later switched back on reports every occurrence
// it missed WHILE INTENTIONALLY OFF: #3623's own tasks, disabled 2026-08-14 and
// re-enabled 2026-09-01, would have read "overdue · missed 432" from the moment
// they came back until their next fire. That is a true statement about the past
// and a useless one about the present — the operator knows why it was off, and
// burying the signal under misses they caused on purpose is how a warning gets
// trained into noise.
//
// The trail is BOUNDED (AuditLimit), so an enable old enough to have fallen off
// the window leaves the reference where it was before. That direction is the
// safe one: it can only make the verdict more eager, never hide a task that has
// genuinely stopped. Tasks written before the trail existed carry none, and
// behave exactly as they did.
//
// A record with no timestamp at all (hand-edited, or written before CreatedAt was
// populated) derives nothing rather than measuring from the zero time, which
// would report every such task as millions of occurrences overdue.
func scheduleReference(t Task) (time.Time, bool) {
	var ref time.Time
	if t.LastRunAt != nil && !t.LastRunAt.IsZero() {
		ref = *t.LastRunAt
	} else if !t.CreatedAt.IsZero() {
		ref = t.CreatedAt
	}
	if restarted, ok := lastScheduleRestartAt(t); ok && restarted.After(ref) {
		ref = restarted
	}
	if ref.IsZero() {
		return time.Time{}, false
	}
	return ref, true
}

// lastScheduleRestartAt returns when the schedule the task is on NOW began,
// from its audit trail: the most recent moment it was switched on, or its cron
// expression changed.
//
// Both are the same event in the only sense that matters here — the schedule a
// task is being judged against started then, so occurrences before it are not
// misses the task owes. The enable case is the deliberate-pause one. The
// expression case is its twin and just as sharp: retiming a daily task to run
// every minute at 23:00 would otherwise be measured against this morning's run
// and report a day's worth of per-minute misses that happened before that
// schedule existed.
//
// An ordinary edit — the prompt, the target session, the program — restarts
// nothing: the schedule did not move.
//
// An entry with no timestamp is SKIPPED rather than answered with. A
// hand-edited or truncated row must not shadow a real restart behind it, and
// measuring from the zero time would report the task as millions of occurrences
// overdue.
func lastScheduleRestartAt(t Task) (time.Time, bool) {
	for i := len(t.Audit) - 1; i >= 0; i-- {
		e := t.Audit[i]
		if e.At.IsZero() {
			continue
		}
		if e.Action == AuditEnabled || slices.Contains(e.Fields, "cron_expr") {
			return e.At, true
		}
	}
	return time.Time{}, false
}

// slackFor is how late a fire may be before the task counts as overdue: one full
// period of its own schedule, or MinSlack, whichever is larger.
//
// One period is the right unit because it is the only lateness that is
// indistinguishable from ordinary operation: an hourly task whose last run was
// 59 minutes ago has simply not reached its next occurrence yet, and one whose
// last run was 61 minutes ago has skipped one. Scaling with the schedule is what
// lets the same rule serve a per-minute watchdog and a monthly report.
//
// The period is measured AT THE REFERENCE POINT, and as the WIDER of the two
// gaps around it, because both halves have a false positive waiting behind them:
//
//   - The gap the task must bridge next (Next(ref) - ref) is the one that
//     matters for an irregular schedule. A weekdays-at-9 task whose last run was
//     a Friday is owed the whole weekend; judged by the Monday-to-Tuesday period
//     instead, it would be called overdue the instant Monday 09:00 arrived —
//     before the fire it is waiting for had any chance to happen.
//   - That gap alone is too tight when the reference is not itself an
//     occurrence, which is exactly the never-run case: a task created at 09:00
//     on a ":20 past" schedule would get 20 minutes of slack and be called
//     overdue at 09:25, five minutes into its first ever run.
//
// Taking the larger of the two is right for both, and reduces to the plain
// period for the ordinary case where the last run sits on an occurrence.
func slackFor(sched cron.Schedule, ref time.Time) (time.Duration, bool) {
	first := sched.Next(ref)
	if first.IsZero() {
		return 0, false
	}
	second := sched.Next(first)
	if second.IsZero() {
		return 0, false
	}
	slack := second.Sub(first)
	if gap := first.Sub(ref); gap > slack {
		slack = gap
	}
	if slack < MinSlack {
		slack = MinSlack
	}
	return slack, true
}

// countOccurrences counts the schedule's fires in (from, now], spending from a
// budget shared with every other task in the same load. The second return says
// whether the count is a floor — because this task hit the per-task cap, or
// because the load's budget ran out first. Both mean the same thing to a reader
// and to a fix.
func countOccurrences(sched cron.Schedule, from, now time.Time, budget *int) (int, bool) {
	if *budget <= 0 {
		// Spent by earlier tasks in this load: report no count rather than the one
		// occurrence a single step would buy. "At least 1" is not more informative
		// than "we did not count", and it costs a Next() call per remaining task to
		// say it. The verdict is already decided; only the number is missing.
		return 0, true
	}
	count := 0
	for at := sched.Next(from); !at.IsZero() && !at.After(now); at = sched.Next(at) {
		count++
		*budget--
		if count >= MaxMissedOccurrences || *budget <= 0 {
			return count, true
		}
	}
	return count, false
}

// WithScheduleHealth populates the derived read-time fields on every task in the
// slice and returns it, so a read path can wrap its load in one call. It is the
// PURE half of the derivation — everything computable from the record alone.
// The live arming half (Arming/NextRunAt) can only be observed by the process
// that holds the scheduler, so the daemon layers it on top of this.
//
// Callers on a write path must not use it: derived fields never reach disk, and
// saveTasks strips them precisely so a mistake here cannot persist one.
func WithScheduleHealth(tasks []Task, now time.Time) []Task {
	for i, health := range DeriveScheduleHealthBatch(tasks, now) {
		// The LIVE fields are cleared first, and this is the read-side twin of
		// stripDerived. af never writes them, but a hand-edited or externally
		// generated tasks.json can carry them, and they decode like any other
		// field — so without this a disk read with no daemon anywhere could report
		// a task as armed, with a next run someone typed. Clearing here also means
		// a not-armed answer from the daemon cannot leave an inherited NextRunAt
		// behind it: the live layer always starts from nothing observed (#3623
		// review).
		tasks[i].Arming = ArmingUnknown
		tasks[i].NextRunAt = nil
		tasks[i].Overdue = health.Overdue
		tasks[i].MissedOccurrences = health.MissedOccurrences
		tasks[i].MissedOccurrencesCapped = health.Saturated
		tasks[i].Unschedulable = health.Unschedulable
		tasks[i].UnschedulableReason = health.Reason
		tasks[i].Unassessable = health.Unassessable
	}
	return tasks
}

// DeriveScheduleHealthBatch derives the full verdict for a whole list against
// ONE shared walk budget, and is what every caller deriving more than one task
// must use.
//
// The budget is the reason this exists rather than a loop over
// DeriveScheduleHealth. Per task it bounds nothing in aggregate — the store
// holds an unbounded number of tasks — so a caller that resets the budget per
// record reintroduces the cost this cap exists to remove, however carefully the
// cap itself is written (#3623 review: the doctor pass did exactly that after
// the load path had been fixed). Handing out the batch is what makes the shared
// budget the easy thing to do.
//
// The result is positionally aligned with tasks, so a caller that already
// iterates the slice can index it.
func DeriveScheduleHealthBatch(tasks []Task, now time.Time) []ScheduleHealth {
	out, _ := deriveScheduleHealthBatch(tasks, now)
	return out
}

// deriveScheduleHealthBatch is DeriveScheduleHealthBatch, also reporting how many
// schedule steps the whole load spent out of the shared budget.
//
// The count exists so the sharing can be asserted as a PROPERTY rather than
// inferred from how long the load took. It was a wall-clock ceiling first, and
// that was the wrong oracle twice over: it measured the runner rather than the
// code, and the margin it had to detect — about 0.5s shared against 2.4s
// unbounded on a quiet machine — sits inside the noise band of a loaded macOS
// runner, so it reddened unrelated PRs while the property it guarded still held
// (#3674).
//
// Reporting the spend rather than only the remainder is what makes it hard to
// satisfy vacuously: a caller that reset the budget per task would have to
// report the sum of those resets to stay consistent with the counts on the
// records, and the test checks that consistency as well as the bound.
func deriveScheduleHealthBatch(tasks []Task, now time.Time) ([]ScheduleHealth, int) {
	budget := MaxMissedOccurrences
	out := make([]ScheduleHealth, len(tasks))
	for i := range tasks {
		out[i] = deriveScheduleHealth(tasks[i], now, &budget)
	}
	return out, MaxMissedOccurrences - budget
}

// stripDerived clears every read-time field. saveTasks calls it on the way to
// disk so the "derived, never persisted" rule is enforced by the write path
// itself rather than by the discipline of each caller — a loaded record now
// always carries these fields, so any future path that loads and re-saves one
// would otherwise store them.
func (t *Task) stripDerived() {
	t.Overdue = false
	t.MissedOccurrences = 0
	t.MissedOccurrencesCapped = false
	t.Unschedulable = false
	t.UnschedulableReason = ""
	t.Unassessable = false
	t.NextRunAt = nil
	t.Arming = ""
	// The row number is derived by the READ that produced this record
	// (stampOrdinals), so it must not reach disk: the file's own row order
	// already carries it, and a stored copy would go stale the moment a row is
	// inserted or removed above it — a record asserting an identity that is no
	// longer its own. Cleared in the one writer for the same reason as the rest
	// (#3680).
	t.Ordinal = unstampedOrdinal
}

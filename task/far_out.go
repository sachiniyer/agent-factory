package task

import (
	"fmt"
	"time"
)

// Far-out next runs (#4843).
//
// A dated cron such as "0 7 21 9 *" is almost always meant as a one-shot, but
// cron has no year field, so after its first fire the scheduler re-arms it for
// the same date next year. Nothing here changes that — an intentional yearly
// task is legitimate — but a year-out fire must not be invisible. Every surface
// that lists tasks flags an enabled task whose next run is further away than
// FarOutThreshold, and all of them ask NextRunFarOut rather than re-deriving it.

// FarOutThreshold is how far in the future a next run may be before surfaces
// flag it. The comparison is strict: exactly 60 days out is not flagged.
const FarOutThreshold = 60 * 24 * time.Hour

// NextRunFarOut reports whether t is enabled and its next run, as read off the
// daemon's live scheduler entry (NextRunAt), is more than FarOutThreshold after
// now.
//
// It never re-parses the cron expression: a task the scheduler is not holding
// has no NextRunAt and is never flagged here — "not armed" is its own signal.
// A watch task has no scheduled next run, and a disabled one is not expected to
// fire, so neither is ever far out.
func NextRunFarOut(t Task, now time.Time) bool {
	if !t.Enabled || t.IsWatch() || t.NextRunAt == nil {
		return false
	}
	return t.NextRunAt.Sub(now) > FarOutThreshold
}

// FarOutNote is the calm wording for a far-out next run, such as
// "2027-09-21 (in 11 months)", or "" when NextRunFarOut is false. The date
// carries the year on purpose: the short "Sep 21" formats the lists use
// elsewhere are exactly what hid a year-out fire.
func FarOutNote(t Task, now time.Time) string {
	if !NextRunFarOut(t, now) {
		return ""
	}
	next := t.NextRunAt.In(now.Location())
	return fmt.Sprintf("%s (%s)", next.Format("2006-01-02"), FarOutDistance(next, now))
}

// FarOutDistance words the distance from now to next in whole calendar months,
// rounded down, so 2026-09-24 to 2027-09-21 reads "in 11 months".
func FarOutDistance(next, now time.Time) string {
	next = next.In(now.Location())
	months := (next.Year()-now.Year())*12 + int(next.Month()) - int(now.Month())
	if next.Day() < now.Day() || (next.Day() == now.Day() && clock(next) < clock(now)) {
		months--
	}
	if months == 1 {
		return "in 1 month"
	}
	return fmt.Sprintf("in %d months", months)
}

func clock(t time.Time) time.Duration {
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute +
		time.Duration(t.Second())*time.Second + time.Duration(t.Nanosecond())
}

// StampNextRunFar sets NextRunFar on every task from NextRunAt and returns the
// slice. Callers run it wherever NextRunAt is populated, so the JSON field and
// the live entry cannot disagree.
func StampNextRunFar(tasks []Task, now time.Time) []Task {
	for i := range tasks {
		tasks[i].NextRunFar = NextRunFarOut(tasks[i], now)
	}
	return tasks
}

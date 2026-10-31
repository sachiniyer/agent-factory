package task

// WithScheduleHealthUnknown returns t with every field derived from OBSERVING its
// schedule cleared, and Unassessable set to say no verdict is available. Those
// fields (armed state, next fire, lateness, the unschedulable verdict) describe
// the definition that was observed; a caller that has just changed the definition
// without a fresh observation must not keep showing them as if they described the
// new one. Unknown is a state every renderer already has (the zero Arming is
// ArmingUnknown); a previous cron's next-fire time is not.
func (t Task) WithScheduleHealthUnknown() Task {
	t.Overdue = false
	t.MissedOccurrences = 0
	t.MissedOccurrencesCapped = false
	t.Unschedulable = false
	t.UnschedulableReason = ""
	t.NextRunAt = nil
	t.NextRunFar = false
	t.Arming = ArmingUnknown
	t.Unassessable = true
	return t
}

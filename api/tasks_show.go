package api

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/task"

	"github.com/spf13/cobra"
)

// `af tasks show` is the human-readable view of one task (#3623): the same
// record `af tasks get` prints as JSON, laid out for a person, plus the two
// things that were previously unanswerable from any surface — whether the task
// is actually firing on its schedule, and who last enabled or disabled it.
//
// It is a SEPARATE verb rather than a flag on `get` because the two answer
// different questions. `get` is the scripting read: one JSON object, stable
// shape, safe to pipe. `show` is what you run when a task looks wrong, and its
// output is free to grow a section the day something new is worth explaining.
// Passing the shared --json flag makes `show` emit the record in the standard
// envelope, so a script that reaches for the wrong verb still gets machine
// output rather than a screen of prose.

// showTimeFormat is the human timestamp layout: local time, sortable, and short
// enough for a label column.
const showTimeFormat = "2006-01-02 15:04"

var tasksShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Show one task's schedule health and audit trail",
	Long: "Show one task in the current project: its trigger, whether the running " +
		"daemon has it armed, when it will next fire, whether it has missed " +
		"scheduled runs, and the bounded audit trail of who created, updated, " +
		"enabled, or disabled it.\n\n" +
		"Overdue is derived, never stored: a cron task is overdue when it has gone " +
		"more than one period (or five minutes, whichever is larger) past its most " +
		"recent scheduled occurrence, measured from the latest of its last run, its " +
		"last enable, and its creation — so a task paused and switched back on does " +
		"not report the occurrences it missed while it was off. Watch tasks have no " +
		"schedule and are never overdue — their arming state is the signal.\n\n" +
		"\"Enabled but not armed\" means the task is enabled on disk and the running " +
		"daemon is not holding it: it will not fire until that is fixed. When nothing " +
		"has reported on it — no daemon running, or one still starting up — the arming " +
		"state is reported as unknown rather than guessed.\n\n" +
		"The task must belong to the resolved project: --repo when given, otherwise " +
		"the current directory's project. Outside a git repository there is no " +
		"project context and the id resolves globally.\n\n" +
		"Pass --json for the same record `af tasks get` returns, in the {data,error} " +
		"envelope.\n\n" +
		"With --daemon-url/AF_DAEMON_URL set, the task is read from that daemon, a " +
		"Daemon row names it beside the project path, and the schedule verdict is " +
		"the one the DAEMON derived — a cron expression is evaluated in the " +
		"scheduler's timezone, and re-deriving it here would answer in this " +
		"terminal's. There is no project context against a remote daemon, so the id " +
		"resolves across its projects and --repo is refused.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		if err := task.ValidateTaskID(args[0]); err != nil {
			return jsonError(err)
		}

		// Scope first, for the same reason `get` resolves it first: an invalid
		// --repo must report the path it could not resolve rather than be masked
		// by a not-found for the id.
		scope, err := resolveProjectScope(false)
		if err != nil {
			return jsonError(err)
		}
		t, err := getTaskByID("af tasks show", args[0])
		if err != nil {
			return jsonError(fmt.Errorf("failed to get task: %w", err))
		}
		if scope.Repo != nil {
			if err := requireTaskInScope(t, scope); err != nil {
				return jsonError(err)
			}
		}

		if envelopeOutput {
			return jsonOut(t)
		}
		renderTaskShow(cmd.OutOrStdout(), *t, time.Now(), apiclient.RemoteTargetURL())
		return nil
	},
}

// renderTaskShow writes the human view. now is passed rather than read so the
// oldest-missed line is derived against the same instant a test pins.
//
// daemonURL is the remote daemon this record came from, or "" for the local one
// (#3730). It does two things: it names the host beside the project path, since
// a path alone reads as this machine's whichever machine it is a path on, and it
// switches the schedule verdict from a client-side derivation to the record's
// own — see describeScheduleHealth.
func renderTaskShow(w io.Writer, t task.Task, now time.Time, daemonURL string) {
	name := t.Name
	if name == "" {
		name = "(unnamed)"
	}
	fmt.Fprintf(w, "%s · %s\n", name, t.ID)

	row := func(label, value string) {
		if value == "" {
			return
		}
		fmt.Fprintf(w, "  %-14s %s\n", label, value)
	}

	if t.IsWatch() {
		row("Trigger", "watch · "+t.WatchCmd)
	} else if t.CronExpr != "" {
		row("Trigger", "cron · "+t.CronExpr)
	}
	enabled := "no"
	if t.Enabled {
		enabled = "yes"
	}
	row("Enabled", enabled)
	row("Arming", describeArming(t))
	if t.NextRunAt != nil {
		row("Next run", t.NextRunAt.Format(showTimeFormat))
	}
	row("Last run", describeLastRun(t))
	row("Schedule", describeScheduleHealth(t, now, daemonURL != ""))
	row("Project", t.ProjectPath)
	row("Daemon", daemonURL)
	row("Program", t.Program)
	row("Target", t.TargetSession)

	fmt.Fprintf(w, "\n  Audit\n")
	if len(t.Audit) == 0 {
		// Every task written before #3623 has an empty trail, and so does one that
		// has not been touched since. Say which of the two this is, rather than
		// leaving a blank section that reads as "nothing ever happened".
		fmt.Fprintf(w, "    no recorded changes — the trail starts at the task's next create, update, enable, or disable\n")
		return
	}
	for _, e := range t.Audit {
		line := fmt.Sprintf("    %s  %-14s %s", e.At.Format(showTimeFormat), e.Actor, e.Action)
		if len(e.Fields) > 0 {
			line += " · " + strings.Join(e.Fields, ", ")
		}
		fmt.Fprintln(w, line)
	}
}

// describeArming turns the live arming observation into a sentence that says
// what it means, not just what it is. The unknown case is named explicitly: a
// reader must never mistake "no daemon answered" for "this task is not armed".
func describeArming(t task.Task) string {
	switch t.Arming {
	case task.ArmingArmed:
		return "armed"
	case task.ArmingNotArmed:
		if t.Enabled {
			return "enabled but not armed — it will not fire until this is fixed"
		}
		return "not armed (disabled)"
	default:
		// Not "no daemon answered": one that is still warming up answers and
		// reports exactly this, because arming has not run yet (see
		// taskScheduler.armed). Sending that user to daemon recovery would be
		// wrong, so the wording covers both ways nothing was observed.
		return "unknown — nothing has reported on it yet (no daemon running, or one still starting up)"
	}
}

func describeLastRun(t task.Task) string {
	if t.LastRunAt == nil {
		if t.LastRunStatus != "" {
			return "never · " + t.LastRunStatus
		}
		return "never"
	}
	out := t.LastRunAt.Format(showTimeFormat)
	if t.LastRunStatus != "" {
		out += " · " + t.LastRunStatus
	}
	return out
}

// describeScheduleHealth is the overdue verdict with the evidence behind it.
//
// remote says the record came from another host, and it changes WHO derives the
// verdict. Every branch below re-derives from the record and a clock, which is
// right for a local record and wrong for a remote one: the derivation normalizes
// to now.Location() so that it agrees with the scheduler that will actually fire
// the task (see task.deriveScheduleHealth), and against a remote daemon the
// scheduler's location is the DAEMON's, not this terminal's. A laptop in UTC
// asking a box in PDT about a midnight cron would otherwise report a verdict
// seven hours out — a confident answer about the wrong machine, which is the
// class of bug #3730 is (and #3627 raised on this exact function). So for a
// remote record the verdict is read off the record, where the daemon's own
// LoadTasks already put it.
func describeScheduleHealth(t task.Task, now time.Time, remote bool) string {
	if t.IsWatch() {
		return ""
	}
	// A disabled task is excluded from schedule health, and the exclusion has to
	// come BEFORE the parse. Otherwise a disabled draft with a malformed
	// expression reports "nothing is scheduled" — true, but it reads as a live
	// scheduling failure — while a disabled task with a VALID one reports nothing
	// at all. The scheduler skips a disabled task before parsing it too, and the
	// expression stays on screen under Trigger for anyone about to enable it
	// (#3623 review).
	if !t.Enabled {
		return ""
	}
	if remote {
		return describeRecordedScheduleHealth(t)
	}
	// Every shape of "the scheduler cannot fire this" comes from the SHARED
	// classifier, so this page cannot disagree with the verdict it is rendering —
	// re-deriving it here is what had it reporting an invalid expression for a
	// record that has none (#3623 review). The wording is the only thing that
	// differs per shape, and wording is this function's job.
	switch task.UnschedulableReason(t, now) {
	case task.ReasonNoTrigger:
		return "this task has no trigger, so nothing will ever run it"
	case task.ReasonInvalidExpression:
		// Name the parse error itself: this page has the expression in hand, and
		// the message is the whole repair instruction.
		_, err := task.ParseCron(t.CronExpr)
		return "cron expression is invalid, so nothing is scheduled: " + err.Error()
	case task.ReasonNoOccurrence:
		// The task is enabled and the scheduler may well hold an entry for it, so
		// every other line on this page reads as healthy; nothing is late only
		// because nothing was ever due. The claim is about the SCHEDULER rather
		// than the calendar, and the horizon is named because that is the whole
		// difference between "February 31st, never" and "a leap-day expression
		// whose next match is more than five years out" — the scheduler will not
		// fire either one.
		return "the scheduler cannot derive a next run from this expression " +
			"(it matches no date within its five-year horizon), so the task will not fire"
	}
	health := task.DeriveScheduleHealth(t, now)
	if health.Unassessable {
		// Never a clean bill for a record nothing could be measured from. It may
		// have been firing perfectly and it may never have fired at all; saying
		// which would be inventing the answer.
		return "cannot be assessed — no lateness could be measured from this record's history against its schedule"
	}
	if !health.Overdue {
		return "on schedule"
	}
	// A saturated ZERO means the shared walk budget was spent before this task was
	// reached, so no count was taken — "missed 0 or more" would be arithmetic on a
	// measurement nobody made, about a task already proven to have missed at least
	// one (#3623 review). Report the silence without a number.
	if health.MissedOccurrences <= 0 {
		return fmt.Sprintf("overdue · oldest missed %s", health.OldestMissedAt.Format(showTimeFormat))
	}
	missed := fmt.Sprintf("%d", health.MissedOccurrences)
	if health.Saturated {
		missed = fmt.Sprintf("%d or more", health.MissedOccurrences)
	}
	return fmt.Sprintf("overdue · missed %s · oldest missed %s",
		missed, health.OldestMissedAt.Format(showTimeFormat))
}

// describeRecordedScheduleHealth renders the verdict the DAEMON derived, carried
// on the record it sent (task.Task's read-time-derived fields). It is the remote
// half of describeScheduleHealth.
//
// It says strictly less than the local rendering, and every omission is a fact
// this client does not have rather than a rendering choice:
//
//   - The invalid-expression case does not quote the parse error. Naming it
//     needs task.ParseCron, and re-parsing here would be the same re-derivation
//     this branch exists to avoid — a client and a daemon on different releases
//     can disagree about which expressions parse, and then the page would
//     contradict the verdict it is rendering.
//   - "overdue" names no oldest-missed instant. ScheduleHealth.OldestMissedAt is
//     the one derived field that does NOT ride the record, so there is nothing to
//     print; inventing one from this terminal's clock is the seven-hours-out
//     answer above. The count still rides the record, cap flag and all.
func describeRecordedScheduleHealth(t task.Task) string {
	if t.Unschedulable {
		switch t.UnschedulableReason {
		case task.ReasonNoTrigger:
			return "this task has no trigger, so nothing will ever run it"
		case task.ReasonInvalidExpression:
			return "cron expression is invalid, so nothing is scheduled (as reported by the daemon)"
		case task.ReasonNoOccurrence:
			return "the scheduler cannot derive a next run from this expression " +
				"(it matches no date within its five-year horizon), so the task will not fire"
		default:
			// A reason this build does not know is still a verdict the daemon
			// reached. Report it rather than dropping to "on schedule", which
			// would turn a newer daemon's finding into a clean bill of health.
			return "the daemon reports this task cannot be scheduled: " + t.UnschedulableReason
		}
	}
	if t.Unassessable {
		return "cannot be assessed — no lateness could be measured from this record's history against its schedule"
	}
	if !t.Overdue {
		return "on schedule"
	}
	if t.MissedOccurrences <= 0 {
		return "overdue"
	}
	missed := fmt.Sprintf("%d", t.MissedOccurrences)
	if t.MissedOccurrencesCapped {
		missed = fmt.Sprintf("%d or more", t.MissedOccurrences)
	}
	return "overdue · missed " + missed
}

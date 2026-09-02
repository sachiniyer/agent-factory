package api

import (
	"fmt"
	"io"
	"strings"
	"time"

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
		"envelope.",
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
		t, err := getTaskByID(args[0])
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
		renderTaskShow(cmd.OutOrStdout(), *t, time.Now())
		return nil
	},
}

// renderTaskShow writes the human view. now is passed rather than read so the
// oldest-missed line is derived against the same instant a test pins.
func renderTaskShow(w io.Writer, t task.Task, now time.Time) {
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
	row("Schedule", describeScheduleHealth(t, now))
	row("Project", t.ProjectPath)
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
func describeScheduleHealth(t task.Task, now time.Time) string {
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
	if !t.Enabled || t.CronExpr == "" {
		return ""
	}
	if t.CronExpr != "" {
		if _, err := task.ParseCron(t.CronExpr); err != nil {
			// Say so rather than falling through to "on schedule". Nothing can be
			// derived from an expression with no occurrences, and a task the
			// scheduler refuses to parse is the opposite of healthy — it is also
			// exactly why the arming line above reads "not armed".
			return "cron expression is invalid, so nothing is scheduled: " + err.Error()
		}
	}
	health := task.DeriveScheduleHealth(t, now)
	if health.Unschedulable {
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
	if health.Unassessable {
		// Never a clean bill for a record nothing could be measured from. It may
		// have been firing perfectly and it may never have fired at all; saying
		// which would be inventing the answer.
		return "cannot be assessed — no lateness could be measured from this record's history against its schedule"
	}
	if !health.Overdue {
		return "on schedule"
	}
	missed := fmt.Sprintf("%d", health.MissedOccurrences)
	if health.Saturated {
		missed = fmt.Sprintf("%d or more", health.MissedOccurrences)
	}
	return fmt.Sprintf("overdue · missed %s · oldest missed %s",
		missed, health.OldestMissedAt.Format(showTimeFormat))
}

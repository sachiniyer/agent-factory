package doctor

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/task"
)

// The automations check (#3623): does every enabled task actually fire?
//
// Two enabled hourly tasks went dark for 18 days on a healthy daemon and no
// surface could say so. Doctor is the surface that is supposed to notice — it
// already sweeps for orphaned processes, leaked tmux sessions and stale homes on
// a box whose owner never opens the TUI — and a scheduled task that silently
// stopped belongs in exactly that list.
//
// Two conditions are reported, both from the same row because they are the same
// question asked twice:
//
//   - OVERDUE: the schedule has moved on without the task. Derived here from the
//     record and the cron expression, so it needs no daemon and catches every
//     cause, including the ones nobody has thought of.
//   - ENABLED BUT NOT ARMED: the task is enabled on disk and the running daemon
//     is not holding it, so it will not fire at all. This one can only be
//     observed by the daemon, which is why it is reported only when a daemon
//     actually answered — an unreachable daemon leaves arming UNKNOWN, and
//     "unknown" must never be rendered as "broken" (a stopped daemon would
//     otherwise report every task on the box as unarmed).

// checkTaskSchedules reports enabled tasks that are not firing.
func checkTaskSchedules(ctx *scanContext, report *Report) {
	tasks, err := ctx.opts.taskInventory()
	if err != nil {
		// A failed read is not an empty result. Say doctor could not look rather
		// than reporting the clean bill an empty list would produce — advisory,
		// because failing to observe is not proof of an unhealthy condition.
		report.Warn(sectionAutomations, "task schedules",
			fmt.Sprintf("could not read the task store: %v", err),
			"check that the agent-factory home is readable, then rerun `af doctor`", false)
		return
	}

	now := time.Now()
	var overdue []task.Task
	var health []task.ScheduleHealth
	var unarmed []task.Task
	var unschedulable []task.Task
	var unassessable []task.Task
	enabled := 0
	armingUnobserved := 0
	for _, t := range tasks {
		if !t.Enabled {
			continue
		}
		enabled++
		h := task.DeriveScheduleHealth(t, now)
		classified := true
		switch {
		case h.Overdue:
			overdue = append(overdue, t)
			health = append(health, h)
		case h.Unschedulable:
			unschedulable = append(unschedulable, t)
		case h.Unassessable:
			// UNKNOWN, and doctor's line on unknowns is that they stay visible and
			// stay advisory: nothing was established here, so this must not raise an
			// alarm — and must not hand out a clean bill either.
			unassessable = append(unassessable, t)
			classified = false
		default:
			classified = false
		}
		// Whether this task's arming was OBSERVED is per-task, not per-run: a
		// daemon answers reads while it is still warming up, and every task it
		// returns in that window carries ArmingUnknown. Counting only "did a
		// daemon answer" would print an unqualified clean bill for tasks whose
		// arming had not happened yet.
		if t.Arming == task.ArmingUnknown {
			armingUnobserved++
			continue
		}
		// A task this run has ALREADY named is not listed again. Not-armed is the
		// cause of an overdue task's silence, and it is the daemon's response to an
		// expression that can never fire — in both cases naming it twice with two
		// remediations makes one problem look like two. `af tasks show <id>`, which
		// every remediation names, reports the arming state.
		if t.Arming == task.ArmingNotArmed && !classified {
			unarmed = append(unarmed, t)
		}
	}

	if enabled == 0 {
		report.Pass(sectionAutomations, "task schedules", "no enabled tasks")
		return
	}

	// Qualifiers say what this run could NOT establish. They ride both the clean
	// row and the alarming one, and neither of them makes the row actionable on
	// its own — the difference between "healthy" and "healthy as far as I could
	// see" is the only thing they are for.
	var qualifiers []string
	if armingUnobserved > 0 {
		qualifiers = append(qualifiers, fmt.Sprintf(
			"arming not observed for %d of them (no daemon has reported on those)", armingUnobserved))
	}
	if len(unassessable) > 0 {
		qualifiers = append(qualifiers, fmt.Sprintf(
			"%d could not be assessed (no lateness could be measured from their history against their schedule) — %s",
			len(unassessable), describeTaskNames(unassessable)))
	}
	withQualifiers := func(detail string) string {
		if len(qualifiers) == 0 {
			return detail
		}
		return detail + "; " + strings.Join(qualifiers, "; ")
	}

	if len(overdue) == 0 && len(unarmed) == 0 && len(unschedulable) == 0 {
		assessed := enabled - len(unassessable)
		if assessed == 0 {
			// Every enabled task is an unknown, so there is nothing to call healthy.
			report.Pass(sectionAutomations, "task schedules",
				strings.Join(qualifiers, "; "))
			return
		}
		report.Pass(sectionAutomations, "task schedules",
			withQualifiers(fmt.Sprintf("%s firing on schedule", countTasksAre(assessed))))
		return
	}

	var details, fixes []string
	if len(overdue) > 0 {
		oldest := health[0].OldestMissedAt
		for _, h := range health[1:] {
			if h.OldestMissedAt.Before(oldest) {
				oldest = h.OldestMissedAt
			}
		}
		details = append(details, fmt.Sprintf("%s not fired on schedule; oldest missed %s — %s",
			countTasksHave(len(overdue)), oldest.Format(taskTimeFormat), describeOverdue(overdue, health)))
		fixes = append(fixes, "inspect one with `af tasks show <id>`; fire it now with `af tasks trigger <id>`")
	}
	if len(unarmed) > 0 {
		details = append(details, fmt.Sprintf("%s enabled but not armed by the running daemon — %s",
			countTasksAre(len(unarmed)), describeTaskNames(unarmed)))
		fixes = append(fixes, "check the daemon log for an arming refusal, then `af daemon restart`")
	}
	if len(unschedulable) > 0 {
		// Enabled, and the scheduler cannot derive a next fire: the expression does
		// not parse, or nothing matches inside its search horizon. Nothing is late,
		// because nothing was ever due — which is why this needs its own clause
		// rather than folding into overdue. It is derived from the record, so a box
		// whose daemon is down or still warming up gets it too; leaning on a live
		// not-armed observation instead is what let doctor call such a task healthy.
		details = append(details, fmt.Sprintf("%s a cron expression the scheduler cannot fire — %s",
			countTasksHave(len(unschedulable)), describeTaskNames(unschedulable)))
		fixes = append(fixes, "correct the expression with `af tasks update <id> --cron <expr>`")
	}
	report.Warn(sectionAutomations, "task schedules",
		withQualifiers(strings.Join(details, " · ")), strings.Join(fixes, "; "), true)
}

// taskTimeFormat is the doctor row's timestamp layout — local, sortable, and
// short enough to sit inside a one-line detail.
const taskTimeFormat = "2006-01-02 15:04"

// maxNamedTasks bounds how many tasks one row names before collapsing the rest,
// following the process checks' summary convention: the actionable fact is the
// count, and naming a few makes it possible to act without `--verbose`.
const maxNamedTasks = 5

// countTasksAre / countTasksHave carry the verb because the row reads as a
// sentence and English does not let the count and the verb disagree.
func countTasksAre(n int) string {
	if n == 1 {
		return "1 enabled task is"
	}
	return fmt.Sprintf("%d enabled tasks are", n)
}

func countTasksHave(n int) string {
	if n == 1 {
		return "1 enabled task has"
	}
	return fmt.Sprintf("%d enabled tasks have", n)
}

// describeOverdue names the overdue tasks with their missed counts, so the row
// is actionable without a second command.
func describeOverdue(tasks []task.Task, health []task.ScheduleHealth) string {
	parts := make([]string, 0, len(tasks))
	for i, t := range tasks {
		missed := fmt.Sprintf("%d", health[i].MissedOccurrences)
		if health[i].Saturated {
			missed += "+"
		}
		parts = append(parts, fmt.Sprintf("%s (missed %s)", taskLabel(t), missed))
	}
	return collapseTaskList(parts)
}

func describeTaskNames(tasks []task.Task) string {
	parts := make([]string, 0, len(tasks))
	for _, t := range tasks {
		parts = append(parts, taskLabel(t))
	}
	return collapseTaskList(parts)
}

// taskLabel identifies a task by id AND name: the id is what every fix command
// takes, and the name is what the person recognizes.
func taskLabel(t task.Task) string {
	if strings.TrimSpace(t.Name) == "" {
		return t.ID
	}
	return fmt.Sprintf("%s %q", t.ID, t.Name)
}

func collapseTaskList(parts []string) string {
	sort.Strings(parts)
	if len(parts) <= maxNamedTasks {
		return strings.Join(parts, ", ")
	}
	return fmt.Sprintf("%s, and %d more", strings.Join(parts[:maxNamedTasks], ", "), len(parts)-maxNamedTasks)
}

// daemonTaskInventory reads the task list, preferring the running daemon's
// answer because it is the only one that carries the LIVE arming state. A disk
// read is a complete and current view of every task and its schedule — overdue
// is derived from the record — but it knows nothing about arming, and every task
// it returns says so by carrying task.ArmingUnknown.
//
// It deliberately reports NO "was a daemon reachable" flag. Whether one answered
// is the weaker question: a daemon that answers during warm-up is reachable and
// has observed nothing, so a run-level flag would let doctor print an unqualified
// clean bill for tasks whose arming had not happened yet (#3623 review). The
// per-task field is the precise answer, and leaving no imprecise one available
// is what stops a later caller from reaching for it.
//
// Mirrors `af tasks list`: never spawn a daemon for a read.
func daemonTaskInventory() ([]task.Task, error) {
	if tasks, err := daemon.ListTasksNoSpawn(); err == nil {
		return tasks, nil
	}
	return task.LoadTasks()
}

package doctor

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/task"
)

// darkTask models #3623's own fixture: an enabled hourly task whose last run
// started 18 days ago and which every surface still called healthy.
func darkTask(id, name string) task.Task {
	last := time.Now().Add(-18 * 24 * time.Hour)
	return task.Task{
		ID:            id,
		Name:          name,
		CronExpr:      "20 * * * *",
		Prompt:        "sweep",
		ProjectPath:   "/tmp/repo",
		Program:       "claude",
		Enabled:       true,
		CreatedAt:     time.Now().Add(-60 * 24 * time.Hour),
		LastRunAt:     &last,
		LastRunStatus: "started",
	}
}

func healthyTask(id string) task.Task {
	last := time.Now().Add(-2 * time.Minute)
	t := darkTask(id, "Healthy")
	t.LastRunAt = &last
	return t
}

// taskRow returns the automations row, failing the test if doctor did not
// produce one.
func taskRow(t *testing.T, report *Report) CheckResult {
	t.Helper()
	for _, c := range report.Checks {
		if c.Section == sectionAutomations && c.Name == "task schedules" {
			return c
		}
	}
	t.Fatalf("doctor produced no automations row; rows: %+v", report.Checks)
	return CheckResult{}
}

// TestDoctor_WarnsAboutOverdueTasks is the check the report in #3623 was
// missing. A person had to dump last_run_at for every task and difference it
// against the cron expression by hand; doctor has to do that itself, name the
// tasks, and say when the silence started.
func TestDoctor_WarnsAboutOverdueTasks(t *testing.T) {
	opts := testOptions(t, false)
	opts.taskInventory = func() ([]task.Task, error) {
		return []task.Task{
			darkTask("4ab7ba4f", "Master Health Watch"),
			darkTask("b2bfd63e", "Fleet Health Sweep"),
			healthyTask("9d7a6aa1"),
		}, nil
	}

	report, err := Run(opts)
	require.NoError(t, err)

	row := taskRow(t, report)
	assert.Equal(t, StatusWarn, row.Status)
	assert.Contains(t, row.Detail, "2 enabled tasks have not fired on schedule")
	assert.Contains(t, row.Detail, "oldest missed")
	assert.Contains(t, row.Detail, `4ab7ba4f "Master Health Watch"`)
	assert.Contains(t, row.Detail, `b2bfd63e "Fleet Health Sweep"`)
	assert.NotContains(t, row.Detail, "9d7a6aa1", "a task firing on schedule is not named")
	assert.Contains(t, row.Detail, "missed 4", "the row carries the missed count, not just the fact")
	assert.NotEmpty(t, row.Remediation, "a row nobody can act on is a row nobody acts on")
	assert.Positive(t, report.UnresolvedCount(),
		"a task that has been dark for 18 days is an unhealthy box, and `af doctor` must exit nonzero on it")
}

// TestDoctor_WarnsAboutEnabledButUnarmedTasks generalizes #2929: whatever the
// reason a task is enabled on disk and absent from the running daemon, it will
// not fire, and the row says so.
func TestDoctor_WarnsAboutEnabledButUnarmedTasks(t *testing.T) {
	opts := testOptions(t, false)
	unarmed := healthyTask("c0ffee01")
	unarmed.Name = "Nightly Docs Audit"
	unarmed.Arming = task.ArmingNotArmed
	armed := healthyTask("d0d0cafe")
	armed.Arming = task.ArmingArmed
	opts.taskInventory = func() ([]task.Task, error) {
		return []task.Task{unarmed, armed}, nil
	}

	report, err := Run(opts)
	require.NoError(t, err)

	row := taskRow(t, report)
	assert.Equal(t, StatusWarn, row.Status)
	assert.Contains(t, row.Detail, "1 enabled task is enabled but not armed")
	assert.Contains(t, row.Detail, `c0ffee01 "Nightly Docs Audit"`)
	assert.NotContains(t, row.Detail, "d0d0cafe")
	assert.Positive(t, report.UnresolvedCount())
}

// TestDoctor_UnobservedArmingIsNotReportedAsUnarmed is the fabricated-negative
// guard. With no daemon answering, every task's arming is UNKNOWN — and reading
// that as "not armed" would report every task on a box with a stopped daemon as
// broken, which is both wrong and exactly the kind of noise that trains people
// to ignore the row that matters.
func TestDoctor_UnobservedArmingIsNotReportedAsUnarmed(t *testing.T) {
	opts := testOptions(t, false)
	opts.taskInventory = func() ([]task.Task, error) {
		// Arming is the zero value: nobody looked. The disk read that produced
		// these records cannot know, and says so by leaving the field empty.
		return []task.Task{healthyTask("c0ffee01"), healthyTask("d0d0cafe")}, nil
	}

	report, err := Run(opts)
	require.NoError(t, err)

	row := taskRow(t, report)
	assert.Equal(t, StatusPass, row.Status)
	assert.Contains(t, row.Detail, "arming not observed for 2 of them",
		"the row must say what it could NOT see rather than implying it saw health")
	assert.Zero(t, report.UnresolvedCount())
}

// TestDoctor_OverdueIsReportedWithoutADaemon: the overdue half is pure
// derivation from the record, so it survives a box whose daemon is not running —
// which is precisely the box this check is for.
func TestDoctor_OverdueIsReportedWithoutADaemon(t *testing.T) {
	opts := testOptions(t, false)
	opts.taskInventory = func() ([]task.Task, error) {
		return []task.Task{darkTask("4ab7ba4f", "Master Health Watch")}, nil
	}

	report, err := Run(opts)
	require.NoError(t, err)

	row := taskRow(t, report)
	assert.Equal(t, StatusWarn, row.Status)
	assert.Contains(t, row.Detail, "1 enabled task has not fired on schedule")
}

// TestDoctor_HealthyTasksPass keeps the row honest in the ordinary case, and
// keeps a green box green.
func TestDoctor_HealthyTasksPass(t *testing.T) {
	opts := testOptions(t, false)
	armed := healthyTask("d0d0cafe")
	armed.Arming = task.ArmingArmed
	opts.taskInventory = func() ([]task.Task, error) {
		return []task.Task{armed}, nil
	}

	report, err := Run(opts)
	require.NoError(t, err)

	row := taskRow(t, report)
	assert.Equal(t, StatusPass, row.Status)
	assert.Contains(t, row.Detail, "1 enabled task is firing on schedule")
	assert.NotContains(t, row.Detail, "arming not observed")
}

// TestDoctor_UnreadableTaskStoreIsAdvisory: a failed read is not an empty
// result. Doctor must say it could not look rather than report the clean bill an
// empty list would produce — and must not fail the run over an observation it
// never made.
func TestDoctor_UnreadableTaskStoreIsAdvisory(t *testing.T) {
	opts := testOptions(t, false)
	opts.taskInventory = func() ([]task.Task, error) {
		return nil, errors.New("tasks.json: permission denied")
	}

	report, err := Run(opts)
	require.NoError(t, err)

	row := taskRow(t, report)
	assert.Equal(t, StatusWarn, row.Status)
	assert.Contains(t, row.Detail, "could not read the task store")
	assert.Zero(t, report.UnresolvedCount(),
		"failing to observe is not proof of an unhealthy condition")
}

// TestDoctor_NoEnabledTasksPasses: a box with nothing scheduled is healthy, and
// the row says which of the two silences this is.
func TestDoctor_NoEnabledTasksPasses(t *testing.T) {
	opts := testOptions(t, false)
	disabled := darkTask("4ab7ba4f", "Master Health Watch")
	disabled.Enabled = false
	opts.taskInventory = func() ([]task.Task, error) {
		return []task.Task{disabled}, nil
	}

	report, err := Run(opts)
	require.NoError(t, err)

	row := taskRow(t, report)
	assert.Equal(t, StatusPass, row.Status)
	assert.Equal(t, "no enabled tasks", row.Detail)
}

// TestDoctor_CollapsesLongTaskLists follows the process checks' convention: the
// count is the actionable fact, and a handful of names makes the row usable
// without turning it into a wall.
func TestDoctor_CollapsesLongTaskLists(t *testing.T) {
	opts := testOptions(t, false)
	var tasks []task.Task
	for i := 0; i < maxNamedTasks+3; i++ {
		tasks = append(tasks, darkTask(strings.Repeat("a", 7)+string(rune('0'+i)), "Dark"))
	}
	opts.taskInventory = func() ([]task.Task, error) { return tasks, nil }

	report, err := Run(opts)
	require.NoError(t, err)
	assert.Contains(t, taskRow(t, report).Detail, "and 3 more")
}

// TestDoctor_WarmingDaemonDoesNotYieldAnUnqualifiedPass: a daemon answers reads
// while it is still warming up, and every task it returns in that window carries
// ArmingUnknown. Keying the qualifier on "did a daemon answer" would print an
// unconditional clean bill for tasks whose arming has not happened yet.
func TestDoctor_WarmingDaemonDoesNotYieldAnUnqualifiedPass(t *testing.T) {
	opts := testOptions(t, false)
	armed := healthyTask("d0d0cafe")
	armed.Arming = task.ArmingArmed
	warming := healthyTask("c0ffee01") // answered by a live daemon, arming not yet observed
	opts.taskInventory = func() ([]task.Task, error) {
		return []task.Task{armed, warming}, nil
	}

	report, err := Run(opts)
	require.NoError(t, err)

	row := taskRow(t, report)
	assert.Equal(t, StatusPass, row.Status)
	assert.Contains(t, row.Detail, "arming not observed for 1 of them",
		"the qualifier is per task, not per run:\n%s", row.Detail)
}

// TestDoctor_OverdueTaskIsNotAlsoListedAsUnarmed: not-armed is the CAUSE of that
// same silence. Naming the task in both clauses makes one problem look like two
// and offers two remediation paths for it.
func TestDoctor_OverdueTaskIsNotAlsoListedAsUnarmed(t *testing.T) {
	opts := testOptions(t, false)
	both := darkTask("4ab7ba4f", "Master Health Watch")
	both.Arming = task.ArmingNotArmed
	opts.taskInventory = func() ([]task.Task, error) { return []task.Task{both}, nil }

	report, err := Run(opts)
	require.NoError(t, err)

	row := taskRow(t, report)
	assert.Contains(t, row.Detail, "1 enabled task has not fired on schedule")
	assert.NotContains(t, row.Detail, "enabled but not armed",
		"one silence, reported once:\n%s", row.Detail)
	assert.Equal(t, 1, strings.Count(row.Detail, "4ab7ba4f"), "and the task is named once")
}

// TestDoctor_ReportsATaskThatCanNeverFire: "0 0 31 2 *" parses, so the scheduler
// arms it and it reads as armed and on schedule — while being incapable of ever
// running. Nothing is late because nothing was ever due, which is why it needs
// its own clause rather than folding into overdue.
func TestDoctor_ReportsATaskThatCanNeverFire(t *testing.T) {
	opts := testOptions(t, false)
	feb31 := healthyTask("feb31111")
	feb31.Name = "February 31st"
	feb31.CronExpr = "0 0 31 2 *"
	feb31.Arming = task.ArmingArmed
	opts.taskInventory = func() ([]task.Task, error) { return []task.Task{feb31}, nil }

	report, err := Run(opts)
	require.NoError(t, err)

	row := taskRow(t, report)
	assert.Equal(t, StatusWarn, row.Status)
	assert.Contains(t, row.Detail, "a cron expression that can never fire")
	assert.Contains(t, row.Detail, `feb31111 "February 31st"`)
	assert.Positive(t, report.UnresolvedCount())
}

// TestDoctor_ReportsAnInvalidExpressionWithNoDaemon is the half the first
// unschedulable fix missed. A hand-edited or legacy expression that does not
// parse was classified only by a LIVE not-armed observation — so on a box whose
// daemon is down or still warming up, doctor reported "firing on schedule" for a
// task the scheduler will refuse forever. The verdict comes from the record now,
// so it survives having no daemon at all.
func TestDoctor_ReportsAnInvalidExpressionWithNoDaemon(t *testing.T) {
	opts := testOptions(t, false)
	broken := healthyTask("badcr0n1")
	broken.Name = "Hand-edited watchdog"
	broken.CronExpr = "99 * * * *"
	// Arming deliberately left unobserved: no daemon answered.
	opts.taskInventory = func() ([]task.Task, error) { return []task.Task{broken}, nil }

	report, err := Run(opts)
	require.NoError(t, err)

	row := taskRow(t, report)
	assert.Equal(t, StatusWarn, row.Status)
	assert.Contains(t, row.Detail, "a cron expression that can never fire")
	assert.Contains(t, row.Detail, `badcr0n1 "Hand-edited watchdog"`)
	assert.NotContains(t, row.Detail, "firing on schedule")
	assert.Positive(t, report.UnresolvedCount())
}

// TestDoctor_UnschedulableTaskIsNotAlsoListedAsUnarmed: the scheduler refuses an
// expression it cannot parse, so a live daemon reports that same task as
// not-armed — which is its RESPONSE to the condition, not a second condition.
// Same rule as the overdue case, and the guard has to cover every class the run
// has already named rather than just the first one it grew.
func TestDoctor_UnschedulableTaskIsNotAlsoListedAsUnarmed(t *testing.T) {
	opts := testOptions(t, false)
	broken := healthyTask("badcr0n2")
	broken.Name = "Hand-edited watchdog"
	broken.CronExpr = "99 * * * *"
	broken.Arming = task.ArmingNotArmed
	opts.taskInventory = func() ([]task.Task, error) { return []task.Task{broken}, nil }

	report, err := Run(opts)
	require.NoError(t, err)

	row := taskRow(t, report)
	assert.Contains(t, row.Detail, "a cron expression that can never fire")
	assert.NotContains(t, row.Detail, "enabled but not armed",
		"one condition, reported once:\n%s", row.Detail)
	assert.Equal(t, 1, strings.Count(row.Detail, "badcr0n2"))
}

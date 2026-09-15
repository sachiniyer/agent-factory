package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/task"
)

const selftestQuietWindow = 11 * time.Hour

type seededSelftestTasks struct {
	cron    map[string]string
	overdue task.Task
}

func runWebSelftestSeedBlock(t *testing.T, now time.Time) seededSelftestTasks {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("web selftest entrypoint is a bash script")
	}

	entryPath := filepath.Join(repoRoot(t), "scripts", "container", "web-selftest-entry.sh")
	source, err := os.ReadFile(entryPath)
	if err != nil {
		t.Fatalf("read web selftest entrypoint: %v", err)
	}
	const startMarker = "# --- seed a scheduled task"
	const endMarker = "# --- seed a web-tab session"
	start := strings.Index(string(source), startMarker)
	end := strings.Index(string(source), endMarker)
	if start < 0 || end <= start {
		t.Fatalf("task seed block markers missing from %s", entryPath)
	}

	dir := t.TempDir()
	callsPath := filepath.Join(dir, "calls")
	commandScript := `#!/bin/sh
{
  printf '%s' "${0##*/}"
  for arg in "$@"; do printf '\t%s' "$arg"; done
  printf '\n'
} >> "$AF_FIXTURE_CALLS"
`
	for _, name := range []string{"af", "curl"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(commandScript), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}
	dateScript := `#!/bin/sh
if [ "$#" -eq 1 ] && [ "$1" = '+%-H' ]; then
  printf '%s\n' "${AF_FIXTURE_CLOCK##* }"
elif [ "$#" -eq 1 ] && [ "$1" = '+%-M %-H' ]; then
  printf '%s\n' "$AF_FIXTURE_CLOCK"
elif [ "$#" -eq 4 ] && [ "$1" = '-u' ] && [ "$2" = '-d' ] && [ "$3" = '30 days ago' ]; then
  printf '%s\n' "$AF_FIXTURE_CREATED"
else
  printf 'unexpected date arguments:' >&2
  printf ' <%s>' "$@" >&2
  printf '\n' >&2
  exit 2
fi
`
	if err := os.WriteFile(filepath.Join(dir, "date"), []byte(dateScript), 0o755); err != nil {
		t.Fatalf("write fake date: %v", err)
	}

	cmd := exec.Command("bash", "-c", string(source[start:end]))
	cmd.Env = append(os.Environ(),
		"PATH="+dir,
		"BIN="+filepath.Join(dir, "af"),
		"MOCK=/work/mock-repo",
		"MOCK3=/work/mock-repo-3",
		"SEEDED_TASK=probe-task",
		"TASK3_NAME=mock3-task",
		"OVERDUE_TASK=overdue-task",
		"BASE_URL=http://127.0.0.1:8899",
		"AF_FIXTURE_CALLS="+callsPath,
		"AF_FIXTURE_CLOCK="+strconv.Itoa(now.Minute())+" "+strconv.Itoa(now.Hour()),
		"AF_FIXTURE_CREATED="+now.AddDate(0, 0, -30).Format(time.RFC3339),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run task seed block: %v\n%s", err, out)
	}

	calls, err := os.ReadFile(callsPath)
	if err != nil {
		t.Fatalf("read captured seed calls: %v", err)
	}
	got := seededSelftestTasks{cron: make(map[string]string)}
	for _, line := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
		fields := strings.Split(line, "\t")
		switch fields[0] {
		case "af":
			var name, cron string
			for i := 1; i+1 < len(fields); i++ {
				switch fields[i] {
				case "--name":
					name = fields[i+1]
				case "--cron":
					cron = fields[i+1]
				}
			}
			if name != "" && cron != "" {
				got.cron[name] = cron
			}
		case "curl":
			for i := 1; i+1 < len(fields); i++ {
				if fields[i] != "-d" {
					continue
				}
				var payload struct {
					Task task.Task `json:"task"`
				}
				if err := json.Unmarshal([]byte(fields[i+1]), &payload); err != nil {
					t.Fatalf("decode AddTask payload: %v", err)
				}
				got.overdue = payload.Task
			}
		}
	}
	return got
}

func assertSelftestScheduleStaysQuiet(t *testing.T, name, expr string, now time.Time) {
	t.Helper()
	if expr == "" {
		t.Fatalf("seed block did not create %s with a cron schedule", name)
	}
	schedule, err := task.ParseCron(expr)
	if err != nil {
		t.Fatalf("parse %s schedule %q: %v", name, expr, err)
	}
	next := schedule.Next(now)
	if until := next.Sub(now); until < selftestQuietWindow {
		t.Fatalf("%s schedule %q fires at %s, only %s after seeding; want at least %s of quiet time",
			name, expr, next.Format(time.RFC3339), until, selftestQuietWindow)
	}
}

func TestWebSelftestSeededSchedulesStayQuietAndOverdueFixtureRemainsOverdue(t *testing.T) {
	for _, now := range []time.Time{
		time.Date(2026, time.September, 11, 8, 59, 0, 0, time.UTC),
		time.Date(2026, time.September, 11, 23, 59, 0, 0, time.UTC),
	} {
		t.Run(now.Format("15-04"), func(t *testing.T) {
			seeded := runWebSelftestSeedBlock(t, now)
			for _, name := range []string{"probe-task", "mock3-task"} {
				t.Run(name, func(t *testing.T) {
					assertSelftestScheduleStaysQuiet(t, name, seeded.cron[name], now)
				})
			}

			t.Run("overdue-task", func(t *testing.T) {
				overdue := seeded.overdue
				if overdue.Name != "overdue-task" || !overdue.Enabled {
					t.Fatalf("overdue fixture must remain an enabled cron task, got %+v", overdue)
				}
				assertSelftestScheduleStaysQuiet(t, overdue.Name, overdue.CronExpr, now)
				health := task.DeriveScheduleHealth(overdue, now)
				if health.Unschedulable || !health.Overdue || health.MissedOccurrences == 0 {
					t.Fatalf("overdue fixture lost its schedulable overdue state: %+v", health)
				}
			})
		})
	}
}

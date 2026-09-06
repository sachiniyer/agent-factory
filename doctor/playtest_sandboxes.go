package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

const defaultPlaytestMaxLifetime = 6 * time.Hour

// Injectable so the doctor suite never observes the host's containers.
var playtestEngineOutput = defaultPlaytestEngineOutput

func defaultPlaytestEngineOutput(ctx context.Context, args ...string) ([]byte, error) {
	// Match scripts/testbox.sh: prefer Docker whenever it is on PATH, and
	// fall back to Podman only when Docker is absent (not when it fails).
	for _, engine := range []string{"docker", "podman"} {
		path, err := exec.LookPath(engine)
		if err == nil {
			return exec.CommandContext(ctx, path, args...).Output()
		}
	}
	return nil, exec.ErrNotFound
}

type playtestContainer struct {
	ID    string `json:"Id"`
	Name  string
	State struct {
		StartedAt string
	}
	Config struct {
		Labels map[string]string
		Env    []string
		Cmd    []string
	}
}

// Keep this predicate in sync with scripts/container/playtest-lifetime.sh.
// StartedAt, rather than Created or a date embedded in the name, determines age.
func (c playtestContainer) stranded(now time.Time) (time.Duration, time.Duration, error) {
	if c.Config.Labels["af.harness"] != "testbox" || !strings.HasPrefix(strings.TrimPrefix(c.Name, "/"), "af-playtest-") {
		return 0, 0, nil
	}
	// Interactive shells have no detached deadline. Legacy sandboxes lack the
	// mode label, so recognize only the exact hold invocation used by testbox.
	switch c.Config.Labels["af.playtest.mode"] {
	case "detached":
	case "":
		if !slices.Equal(c.Config.Cmd, []string{"bash", "/src/scripts/container/playtest-entry.sh", "hold"}) {
			return 0, 0, nil
		}
	default:
		return 0, 0, nil
	}
	started, err := time.Parse(time.RFC3339Nano, c.State.StartedAt)
	if err != nil || started.IsZero() {
		return 0, 0, fmt.Errorf("invalid StartedAt for %s", c.Name)
	}
	value := ""
	for _, entry := range c.Config.Env {
		if configured, ok := strings.CutPrefix(entry, "AF_PLAYTEST_MAX_LIFETIME="); ok {
			value = configured
		}
	}
	limit := defaultPlaytestMaxLifetime
	if value != "" {
		seconds, parseErr := strconv.ParseUint(value, 10, 31)
		if parseErr != nil || seconds == 0 || value[0] < '1' || value[0] > '9' || len(value) > 10 {
			return 0, 0, fmt.Errorf("invalid AF_PLAYTEST_MAX_LIFETIME for %s", c.Name)
		}
		limit = time.Duration(seconds) * time.Second
	}
	return now.Sub(started), limit, nil
}

// checkStrandedPlaytestSandboxes is deliberately report-only: the harness owns
// reaping, including its fix-time identity checks; doctor never removes
// containers, even with --fix.
func checkStrandedPlaytestSandboxes(report *Report) {
	const check = "stranded-playtest-sandbox"
	// Container age must remain available when the process snapshot failed.
	now := time.Now()
	deadline, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := playtestEngineOutput(deadline, "ps", "-aq", "--filter", "label=af.harness=testbox", "--filter", "name=af-playtest-")
	if errors.Is(err, exec.ErrNotFound) {
		report.Pass(sectionProcesses, check, "Neither Docker nor Podman is installed; sandbox inspection is not applicable")
		return
	}
	incomplete := func(detail string) {
		report.markIncomplete(check)
		report.Warn(sectionProcesses, check, detail, "check container engine connectivity, then rerun `af doctor`", false)
	}
	if err != nil {
		incomplete(fmt.Sprintf("could not list play-test sandboxes: %v", err))
		return
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		report.Pass(sectionProcesses, check, "no labelled play-test sandboxes")
		return
	}
	out, err = playtestEngineOutput(deadline, append([]string{"inspect"}, ids...)...)
	if err != nil {
		incomplete(fmt.Sprintf("could not inspect play-test sandboxes: %v", err))
		return
	}
	var containers []playtestContainer
	if err := json.Unmarshal(out, &containers); err != nil {
		incomplete("could not decode container sandbox inspection")
		return
	}
	if len(containers) != len(ids) {
		incomplete("Container inspection did not return every listed sandbox")
		return
	}
	found := 0
	complete := true
	for _, c := range containers {
		age, limit, err := c.stranded(now)
		if err != nil {
			complete = false
			incomplete(err.Error())
			continue
		}
		if limit == 0 || age < limit {
			continue
		}
		found++
		report.addActionableFinding(Finding{
			Check: check, Section: sectionProcesses, Severity: StatusWarn,
			Detail:      fmt.Sprintf("%s (%s) started %s ago, exceeding its %s maximum lifetime", strings.TrimPrefix(c.Name, "/"), c.ID, age.Round(time.Second), limit),
			Remediation: "the next detached play-test start reaps expired labelled sandboxes; doctor reports them without removing containers",
		})
	}
	if found == 0 && complete {
		report.Pass(sectionProcesses, check, "no labelled play-test sandboxes exceed their maximum lifetime")
	}
}

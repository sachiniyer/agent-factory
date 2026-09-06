package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func sandboxFixture(started time.Time) playtestContainer {
	var c playtestContainer
	c.ID, c.Name = "abc123", "/af-playtest-20990101-future-name"
	c.Config.Labels = map[string]string{"af.harness": "testbox"}
	c.State.StartedAt = started.Format(time.RFC3339Nano)
	return c
}

func TestPlaytestSandboxAgePredicate(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name   string
		modify func(*playtestContainer)
		stale  bool
		bad    bool
	}{
		{"deadline", func(*playtestContainer) {}, true, false},
		{"young", func(c *playtestContainer) { c.State.StartedAt = now.Add(-time.Minute).Format(time.RFC3339Nano) }, false, false},
		{"future", func(c *playtestContainer) { c.State.StartedAt = now.Add(time.Hour).Format(time.RFC3339Nano) }, false, false},
		{"unlabelled", func(c *playtestContainer) { c.Config.Labels = nil }, false, false},
		{"other harness", func(c *playtestContainer) { c.Config.Labels["af.harness"] = "other" }, false, false},
		{"other name", func(c *playtestContainer) { c.Name = "/unrelated-af-playtest-old" }, false, false},
		{"invalid timestamp", func(c *playtestContainer) { c.State.StartedAt = "bad" }, false, true},
		{"never started", func(c *playtestContainer) { c.State.StartedAt = time.Time{}.Format(time.RFC3339Nano) }, false, true},
		{"longer lifetime", func(c *playtestContainer) { c.Config.Env = []string{"AF_PLAYTEST_MAX_LIFETIME=86400"} }, false, false},
		{"short lifetime", func(c *playtestContainer) { c.Config.Env = []string{"AF_PLAYTEST_MAX_LIFETIME=60"} }, true, false},
		{"empty default", func(c *playtestContainer) { c.Config.Env = []string{"AF_PLAYTEST_MAX_LIFETIME="} }, true, false},
		{"latest wins", func(c *playtestContainer) {
			c.Config.Env = []string{"AF_PLAYTEST_MAX_LIFETIME=60", "AF_PLAYTEST_MAX_LIFETIME=86400"}
		}, false, false},
		{"zero", func(c *playtestContainer) { c.Config.Env = []string{"AF_PLAYTEST_MAX_LIFETIME=0"} }, false, true},
		{"leading zero", func(c *playtestContainer) { c.Config.Env = []string{"AF_PLAYTEST_MAX_LIFETIME=060"} }, false, true},
		{"overflow", func(c *playtestContainer) { c.Config.Env = []string{"AF_PLAYTEST_MAX_LIFETIME=2147483648"} }, false, true},
		{"negative", func(c *playtestContainer) { c.Config.Env = []string{"AF_PLAYTEST_MAX_LIFETIME=-1"} }, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := sandboxFixture(now.Add(-defaultPlaytestMaxLifetime))
			tt.modify(&c)
			age, limit, err := c.stranded(now)
			require.Equal(t, tt.bad, err != nil)
			require.Equal(t, tt.stale, err == nil && limit > 0 && age >= limit)
		})
	}
}

func TestDoctorStrandedSandboxesReadOnly(t *testing.T) {
	now := time.Now()
	old := sandboxFixture(now.Add(-7 * time.Hour))
	young := sandboxFixture(now.Add(-time.Minute))
	unlabelled := old
	unlabelled.Config.Labels = nil
	original := playtestDockerOutput
	t.Cleanup(func() { playtestDockerOutput = original })
	calls := 0
	playtestDockerOutput = func(ctx context.Context, args ...string) ([]byte, error) {
		_, bounded := ctx.Deadline()
		require.True(t, bounded)
		calls++
		switch calls {
		case 1:
			require.Equal(t, []string{"ps", "-aq", "--filter", "label=af.harness=testbox", "--filter", "name=af-playtest-"}, args)
			return []byte("abc123\ndef456\nghi789\n"), nil
		case 2:
			require.Equal(t, []string{"inspect", "abc123", "def456", "ghi789"}, args)
			return json.Marshal([]playtestContainer{old, young, unlabelled})
		default:
			t.Fatal("doctor must never mutate containers")
			return nil, nil
		}
	}
	report := &Report{}
	checkStrandedPlaytestSandboxes(&scanContext{opts: Options{Fix: true}, snapAt: now}, report)
	findings := findByCheck(report, "stranded-playtest-sandbox")
	require.Len(t, findings, 1)
	require.Contains(t, findings[0].Detail, "abc123")
	require.Contains(t, findings[0].Detail, "7h0m0s")
	require.Nil(t, findings[0].fix)
	require.Empty(t, findings[0].FixAction)
	require.Equal(t, sectionProcesses, findings[0].Section)
}

func TestDoctorSandboxInspectionFailures(t *testing.T) {
	for _, tt := range []struct {
		name       string
		out        string
		err        error
		incomplete bool
	}{
		{"missing docker", "", exec.ErrNotFound, false},
		{"unavailable docker", "", errors.New("permission denied"), true},
		{"empty list", "", nil, false},
		{"malformed inspect", "abc123", nil, true},
		{"missing inspect entries", "null", nil, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			original := playtestDockerOutput
			t.Cleanup(func() { playtestDockerOutput = original })
			playtestDockerOutput = func(context.Context, ...string) ([]byte, error) { return []byte(tt.out), tt.err }
			report := &Report{}
			checkStrandedPlaytestSandboxes(&scanContext{snapAt: time.Now()}, report)
			require.Equal(t, tt.incomplete, len(report.Incomplete) > 0)
			require.Empty(t, report.Findings)
			require.Zero(t, report.UnresolvedCount())
		})
	}
}

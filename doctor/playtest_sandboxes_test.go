package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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
	original := playtestEngineOutput
	t.Cleanup(func() { playtestEngineOutput = original })
	calls := 0
	playtestEngineOutput = func(ctx context.Context, args ...string) ([]byte, error) {
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
	checkStrandedPlaytestSandboxes(report)
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
			original := playtestEngineOutput
			t.Cleanup(func() { playtestEngineOutput = original })
			playtestEngineOutput = func(context.Context, ...string) ([]byte, error) { return []byte(tt.out), tt.err }
			report := &Report{}
			checkStrandedPlaytestSandboxes(report)
			require.Equal(t, tt.incomplete, len(report.Incomplete) > 0)
			require.Empty(t, report.Findings)
			require.Zero(t, report.UnresolvedCount())
		})
	}
}

func TestDoctorSandboxWithoutProcessSnapshot(t *testing.T) {
	original := playtestEngineOutput
	t.Cleanup(func() { playtestEngineOutput = original })
	playtestEngineOutput = func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "ps" {
			return []byte("abc123\n"), nil
		}
		return json.Marshal([]playtestContainer{sandboxFixture(time.Now().Add(-7 * time.Hour))})
	}
	report := &Report{}
	// A failed process scan leaves snapAt zero, but container inspection is
	// independent and must still diagnose expired sandboxes.
	checkStrandedPlaytestSandboxes(report)
	require.Len(t, findByCheck(report, "stranded-playtest-sandbox"), 1)
}

func TestDoctorSandboxContainerEngineSelection(t *testing.T) {
	for _, tt := range []struct {
		name   string
		docker bool
		podman bool
		want   string
	}{
		{"docker preferred", true, true, "docker"},
		{"podman fallback", false, true, "podman"},
		{"neither installed", false, false, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			bin := t.TempDir()
			t.Setenv("PATH", bin)
			for engine, present := range map[string]bool{"docker": tt.docker, "podman": tt.podman} {
				if present {
					require.NoError(t, os.WriteFile(filepath.Join(bin, engine), []byte("#!/bin/sh\nprintf '%s' '"+engine+"'\n"), 0o700))
				}
			}
			out, err := defaultPlaytestEngineOutput(context.Background(), "ps", "-aq")
			if tt.want == "" {
				require.ErrorIs(t, err, exec.ErrNotFound)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.want, string(out))
			}
		})
	}
}

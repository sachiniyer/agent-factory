package doctor

import (
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// startEmptyEnvChild starts a process whose environment reads back empty, the
// same bytes a darwin kernel returns when it withholds one. Linux produces it
// honestly through `env -i`; the platform attribution is then staged, since
// only a SIP-enabled Mac can produce it for real (#3584).
func startEmptyEnvChild(t *testing.T) proctree.Process {
	t.Helper()
	c := exec.Command("env", "-i", "sleep", "60")
	require.NoError(t, c.Start())
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	pid := c.Process.Pid
	// Until `env` execs sleep, the pid still carries this test's environment.
	require.Eventually(t, func() bool {
		_, st := proctree.LookupEnv(pid, tmux.EnvMarkerSession)
		return st == proctree.EnvUnknown
	}, 5*time.Second, 10*time.Millisecond, "the child never reached an empty environment")
	p, err := proctree.Lookup(pid)
	require.NoError(t, err)
	return p
}

func stageWithheldEnvCause(t *testing.T, f func(int) (string, bool)) {
	t.Helper()
	orig := withheldEnvCause
	withheldEnvCause = f
	t.Cleanup(func() { withheldEnvCause = orig })
}

func attributionRows(report *Report) []CheckResult {
	var rows []CheckResult
	for _, c := range report.Checks {
		if c.Name == "process-attribution" {
			rows = append(rows, c)
		}
	}
	return rows
}

// TestOrphanScanStatesAWithheldEnvironmentOnce is #3584's doctor half: a
// process whose markers the kernel withheld used to vanish from every count
// with nothing said. Doctor must now state the limitation, as one warning that
// names the count and the cause and does not count toward the exit code.
func TestOrphanScanStatesAWithheldEnvironmentOnce(t *testing.T) {
	p := startEmptyEnvChild(t)
	stageWithheldEnvCause(t, func(pid int) (string, bool) {
		if pid == p.PID {
			return "staged cause", true
		}
		return "", false
	})
	ctx := &scanContext{snap: map[int]proctree.Process{p.PID: p}, tmuxScanned: true}
	report := &Report{}

	checkOrphanedProcesses(ctx, report)

	rows := attributionRows(report)
	require.Len(t, rows, 1, "a withheld environment must be reported, once")
	require.Equal(t, StatusWarn, rows[0].Status)
	require.False(t, rows[0].Problem, "a limit of the host is not a problem the user can fix")
	require.Contains(t, rows[0].Detail, "1 process because staged cause")
	require.Empty(t, report.Findings, "an unattributable process is never a finding")
}

// TestOrphanScanStaysSilentOnAnUnattributedEmptyEnvironment is the control:
// the same empty read with no platform cause (a genuine `env -i`, Linux,
// a SIP-off Mac) adds no row, so the warning cannot fire where it is untrue.
func TestOrphanScanStaysSilentOnAnUnattributedEmptyEnvironment(t *testing.T) {
	p := startEmptyEnvChild(t)
	ctx := &scanContext{snap: map[int]proctree.Process{p.PID: p}, tmuxScanned: true}
	report := &Report{}

	checkOrphanedProcesses(ctx, report)

	require.Empty(t, attributionRows(report))
}

package doctor

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// These tests pin the one property that made #1939 dangerous rather than
// merely broken: a doctor that cannot see must SAY so.
//
// The bug was never that macOS lacked /proc. It was that proctree returned an
// error, Run threw the error away, the process checks saw a nil snapshot and
// returned early, and the report came out clean. `af doctor` on a Mac answered
// "no orphaned processes" without ever having read the process table. The
// checks below fail against that behaviour: they assert the report is a FAIL
// with a nonzero exit, on the exact code path (snapshot returns an error) that
// every macOS run took.
//
// A real darwin backend (#1939) means this state should no longer occur on the
// platforms we ship. These tests still matter, because the honesty is what
// keeps the NEXT unreadable platform — or an unreadable table on a locked-down
// machine — from being reported as health.

// blindOptions returns Options whose snapshot fails with err, reproducing what
// every `af doctor` run on macOS did before #1939.
func blindOptions(t *testing.T, err error) Options {
	t.Helper()
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	return Options{
		ConfigDir:      home,
		TempDir:        t.TempDir(),
		Exec:           cmd.MakeExecutor(),
		MinTempHomeAge: time.Hour,
		killGrace:      100 * time.Millisecond,
		killTermWait:   200 * time.Millisecond,
		snapshot:       func() (map[int]proctree.Process, error) { return nil, err },
		remoteConfig:   func() (*config.RemoteHooks, string, error) { return nil, "", nil },
	}
}

// TestUnreadableProcessTableFailsRatherThanPasses is the #1939 regression: an
// unreadable process table must produce a FAIL row that counts toward the exit
// code — never a silent omission that reads as a clean bill of health.
func TestUnreadableProcessTableFailsRatherThanPasses(t *testing.T) {
	r, err := Run(blindOptions(t, errors.New("reading /proc: open /proc: no such file or directory")))
	require.NoError(t, err)

	rows := findCheckRows(r, "process-inspection")
	require.Len(t, rows, 1, "an unreadable process table must report exactly one process-inspection row")
	require.Equal(t, StatusFail, rows[0].Status,
		"doctor reported %s for a process table it could not read — blindness must never render as health (#1939)",
		rows[0].Status)
	require.True(t, rows[0].Problem,
		"the process-inspection failure must count toward the exit code, or `af doctor` still exits 0 while blind")
	require.NotZero(t, r.UnresolvedCount(),
		"a doctor that cannot see the process table has an unresolved issue")

	// The row must name the cause, not just the symptom: the operator needs to
	// know WHY it could not look.
	require.Contains(t, rows[0].Detail, "/proc",
		"the failure must name the underlying error so the cause is diagnosable")
}

// TestUnsupportedPlatformIsNamedInTheReport pins the message for a platform
// with no process-table backend at all (proctree_other.go). The operator must
// learn that the checks did not run, rather than inferring health from their
// absence.
func TestUnsupportedPlatformIsNamedInTheReport(t *testing.T) {
	r, err := Run(blindOptions(t, proctree.ErrUnsupportedPlatform))
	require.NoError(t, err)

	rows := findCheckRows(r, "process-inspection")
	require.Len(t, rows, 1)
	require.Equal(t, StatusFail, rows[0].Status)
	require.Contains(t, rows[0].Remediation, "no process-table backend",
		"an unsupported platform must be named as such, not reported as a generic read error")
}

// TestBlindDoctorSaysSoInRenderedOutput checks what the user actually reads.
// The report struct being correct is not enough — the rendered page is the
// product, and this is the line that stops a Mac user concluding "af doctor
// says I am fine".
func TestBlindDoctorSaysSoInRenderedOutput(t *testing.T) {
	r, err := Run(blindOptions(t, errors.New("open /proc: no such file or directory")))
	require.NoError(t, err)

	var buf bytes.Buffer
	Render(&buf, r, false, false)
	out := buf.String()

	require.Contains(t, out, "process-inspection")
	require.Contains(t, out, "cannot read the process table")
	require.Contains(t, strings.ToUpper(out), "FAIL",
		"the rendered report must show a FAIL for an unreadable process table")
}

// TestReadableProcessTablePasses is the other half of the contract: when the
// table IS readable the row passes, so the FAIL above is a real signal rather
// than a row that is always red.
func TestReadableProcessTablePasses(t *testing.T) {
	opts := blindOptions(t, nil)
	opts.snapshot = func() (map[int]proctree.Process, error) {
		return map[int]proctree.Process{1: {PID: 1, PPID: 0, Comm: "init"}}, nil
	}
	r, err := Run(opts)
	require.NoError(t, err)

	rows := findCheckRows(r, "process-inspection")
	require.Len(t, rows, 1)
	require.Equal(t, StatusPass, rows[0].Status,
		"a readable process table must pass; got %s (%s)", rows[0].Status, rows[0].Detail)
}

// TestRealSnapshotIsReadableOnThisPlatform is the end-to-end assertion that
// this platform has a working backend — the check that would have caught #1939
// on day one had it existed, and the reason it is not skipped anywhere.
//
// It runs on every OS we ship. On darwin it exercises the sysctl backend; on
// Linux, /proc. A platform where this fails is a platform where af doctor,
// tmux orphan reaping and `af reset` are all blind, and CI should say so.
func TestRealSnapshotIsReadableOnThisPlatform(t *testing.T) {
	snap, err := proctree.Snapshot()
	require.NoError(t, err, "proctree cannot read this platform's process table — doctor and orphan reaping are blind here (#1939)")
	require.NotEmpty(t, snap, "the process table cannot be empty on a running system")
	require.Contains(t, snap, 1, "pid 1 must be present in any real process table")
}

package session

import (
	"errors"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	aflog "github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/log/logtest"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

// #5138: "af asked" is decided from af's own state — the generation mark
// close() lands, and the instance's teardownExpected predicate for the window
// before it — never from timing or the error text. These tests pin both the
// predicate's truth table and the end-to-end property at the teardown core:
// a monitored session torn down through the kill/archive path logs no ERROR,
// while a session killed out from under af still does.

// goneExpectTmux is a hermetic tmux for instance-level teardown tests: every
// verb answers from the alive flag and nothing execs. Empty answers elsewhere
// leave the monitor unbound — enough for the gone-classification branch.
type goneExpectTmux struct {
	alive atomic.Bool
}

func (m *goneExpectTmux) run(c *exec.Cmd) ([]byte, error) {
	args := strings.Join(c.Args, " ")
	switch {
	case strings.Contains(args, "has-session"):
		if m.alive.Load() {
			return nil, nil
		}
		return nil, errors.New("can't find session")
	case strings.Contains(args, "kill-session"):
		m.alive.Store(false)
		return nil, nil
	case strings.Contains(args, "capture-pane"):
		if m.alive.Load() {
			return []byte("pane output"), nil
		}
		return nil, errors.New("exit status 1")
	default:
		// display-message probes, list-panes, set-option, show-options:
		// empty — an unbound monitor and no pane PIDs to reap.
		return nil, nil
	}
}

// newGoneExpectInstance returns a started instance whose agent tab polls a
// live, monitored tmux session — the shape refreshInstanceStatus holds before
// af tears the session down.
func newGoneExpectInstance(t *testing.T) (*Instance, *tmux.TmuxSession, *goneExpectTmux) {
	t.Helper()
	m := &goneExpectTmux{}
	m.alive.Store(true)
	ts := tmux.NewTmuxSessionFromSanitizedNameWithDeps(
		"af_gone_expect_test", "claude", nil, cmd_test.MockCmdExec{
			RunFunc:    func(c *exec.Cmd) error { _, err := m.run(c); return err },
			OutputFunc: m.run,
		})
	require.NoError(t, ts.Restore(t.TempDir()), "the reattach installs the status monitor")
	inst := instanceWithTmuxTab(t, ts)
	inst.SetStartedForTest(true)
	return inst, ts, m
}

func captureGoneLogs(t *testing.T) (infos, errs *logtest.Buffer) {
	t.Helper()
	infos, errs = &logtest.Buffer{}, &logtest.Buffer{}
	prevInfo, prevErr := aflog.InfoLog.Writer(), aflog.ErrorLog.Writer()
	aflog.InfoLog.SetOutput(infos)
	aflog.ErrorLog.SetOutput(errs)
	t.Cleanup(func() {
		aflog.InfoLog.SetOutput(prevInfo)
		aflog.ErrorLog.SetOutput(prevErr)
	})
	return infos, errs
}

func TestTeardownExpectedFromInstanceState(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Instance)
		want   bool
	}{
		{name: "live session, nothing in flight", mutate: func(*Instance) {}, want: false},
		// Cleared started alone is NOT teardown evidence: teardownReleasePTY
		// and MarkStartupStateUnknown clear it while retaining the runtime —
		// a vanish there is the unexpected loss ERROR exists for.
		{name: "started cleared without teardown", mutate: func(i *Instance) { i.started = false }, want: false},
		{name: "startup unknown with cleared started", mutate: func(i *Instance) {
			i.started = false
			i.startupStateUnknown = true
		}, want: false},
		{name: "kill tombstone", mutate: func(i *Instance) { i.userKilled = true }, want: true},
		{name: "OpKilling", mutate: func(i *Instance) { i.SetInFlightOpForTest(OpKilling) }, want: true},
		{name: "OpArchiving", mutate: func(i *Instance) { i.SetInFlightOpForTest(OpArchiving) }, want: true},
		// A filed on_complete is NOT teardown evidence: it is recorded before
		// teardown begins and the session is still meant to be running until
		// the discharge, which itself raises the kill/archive fence.
		{name: "owed on_complete filed, not discharged", mutate: func(i *Instance) { i.owedOnComplete = &PendingOnCompleteData{} }, want: false},
		// Deliberately not teardown state: OpCreating is af building the
		// session — a vanish there is the anomaly ERROR exists for — and the
		// replacement ops attribute through their own close()'s mark.
		{name: "OpCreating is not a teardown", mutate: func(i *Instance) { i.SetInFlightOpForTest(OpCreating) }, want: false},
		{name: "OpRestoring is not a teardown", mutate: func(i *Instance) { i.SetInFlightOpForTest(OpRestoring) }, want: false},
		{name: "OpRespawning is not a teardown", mutate: func(i *Instance) { i.SetInFlightOpForTest(OpRespawning) }, want: false},
		{name: "OpReplacing is not a teardown", mutate: func(i *Instance) { i.SetInFlightOpForTest(OpReplacing) }, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inst := &Instance{}
			inst.SetStartedForTest(true)
			tc.mutate(inst)
			require.Equal(t, tc.want, inst.teardownExpected())
		})
	}
}

// TestKillTeardownMonitorGoesSilentAtInfo is the issue's kill half end to
// end: a monitored session torn down through the real teardown core —
// closeTab → close() → mark → kill-session — must not log ERROR when a poll
// subsequently finds it gone.
func TestKillTeardownMonitorGoesSilentAtInfo(t *testing.T) {
	inst, ts, _ := newGoneExpectInstance(t)

	require.NoError(t, (&LocalBackend{}).Kill(inst, false))

	// The poll an in-flight monitor still runs after the close: af's own
	// state explains the disappearance — INFO at most, never ERROR.
	infos, errs := captureGoneLogs(t)
	ts.HasUpdatedExpectingTeardown(inst.teardownExpected)
	require.Contains(t, infos.String(), "going silent")
	require.NotContains(t, errs.String(), "going silent")
}

// TestArchiveTeardownMonitorGoesSilentAtInfo is the issue's archive half:
// archive keeps started=true while the OpArchiving fence owns the teardown
// window, so a poll that still observes the close from inside the fence is
// demoted by the instance predicate — af's own op state — exactly as a
// SnapshotAgent call in flight when BeginArchive landed would be.
func TestArchiveTeardownMonitorGoesSilentAtInfo(t *testing.T) {
	inst, ts, _ := newGoneExpectInstance(t)
	inst.SetInFlightOpForTest(OpArchiving)

	// nil worktree: the pane teardown — including the mark — already ran by
	// the time handleWorktree reports there is nothing to relocate.
	err := inst.teardownTabs(teardownArchive{dest: t.TempDir()})
	require.Error(t, err)

	infos, errs := captureGoneLogs(t)
	ts.HasUpdatedExpectingTeardown(inst.teardownExpected)
	require.Contains(t, infos.String(), "going silent",
		"af's close() ran; the monitor's silence is the expected end of that teardown")
	require.NotContains(t, errs.String(), "going silent")
}

// TestExternalTmuxDeathMonitorGoesSilentAtError is the property's other
// half: a session whose tmux is killed out from under af — no mark, no
// tombstone, no teardown op — still reports at ERROR.
func TestExternalTmuxDeathMonitorGoesSilentAtError(t *testing.T) {
	inst, ts, m := newGoneExpectInstance(t)

	m.alive.Store(false) // killed externally — nothing in af's state explains it

	infos, errs := captureGoneLogs(t)
	ts.HasUpdatedExpectingTeardown(inst.teardownExpected)
	require.Contains(t, errs.String(), "going silent",
		"a vanish af's own state does not explain is the ERROR the level exists for")
	require.NotContains(t, infos.String(), "going silent")
}

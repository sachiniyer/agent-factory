package tmux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// #4506 review: a process tab runs under remain-on-exit, so a finished command
// leaves a HELD DEAD PANE. tmux keeps reporting that pane's pane_pid, which is
// the pid that already exited and was reaped. Measured on tmux 3.4 with
// `sh -c 'echo hello; exit 7'`:
//
//	list-panes -F '#{pane_pid} #{pane_dead}'  ->  "<pid> 1", and <pid> is not in the process table
//
// The teardown read that pid as a pane process that "disappeared before its
// descendants could be captured" and refused, so archive, kill and the
// account-scope stop all failed on a finished process tab.

// startHeldPane starts a real session on the test's private server with
// remain-on-exit set in the same invocation (as Start does), and waits until
// tmux reports the pane dead. The pane carries the launch markers a real af
// pane gets (#1104): they are the only provenance left for a pane whose pid
// tmux ≥ 3.8 no longer reports, and the detached-survivor test below relies on
// them exactly the way production relies on them. They go through the same
// capability gate production uses — new-session -e needs tmux ≥ 3.2, which
// this fixture still has to run on.
func startHeldPane(t *testing.T, name, command string) {
	t.Helper()
	args := []string{"new-session", "-d", "-s", name}
	args = append(args, sessionEnvFlags(name, t.Name())...)
	args = append(args, command, ";", "set-option", "-w", "-t", exactTarget(name), "remain-on-exit", "on")
	require.NoError(t, exec.Command("tmux", args...).Run())
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", exactTarget(name)).Run() })
	require.Eventually(t, func() bool {
		out, err := exec.Command("tmux", "display-message", "-p", "-t", exactTarget(name), "#{pane_dead}").Output()
		return err == nil && strings.TrimSpace(string(out)) == "1"
	}, 5*time.Second, 20*time.Millisecond, "the pane never died")
}

// probeHeldExit probes a held dead pane once and requires its exit status. One
// probe is the contract: ProbePaneExit waits for tmux to collect the root and
// nudges a tmux that lost the root's SIGCHLD (#4682), so a status missing here
// is a product failure, not a slow runner. The failure says which of the two
// states tmux was left in.
func probeHeldExit(t *testing.T, s *TmuxSession, name string) (status int, at time.Time) {
	t.Helper()
	dead, status, statusKnown, at, known := s.ProbePaneExit()
	require.True(t, known)
	require.True(t, dead)
	if !statusKnown {
		out, _ := exec.Command("tmux", "display-message", "-p", "-t", exactTarget(name),
			"#{pane_dead_status}|#{pane_pid}").Output()
		fields := strings.Split(strings.TrimSpace(string(out)), "|")
		state := "unreadable"
		if pid, err := strconv.Atoi(fields[len(fields)-1]); err == nil {
			_, lookupErr := proctree.Lookup(pid)
			switch {
			case errors.Is(lookupErr, proctree.ErrProcessExited):
				state = "still a zombie: tmux never collected it"
			case pidGone(pid):
				state = "gone"
			default:
				state = "running"
			}
		}
		t.Fatalf("no exit status after one probe (tmux now reports %q; root %s)", out, state)
	}
	return status, at
}

func requireSessionGone(t *testing.T, name string) {
	t.Helper()
	require.Error(t, exec.Command("tmux", "has-session", "-t", exactTarget(name)).Run(),
		"the held dead pane's session must be gone after teardown")
}

func TestCloseAndWaitForPaneExit_TearsDownAHeldDeadPane(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)
	name := fmt.Sprintf("af_test_dead_pane_%d", time.Now().UnixNano())
	before := time.Now().Truncate(time.Second)
	startHeldPane(t, name, "sh -c 'echo hello; exit 7'")

	s := NewTmuxSessionFromSanitizedName(name, "sh")
	status, at := probeHeldExit(t, s, name)
	require.Equal(t, 7, status)
	require.False(t, at.Before(before), "death time %v predates the pane", at)

	state, err := s.CloseAndWaitForPaneExit()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state,
		"a pane whose command already exited has nothing left to wait for")
	requireSessionGone(t, name)
	require.True(t, s.ClosedConclusively(), "the observed close must latch its proof")
}

// #4682, on real tmux: Ubuntu's tmux 3.4 leaves about one in sixteen of these
// roots uncollected (25 of 400 measured), so a single pane passes by luck. Many
// panes on one server make the lost SIGCHLD all but certain to occur, and every
// probe must still recover its status. On a tmux that loses nothing (3.6+, or a
// build without utempter) this passes trivially.
//
// An anchor session holds the server for the whole loop. Without it, killing
// each pane's session left the server with none, so it exited (exit-empty) and
// every pane got a fresh server; the next new-session could also reach a server
// already on its way out and fail with "server exited unexpectedly" (#4217).
func TestProbePaneExitRecoversTheStatusOfEveryHeldExit(t *testing.T) {
	testguard.IsolateTmux(t)
	const anchor = "af_test_held_exit_anchor"
	require.NoError(t, exec.Command("tmux", "new-session", "-d", "-s", anchor, "sleep 3600").Run())
	serverPID := func() string {
		out, err := exec.Command("tmux", "display-message", "-p", "-t", exactTarget(anchor), "#{pid}").Output()
		require.NoError(t, err)
		return strings.TrimSpace(string(out))
	}
	server := serverPID()
	const panes = 60
	for i := range panes {
		name := fmt.Sprintf("af_test_held_exit_%d_%d", time.Now().UnixNano(), i)
		startHeldPane(t, name, "sh -c 'echo hello; exit 7'")
		status, _ := probeHeldExit(t, NewTmuxSessionFromSanitizedName(name, "sh"), name)
		require.Equal(t, 7, status, "pane %d", i)
		require.NoError(t, exec.Command("tmux", "kill-session", "-t", exactTarget(name)).Run())
	}
	require.Equal(t, server, serverPID(), "every pane must be probed on the same tmux server")
}

// The root exited, but a child it backgrounded is still running: it left the
// pane's ppid tree when its parent died, and only its kernel session still ties
// it to the pane. The teardown must find it through that session and stop it.
func TestCloseAndWaitForPaneExit_HeldDeadPaneReapsItsDetachedSurvivor(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)
	name := fmt.Sprintf("af_test_dead_pane_bg_%d", time.Now().UnixNano())
	dir := t.TempDir()
	childFile := filepath.Join(dir, "child.pid")
	// The child writes its pid only after it ignores SIGHUP, and the root exits
	// only after that: the root's exit hangs up the terminal, and a child still
	// starting would die of it and leave nothing to reap.
	childScript := filepath.Join(dir, "child.sh")
	require.NoError(t, os.WriteFile(childScript, []byte(
		"trap '' HUP\necho $$ > \"$1\"\nexec sleep 300 </dev/null >/dev/null 2>&1\n"), 0o600))
	startHeldPane(t, name, fmt.Sprintf(
		`sh -c 'sh %[1]s %[2]s & while [ ! -s %[2]s ]; do sleep 0.02; done; exit 3'`,
		strconv.Quote(childScript), strconv.Quote(childFile)))

	childPID := readPIDFile(t, childFile)
	child := processIdentity(t, childPID)
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })
	require.NotEqual(t, child.SID, childPID, "the fixture must keep the survivor in the dead root's session")

	s := NewTmuxSessionFromSanitizedName(name, "sh")
	state, err := s.CloseAndWaitForPaneExit()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state)
	requireSessionGone(t, name)
	require.False(t, proctree.AliveSame(child),
		"a detached child of a finished command survived the teardown")
}

// pane_dead is not proof the command exited. A root that closes its terminal
// makes tmux mark the pane dead while the process keeps running, and tmux has
// not reaped it, so no status, signal or death time is reported. The teardown
// must still treat that root as a live pane process.
//
// Linux only: that state is how a Linux pty answers the root closing its last
// terminal descriptor. On macOS (tmux 3.7c in CI) the pane is never marked dead
// while the root runs, so there is no such pane to tear down there.
func TestCloseAndWaitForPaneExit_DeadPaneWithARunningRootStopsTheRoot(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("a pane is held dead with its root running only on Linux ptys, not on %s", runtime.GOOS)
	}
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)
	name := fmt.Sprintf("af_test_dead_pane_eof_%d", time.Now().UnixNano())
	// HUP is ignored before the terminal is closed: closing it is what makes tmux
	// hang the pane up.
	startHeldPane(t, name, `sh -c 'trap "" HUP; exec </dev/null >/dev/null 2>&1; exec sleep 300'`)

	out, err := exec.Command("tmux", "display-message", "-p", "-t", exactTarget(name),
		"#{pane_pid}|#{pane_dead_status}#{pane_dead_signal}#{pane_dead_time}").Output()
	require.NoError(t, err)
	fields := strings.Split(strings.TrimSpace(string(out)), "|")
	require.Len(t, fields, 2)
	require.Empty(t, fields[1], "the fixture must hold a dead pane whose root tmux has not reaped")
	rootPID, err := strconv.Atoi(fields[0])
	require.NoError(t, err)
	root := processIdentity(t, rootPID)
	t.Cleanup(func() { _ = syscall.Kill(rootPID, syscall.SIGKILL) })

	s := NewTmuxSessionFromSanitizedName(name, "sh")
	dead, _, _, _, known := s.ProbePaneExit()
	require.True(t, known)
	require.False(t, dead, "a pane whose root is still running has not finished")

	state, err := s.CloseAndWaitForPaneExit()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state)
	require.False(t, proctree.AliveSame(root), "the running root of a dead pane survived the teardown")
}

// heldPaneExec answers every pane query as tmux would for one held dead pane:
// it expands whatever format was asked for, so the fixture tracks the query
// instead of a canned shape. Every command is accepted, including kill-session.
func heldPaneExec(pid int, status, signal, deadTime string, killed *bool) cmd_test.MockCmdExec {
	return paneRowExec(strconv.Itoa(pid), "1", status, signal, deadTime, killed)
}

// paneRowExec answers every pane query by expanding the asked-for format with
// the given row fields: an empty pid is tmux ≥ 3.8's answer for a pane whose
// terminal is gone (#5319), an empty dead flag is tmux's answer for a target
// that names no pane at all. Every command is accepted, including kill-session.
func paneRowExec(pid, dead, status, signal, deadTime string, killed *bool) cmd_test.MockCmdExec {
	expand := strings.NewReplacer(
		"#{pane_pid}", pid,
		"#{pane_dead}", dead,
		"#{pane_dead_status}", status,
		"#{pane_dead_signal}", signal,
		"#{pane_dead_time}", deadTime,
		"#{session_id}", "$1",
	)
	return cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			if strings.Contains(c.String(), "kill-session") && killed != nil {
				*killed = true
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			s := c.String()
			if strings.Contains(s, "list-panes") || strings.Contains(s, "display-message") {
				return []byte(expand.Replace(c.Args[len(c.Args)-1]) + "\n"), nil
			}
			return nil, nil
		},
	}
}

// deadPaneCohortExec answers pane queries for a session holding one LIVE
// marked pane — anchorPID, a real marked process whose captured membership
// seeds the generation cohort the survivor sweep matches on — and one held
// dead pane reporting no pid at all, the tmux ≥ 3.8 #5319 shape.
func deadPaneCohortExec(anchorPID int, status, signal, deadTime string, killed *bool) cmd_test.MockCmdExec {
	rowExpand := strings.NewReplacer(
		"#{pane_pid}", "",
		"#{pane_dead}", "1",
		"#{pane_dead_status}", status,
		"#{pane_dead_signal}", signal,
		"#{pane_dead_time}", deadTime,
		"#{session_id}", "$0",
	)
	return cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			if strings.Contains(c.String(), "kill-session") && killed != nil {
				*killed = true
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			s := c.String()
			switch {
			case strings.Contains(s, "list-panes"):
				return []byte(fmt.Sprintf("%d|0||$0\n|1|%s%s%s|$0\n", anchorPID, status, signal, deadTime)), nil
			case strings.Contains(s, "display-message"):
				return []byte(rowExpand.Replace(c.Args[len(c.Args)-1]) + "\n"), nil
			}
			return nil, nil
		},
	}
}

// markerEnv builds the launch-marker environment a process spawned inside an
// af pane carries.
func markerEnv(name, home, generation string) []string {
	return []string{
		EnvMarkerSession + "=" + name,
		EnvMarkerHome + "=" + home,
		EnvMarkerGeneration + "=" + generation,
	}
}

// The mock form of the defect: a held dead pane reports a pid that is absent
// from the process table. Refusing it made a finished process tab impossible to
// archive or kill.
func TestCloseAndWaitForPaneExit_HeldPaneWithAnAbsentPidIsConclusive(t *testing.T) {
	gone := exitedProcess(t)
	killed := false
	s := newTmuxSession(toTmuxName("dead-pane-absent", ""), "sh", NewMockPtyFactory(t),
		heldPaneExec(gone.PID, "7", "", "1726000000", &killed))

	state, err := s.CloseAndWaitForPaneExit()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state)
	require.True(t, killed, "the dead pane's session must still be killed")
}

// tmux reaped the dead root long ago, and the kernel has since handed its pid to
// an unrelated process. That process is not the pane's: nothing may wait on it,
// and nothing may signal it.
func TestCloseAndWaitForPaneExit_HeldPaneNeverTouchesARecycledPid(t *testing.T) {
	shrinkReapWaits(t)
	stranger := startStranger(t, nil)
	s := newTmuxSession(toTmuxName("dead-pane-recycled", ""), "sh", NewMockPtyFactory(t),
		heldPaneExec(stranger.PID, "0", "", "1726000000", nil))

	start := time.Now()
	state, err := s.CloseAndWaitForPaneExit()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state)
	require.Less(t, time.Since(start), paneExitWait, "the teardown waited on a process that is not the pane's")
	require.True(t, proctree.AliveSame(stranger), "the teardown signalled a process that merely reuses the dead pane's pid")
}

// Without a reported status the root may not be reaped yet, and tmux older than
// 3.3 never reports a signal death either, so a live process behind the pid is
// ambiguous. A launch marker naming ANOTHER session settles it: the pid now
// belongs to a different pane.
func TestCloseAndWaitForPaneExit_UnreapedDeadPaneSkipsAPidMarkedForAnotherSession(t *testing.T) {
	shrinkReapWaits(t)
	stranger := startStranger(t, []string{EnvMarkerSession + "=af_someone_else"})
	s := newTmuxSession(toTmuxName("dead-pane-marked", ""), "sh", NewMockPtyFactory(t),
		heldPaneExec(stranger.PID, "", "", "", nil))

	state, err := s.CloseAndWaitForPaneExit()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state)
	require.True(t, proctree.AliveSame(stranger), "the teardown signalled another session's process")
}

// #5319: tmux ≥ 3.8 reports no pane_pid for a held dead pane — the pane's pty
// is gone — so list-panes returns `|1|<status+signal+time>`. This exact record
// shape is what the macOS CI failure parsed as a pid and refused on. A dead
// pane has no live root to trace, so an empty pid parses instead of erroring.
func TestParsePaneRow_DeadPaneWithNoReportedPid(t *testing.T) {
	row, err := parsePaneRow("|1|71792068687")
	require.NoError(t, err)
	require.Equal(t, 0, row.pid)
	require.True(t, row.dead)
	require.True(t, row.reaped)
}

// The #2962 guard's other half: a pane tmux does NOT report dead may have a
// live root, and a pid it cannot read leaves that root unverifiable — so the
// row must still be a parse error, empty pid or garbage alike. The same holds
// for a dead row carrying a corrupt pid: only the truly empty field is
// absence.
func TestParsePaneRow_LivePaneWithNoReportedPidErrors(t *testing.T) {
	for _, field := range []string{"|0|", "|0|3", "garbage|0|", "garbage|1|7", "content"} {
		_, err := parsePaneRow(field)
		require.Error(t, err, "a non-dead pane row with an unreadable pid must stay an error: %q", field)
	}
}

// The mock form of #5319: tmux ≥ 3.8's held dead pane reports no pid at all.
// The teardown must still be conclusive — the pane is dead, so there is no
// live root whose identity could not be established.
func TestCloseAndWaitForPaneExit_HeldPaneWithNoReportedPidIsConclusive(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	name := toTmuxName("dead-pane-no-pid", "")
	killed := false
	s := newTmuxSession(name, "sh", NewMockPtyFactory(t),
		paneRowExec("", "1", "7", "", "1726000000", &killed))

	state, err := s.CloseAndWaitForPaneExit()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state)
	require.True(t, killed, "the dead pane's session must still be killed")
}

// With no pid there is no kernel session key to look the pane's detached
// survivors up by, so the capture attributes them the way the
// vanished-session sweep does (#5319): the markers af stamps into every pane
// it spawns, placed against the generation cohort the session's live panes
// already establish.
func TestCloseAndWaitForPaneExit_HeldPaneWithNoReportedPidReapsItsMarkedSurvivor(t *testing.T) {
	shrinkReapWaits(t)
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	name := toTmuxName("dead-pane-no-pid-survivor", "")
	anchor := startStranger(t, markerEnv(name, home, "gen1"))
	survivor := startStranger(t, markerEnv(name, home, "gen1"))
	s := newTmuxSession(name, "sh", NewMockPtyFactory(t),
		deadPaneCohortExec(anchor.PID, "3", "", "1726000000", nil))

	state, err := s.CloseAndWaitForPaneExit()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state)
	require.False(t, proctree.AliveSame(survivor),
		"a marked survivor of a pane tmux reports no pid for survived the teardown")
}

// Processes the sweep cannot attribute are not evidence of membership: an
// unmarked one, one from another home, and one from another generation of the
// same name are all left alone.
func TestCloseAndWaitForPaneExit_HeldPaneWithNoReportedPidLeavesUnattributableProcesses(t *testing.T) {
	shrinkReapWaits(t)
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	name := toTmuxName("dead-pane-no-pid-unmarked", "")
	anchor := startStranger(t, markerEnv(name, home, "gen1"))
	unmarked := startStranger(t, nil)
	wrongHome := startStranger(t, []string{
		EnvMarkerSession + "=" + name,
		EnvMarkerHome + "=" + t.TempDir(),
		EnvMarkerGeneration + "=gen1",
	})
	wrongGeneration := startStranger(t, []string{
		EnvMarkerSession + "=" + name,
		EnvMarkerHome + "=" + home,
		EnvMarkerGeneration + "=gen0",
	})
	s := newTmuxSession(name, "sh", NewMockPtyFactory(t),
		deadPaneCohortExec(anchor.PID, "3", "", "1726000000", nil))

	state, err := s.CloseAndWaitForPaneExit()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state)
	require.True(t, proctree.AliveSame(unmarked), "the teardown reaped an unmarked process")
	require.True(t, proctree.AliveSame(wrongHome), "the teardown reaped another home's process")
	require.True(t, proctree.AliveSame(wrongGeneration), "the teardown reaped another generation's process")
}

// A numeric sentinel in the pid field is malformed output, not the empty
// field tmux ≥ 3.8 reports: a dead row reading `0|1|7` or `1|1|7` must still
// fail the capture rather than laundering corruption into a clean scan.
func TestCloseAndWaitForPaneExit_DeadPaneWithSentinelPidRefuses(t *testing.T) {
	for _, pid := range []string{"0", "1", "-1"} {
		s := newTmuxSession(toTmuxName("dead-pane-sentinel-pid", ""), "sh", NewMockPtyFactory(t),
			paneRowExec(pid, "1", "7", "", "1726000000", nil))
		state, err := s.CloseAndWaitForPaneExit()
		require.Error(t, err, "a dead row carrying pid %s must stay a capture error", pid)
		require.Equal(t, PaneStateUnknown, state)
	}
}

// A detached survivor that `env -i`'d itself is invisible to every mechanism
// the sweep stands on: its environment cannot be read for markers, and
// nothing kernel-attributed links it to the session. It is left alone — the
// same blind spot the SID/SID-refresh paths already accept (#5319).
func TestCloseAndWaitForPaneExit_HeldPaneWithNoReportedPidLeavesUninspectableOrphan(t *testing.T) {
	shrinkReapWaits(t)
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	name := toTmuxName("dead-pane-no-pid-envless", "")
	anchor := startStranger(t, markerEnv(name, home, "gen1"))
	stranger := startEnvlessStranger(t)
	s := newTmuxSession(name, "sh", NewMockPtyFactory(t),
		deadPaneCohortExec(anchor.PID, "3", "", "1726000000", nil))

	state, err := s.CloseAndWaitForPaneExit()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state)
	require.True(t, proctree.AliveSame(stranger),
		"a detached process the sweep cannot inspect must be left alone")
}

// The `env -u AF_SESSION` survivor: its environment is readable and it still
// carries the generation stamp, but the discovery scan keys on AF_SESSION —
// without it the process is never a candidate, and no kernel-attributed link
// exists to find it through. It is left alone (#5319).
func TestCloseAndWaitForPaneExit_HeldPaneWithNoReportedPidLeavesStrangerWithoutSessionMarker(t *testing.T) {
	shrinkReapWaits(t)
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	name := toTmuxName("dead-pane-no-pid-partial", "")
	anchor := startStranger(t, markerEnv(name, home, "gen1"))
	stripped := startStranger(t, []string{EnvMarkerGeneration + "=gen1"})
	s := newTmuxSession(name, "sh", NewMockPtyFactory(t),
		deadPaneCohortExec(anchor.PID, "3", "", "1726000000", nil))

	state, err := s.CloseAndWaitForPaneExit()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state)
	require.True(t, proctree.AliveSame(stripped),
		"a process the marker scan cannot key on must be left alone")
}

// A detached child that stripped ALL of its markers is invisible to the
// marker scan, but while its marked parent is a kernel-attributed member the
// ancestry refresh still reaches it — and inside the bounded candidate set an
// unmarked live pid is unprovable rather than harmless, so the capture is
// incomplete and the teardown refuses (#5319, same rule the vanished sweep
// applies to marker-less candidates).
func TestCloseAndWaitForPaneExit_HeldPaneRefusesUnmarkedChildOfMarkedMember(t *testing.T) {
	shrinkReapWaits(t)
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	name := toTmuxName("dead-pane-no-pid-stripped-child", "")
	parent := startMarkedParentWithStrippedChild(t, markerEnv(name, home, "gen1"))
	s := newTmuxSession(name, "sh", NewMockPtyFactory(t),
		deadPaneCohortExec(parent.PID, "3", "", "1726000000", nil))

	state, err := s.CloseAndWaitForPaneExit()
	require.Error(t, err)
	require.Equal(t, PaneStateUnknown, state,
		"an unmarked descendant of an attributed member must make the capture incomplete")
	require.False(t, proctree.AliveSame(parent),
		"the positively identified member must still be reaped alongside the refusal")
}

// af doctor's escaped-process check hunts exactly the processes the teardown
// capture ADDS via launch markers: a marked process that escaped its dead
// pane's tree is the finding, so merging it into the diagnostic result would
// hide it. The verified variant returns tree/SID-verified members only — the
// pid-less dead pane contributes nothing, and errors nothing.
func TestCaptureVerifiedSessionProcessTrees_LeavesMarkerSurvivorUnclaimed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	name := toTmuxName("dead-pane-no-pid-verified", "")
	anchor := startStranger(t, markerEnv(name, home, "gen1"))
	survivor := startStranger(t, markerEnv(name, home, "gen1"))
	exec := deadPaneCohortExec(anchor.PID, "3", "", "1726000000", nil)

	verified, err := CaptureVerifiedSessionProcessTrees(exec, name)
	require.NoError(t, err)
	for _, p := range verified {
		require.NotEqual(t, survivor.PID, p.PID,
			"the verified capture must not claim a marker-attributed survivor as tree-verified")
	}

	merged, err := CaptureSessionProcessTrees(exec, name)
	require.NoError(t, err)
	found := false
	for _, p := range merged {
		if p.PID == survivor.PID {
			found = true
		}
	}
	require.True(t, found, "the teardown capture must still attribute the marked survivor")
}

// A live member that carries no AF_SESSION marker at all is the bounded
// set's unprovable element: the sweep cannot attribute what it cannot
// verify, so the capture is incomplete and the teardown refuses — the same
// rule the vanished sweep applies to captured pids with no marker (#5319).
func TestCloseAndWaitForPaneExit_UnmarkedDeadPaneWithNoReportedPidRefuses(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	name := toTmuxName("dead-pane-no-pid-unmarked-session", "")
	anchor := startStranger(t, nil)
	s := newTmuxSession(name, "sh", NewMockPtyFactory(t),
		deadPaneCohortExec(anchor.PID, "3", "", "1726000000", nil))

	state, err := s.CloseAndWaitForPaneExit()
	require.Error(t, err)
	require.Equal(t, PaneStateUnknown, state,
		"a member carrying no session marker makes the capture incomplete")
}

// A pane tmux does NOT report dead whose pid cannot be read is the opposite
// case: its root may be live and unverifiable, so the #2962 refusal stands.
func TestCloseAndWaitForPaneExit_LivePaneWithNoReportedPidRefuses(t *testing.T) {
	s := newTmuxSession(toTmuxName("live-pane-no-pid", ""), "sh", NewMockPtyFactory(t),
		paneRowExec("", "0", "", "", "", nil))

	state, err := s.CloseAndWaitForPaneExit()
	require.Error(t, err)
	require.Equal(t, PaneStateUnknown, state,
		"a live pane whose pid cannot be read must not authorize worktree cleanup")
}

// startStranger starts a process the teardown under test must leave alone.
//
// It leads its own kernel session, and that is a safety property rather than
// realism. This package's test binary is named tmux.test, so its direct children
// pass the "child of a tmux server" check. A teardown that wrongly adopts the
// stranger as a pane root also reaps the stranger's whole session. Without
// Setsid, that session is the one `go test` runs in, and the unfixed code killed
// the shell that launched the test.
func startStranger(t *testing.T, extraEnv []string) proctree.Process {
	t.Helper()
	c := exec.Command("sleep", "300")
	c.Env = append(os.Environ(), extraEnv...)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	require.NoError(t, c.Start())
	t.Cleanup(func() {
		_ = c.Process.Kill()
		_, _ = c.Process.Wait()
	})
	return processIdentity(t, c.Process.Pid)
}

// startMarkedParentWithStrippedChild starts a marker-carrying process that
// keeps a live child whose environment has every af launch marker stripped —
// the `env -u` survivor shape, except the marked parent stays alive so
// parentage still links the child to the session.
func startMarkedParentWithStrippedChild(t *testing.T, extraEnv []string) proctree.Process {
	t.Helper()
	c := exec.Command("sh", "-c",
		"env -u "+EnvMarkerSession+" -u "+EnvMarkerHome+" -u "+EnvMarkerGeneration+" sleep 300 & wait")
	c.Env = append(os.Environ(), extraEnv...)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	require.NoError(t, c.Start())
	t.Cleanup(func() {
		_ = c.Process.Kill()
		_, _ = c.Process.Wait()
	})
	parent := processIdentity(t, c.Process.Pid)
	// The scan's descendant check needs the stripped child to exist before
	// the process snapshot: `& wait` forks it near-instantly, but assert
	// rather than race it.
	require.Eventually(t, func() bool {
		snap, err := proctree.Snapshot()
		return err == nil && len(proctree.TreeOf(snap, parent.PID)) > 0
	}, 3*time.Second, 20*time.Millisecond, "the marked parent never spawned its stripped child")
	return parent
}

// startEnvlessStranger starts a process with an empty environment — the
// `env -i` shape the marker scan cannot inspect — that still wears the kernel
// session of a leader that has already exited: the detached-survivor shape.
// A process that merely leads its own session is cleared by the scan;
// orphanage inside the session's lifetime is what makes it unprovable.
func startEnvlessStranger(t *testing.T) proctree.Process {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "pid")
	// sh leads a fresh kernel session, spawns the env-less child wearing
	// that session, then exits — leaving the child an orphaned member.
	c := exec.Command("sh", "-c", "env -i sleep 300 & echo $! > "+pidFile)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	require.NoError(t, c.Start())
	require.NoError(t, c.Wait())
	pid := readPIDFile(t, pidFile)
	proc := processIdentity(t, pid)
	t.Cleanup(func() {
		_ = proctree.Signal(proc, syscall.SIGKILL)
	})
	return proc
}

func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	var pid int
	require.Eventually(t, func() bool {
		raw, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(raw)))
		return err == nil && pid > 1
	}, 3*time.Second, 20*time.Millisecond)
	return pid
}

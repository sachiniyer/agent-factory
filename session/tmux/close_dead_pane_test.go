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
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
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
// tmux reports the pane dead. Production writes the pane's identity bundle
// from the pane's own launch shim (the shim IS the pane root); the fixture
// records the same options through the production panePidStampArgs helper so
// the dead row has exactly the shape a shim-stamped pane leaves, deaths
// counter and command pin included.
func startHeldPane(t *testing.T, name, command string) {
	t.Helper()
	args := []string{"new-session", "-d", "-s", name, command,
		";", "set-option", "-w", "-t", exactTarget(name), "remain-on-exit", "on"}
	args = append(append(args, ";"), panePidStampArgs(exactTarget(name))...)
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

// shimStartCommand is what #{pane_start_command} answers for a pane af
// launched through its exec shim. The fixture keeps one around only as the
// recorded command string — freshness no longer reads launch markers out of
// it, it just has to byte-match what the stamp pinned.
const shimStartCommand = "/usr/bin/af " + sessionenv.ExecMarker + " sh 0 'sh -c true'"

// paneState is the part of a dead pane's row the stamp's freshness is judged
// against: the deaths counter the pane-died hook maintains, the pane's live
// pty name, the command the stamp pinned (@af_pane_cmd), and the start
// command tmux records now. stampCmd == startCmd is what a commandless
// respawn — which re-ran the shim — reports.
type paneState struct {
	deaths, tty, stampCmd, startCmd string
}

// freshPane is the state a shim-stamped pane reports after its first death:
// its own exit is the counter's first increment, so the stamp's deaths field
// is exactly one behind.
func freshPane() paneState {
	return paneState{deaths: "1", tty: "/dev/pts/0", stampCmd: shimStartCommand, startCmd: shimStartCommand}
}

func (s paneState) cmdMatch() string {
	if s.stampCmd == s.startCmd {
		return "1"
	}
	return "0"
}

// heldPaneExec answers every pane query as tmux would for one held dead pane:
// it expands whatever format was asked for, so the fixture tracks the query
// instead of a canned shape. Every command is accepted, including kill-session.
func heldPaneExec(pid int, status, signal, deadTime string, killed *bool) cmd_test.MockCmdExec {
	return deadPaneExec(strconv.Itoa(pid), strconv.Itoa(pid)+":0:/dev/pts/0", freshPane(), status, signal, deadTime, killed)
}

// deadPaneExec is heldPaneExec with independent control over the pid fields
// and the pane's incarnation state: panePID is what #{pane_pid} expands to —
// "" models tmux ≥3.8's held dead pane (#5319) — stamp is what the pane's
// @af_pane_pid bundle answers ("" for a pane that was never recorded), and
// state carries the freshness inputs: the deaths counter, the live pty, and
// the stamped/recorded commands the server-side equality reads.
func deadPaneExec(panePID, stamp string, state paneState, status, signal, deadTime string, killed *bool) cmd_test.MockCmdExec {
	expand := strings.NewReplacer(
		"#{pane_pid}", panePID,
		"#{pane_dead}", "1",
		"#{pane_dead_status}", status,
		"#{pane_dead_signal}", signal,
		"#{pane_dead_time}", deadTime,
		"#{"+PanePidMarker+"}", stamp,
		"#{"+paneDeathsMarker+"}", state.deaths,
		"#{pane_tty}", state.tty,
		paneCommandMatchField, state.cmdMatch(),
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

// The #5319 shape on tmux ≥3.8: a held dead pane whose pane_pid is EMPTY,
// carrying only the @af_pane_pid stamp set at spawn. The stamp names the dead
// root, so its kernel session members are attributed exactly as they were
// while pane_pid still reported it — the detached child gets reaped and the
// teardown completes instead of refusing on an empty pid.
func TestCloseAndWaitForPaneExit_StampedDeadPaneReapsItsDetachedSurvivor(t *testing.T) {
	shrinkReapWaits(t)
	leaderPID, child := startDeadLeaderWithSessionChild(t)
	killed := false
	s := newTmuxSession(toTmuxName("dead-pane-stamped", ""), "sh", NewMockPtyFactory(t),
		deadPaneExec("", strconv.Itoa(leaderPID)+":0:/dev/pts/0", freshPane(), "7", "", "1726000000", &killed))

	state, err := s.CloseAndWaitForPaneExit()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state)
	require.True(t, killed, "the stamped dead pane's session must still be killed")
	require.False(t, proctree.AliveSame(child),
		"the dead root's kernel-session survivor must be reaped through the stamped pid")
}

// The respawn-pane trap the stamp must not fall into (#5328 review): pane
// options survive respawn-pane, so a pane respawned into a command af did
// not launch still carries the OLD root's stamp — and the recorded start
// command changes to the replacement, so even argv containing a launch
// marker fails the pinned-command equality. The deaths counter saw the first
// death and the respawn's, leaving the stamped count two behind.
func TestCloseAndWaitForPaneExit_RespawnedDeadPaneDistrustsItsStaleStamp(t *testing.T) {
	shrinkReapWaits(t)
	leaderPID, child := startDeadLeaderWithSessionChild(t)
	killed := false
	stale := paneState{deaths: "2", tty: "/dev/pts/1", stampCmd: shimStartCommand,
		startCmd: `/bin/sh -c 'echo ` + sessionenv.ExecMarker + `; sleep 5'`}
	s := newTmuxSession(toTmuxName("dead-pane-respawned", ""), "sh", NewMockPtyFactory(t),
		deadPaneExec("", strconv.Itoa(leaderPID)+":0:/dev/pts/0", stale, "7", "", "1726000000", &killed))

	state, err := s.CloseAndWaitForPaneExit()
	require.Error(t, err)
	require.Equal(t, PaneStateUnknown, state)
	require.True(t, killed, "kill-session still runs — refusal is about the process tree, not the session")
	require.Contains(t, err.Error(), "respawn",
		"the refusal must say why the recorded stamp cannot be trusted")
	require.True(t, proctree.AliveSame(child),
		"an unattributed survivor must not be reaped on a stale stamp's say-so")
}

// The other respawn shape (#5328 review): a COMMANDLESS respawn re-runs the
// same recorded command, so the shim executes and restamps — but when that
// write silently fails (a respawn environment without tmux, a wedged server),
// the previous incarnation's stamp is what survives. The recorded command is
// unchanged and still af's, so command equality cannot catch it — the deaths
// counter and the fresh pty do: the deaths gap is two and the pty moved.
func TestCloseAndWaitForPaneExit_FailedRestampDistrustsItsStaleStamp(t *testing.T) {
	shrinkReapWaits(t)
	leaderPID, child := startDeadLeaderWithSessionChild(t)
	killed := false
	stale := paneState{deaths: "2", tty: "/dev/pts/1", stampCmd: shimStartCommand, startCmd: shimStartCommand}
	s := newTmuxSession(toTmuxName("dead-pane-restamp-failed", ""), "sh", NewMockPtyFactory(t),
		deadPaneExec("", strconv.Itoa(leaderPID)+":0:/dev/pts/0", stale, "7", "", "1726000000", &killed))

	state, err := s.CloseAndWaitForPaneExit()
	require.Error(t, err)
	require.Equal(t, PaneStateUnknown, state)
	require.True(t, killed, "kill-session still runs — refusal is about the process tree, not the session")
	require.True(t, proctree.AliveSame(child),
		"an unattributed survivor must not be reaped on a stale stamp's say-so")
}

// The positive half of the same respawn: a commandless respawn re-runs af's
// launch shim, which restamps with the NEW root's pid, the deaths count it
// saw at birth, and the fresh pty — the second death then reads as fresh and
// the teardown completes through the new stamp.
func TestCloseAndWaitForPaneExit_RespawnedShimPaneStampStaysFresh(t *testing.T) {
	shrinkReapWaits(t)
	leaderPID, child := startDeadLeaderWithSessionChild(t)
	killed := false
	restamped := paneState{deaths: "2", tty: "/dev/pts/1", stampCmd: shimStartCommand, startCmd: shimStartCommand}
	s := newTmuxSession(toTmuxName("dead-pane-respawned-fresh", ""), "sh", NewMockPtyFactory(t),
		deadPaneExec("", strconv.Itoa(leaderPID)+":1:/dev/pts/1", restamped, "7", "", "1726000000", &killed))

	state, err := s.CloseAndWaitForPaneExit()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state)
	require.True(t, killed)
	require.False(t, proctree.AliveSame(child),
		"a survivor of a shim-respawned pane's dead root must be reaped through the restamped pid")
}

// A held dead pane that was never stamped — created by a pre-#5319 af, or not
// created by af at all — reports nothing usable on tmux ≥3.8: pane_pid is
// empty and @af_pane_pid is unset. Teardown must fail closed exactly as
// master does on any unprovable capture: kill-session still runs, but the
// answer is an error that leaves the workspace intact and says how to finish
// manually. There is no marker-attribution fallback for it in this PR.
func TestCloseAndWaitForPaneExit_UnstampedDeadPaneRefuses(t *testing.T) {
	shrinkReapWaits(t)
	killed := false
	s := newTmuxSession(toTmuxName("dead-pane-unstamped", ""), "sh", NewMockPtyFactory(t),
		deadPaneExec("", "", paneState{}, "7", "", "1726000000", &killed))

	state, err := s.CloseAndWaitForPaneExit()
	require.Error(t, err)
	require.Equal(t, PaneStateUnknown, state)
	require.True(t, killed, "kill-session still runs — refusal is about the process tree, not the session")
	require.Contains(t, err.Error(), PanePidMarker,
		"the refusal must name what the pane lacks")
	require.Contains(t, err.Error(), "af sessions kill",
		"the refusal must name the step that actually finishes cleanup — the recorded kill retries automatically, and a re-run drives it (measured on tmux 3.8: the tombstoned kill converged via the vanished-session sweep once the session was gone)")
}

// pid-bearing rows stay pane_pid's rows: the stamp is a fallback authority
// consulted only when tmux reports none, and only while the stamp's
// incarnation proof holds — pane options outlive respawn-pane, so a stamp on
// a pane respawned or never properly stamped is stale.
func TestParsePaneRowStampedPid(t *testing.T) {
	for _, tc := range []struct {
		field   string
		wantPID int
		dead    bool
		reaped  bool
		wantErr string
	}{
		{"123|0||123:0:/dev/pts/0|0|/dev/pts/0|1", 123, false, false, ""},            // live, stamped, agrees
		{"123|0||", 123, false, false, ""},                                           // live, unstamped — pane_pid rules
		{"|1|71726000000|456:0:/dev/pts/0|1|/dev/pts/0|1", 456, true, true, ""},      // 3.8 dead row: fresh stamp answers
		{"|1|71726000000|456:0:/dev/pts/0|1|/dev/pts/0|0", 0, true, true, "respawn"}, // command pin moved
		{"|1|71726000000|456:0:/dev/pts/0|2|/dev/pts/0|1", 0, true, true, "respawn"}, // deaths gap > 1
		{"|1|71726000000|456:0:/dev/pts/0|1|/dev/pts/9|1", 0, true, true, "respawn"}, // pty moved
		{"|1|71726000000|456:0:/dev/pts/0|0|/dev/pts/0|1", 0, true, true, "respawn"}, // no death counted
		{"|1|71726000000|456|1|/dev/pts/0|1", 0, true, true, "respawn"},              // pre-proof bare-pid stamp
		{"|1|71726000000|456", 0, true, true, "respawn"},                             // no freshness fields at all
		{"123|1|71726000000|456:0:/dev/pts/0|9|/dev/pts/9|0", 123, true, true, ""},   // populated pane_pid wins over any stamp
		{"|1|71726000000|", 0, true, true, "af sessions kill"},
		{"|0||", 0, false, false, "invalid pane pid"},
		{"abc|1|71726000000|", 0, true, true, "invalid pane pid"},
	} {
		row, err := parsePaneRow(tc.field)
		if tc.wantErr != "" {
			require.ErrorContains(t, err, tc.wantErr, tc.field)
			continue
		}
		require.NoError(t, err, tc.field)
		require.Equal(t, tc.wantPID, row.pid, tc.field)
		require.Equal(t, tc.dead, row.dead, tc.field)
		require.Equal(t, tc.reaped, row.reaped, tc.field)
	}
}

// The production stamp on real tmux: a held dead pane still answers
// @af_pane_pid, the bundle's first field is exactly the pane's live root pid —
// equal to pane_pid wherever tmux still reports it — and the freshness inputs
// the dead row must satisfy are all present: one death counted, the pty
// unchanged, the start command pinned.
func TestStartHeldPaneStampsItsRootPid(t *testing.T) {
	testguard.IsolateTmux(t)
	name := fmt.Sprintf("af_test_dead_pane_stamp_%d", time.Now().UnixNano())
	startHeldPane(t, name, "sh -c 'exit 7'")

	out, err := exec.Command("tmux", "display-message", "-p", "-t", exactTarget(name), paneRowFormat).Output()
	require.NoError(t, err)
	fields := strings.Split(strings.TrimSpace(string(out)), "|")
	require.Len(t, fields, 7)
	require.Equal(t, "1", fields[1])
	stampParts := strings.SplitN(fields[3], ":", 3)
	require.Len(t, stampParts, 3, "the record must carry pid:deaths:tty, got %q", fields[3])
	require.NotEmpty(t, stampParts[0], "the spawn chain must record %s", PanePidMarker)
	require.Equal(t, "1", fields[4], "the pane-died hook must have counted this death")
	require.Equal(t, fields[5], stampParts[2], "the stamped tty must be the pane's own pty")
	require.Equal(t, "1", fields[6], "the pinned start command must be the pane's own")
	if fields[0] != "" {
		require.Equal(t, fields[0], stampParts[0],
			"where tmux still reports pane_pid, the stamp must record the same pid")
	}
}

// startDeadLeaderWithSessionChild spawns a kernel-session leader with one
// long-lived member, then kills the leader: the child is orphaned but keeps
// the leader's pid as its SID, which is exactly how a held dead pane's
// backgrounded survivor still names its pane. Returns (leaderPID, child).
func startDeadLeaderWithSessionChild(t *testing.T) (leaderPID int, child proctree.Process) {
	t.Helper()
	childFile := filepath.Join(t.TempDir(), "child.pid")
	cmd := exec.Command("sh", "-c",
		"sleep 300 & echo $! > \"$1\"; kill -9 $$", "sh", childFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	require.NoError(t, cmd.Start())
	_ = cmd.Wait()
	childPID := readPIDFile(t, childFile)
	child = processIdentity(t, childPID)
	require.Equal(t, cmd.Process.Pid, child.SID,
		"the fixture must leave the survivor in the dead leader's session")
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })
	return cmd.Process.Pid, child
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

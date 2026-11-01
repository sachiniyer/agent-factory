package tmux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	aflog "github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/log/logtest"
)

// #4506 review: tmux leaves pane_dead_status EMPTY for a command killed by a
// signal while still filling pane_dead_time. A whitespace split collapsed the
// empty field and read the timestamp as the exit status.
func TestProbePaneExitKeepsAnEmptyStatusUnknown(t *testing.T) {
	shrinkPaneStatusWait(t)
	gone := exitedProcess(t).PID
	uncollected := uncollectedProcess(t)
	for _, tc := range []struct {
		name                      string
		status, signal, deadTime  string
		pid                       int
		wantDead, wantKnownStatus bool
		wantStatus                int
		wantAt                    time.Time
	}{
		{name: "signal-killed", signal: "9", deadTime: "1726000000", pid: gone,
			wantDead: true, wantAt: time.Unix(1726000000, 0)},
		{name: "exited", status: "3", deadTime: "1726000000", pid: gone,
			wantDead: true, wantKnownStatus: true, wantStatus: 3, wantAt: time.Unix(1726000000, 0)},
		{name: "exited zero", status: "0", deadTime: "1726000000", pid: gone,
			wantDead: true, wantKnownStatus: true, wantAt: time.Unix(1726000000, 0)},
		{name: "no death time", status: "5", pid: gone,
			wantDead: true, wantKnownStatus: true, wantStatus: 5},
		// tmux reports nothing about the root: the pane pid decides.
		{name: "unreported, root gone", pid: gone, wantDead: true},
		{name: "unreported, root running", pid: os.Getpid()},
		// tmux has not collected the root yet, and does not within the wait:
		// it has still finished.
		{name: "unreported, root exited but uncollected", pid: uncollected, wantDead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := NewTmuxSessionWithDeps("pane-exit", "true", nil,
				heldPaneExec(t, tc.pid, tc.status, tc.signal, tc.deadTime, nil))
			dead, status, statusKnown, at, known := ts.ProbePaneExit()
			assert.True(t, known)
			assert.Equal(t, tc.wantDead, dead)
			assert.Equal(t, tc.wantKnownStatus, statusKnown)
			assert.Equal(t, tc.wantStatus, status)
			assert.True(t, at.Equal(tc.wantAt), "at = %v, want %v", at, tc.wantAt)
		})
	}
}

// #4682: tmux marks a pane dead when its terminal closes, which can come before
// it collects the root and fills pane_dead_status. A probe in that window must
// wait for the status rather than record a plain exit as unknown: a process
// tab's immediate-failure check and its durable exit record both read it.
func TestProbePaneExitWaitsForTheStatusOfAnUncollectedRoot(t *testing.T) {
	uncollected := uncollectedProcess(t)
	reads := 0
	before := heldPaneExec(t, uncollected, "", "", "", nil)
	after := heldPaneExec(t, uncollected, "7", "", "1726000000", nil)
	ts := NewTmuxSessionWithDeps("pane-exit-pending", "true", nil, cmd_test.MockCmdExec{
		RunFunc: before.RunFunc,
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			reads++
			if reads < 3 {
				return before.OutputFunc(c)
			}
			return after.OutputFunc(c)
		},
	})
	dead, status, statusKnown, at, known := ts.ProbePaneExit()
	require.True(t, known)
	require.True(t, dead)
	require.True(t, statusKnown, "the probe answered before tmux reported the status")
	require.Equal(t, 7, status)
	require.True(t, at.Equal(time.Unix(1726000000, 0)), "at = %v", at)
	require.Equal(t, 3, reads)
}

// #4682: tmux 3.4 built with utempter can lose the root's SIGCHLD, and then it
// collects the root only on its next SIGCHLD. The fixture's server behaves that
// way: it reports the status only once something runs a job through it. The
// probe must make that happen rather than wait out paneStatusWait.
func TestProbePaneExitNudgesTheServerToCollectAStrandedRoot(t *testing.T) {
	uncollected := uncollectedProcess(t)
	nudges := 0
	stranded := heldPaneExec(t, uncollected, "", "", "", nil)
	collected := heldPaneExec(t, uncollected, "7", "", "1726000000", nil)
	ts := NewTmuxSessionWithDeps("pane-exit-stranded", "true", nil, cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			if strings.Contains(c.String(), "run-shell") {
				nudges++
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			if nudges == 0 {
				return stranded.OutputFunc(c)
			}
			return collected.OutputFunc(c)
		},
	})
	dead, status, statusKnown, _, known := ts.ProbePaneExit()
	require.True(t, known)
	require.True(t, dead)
	require.True(t, statusKnown, "the probe never got the server to collect the root")
	require.Equal(t, 7, status)
	require.Equal(t, 1, nudges, "one nudge collects the root; more are noise")
}

// A server that never collects the root, however often it is nudged, gets a
// fixed number of nudges, and the probe still answers within paneStatusWait.
func TestProbePaneExitCapsItsNudges(t *testing.T) {
	shrinkPaneStatusWait(t)
	uncollected := uncollectedProcess(t)
	nudges := 0
	stranded := heldPaneExec(t, uncollected, "", "", "", nil)
	ts := NewTmuxSessionWithDeps("pane-exit-never-collected", "true", nil, cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			if strings.Contains(c.String(), "run-shell") {
				nudges++
			}
			return nil
		},
		OutputFunc: stranded.OutputFunc,
	})
	dead, _, statusKnown, _, known := ts.ProbePaneExit()
	require.True(t, known)
	require.True(t, dead)
	require.False(t, statusKnown)
	require.Equal(t, maxReapNudges, nudges)
}

// #4906 review: the nudge's log line names the version of the SERVER that lost
// the SIGCHLD, read in the probe's own display-message. It never runs the tmux
// client: `tmux -V` reports the installed binary, which after an upgrade is not
// the running server, and it would start a fresh command timeout after the
// probe's wait had already run out.
func TestProbePaneExitLogsTheServersVersionWithoutRunningTheClient(t *testing.T) {
	shrinkPaneStatusWait(t)
	dir := t.TempDir()
	ranClient := filepath.Join(dir, "ran")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tmux"), []byte(
		"#!/bin/sh\ntouch "+ranClient+"\necho 'tmux 3.6'\n"), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var info logtest.Buffer
	previous := aflog.InfoLog.Writer()
	aflog.InfoLog.SetOutput(&info)
	t.Cleanup(func() { aflog.InfoLog.SetOutput(previous) })

	stranded := heldPaneExec(t, uncollectedProcess(t), "", "", "", nil)
	ts := NewTmuxSessionWithDeps("pane-exit-version", "true", nil, cmd_test.MockCmdExec{
		RunFunc: stranded.RunFunc,
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			last := len(c.Args) - 1
			c.Args[last] = strings.ReplaceAll(c.Args[last], "#{version}", "3.4")
			return stranded.OutputFunc(c)
		},
	})
	dead, _, _, _, known := ts.ProbePaneExit()
	require.True(t, known)
	require.True(t, dead)
	require.NoFileExists(t, ranClient, "the probe ran the tmux client for its log line")
	require.Contains(t, info.String(), "tmux 3.4", "the log line must name the server's version")
	require.NotContains(t, info.String(), "3.6")
}

// Once a read has established that the root exited, a re-read that cannot
// answer (tmux failed, the session went away) must not turn the finished command
// back into an unknown one: tab creation and restore would leave it unstamped.
func TestProbePaneExitKeepsTheExitWhenARereadFails(t *testing.T) {
	uncollected := uncollectedProcess(t)
	reads := 0
	before := heldPaneExec(t, uncollected, "", "", "", nil)
	ts := NewTmuxSessionWithDeps("pane-exit-reread-fails", "true", nil, cmd_test.MockCmdExec{
		RunFunc: before.RunFunc,
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			reads++
			if reads == 1 {
				return before.OutputFunc(c)
			}
			return nil, errors.New("no server running")
		},
	})
	dead, _, statusKnown, _, known := ts.ProbePaneExit()
	require.True(t, known, "a failed re-read discarded an exit the first read established")
	require.True(t, dead)
	require.False(t, statusKnown)
	require.Equal(t, 2, reads)
}

// A tmux that stalls while the probe waits for the status must be bounded by
// what is left of paneStatusWait, not by a fresh tmuxCommandTimeout per read.
// The stalled read is a real process under the probe's own context, so only
// that context can end it.
func TestProbePaneExitBoundsAStalledRereadByTheWait(t *testing.T) {
	shrinkPaneStatusWait(t)
	sleepPath, err := exec.LookPath("sleep")
	require.NoError(t, err)
	uncollected := uncollectedProcess(t)
	reads := 0
	before := heldPaneExec(t, uncollected, "", "", "", nil)
	ts := NewTmuxSessionWithDeps("pane-exit-reread-stalls", "true", nil, cmd_test.MockCmdExec{
		RunFunc: before.RunFunc,
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			reads++
			if reads == 1 {
				return before.OutputFunc(c)
			}
			c.Path, c.Args = sleepPath, []string{"sleep", "300"}
			return c.Output()
		},
	})
	type answer struct{ dead, known bool }
	done := make(chan answer, 1)
	go func() {
		dead, _, _, _, known := ts.ProbePaneExit()
		done <- answer{dead, known}
	}()
	select {
	case got := <-done:
		require.Equal(t, answer{dead: true, known: true}, got)
	case <-time.After(tmuxCommandTimeout):
		t.Fatalf("a stalled re-read held the probe for a whole %s command timeout", tmuxCommandTimeout)
	}
}

// #5335: a stamped dead pane whose deaths counter still reads 0 is pending,
// not uncollectible — the queued pane-died hook has not run yet. While it is
// pending the pid checks that prove the root is gone cannot run, so the probe
// must not answer a conclusive dead: when the hook lands the stamp validates
// and the probe completes; when it never lands the answer stays unknown and no
// durable TabExit is stamped for a root that may still be running.
func TestProbePaneExitWaitsOutAPendingStamp(t *testing.T) {
	gone := exitedProcess(t).PID
	fresh := paneState{deaths: "1", tty: "/dev/pts/0", stampCmd: shimStartCommand, startCmd: shimStartCommand}
	stamp := strconv.Itoa(gone) + ":0:/dev/pts/0"
	ts := NewTmuxSessionWithDeps("pane-exit-pending-stamp", "true", nil,
		laggingDeathsExec(t, "", stamp, fresh, "", "", "", nil))
	dead, _, _, _, known := ts.ProbePaneExit()
	require.True(t, known)
	require.True(t, dead)
}

// A pending stamp that resolves to a root still running — pane_dead fired on
// the terminal closing, not on the process ending — must replace the
// provisional dead answer with a live one: known=true, dead=false, and
// promptly rather than after paneStatusWait. An older draft of the pending
// fold kept the provisional reading because the re-read loop only discarded
// live answers while guarding an ESTABLISHED exit; a provisional one is not.
func TestProbePaneExitPendingStampAcceptsALiveRoot(t *testing.T) {
	fresh := paneState{deaths: "1", tty: "/dev/pts/0", stampCmd: shimStartCommand, startCmd: shimStartCommand}
	stamp := strconv.Itoa(os.Getpid()) + ":0:/dev/pts/0"
	ts := NewTmuxSessionWithDeps("pane-exit-pending-live", "true", nil,
		laggingDeathsExec(t, "", stamp, fresh, "", "", "", nil))
	start := time.Now()
	dead, _, _, _, known := ts.ProbePaneExit()
	require.True(t, known)
	require.False(t, dead, "the stamped pid is alive — pane_dead marked the closed terminal, not the root")
	require.Less(t, time.Since(start), paneStatusWait,
		"a resolving live answer must not burn the whole uncollected wait")
}

func TestProbePaneExitPendingStampStaysUnknown(t *testing.T) {
	shrinkPaneStatusWait(t)
	pending := paneState{deaths: "0", tty: "/dev/pts/0", stampCmd: shimStartCommand, startCmd: shimStartCommand}
	ts := NewTmuxSessionWithDeps("pane-exit-pending-forever", "true", nil,
		deadPaneExec(t, "", "456:0:/dev/pts/0", pending, "", "", "", nil))
	_, _, _, _, known := ts.ProbePaneExit()
	require.False(t, known, "a stamp that never becomes provable must not stamp a durable exit")
}

// shrinkPaneStatusWait bounds ProbePaneExit's wait for an uncollected root, so a
// fixture whose root tmux never collects gives up quickly.
func shrinkPaneStatusWait(t *testing.T) {
	t.Helper()
	old := paneStatusWait
	paneStatusWait = 100 * time.Millisecond
	t.Cleanup(func() { paneStatusWait = old })
}

// A live pane is answered live whatever the other fields hold.
func TestProbePaneExitLivePane(t *testing.T) {
	ts := NewTmuxSessionWithDeps("pane-live", "true", nil, cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error { return nil },
		OutputFunc: func(*exec.Cmd) ([]byte, error) {
			return []byte("0||||123\n"), nil
		},
	})
	dead, _, statusKnown, _, known := ts.ProbePaneExit()
	assert.True(t, known)
	assert.False(t, dead)
	assert.False(t, statusKnown)
}

// A pane af did not launch carries no @af_pane_pid stamp. Under remain-on-exit
// the user can split a process tab's window, end up focused on that split pane,
// and exit it; tmux holds the dead split pane as the session's ACTIVE pane and
// reports its collected exit status on the recorded command's target. That
// status is not the command's exit: the active pane never ran the recorded
// command, and stamping it would fabricate an exit a live root never earned —
// one the first-wins guard on Tab.Exit would then make permanent. The probe
// must answer not-dead so the row stays unstamped and the command's real exit
// can still be recorded.
func TestProbePaneExitRefusesAnUnstampedForeignPane(t *testing.T) {
	// A pane the user created by splitting: dead, reaped (status 7), but it
	// carries no @af_pane_pid stamp, no deaths counter, and no @af_pane_cmd
	// pin — its own start command does not byte-match the unset pin.
	foreign := paneState{deaths: "", tty: "/dev/pts/3", stampCmd: "", startCmd: "sh -c 'exit 7'"}
	ts := NewTmuxSessionWithDeps("pane-exit-foreign", "true", nil,
		deadPaneExec(t, "8576", "", foreign, "7", "", "1726000000", nil))
	dead, status, statusKnown, at, known := ts.ProbePaneExit()
	require.True(t, known)
	require.False(t, dead, "an unstamped foreign pane's death is not the recorded command's exit")
	require.False(t, statusKnown)
	require.Zero(t, status)
	require.True(t, at.IsZero(), "no exit time is fabricated for a death not attributed to the command")
}

// A stale stamp — one a respawn carried past the deaths counter and pty it
// pinned — cannot name the pane's current incarnation, so its collected status
// is no more the recorded command's exit than an unstamped pane's is. The probe
// refuses rather than attributing a foreign incarnation's death.
func TestProbePaneExitRefusesAStaleStampedPane(t *testing.T) {
	gone := exitedProcess(t).PID
	// deaths gap of 2 and a moved pty: the stamp names an earlier incarnation,
	// not the pane that most recently died.
	stale := paneState{deaths: "2", tty: "/dev/pts/1", stampCmd: shimStartCommand, startCmd: shimStartCommand}
	ts := NewTmuxSessionWithDeps("pane-exit-stale", "true", nil,
		deadPaneExec(t, strconv.Itoa(gone), strconv.Itoa(gone)+":0:/dev/pts/0", stale, "7", "", "1726000000", nil))
	dead, _, _, _, known := ts.ProbePaneExit()
	require.True(t, known)
	require.False(t, dead, "a stale stamp that cannot name the current incarnation is not the command's exit")
}

// When tmux has collected the status but the @af_pane_deaths counter has not
// caught up yet (#5335 — the pane-died hook runs on the command queue, after
// the status is filled), a freshly-dead af pane reads pending on the
// collected-status path. The probe keeps probing rather than dropping the
// command's own exit, and records it once the hook lands — the collected-status
// analogue of TestProbePaneExitWaitsOutAPendingStamp.
func TestProbePaneExitWaitsForAStampPendingOnACollectedStatus(t *testing.T) {
	gone := exitedProcess(t).PID
	stamp := strconv.Itoa(gone) + ":0:/dev/pts/0"
	ts := NewTmuxSessionWithDeps("pane-exit-collected-pending", "true", nil,
		laggingDeathsExec(t, strconv.Itoa(gone), stamp, freshPane(), "7", "", "1726000000", nil))
	dead, status, statusKnown, at, known := ts.ProbePaneExit()
	require.True(t, known)
	require.True(t, dead)
	require.True(t, statusKnown, "the pending stamp resolved and the command's exit was recorded")
	require.Equal(t, 7, status)
	require.True(t, at.Equal(time.Unix(1726000000, 0)))
}

// Real-tmux reproduction of the active-pane misattribution: a process tab
// runs `sleep 600` in an af-stamped single pane; the user attaches, splits the
// window, and exits the split with status 7; tmux holds the dead split pane as
// the session's active pane under remain-on-exit. Before the fix the probe
// read that active pane's collected status as the recorded command's exit
// while `sleep 600` kept running. The fix refuses an unstamped pane's death,
// so the probe answers not-dead and the command's row stays unstamped.
func TestProbePaneExitRefusesAnUnstampedForeignPaneOnRealTmux(t *testing.T) {
	testguard.IsolateTmux(t)
	name := fmt.Sprintf("af_test_foreign_pane_%d", time.Now().UnixNano())
	// Start the recorded command's pane the way af does: the exec-marker
	// launch command, remain-on-exit on, and the @af_pane_pid stamp +
	// pane-died hook installed against the living pane (mirroring
	// startHeldPane, but the command stays alive).
	require.NoError(t, exec.Command("tmux", "new-session", "-d", "-s", name, afPaneLaunch("sleep 600"),
		";", "set-option", "-w", "-t", ExactTarget(name), "remain-on-exit", "on").Run())
	require.NoError(t, exec.Command("tmux", panePidStampArgs(ExactTarget(name))...).Run())
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", ExactTarget(name)).Run() })
	require.Equal(t, "0", tmuxDisplay(t, name, "#{pane_dead}"),
		"the recorded command's pane is alive, not dead")
	require.NotEqual(t, "", tmuxDisplay(t, name, "#{"+PanePidMarker+"}"),
		"sanity: af stamped the recorded command's pane")

	// What a user attaching to the session does: split the window and exit the
	// new pane with status 7. The split pane inherits remain-on-exit (a window
	// option), dies, and is held dead.
	require.NoError(t, exec.Command("tmux", "split-window", "-h", "-t", ExactTarget(name),
		"sh -c 'exit 7'").Run())
	require.Eventually(t, func() bool {
		return tmuxDisplay(t, name, "#{pane_dead}") == "1"
	}, 5*time.Second, 20*time.Millisecond, "the split pane never died")
	// Confirm the dead split pane is the active pane and carries no af stamp —
	// the exact shape the probe must not attribute to the recorded command.
	require.Equal(t, "", tmuxDisplay(t, name, "#{"+PanePidMarker+"}"),
		"the dead active pane is the user's split: it carries no af stamp")

	// The recorded command's pane is alive and stamped; the active pane is the
	// dead, unstamped split. The probe must answer not-dead.
	s := NewTmuxSessionFromSanitizedName(name, "sleep 600")
	dead, status, statusKnown, _, known := s.ProbePaneExit()
	require.True(t, known)
	require.False(t, dead, "the dead split pane is not the recorded command's pane; its exit (status %d) must not be stamped as the command's", status)
	require.False(t, statusKnown, "no status is fabricated for a death not attributed to the recorded command")
	require.Zero(t, status)
}

// The remain-on-exit heal (ApplyRemainOnExit) stamps whatever pane is ACTIVE —
// under a user split that is the foreign pane, and once the pane-died hook
// counts the death the healed stamp reads stampFresh: pid, deaths gap 1, pty,
// and the command pin all hold because the heal pinned the foreign pane's OWN
// start command. The stamp therefore proves af touched the pane, never that af
// launched it — pane_start_command (no exec marker) is what refuses it.
func TestProbePaneExitRefusesAHealStampedForeignPane(t *testing.T) {
	// A heal-written stamp on the user's split pane: fresh in every stamp
	// field, but the pane's recorded start command is the user's own.
	healed := paneState{deaths: "1", tty: "/dev/pts/3", stampCmd: "sh -c 'exit 7'", startCmd: "sh -c 'exit 7'"}
	ts := NewTmuxSessionWithDeps("pane-exit-healed-foreign", "true", nil,
		deadPaneExec(t, "", "8576:0:/dev/pts/3", healed, "7", "", "1726000000", nil))
	dead, _, statusKnown, at, known := ts.ProbePaneExit()
	require.True(t, known)
	require.False(t, dead, "a heal stamp on a foreign pane is not launch provenance; its death is not the command's exit")
	require.False(t, statusKnown)
	require.True(t, at.IsZero())
}

// The mirrored legacy cases (Codex P1 on #6336): a process pane af launched
// BEFORE PID stamping existed carries no stamp, yet its collected status is
// still the recorded command's exit — pane_start_command keeps the shim
// marker that proves af launched it. Refusing here would drop a real exit the
// pre-gate path recorded and misclassify the finished tab as stopped.
func TestProbePaneExitRecordsAnUnstampedLegacyPane(t *testing.T) {
	legacy := paneState{deaths: "", tty: "/dev/pts/0", stampCmd: "", startCmd: shimStartCommand}
	ts := NewTmuxSessionWithDeps("pane-exit-legacy", "true", nil,
		deadPaneExec(t, "", "", legacy, "7", "", "1726000000", nil))
	dead, status, statusKnown, at, known := ts.ProbePaneExit()
	require.True(t, known)
	require.True(t, dead, "an unstamped pane af launched is still the recorded command's pane")
	require.True(t, statusKnown)
	require.Equal(t, 7, status)
	require.True(t, at.Equal(time.Unix(1726000000, 0)))
}

// The sibling legacy shape (Codex P2 on #6336): a pane af launched before
// the incarnation bundle existed carries a BARE-PID @af_pane_pid stamp —
// readable, but holding none of the proof fields. paneStampPID rules it
// stampLegacy: unable to name the current incarnation, yet contradicting
// nothing either. tmux's collected status is still this pane's own last
// death, so the probe records it; refusing would drop a real exit the
// pre-gate path kept.
func TestProbePaneExitRecordsALegacyStampedPane(t *testing.T) {
	gone := exitedProcess(t).PID
	legacy := paneState{deaths: "", tty: "/dev/pts/0", stampCmd: "", startCmd: shimStartCommand}
	ts := NewTmuxSessionWithDeps("pane-exit-legacy-stamp", "true", nil,
		deadPaneExec(t, strconv.Itoa(gone), strconv.Itoa(gone), legacy, "7", "", "1726000000", nil))
	dead, status, statusKnown, at, known := ts.ProbePaneExit()
	require.True(t, known)
	require.True(t, dead, "a bare-pid stamp predates the incarnation proof; it does not contradict the collected exit")
	require.True(t, statusKnown)
	require.Equal(t, 7, status)
	require.True(t, at.Equal(time.Unix(1726000000, 0)))
}

// A legacy pane the heal stamped AFTER it died (tmux <3.8 still answers
// pane_pid then) reads stampPending forever — the pane-died hook cannot count
// a death that already happened. tmux's collected status is still this pane's
// own last death, so the probe records it rather than waiting out a hook that
// can never land.
func TestProbePaneExitRecordsAHealPendingLegacyPane(t *testing.T) {
	gone := exitedProcess(t).PID
	stamp := strconv.Itoa(gone) + ":0:/dev/pts/0"
	healed := paneState{deaths: "0", tty: "/dev/pts/0", stampCmd: shimStartCommand, startCmd: shimStartCommand}
	ts := NewTmuxSessionWithDeps("pane-exit-heal-pending", "true", nil,
		deadPaneExec(t, "", stamp, healed, "7", "", "1726000000", nil))
	dead, status, statusKnown, at, known := ts.ProbePaneExit()
	require.True(t, known)
	require.True(t, dead, "a healed pane's pending stamp must not stall the exit tmux already collected")
	require.True(t, statusKnown)
	require.Equal(t, 7, status)
	require.True(t, at.Equal(time.Unix(1726000000, 0)))
}

// The same pending verdict on a pane whose start command is not af's stays
// refused: pending is not a way through the launch-provenance gate.
func TestProbePaneExitRefusesAPendingStampedForeignPane(t *testing.T) {
	gone := exitedProcess(t).PID
	stamp := strconv.Itoa(gone) + ":0:/dev/pts/3"
	foreign := paneState{deaths: "0", tty: "/dev/pts/3", stampCmd: "sh -c 'exit 7'", startCmd: "sh -c 'exit 7'"}
	ts := NewTmuxSessionWithDeps("pane-exit-pending-foreign", "true", nil,
		deadPaneExec(t, "", stamp, foreign, "7", "", "1726000000", nil))
	dead, status, statusKnown, _, known := ts.ProbePaneExit()
	require.True(t, known)
	require.False(t, dead, "a pending foreign stamp is not the recorded command's exit either")
	require.False(t, statusKnown)
	require.Zero(t, status)
}

// The marker alone is not launch provenance (Codex re-review P1 on #6336): an
// account-scoped session's default-command wraps an interactive account shell
// through the same sessionenv exec marker af's own launches carry, so a split
// pane the user created from that default reports a GENUINE marker — and the
// remain-on-exit heal can give it a genuine stamp — without ever having run
// the recorded command. The wrapper leaves the recorded command as its last
// argument; the account shell's last word is the shell, so the split's death
// still is not the command's exit.
func TestProbePaneExitRefusesAnAccountScopedDefaultCommandSplit(t *testing.T) {
	gone := exitedProcess(t).PID
	// The account-scoped default command prepareLaunchEnvironment installs:
	// a sessionenv-wrapped interactive account shell — marker present, but
	// the last word is the shell, not the recorded program "true".
	defaultShell := "/usr/bin/af " + sessionenv.AccountEnvironmentExecMarker + " /tmp/af-claim/claim.sock tok /bin/sh"
	split := paneState{deaths: "1", tty: "/dev/pts/3", stampCmd: defaultShell, startCmd: defaultShell}
	ts := NewTmuxSessionWithDeps("pane-exit-account-split", "true", nil,
		deadPaneExec(t, strconv.Itoa(gone), strconv.Itoa(gone)+":0:/dev/pts/3", split, "7", "", "1726000000", nil))
	dead, status, statusKnown, at, known := ts.ProbePaneExit()
	require.True(t, known)
	require.False(t, dead, "a marker-bearing default-command split never ran the recorded command; its death is not the command's exit")
	require.False(t, statusKnown)
	require.Zero(t, status)
	require.True(t, at.IsZero())
}

// tmuxDisplay returns tmux's expansion of format for the session's active pane.
func tmuxDisplay(t *testing.T, name, format string) string {
	t.Helper()
	out, err := exec.Command("tmux", "display-message", "-p", "-t", ExactTarget(name), format).Output()
	require.NoError(t, err, "tmux display-message -t %s %q", name, format)
	return strings.TrimSpace(string(out))
}

// uncollectedProcess returns the pid of a child that has exited and that
// nothing has collected yet, which is what a pane root is between its exit and
// tmux reaping it.
func uncollectedProcess(t *testing.T) int {
	t.Helper()
	c := exec.Command("sleep", "300")
	require.NoError(t, c.Start())
	t.Cleanup(func() { _, _ = c.Process.Wait() })
	require.NoError(t, c.Process.Kill())
	require.Eventually(t, func() bool {
		_, err := proctree.Lookup(c.Process.Pid)
		return errors.Is(err, proctree.ErrProcessExited)
	}, 3*time.Second, 10*time.Millisecond, "the child never became an uncollected exit")
	return c.Process.Pid
}

// tmux runs a pane command as `<default-shell> -c <command line>`, and af's
// command line starts its launch shim, which then execs /bin/sh -c <command>.
// Only the first two stages mean the command has not started.
func TestArgvIsLaunchShim(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want bool
	}{
		{[]string{"zsh", "-c", "/opt/af __af-session-env-exec-account-environment claude 0 work '' 0 ./deploy.sh"}, true},
		{[]string{"/opt/af", "__af-session-env-exec-account-environment", "claude", "0", "work", "", "0", "./deploy.sh"}, true},
		{[]string{"/opt/af", "__af-session-env-exec", "claude", "0", "claude"}, true},
		{[]string{"/opt/af", "__af-session-env-exec-account", "claude", "0", "work", "", "0", "claude"}, true},
		{[]string{"/bin/sh", "-c", "./deploy.sh"}, false},
		{[]string{"/bin/sh", "./deploy.sh"}, false},
		{[]string{"sleep", "300"}, false},
		{nil, false},
	} {
		assert.Equal(t, tc.want, argvIsLaunchShim(tc.argv), "%q", tc.argv)
	}
}

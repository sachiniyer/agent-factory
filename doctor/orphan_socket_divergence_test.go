package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// These tests pin the fix for the orphaned-process kill arming off a tmux
// listing of the WRONG server. `af doctor`'s session listing used to be a bare
// `tmux ls` that resolved its server from doctor's own $TMUX/$TMUX_TMPDIR, never
// the socket the daemon recorded in tmux-server-state.json — so a live, same-home
// af session on a DIFFERENT tmux server was absent from the listing, every process
// carrying its AF_SESSION marker read as a verified orphan, and `af doctor --fix`
// SIGKILLed it. The fix pins every doctor tmux read to the daemon's recorded
// socket and refuses to arm the kill when that record exists but cannot be read
// (a successful listing of the wrong server is not evidence of death either).
//
// Every test here stages the divergence the existing suite never exercised:
// doctor's ambient tmux server is NOT the one the daemon hosts the live session
// on. The single-private-server shape every other test shares would mask the
// bug, because there doctor's ambient resolution matches the session's server.

// writeTmuxServerState writes a tmux-server-state.json record the daemon would
// have written, so doctor's recordedSocket resolves and pins its tmux reads.
func writeTmuxServerState(t *testing.T, home string, state tmux.ServerState) {
	t.Helper()
	data, err := json.MarshalIndent(state, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(home, "tmux-server-state.json"), append(data, '\n'), 0o600))
}

// writeCorruptTmuxServerState writes a tmux-server-state.json the daemon's
// reader cannot parse, so doctor knows a record exists but cannot pin its
// listing to the socket it names — the state that must NOT arm a kill.
func writeCorruptTmuxServerState(t *testing.T, home string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(home, "tmux-server-state.json"), []byte("{not valid json"), 0o600))
}

// TestDoctorPinsTmuxListingToRecordedSocket is the unit-level half of the fix:
// when the daemon recorded a socket, doctor's session listing is run against
// THAT socket (`tmux -S <socket> ls`) rather than the ambient `tmux ls` the
// daemon's own teardown uses. A bare listing reads the wrong server whenever
// doctor's $TMUX/$TMUX_TMPDIR resolves a different one, which is exactly the
// divergence that armed kills for live sessions.
func TestDoctorPinsTmuxListingToRecordedSocket(t *testing.T) {
	home := testguard.SocketTempDir(t)
	recorded := filepath.Join(home, "recorded-tmux-socket")
	writeTmuxServerState(t, home, tmux.ServerState{
		Mode:   tmux.ServerModeDedicated,
		Socket: recorded,
		Unit:   "test-unit",
		At:     time.Now(),
	})

	var lsCalls [][]string
	opts := testOptionsWithHome(t, home, false)
	opts.Exec = cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error { return nil },
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			lsCalls = append(lsCalls, append([]string(nil), c.Args...))
			return []byte(""), nil // an authoritative empty listing, not a failed read
		},
	}
	opts.snapshot = func() (map[int]proctree.Process, error) {
		return map[int]proctree.Process{1: {PID: 1, PPID: 0, Comm: "init"}}, nil
	}

	report, err := Run(opts)
	require.NoError(t, err)

	// The listing succeeded (pinned to the recorded socket), so its row is PASS.
	rows := findCheckRows(report, "tmux-inspection")
	require.Len(t, rows, 1)
	require.Equal(t, StatusPass, rows[0].Status,
		"a listing pinned to the recorded socket is a read, not blindness: %s", rows[0].Detail)

	// Every `ls` the run issued is pinned to the recorded socket; a bare ambient
	// `tmux ls` would list the wrong server and read a live session as dead.
	var pinned, ambientListings int
	for _, args := range lsCalls {
		if len(args) < 4 || args[3] != "ls" {
			continue
		}
		if args[1] == "-S" {
			pinned++
			require.Equal(t, recorded, args[2],
				"the listing must pin to the daemon's recorded socket, not %v", args)
		} else {
			ambientListings++
		}
	}
	require.GreaterOrEqual(t, pinned, 1,
		"the session listing must be run as `tmux -S <recorded-socket> ls`, not a bare ambient `tmux ls`")
	require.Zero(t, ambientListings,
		"no ambient `tmux ls` may run while a recorded socket pins the listing — it lists the wrong server")
}

// TestRecordedSocketPin_LiveSessionOnOtherServerNotKilledByFix is the
// end-to-end regression: a LIVE af session on a tmux server the daemon recorded
// (server A), while doctor's own ambient $TMUX_TMPDIR resolves a DIFFERENT server
// (a decoy-bearing server B) so a bare listing would not name the live session.
// Before the fix, `af doctor --fix` armed `kill pid N` for the live session's
// marked process and SIGKILLed it. After the fix, the listing and the pane
// capture are pinned to the recorded socket, the session reads live, and the
// process is reported (at most) as escaped — never armed for kill.
func TestRecordedSocketPin_LiveSessionOnOtherServerNotKilledByFix(t *testing.T) {
	testguard.IsolateTmux(t) // doctor's ambient server (server B); TMUX cleared, TMUX_TMPDIR set
	home := testguard.SocketTempDir(t)

	// Server A: an explicit-socket tmux server holding a LIVE af session that
	// carries this home's ancestry markers — the production shape of a daemon
	// session the daemon recorded on its own socket.
	sockA := testguard.SocketPath(t, "a")
	const liveName = "af_doctor-live-on-recorded-socket"
	out, err := exec.Command("tmux", "-S", sockA, "new-session", "-d", "-s", liveName,
		"-e", tmux.EnvMarkerSession+"="+liveName, "-e", tmux.EnvMarkerHome+"="+home,
		"sleep", "300").CombinedOutput()
	require.NoError(t, err, "tmux new-session on server A: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", sockA, "kill-server").Run() })

	// Server B: the ambient IsolateTmux server, kept alive with a decoy session so
	// a listing against it SUCCEEDS and is non-empty — i.e. a real "successful
	// listing of the wrong server", not a failed/empty one the existing blindness
	// guard already covers.
	const decoyName = "af_doctor-ambient-decoy"
	out, err = exec.Command("tmux", "new-session", "-d", "-s", decoyName, "sleep", "300").CombinedOutput()
	require.NoError(t, err, "tmux new-session decoy on server B: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", "="+decoyName+":").Run() })

	// The daemon recorded its sessions on server A's socket.
	writeTmuxServerState(t, home, tmux.ServerState{
		Mode:   tmux.ServerModeDedicated,
		Socket: sockA,
		Unit:   "test-unit",
		At:     time.Now(),
	})

	// A process carrying the LIVE session's markers and this home — the exact
	// shape checkOrphanedProcesses arms a kill for once the session looks dead.
	// It is a spawnWithEnv child, not a real pane process of the live session, so
	// the escaped arm finds it outside the pane tree (the same staging the live
	// session escaped test uses); what matters is that the session is live, so
	// the kill must never arm.
	victim := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: liveName,
		tmux.EnvMarkerHome:    home,
	})

	opts := testOptionsWithHome(t, home, true, victim.PID) // Fix: true
	report, err := Run(opts)
	require.NoError(t, err)

	require.True(t, alive(victim),
		"`af doctor --fix` killed a process whose session is live on the daemon's recorded socket "+
			"but absent from doctor's ambient listing — the wrong-server listing bug")

	// The session is live on the recorded socket, so its marked process is never
	// an actionable orphan with a kill armed.
	for _, f := range findByCheck(report, "orphaned-process") {
		require.Empty(t, f.FixAction,
			"a live session's process must not be armed for kill: %s", f.Detail)
		require.False(t, f.Fixed, "a live session's process must not be fixed: %s", f.Detail)
	}

	// Pinned to the recorded socket, the session reads LIVE: the process was
	// classified as escaped (report-only), the same classification it gets on a
	// single-socket setup where doctor's ambient resolution matches the session.
	escaped := findByCheck(report, "escaped-process")
	require.NotEmpty(t, escaped,
		"a process of a live session must be reported as escaped once the listing is pinned to the recorded socket")
	for _, f := range escaped {
		require.Empty(t, f.FixAction, "an escaped process of a live session is report-only")
	}

	// And the tmux-inspection row attested a read of the recorded server, not a
	// blind PASS over the wrong (ambient) one.
	rows := findCheckRows(report, "tmux-inspection")
	require.Len(t, rows, 1)
	require.Equal(t, StatusPass, rows[0].Status,
		"the pinned listing read the recorded server: %s", rows[0].Detail)
}

// TestUnreadableServerRecord_DoesNotArmKillForAmbientAbsentSession is the
// corroboration guard: the daemon's server record EXISTS but is unreadable, so
// doctor cannot pin its listing to the socket the daemon used. A successful
// listing of whatever server doctor's ambient environment reaches is then not
// proof of death — a live session on the daemon's (unreadable) socket would be
// absent from it and read as a verified orphan. The kill arm refuses to act and
// reports instead, the same rule the failed-listing guard enforces for one that
// did not answer. The session is staged live on server A while doctor's ambient
// (server B) does not name it, so without the guard this arms and fires a kill.
func TestUnreadableServerRecord_DoesNotArmKillForAmbientAbsentSession(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)

	// A live af session on a non-ambient socket (server A), carrying this home's
	// markers. The record that would name server A is corrupt, so doctor falls
	// back to the ambient listing — which does not name this session.
	sockA := testguard.SocketPath(t, "a")
	const liveName = "af_doctor-live-behind-unreadable-record"
	out, err := exec.Command("tmux", "-S", sockA, "new-session", "-d", "-s", liveName,
		"-e", tmux.EnvMarkerSession+"="+liveName, "-e", tmux.EnvMarkerHome+"="+home,
		"sleep", "300").CombinedOutput()
	require.NoError(t, err, "tmux new-session on server A: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", sockA, "kill-server").Run() })

	// Keep server B (the ambient IsolateTmux server) alive and non-empty so the
	// ambient listing succeeds and is non-empty — the "successful listing of the
	// wrong server" case the guard exists for, not a failed read.
	const decoyName = "af_doctor-ambient-decoy-unreadable"
	out, err = exec.Command("tmux", "new-session", "-d", "-s", decoyName, "sleep", "300").CombinedOutput()
	require.NoError(t, err, "tmux new-session decoy: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", "="+decoyName+":").Run() })

	writeCorruptTmuxServerState(t, home)

	victim := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: liveName,
		tmux.EnvMarkerHome:    home,
	})

	opts := testOptionsWithHome(t, home, true, victim.PID) // Fix: true
	report, err := Run(opts)
	require.NoError(t, err)

	require.True(t, alive(victim),
		"`af doctor --fix` killed a process while the daemon's server record was unreadable, so the "+
			"listing could not be pinned to the socket that hosts its session — the corroboration guard")

	// The unreadable record is reported by the environment row, and a kill that
	// depends on a listing doctor cannot corroborate stays report-only.
	rows := findCheckRows(report, "tmux server")
	require.Len(t, rows, 1)
	require.Contains(t, rows[0].Detail, "could not read",
		"the unreadable record must be reported so the operator knows the listing is uncorroborated: %s", rows[0].Detail)

	var orphanFindings []Finding
	for _, f := range findByCheck(report, "orphaned-process") {
		require.Empty(t, f.FixAction,
			"a session death the listing cannot corroborate against the daemon's recorded socket must not arm a kill: %s", f.Detail)
		require.False(t, f.Actionable,
			"an uncorroborated orphan must not fail the health probe: %s", f.Detail)
		require.False(t, f.Fixed)
		orphanFindings = append(orphanFindings, f)
	}
	require.NotEmpty(t, orphanFindings,
		"the process must stay VISIBLE (reported, not killed) so the operator is not told the run is clean")
	require.Zero(t, report.UnresolvedCount(),
		"an uncorroborated orphan is UNKNOWN, not a proven health failure")
}

// TestRecordedSocketPin_GenuineOrphanStillKilled is the no-regression direction
// for the pinning: with a recorded socket that names the same server the orphan
// is absent from, a genuine orphan's death IS corroborated by the pinned listing,
// so the kill still arms and fires under `--fix`. The pinning must not trade the
// wrong-server kill for an under-kill of real orphans.
func TestRecordedSocketPin_GenuineOrphanStillKilled(t *testing.T) {
	testguard.IsolateTmux(t) // doctor's ambient server is the recorded server; one socket, no divergence
	home := testguard.SocketTempDir(t)

	// A recorded socket pointing at doctor's ambient (IsolateTmux) server: the
	// daemon hosts sessions here, and doctor's ambient resolution agrees. The
	// record carries a daemon_pid so it is a generation-stamped record — with
	// no live daemon to contradict it, it is the last word a daemon generation
	// left and corroborates the no-TMUX-env orphan's death (#6339).
	sockAmbient := filepath.Join(os.Getenv("TMUX_TMPDIR"), fmt.Sprintf("tmux-%d/default", os.Getuid()))
	writeTmuxServerState(t, home, tmux.ServerState{
		Mode:      tmux.ServerModeDedicated,
		Socket:    sockAmbient,
		Unit:      "test-unit",
		At:        time.Now(),
		DaemonPID: 424242,
	})

	// A genuinely dead session name (never created on any server) — the verified
	// orphan shape. Its AF_HOME matches doctor's, so the kill arm is the only
	// question, and the pinned listing (which names the recorded/ambient server)
	// corroborates its absence.
	const deadName = "af_doctor-genuine-orphan"
	orphan := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: deadName,
		tmux.EnvMarkerHome:    home,
	})

	opts := testOptionsWithHome(t, home, true, orphan.PID) // Fix: true
	report, err := Run(opts)
	require.NoError(t, err)

	findings := findByCheck(report, "orphaned-process")
	require.Len(t, findings, 1)
	require.NotEmpty(t, findings[0].FixAction,
		"a genuine orphan whose death the pinned listing corroborates must still arm the kill")
	require.True(t, findings[0].Fixed, "fix outcome: %v", findings[0].FixErr)
	require.Eventually(t, func() bool { return !alive(orphan) }, 5*time.Second, 25*time.Millisecond,
		"a genuine orphan must still be killed by --fix once the listing is pinned to the recorded socket")
}

// TestRecordedSocketPin_LeakedSessionKillCommandPinnedToRecordedSocket asserts the
// manual remedy checkLeakedTmuxSessions prints targets the SAME server the pinned
// listing found the leaked session on. The listing is `tmux -S <socket> ls`, so a
// bare `tmux kill-session -t ...` remedy would resolve doctor's own
// $TMUX/$TMUX_TMPDIR and miss the session (or, on a shared name, kill an unrelated
// one on the ambient server) — the wrong-server divergence the listing pinning
// closed, reopened on the remedy side.
func TestRecordedSocketPin_LeakedSessionKillCommandPinnedToRecordedSocket(t *testing.T) {
	testguard.IsolateTmux(t) // doctor's ambient server (server B); TMUX cleared, TMUX_TMPDIR set
	home := testguard.SocketTempDir(t)

	// A leaked af session on the daemon's recorded socket (server A), carrying
	// this home's ownership marker so it is a proven, actionable leak. No config
	// instance backs it, so it is absent from recordedTmuxNames.
	sockA := testguard.SocketPath(t, "a")
	const leakedName = "af_doctor-leaked-on-recorded-socket"
	out, err := exec.Command("tmux", "-S", sockA, "new-session", "-d", "-s", leakedName,
		"-e", tmux.EnvMarkerHome+"="+home, "sleep", "300").CombinedOutput()
	require.NoError(t, err, "tmux new-session on server A: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", sockA, "kill-server").Run() })

	writeTmuxServerState(t, home, tmux.ServerState{
		Mode:   tmux.ServerModeDedicated,
		Socket: sockA,
		Unit:   "test-unit",
		At:     time.Now(),
	})

	opts := testOptionsWithHome(t, home, false)
	report, err := Run(opts)
	require.NoError(t, err)

	leaked := findByCheck(report, "leaked-tmux-session")
	require.Len(t, leaked, 1, "exactly one leaked session on the recorded socket: %v", leaked)
	require.True(t, leaked[0].Actionable,
		"a session carrying this home's marker with no backing record is a proven leak: %s", leaked[0].Detail)
	require.Contains(t, leaked[0].Detail, "-S "+sockA,
		"the manual kill remedy must pin to the recorded socket the listing found the session on: %s", leaked[0].Detail)
	require.Contains(t, leaked[0].Detail, "kill-session",
		"the remedy is a kill-session: %s", leaked[0].Detail)
	require.NotContains(t, leaked[0].Detail, "tmux kill-session -t",
		"a bare ambient `tmux kill-session -t ...` would target the wrong server when the listing is pinned: %s", leaked[0].Detail)
}

// TestMissingServerRecord_LiveDaemonDoesNotArmKill is the missing-record half of
// the corroboration guard. recordServerState silently ignores write failures,
// so a daemon can be live on its own socket while tmux-server-state.json is
// absent (a failed write, or a deleted file). The historical reading of an
// absent record — "this home never ran the daemon, so the ambient listing is
// authoritative" — would then arm the kill off doctor's ambient (wrong) server,
// the same wrong-server kill the pinning closed for the recorded case. When a
// daemon IS serving this home the record's absence is suspicious rather than
// evidence of "never ran", so the kill arm refuses to act and reports instead —
// the same rule the unreadable-record guard above enforces.
func TestMissingServerRecord_LiveDaemonDoesNotArmKill(t *testing.T) {
	testguard.IsolateTmux(t) // doctor's ambient server (server B)
	home := testguard.SocketTempDir(t)

	// A live af session on a non-ambient socket (server A), carrying this
	// home's markers. No record names server A, so without the guard the
	// ambient listing (server B) reads the session as dead.
	sockA := testguard.SocketPath(t, "a")
	const liveName = "af_doctor-live-behind-missing-record"
	out, err := exec.Command("tmux", "-S", sockA, "new-session", "-d", "-s", liveName,
		"-e", tmux.EnvMarkerSession+"="+liveName, "-e", tmux.EnvMarkerHome+"="+home,
		"sleep", "300").CombinedOutput()
	require.NoError(t, err, "tmux new-session on server A: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", sockA, "kill-server").Run() })

	// Server B (the ambient IsolateTmux server) kept alive with a decoy so the
	// ambient listing succeeds and is non-empty — the "successful listing of
	// the wrong server" shape, not a failed read.
	const decoyName = "af_doctor-ambient-decoy-missing-record"
	out, err = exec.Command("tmux", "new-session", "-d", "-s", decoyName, "sleep", "300").CombinedOutput()
	require.NoError(t, err, "tmux new-session decoy: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", "="+decoyName+":").Run() })

	// No writeTmuxServerState: the record is ABSENT. But a daemon IS serving
	// this home, staged through the daemon-scan seams so recordedSocket sees a
	// live active-home daemon and downgrades the absent record to unconfirmed
	// instead of arming kills from the ambient listing.
	daemonProc := spawnWithEnv(t, "af", []string{"--daemon"}, nil)
	stubDaemonProcessArgv(t, func(pid int) []string {
		if pid == daemonProc.PID {
			return []string{"af", "--daemon"}
		}
		return nil
	})
	stubProcessHomes(t, map[int]string{daemonProc.PID: home})

	// A process carrying the LIVE session's markers and this home — the exact
	// shape checkOrphanedProcesses arms a kill for once the session looks dead.
	victim := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: liveName,
		tmux.EnvMarkerHome:    home,
	})

	opts := testOptionsWithHome(t, home, true, daemonProc.PID, victim.PID) // Fix: true
	report, err := Run(opts)
	require.NoError(t, err)

	require.True(t, alive(victim),
		"`af doctor --fix` killed a process whose session is live on a socket the daemon never "+
			"recorded, because a live daemon made the missing record uncorroborated — the wrong-server "+
			"listing bug reopened on the missing-record path")

	for _, f := range findByCheck(report, "orphaned-process") {
		require.Empty(t, f.FixAction,
			"a session death the listing cannot corroborate (missing record, live daemon) must not arm a kill: %s", f.Detail)
		require.False(t, f.Fixed, "an uncorroborated orphan must not be fixed: %s", f.Detail)
	}
}

// TestStaleTempHomeClaimedByAmbientOnlySessionSurvives pins the union half of
// the socket fix: the rm -rf home guards consume liveTmuxHomes, which lists
// only the recorded socket once the listing is pinned — but a stale temp
// home's live session can sit on ANOTHER socket (its own daemon's, or doctor's
// ambient server). A pinned-only read then concludes the candidate is unused
// and --fix rm -rfs it from under the session claiming it. The guard must see
// the union: recorded socket AND ambient.
func TestStaleTempHomeClaimedByAmbientOnlySessionSurvives(t *testing.T) {
	tempRoot := t.TempDir()
	home := t.TempDir()
	dir := makeOldTempAFHome(t, tempRoot, "tmp.claimed-ambient")

	// The daemon recorded socket S, so the session listing is pinned there.
	recorded := filepath.Join(home, "recorded-tmux-socket")
	writeTmuxServerState(t, home, tmux.ServerState{
		Mode:   tmux.ServerModeDedicated,
		Socket: recorded,
		Unit:   "test-unit",
		At:     time.Now(),
	})

	// The lock probe answers "takeable" so ONLY the tmux claim set can spare
	// the candidate — the exact shape where a pinned-only listing removes a
	// home that is still in use.
	stubTempHomeLockProbe(t, func(string) daemon.ProbeAnswer { return daemon.AnswerNo() })

	opts := testOptionsWithHome(t, home, true) // Fix: true
	opts.TempDir = tempRoot
	opts.Exec = cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error { return nil },
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			args := cmd.Args
			joined := strings.Join(args, " ")
			pinned := len(args) > 2 && args[1] == "-S"
			switch {
			case pinned && len(args) > 3 && args[3] == "ls":
				require.Equal(t, recorded, args[2])
				return []byte(""), nil // nothing lives on the recorded socket
			case !pinned && len(args) > 1 && args[1] == "ls":
				// The stale home's live session is on the AMBIENT server only.
				return []byte("af_ambient-claimant\n"), nil
			case pinned && strings.Contains(joined, "show-environment"):
				return nil, fmt.Errorf("no session lives on the recorded socket: %s", joined)
			case !pinned && strings.Contains(joined, "show-environment"):
				if !strings.Contains(joined, "af_ambient-claimant") {
					return nil, fmt.Errorf("marker read for a session the ambient listing did not name: %s", joined)
				}
				return []byte("AF_HOME=" + dir + "\n"), nil
			}
			return []byte(""), nil
		},
	}

	report, err := Run(opts)
	require.NoError(t, err)
	require.DirExists(t, dir,
		"a temp home claimed by a live session on another socket must survive --fix; the pinned listing alone cannot see it")
	require.Empty(t, findByCheck(report, "stale-temp-home"),
		"a claimed home is not a stale finding to remove")
}

// The dead-daemon half of the missing-record corroboration guard. #6271
// refused the kill when a record was unreadable, named no socket, or was
// absent WITH a live daemon — but left the record-absent, dead-daemon window
// arming from the ambient listing, on the premise that an absent record means
// the home never ran the record-writing daemon. That premise is unsound: a
// daemon that hosted sessions and then died leaves no record (recordServerState
// silently ignores write failures; macOS never writes one), so the ambient
// listing is the only evidence and may be looking at a different server than
// the one hosting a live session — the wrong-server kill #6271's pinning
// closed for the recorded case, reopened here.
//
// The fix corroborates the ambient listing PER PROCESS against the socket the
// marked process's own TMUX env names ("socketPath,serverPID,sessionIdx", set by
// tmux on every pane process), arming the kill only when that socket agrees
// with the ambient one — proving the ambient server IS the one that hosted the
// dead session. A mismatch, a no-TMUX-env, or an empty (no-server) ambient
// listing all refuse, the same rule the unreadable-record guard enforces. The
// tests below stage the divergence the existing suite never exercised: a LIVE
// af session on a non-ambient socket the absent record cannot pin to, while
// doctor's ambient server is a different one (or none at all).

// TestMissingServerRecord_DeadDaemon_LiveOnOtherSocket_NotKilled is the precise
// fix. No record exists, no daemon is serving this home (recordedSocketAbsent,
// the dead-daemon window), and a live af session sits on a non-ambient socket
// (server A) the ambient listing (server B, kept alive with a decoy for a real
// successful read) cannot reach. The victim carries the live session's markers
// AND the TMUX env it inherited from tmux, naming server A. The ambient listing
// reads the session as dead, but the per-process guard sees the victim's socket
// does not match the ambient one and refuses the kill.
func TestMissingServerRecord_DeadDaemon_LiveOnOtherSocket_NotKilled(t *testing.T) {
	testguard.IsolateTmux(t) // doctor's ambient server (server B); TMUX cleared, TMUX_TMPDIR set
	home := testguard.SocketTempDir(t)

	// Server A: an explicit-socket tmux server holding a LIVE af session that
	// carries this home's ancestry markers — the production shape of a session
	// the daemon hosted on its own (now-unrecorded) socket before it died.
	sockA := testguard.SocketPath(t, "a")
	const liveName = "af_doctor-live-behind-absent-record"
	out, err := exec.Command("tmux", "-S", sockA, "new-session", "-d", "-s", liveName,
		"-e", tmux.EnvMarkerSession+"="+liveName, "-e", tmux.EnvMarkerHome+"="+home,
		"sleep", "300").CombinedOutput()
	require.NoError(t, err, "tmux new-session on server A: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", sockA, "kill-server").Run() })

	// Server B (the ambient IsolateTmux server) kept alive with a decoy so the
	// ambient listing succeeds and is non-empty — the "successful listing of
	// the wrong server" shape, not a failed read the blindness guard covers.
	const decoyName = "af_doctor-ambient-decoy-absent-record"
	out, err = exec.Command("tmux", "new-session", "-d", "-s", decoyName, "sleep", "300").CombinedOutput()
	require.NoError(t, err, "tmux new-session decoy: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", "="+decoyName+":").Run() })

	// No writeTmuxServerState and no staged daemon: recordedSocket falls to
	// recordedSocketAbsent and the listing is the unpinned ambient one.

	// A process carrying the LIVE session's markers, this home, and the TMUX
	// env a real pane process inherits from tmux — naming server A, which the
	// ambient listing does not reach. The server pid must be real and in the
	// run's snapshot: a pid the snapshot does not contain proves the SERVER
	// dead, which is a different (genuine) corroboration path.
	serverAPID := tmuxServerPID(t, sockA)
	victim := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: liveName,
		tmux.EnvMarkerHome:    home,
		"TMUX":                fmt.Sprintf("%s,%d,0", sockA, serverAPID),
	})

	opts := testOptionsWithHome(t, home, true, victim.PID, serverAPID) // Fix: true
	report, err := Run(opts)
	require.NoError(t, err)

	require.True(t, alive(victim),
		"`af doctor --fix` killed a process whose session is live on a non-ambient socket the "+
			"daemon never recorded — the record-absent dead-daemon window the per-process "+
			"TMUX-env corroboration guard closes")

	// The victim's own TMUX env names server A, and a listing of THAT socket
	// proves its session live — so it is classified escaped (report-only), the
	// same classification a process outside a live session's pane tree gets on
	// the recorded socket, rather than an uncorroborated orphan.
	visible := append(findByCheck(report, "orphaned-process"), findByCheck(report, "escaped-process")...)
	require.NotEmpty(t, visible,
		"the process must stay VISIBLE (reported, not killed) so the run does not report clean")
	for _, f := range visible {
		require.Empty(t, f.FixAction,
			"a session live on the socket the process's TMUX env names must not arm a kill: %s", f.Detail)
		require.False(t, f.Fixed, "an uncorroborated orphan must not be fixed: %s", f.Detail)
	}
	// The tmux-inspection row attests a healthy read of the (wrong) ambient
	// server — the insidious shape: nothing in the row names the divergence.
	rows := findCheckRows(report, "tmux-inspection")
	require.Len(t, rows, 1)
	require.Equal(t, StatusPass, rows[0].Status,
		"the ambient listing succeeded, so its row is a PASS — the kill must be refused by the "+
			"per-process guard, not by blindness: %s", rows[0].Detail)
}

// TestMissingServerRecord_DeadDaemon_NoAmbientServer_NotKilled is the
// no-ambient-server shape of the same window. Nothing runs on doctor's ambient
// socket, so the ambient listing is tmux's definitive "no server running on
// <socket>", which session/tmux's listSessionField converts to an error-free
// empty list. That empty list passes the failed-listing guard, the live session
// on server A reads dead, and before the guard the kill armed from a listing
// that never named the server the session lived on. The per-process TMUX-env
// corroboration refuses it the same way the non-empty-wrong-server shape does.
func TestMissingServerRecord_DeadDaemon_NoAmbientServer_NotKilled(t *testing.T) {
	testguard.IsolateTmux(t) // doctor's ambient socket dir is empty: no server, no sessions
	home := testguard.SocketTempDir(t)

	sockA := testguard.SocketPath(t, "a")
	const liveName = "af_doctor-live-behind-absent-record-no-ambient"
	out, err := exec.Command("tmux", "-S", sockA, "new-session", "-d", "-s", liveName,
		"-e", tmux.EnvMarkerSession+"="+liveName, "-e", tmux.EnvMarkerHome+"="+home,
		"sleep", "300").CombinedOutput()
	require.NoError(t, err, "tmux new-session on server A: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", sockA, "kill-server").Run() })

	// No decoy: the ambient socket holds no server, so `tmux ls` answers the
	// definitive no-server diagnostic and listSessionField returns an empty,
	// error-free list — the shape that armed the kill before the fix.

	serverAPID := tmuxServerPID(t, sockA)
	victim := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: liveName,
		tmux.EnvMarkerHome:    home,
		"TMUX":                fmt.Sprintf("%s,%d,0", sockA, serverAPID),
	})

	opts := testOptionsWithHome(t, home, true, victim.PID, serverAPID) // Fix: true
	report, err := Run(opts)
	require.NoError(t, err)

	require.True(t, alive(victim),
		"`af doctor --fix` killed a process whose session is live on a non-ambient socket while the "+
			"ambient server was not running at all — the empty listing was read as proof of death")

	// The victim's TMUX env names server A and a listing there proves its
	// session live, so it is classified escaped (report-only), not orphaned.
	visible := append(findByCheck(report, "orphaned-process"), findByCheck(report, "escaped-process")...)
	require.NotEmpty(t, visible,
		"an empty ambient listing is not corroboration against a process live on another socket")
	for _, f := range visible {
		require.Empty(t, f.FixAction,
			"an empty ambient listing is not corroboration against a process on another socket: %s", f.Detail)
		require.False(t, f.Fixed, "an uncorroborated orphan must not be fixed: %s", f.Detail)
	}
}

// TestMissingServerRecord_DeadDaemon_ProcessWithoutTmuxEnv_NotKilled locks the
// conservative direction of the guard. A marked process that carries no TMUX
// env cannot be tied to the ambient server at all, so the ambient listing cannot
// corroborate its session's death — the exact shape the report's repro stages
// (a process spawned with the session's markers but no inherited TMUX). Before
// the fix the ambient listing armed the kill from that uncorroborated absence;
// after, the arm refuses and the process is reported, not killed.
func TestMissingServerRecord_DeadDaemon_ProcessWithoutTmuxEnv_NotKilled(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)

	// A live af session on a non-ambient socket (server A) carrying this
	// home's markers — the session the ambient listing cannot see.
	sockA := testguard.SocketPath(t, "a")
	const liveName = "af_doctor-live-no-tmux-env"
	out, err := exec.Command("tmux", "-S", sockA, "new-session", "-d", "-s", liveName,
		"-e", tmux.EnvMarkerSession+"="+liveName, "-e", tmux.EnvMarkerHome+"="+home,
		"sleep", "300").CombinedOutput()
	require.NoError(t, err, "tmux new-session on server A: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", sockA, "kill-server").Run() })

	// Server B (ambient) kept alive with a decoy so the listing succeeds.
	const decoyName = "af_doctor-ambient-decoy-no-tmux-env"
	out, err = exec.Command("tmux", "new-session", "-d", "-s", decoyName, "sleep", "300").CombinedOutput()
	require.NoError(t, err, "tmux new-session decoy: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", "="+decoyName+":").Run() })

	// No record, no daemon: recordedSocketAbsent. The victim carries the live
	// session's markers but no TMUX env, so ambientListingCoversProcess cannot
	// tie it to the ambient server and the kill refuses.
	victim := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: liveName,
		tmux.EnvMarkerHome:    home,
	})

	opts := testOptionsWithHome(t, home, true, victim.PID) // Fix: true
	report, err := Run(opts)
	require.NoError(t, err)

	require.True(t, alive(victim),
		"`af doctor --fix` killed a marked process that carries no TMUX env, so the ambient listing "+
			"could not corroborate which server hosted its session — the uncorroborated-ambient kill")

	orphanFindings := findByCheck(report, "orphaned-process")
	require.NotEmpty(t, orphanFindings)
	for _, f := range orphanFindings {
		require.Empty(t, f.FixAction,
			"a process with no TMUX env to corroborate against the ambient server must not arm a kill: %s", f.Detail)
		require.False(t, f.Fixed)
	}
}

// TestMissingServerRecord_DeadDaemon_GenuineOrphanOnAmbientSocket_Killed is the
// no-regression direction for the per-process guard. With no record and no
// daemon, the ambient listing STILL corroborates a session's death when the
// marked process's TMUX env names the AMBIENT socket — proving the ambient
// server IS the one that hosted the (now-dead) session. The kill arms and
// fires, so --fix cleanup is preserved for the never-ran-a-record-writing-
// daemon homes that do corroborate (the macOS common case), at the cost only of
// the wrong-server and no-TMUX-env shapes the guards above now refuse.
func TestMissingServerRecord_DeadDaemon_GenuineOrphanOnAmbientSocket_Killed(t *testing.T) {
	testguard.IsolateTmux(t) // doctor's ambient server is the one that hosted the dead session
	home := testguard.SocketTempDir(t)

	// Server B (the ambient IsolateTmux server) kept alive with a decoy so the
	// ambient listing succeeds and is non-empty — a real read, not blindness.
	const decoyName = "af_doctor-ambient-decoy-genuine-absent"
	out, err := exec.Command("tmux", "new-session", "-d", "-s", decoyName, "sleep", "300").CombinedOutput()
	require.NoError(t, err, "tmux new-session decoy: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", "="+decoyName+":").Run() })

	// A genuinely dead session name (never created on any server) carrying this
	// home's markers and a TMUX env naming the ambient socket AND the ambient
	// server's real pid — the production shape of a process the daemon hosted
	// on the ambient server before its session died. The ambient listing does
	// not name the dead session on the server the process's TMUX env names, so
	// the absence is corroborated death and the kill arms.
	serverBPID := tmuxServerPID(t, tmux.DefaultSocketPath())
	const deadName = "af_doctor-genuine-orphan-absent-record"
	orphan := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: deadName,
		tmux.EnvMarkerHome:    home,
		"TMUX":                fmt.Sprintf("%s,%d,0", tmux.DefaultSocketPath(), serverBPID),
	})

	opts := testOptionsWithHome(t, home, true, orphan.PID, serverBPID) // Fix: true
	report, err := Run(opts)
	require.NoError(t, err)

	findings := findByCheck(report, "orphaned-process")
	require.Len(t, findings, 1)
	require.NotEmpty(t, findings[0].FixAction,
		"a genuine orphan whose TMUX socket matches the ambient server is corroborated and must arm the kill")
	require.True(t, findings[0].Fixed, "fix outcome: %v", findings[0].FixErr)
	require.Eventually(t, func() bool { return !alive(orphan) }, 5*time.Second, 25*time.Millisecond,
		"a genuine orphan corroborated against the ambient server must still be killed by --fix")
}

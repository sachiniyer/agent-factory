package doctor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// These tests pin #6339: the daemon's tmux-server-state.json is its LAST
// recorded decision, and doctor could not prove it current. A restart that
// adopts an existing server returned early without rewriting the record, so a
// record can name a socket the daemon no longer uses; a restart under a
// different TMUX_TMPDIR leaves the OLD dedicated server and its sessions alive
// on a socket the record never names. In both cases `af doctor --fix` listed
// the recorded socket, read a still-live session's absence as proof of death,
// and armed `kill pid N` for its processes. The fix has the daemon rewrite the
// record on every adopt — stamped with its own pid as a generation marker —
// and retain every socket it previously recorded in prior_sockets, while the
// kill arm corroborates each process against the socket its own TMUX env names
// and refuses whenever a socket generation cannot be accounted for.
//
// Records here are written as raw JSON rather than the tmux.ServerState struct
// so the daemon_pid and prior_sockets fields survive a reader that predates
// them — which is exactly the on-disk shape a pre-fix daemon leaves behind.

// writeServerStateJSON writes a tmux-server-state.json record verbatim, so a
// test can carry fields (daemon_pid, prior_sockets) that a ServerState literal
// in an older build would silently drop on marshal.
func writeServerStateJSON(t *testing.T, home string, record string) {
	t.Helper()
	require.NoError(t, os.WriteFile(
		filepath.Join(home, "tmux-server-state.json"), []byte(record+"\n"), 0o600))
}

// tmuxServerPID returns the pid of the tmux server listening on socket — the
// pid tmux stamps into every pane process's TMUX env ("socket,serverPID,idx").
// A marked process's corroboration is only realistic when that pid is live and
// in the run's process snapshot, so tests stage it explicitly rather than
// inventing one.
func tmuxServerPID(t *testing.T, socket string) int {
	t.Helper()
	out, err := exec.Command("tmux", "-S", socket, "display-message", "-p", "#{pid}").Output()
	require.NoError(t, err, "read tmux server pid on %s", socket)
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	require.NoError(t, err, "tmux server pid %q is not a number", out)
	return pid
}

// TestStaleRecordedSocket_LiveOnUnrecordedSocket_NotKilled stages the
// restart-adopt half of #6339: the daemon's record still names its OLD socket
// while its sessions now live on a different server it adopted without
// rewriting the record. Doctor pins its listing to the recorded (stale)
// socket, the live session is absent from it, and without per-process
// corroboration the kill arms and fires. The victim's own TMUX env names the
// server its session actually lives on, and a live session there must refuse
// the kill — the record's absence-evidence is only as good as its currency.
func TestStaleRecordedSocket_LiveOnUnrecordedSocket_NotKilled(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)

	// Server A: an explicit-socket tmux server holding a LIVE af session with
	// this home's markers — the production shape of sessions hosted on a
	// socket the stale record does not name.
	sockA := testguard.SocketPath(t, "a")
	const liveName = "af_doctor-live-on-unrecorded-socket"
	out, err := exec.Command("tmux", "-S", sockA, "new-session", "-d", "-s", liveName,
		"-e", tmux.EnvMarkerSession+"="+liveName, "-e", tmux.EnvMarkerHome+"="+home,
		"sleep", "300").CombinedOutput()
	require.NoError(t, err, "tmux new-session on server A: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", sockA, "kill-server").Run() })
	serverAPID := tmuxServerPID(t, sockA)

	// The record still names the socket an earlier daemon generation used —
	// dead now, so the pinned listing is an authoritative EMPTY, not even a
	// wrong-but-nonempty read.
	staleSock := testguard.SocketPath(t, "stale")
	writeServerStateJSON(t, home, fmt.Sprintf(
		`{"mode":"dedicated","socket":%q,"unit":"agent-factory-tmux-server-old","at":%q}`,
		staleSock, time.Now().Format(time.RFC3339)))

	// A process carrying the LIVE session's markers, this home, and the TMUX
	// env tmux itself stamps on every pane process — naming server A.
	victim := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: liveName,
		tmux.EnvMarkerHome:    home,
		"TMUX":                fmt.Sprintf("%s,%d,0", sockA, serverAPID),
	})

	opts := testOptionsWithHome(t, home, true, victim.PID, serverAPID) // Fix: true
	report, err := Run(opts)
	require.NoError(t, err)

	require.True(t, alive(victim),
		"`af doctor --fix` killed a process whose session is live on a socket the stale record "+
			"no longer names — absence from the recorded socket is not proof of death (#6339)")

	for _, f := range findByCheck(report, "orphaned-process") {
		require.Empty(t, f.FixAction,
			"a session death the stale record cannot corroborate must not arm a kill: %s", f.Detail)
		require.False(t, f.Fixed, "an uncorroborated orphan must not be fixed: %s", f.Detail)
	}
}

// TestPriorRecordedSocket_SessionLiveOnEarlierGeneration_NotKilled stages the
// prior-socket half of #6339: the daemon restarted under a different
// TMUX_TMPDIR, recorded its NEW socket, and left the OLD dedicated server and
// its sessions alive. The old socket is retained in the record's prior_sockets
// so doctor can account for it: a session live on a prior socket generation is
// live, not orphaned — and the process's own TMUX env names that socket too.
func TestPriorRecordedSocket_SessionLiveOnEarlierGeneration_NotKilled(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)

	// Server A: the socket an EARLIER daemon generation recorded — still
	// alive, still hosting its session, now reachable only through the
	// record's prior_sockets history.
	sockA := testguard.SocketPath(t, "a")
	const liveName = "af_doctor-live-on-prior-socket"
	out, err := exec.Command("tmux", "-S", sockA, "new-session", "-d", "-s", liveName,
		"-e", tmux.EnvMarkerSession+"="+liveName, "-e", tmux.EnvMarkerHome+"="+home,
		"sleep", "300").CombinedOutput()
	require.NoError(t, err, "tmux new-session on server A: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", sockA, "kill-server").Run() })
	serverAPID := tmuxServerPID(t, sockA)

	// The current daemon generation recorded socket B; socket A survives only
	// in prior_sockets.
	sockB := testguard.SocketPath(t, "b")
	writeServerStateJSON(t, home, fmt.Sprintf(
		`{"mode":"dedicated","socket":%q,"unit":"agent-factory-tmux-server-new","at":%q,"daemon_pid":%d,"prior_sockets":[%q]}`,
		sockB, time.Now().Format(time.RFC3339), os.Getpid(), sockA))

	victim := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: liveName,
		tmux.EnvMarkerHome:    home,
		"TMUX":                fmt.Sprintf("%s,%d,0", sockA, serverAPID),
	})

	opts := testOptionsWithHome(t, home, true, victim.PID, serverAPID) // Fix: true
	report, err := Run(opts)
	require.NoError(t, err)

	require.True(t, alive(victim),
		"`af doctor --fix` killed a process whose session is live on a prior recorded socket "+
			"generation — a socket the daemon has run must be accounted for before absence "+
			"proves death (#6339)")

	for _, f := range findByCheck(report, "orphaned-process") {
		require.Empty(t, f.FixAction,
			"a session live on a prior recorded socket must not arm a kill: %s", f.Detail)
		require.False(t, f.Fixed, "a session live on a prior recorded socket must not be fixed: %s", f.Detail)
	}
	// The session reads LIVE through the prior-socket listing, so its marked
	// process is classified as escaped (report-only) — the same classification
	// a live session's out-of-pane process gets on the current socket.
	escaped := findByCheck(report, "escaped-process")
	require.NotEmpty(t, escaped,
		"a process of a session live on a prior socket must be reported as escaped, not orphaned")
	for _, f := range escaped {
		require.Empty(t, f.FixAction, "an escaped process of a live session is report-only")
	}
}

// TestPriorRecordedSocket_UnaccountableGeneration_BlindsRun is the fail-closed
// half: a prior socket generation that cannot be listed (wedged server,
// unreachable socket) is a generation doctor cannot account for, so NO absence
// it computes is evidence — not even for a genuinely dead session on the
// current socket. The whole run goes blind rather than kill on partial
// evidence: the tmux-inspection row reports the blindness and the kill stays
// holstered.
func TestPriorRecordedSocket_UnaccountableGeneration_BlindsRun(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)

	sockB := testguard.SocketPath(t, "b")
	sockWedged := testguard.SocketPath(t, "wedged")

	// A daemon IS serving this home and owns the record, so the record is
	// current — the failure here is purely the unaccountable prior socket.
	daemonProc := spawnWithEnv(t, "af", []string{"--daemon"}, nil)
	stubDaemonProcessArgv(t, func(pid int) []string {
		if pid == daemonProc.PID {
			return []string{"af", "--daemon"}
		}
		return nil
	})
	stubProcessHomes(t, map[int]string{daemonProc.PID: home})

	writeServerStateJSON(t, home, fmt.Sprintf(
		`{"mode":"dedicated","socket":%q,"unit":"agent-factory-tmux-server-new","at":%q,"daemon_pid":%d,"prior_sockets":[%q]}`,
		sockB, time.Now().Format(time.RFC3339), daemonProc.PID, sockWedged))

	// A genuinely dead session's process — under --fix the only question is
	// whether the kill arm may fire while a socket generation is unaccounted.
	orphan := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: "af_doctor-genuine-orphan",
		tmux.EnvMarkerHome:    home,
	})

	opts := testOptionsWithHome(t, home, true, daemonProc.PID, orphan.PID) // Fix: true
	opts.Exec = cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error { return nil },
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			args := c.Args
			if len(args) > 3 && args[1] == "-S" && args[3] == "ls" {
				switch args[2] {
				case sockWedged:
					return nil, fmt.Errorf("server on %s is wedged", sockWedged)
				case sockB:
					return []byte(""), nil // nothing live on the current socket
				}
				return nil, fmt.Errorf("unpinned socket listed: %s", args[2])
			}
			return []byte(""), nil
		},
	}

	report, err := Run(opts)
	require.NoError(t, err)

	require.True(t, alive(orphan),
		"`af doctor --fix` killed a process while a recorded tmux socket generation "+
			"could not be listed — an unaccounted generation voids every absence it computes (#6339)")
	require.Empty(t, findByCheck(report, "orphaned-process"),
		"nothing may be classified as orphaned while a recorded socket generation is unaccountable")
	rows := findCheckRows(report, "tmux-inspection")
	require.Len(t, rows, 1)
	require.Equal(t, StatusFail, rows[0].Status,
		"the unaccountable socket generation must surface as blindness, not silence: %s", rows[0].Detail)
}

// TestRecordWrittenByOlderDaemonGeneration_RefusesKillWithoutTmuxEnv pins the
// generation binding: a daemon serving this home whose pid does NOT match the
// record's daemon_pid means the record was written by a previous generation —
// sessions may have moved to a socket it does not name. For a marked process
// with no TMUX env to corroborate itself against, the recorded listing alone
// cannot prove death, so the kill arm refuses.
func TestRecordWrittenByOlderDaemonGeneration_RefusesKillWithoutTmuxEnv(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)

	daemonProc := spawnWithEnv(t, "af", []string{"--daemon"}, nil)
	stubDaemonProcessArgv(t, func(pid int) []string {
		if pid == daemonProc.PID {
			return []string{"af", "--daemon"}
		}
		return nil
	})
	stubProcessHomes(t, map[int]string{daemonProc.PID: home})

	// The record names the daemon's ambient socket but carries a DIFFERENT
	// generation's pid — the stale-authorship shape an adopt-without-rewrite
	// leaves behind.
	sockAmbient := filepath.Join(os.Getenv("TMUX_TMPDIR"), fmt.Sprintf("tmux-%d/default", os.Getuid()))
	writeServerStateJSON(t, home, fmt.Sprintf(
		`{"mode":"dedicated","socket":%q,"unit":"agent-factory-tmux-server-old","at":%q,"daemon_pid":%d}`,
		sockAmbient, time.Now().Format(time.RFC3339), daemonProc.PID+424242))

	victim := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: "af_doctor-gone-session",
		tmux.EnvMarkerHome:    home,
	})

	opts := testOptionsWithHome(t, home, true, daemonProc.PID, victim.PID) // Fix: true
	report, err := Run(opts)
	require.NoError(t, err)

	require.True(t, alive(victim),
		"`af doctor --fix` killed a process on the strength of a record a previous daemon "+
			"generation wrote — the record could not be proven current (#6339)")

	orphanFindings := findByCheck(report, "orphaned-process")
	require.NotEmpty(t, orphanFindings,
		"the unprovable orphan must stay VISIBLE (reported, not killed) so the run does not report clean")
	for _, f := range orphanFindings {
		require.Empty(t, f.FixAction,
			"a record that cannot be proven current must not arm a kill: %s", f.Detail)
		require.False(t, f.Fixed, "an uncorroborated orphan must not be fixed: %s", f.Detail)
	}
}

// TestRecordWithoutDaemonBinding_LiveDaemon_RefusesKillWithoutTmuxEnv is the
// upgrade half of record compatibility: a record written before daemon_pid
// existed decodes with no generation binding, and while a live daemon serves
// the home that record cannot be proven its work — a previous generation may
// have run other sockets. Doctor reads the record, reports the process, and
// refuses the kill: merely more cautious, never broken (#6339).
func TestRecordWithoutDaemonBinding_LiveDaemon_RefusesKillWithoutTmuxEnv(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)

	daemonProc := spawnWithEnv(t, "af", []string{"--daemon"}, nil)
	stubDaemonProcessArgv(t, func(pid int) []string {
		if pid == daemonProc.PID {
			return []string{"af", "--daemon"}
		}
		return nil
	})
	stubProcessHomes(t, map[int]string{daemonProc.PID: home})

	sockAmbient := filepath.Join(os.Getenv("TMUX_TMPDIR"), fmt.Sprintf("tmux-%d/default", os.Getuid()))
	// No daemon_pid: the exact record shape every pre-fix daemon leaves.
	writeServerStateJSON(t, home, fmt.Sprintf(
		`{"mode":"dedicated","socket":%q,"unit":"agent-factory-tmux-server-old","at":%q}`,
		sockAmbient, time.Now().Format(time.RFC3339)))

	victim := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: "af_doctor-gone-session",
		tmux.EnvMarkerHome:    home,
	})

	opts := testOptionsWithHome(t, home, true, daemonProc.PID, victim.PID) // Fix: true
	report, err := Run(opts)
	require.NoError(t, err)

	require.True(t, alive(victim),
		"`af doctor --fix` killed a process on the strength of a record that predates "+
			"generation binding — an unstamped record under a live daemon cannot be "+
			"proven current (#6339)")
	for _, f := range findByCheck(report, "orphaned-process") {
		require.Empty(t, f.FixAction,
			"an unstamped record must not arm a kill while a daemon is live: %s", f.Detail)
		require.False(t, f.Fixed)
	}
}

// TestRecordWithoutDaemonBinding_DeadDaemon_RefusesKillWithoutTmuxEnv is the
// same refusal for a record that predates generation binding entirely: with no
// live daemon to author it and no daemon_pid to trust, the record's socket set
// cannot be proven complete — a prior socket generation could still host live
// sessions the record never named.
func TestRecordWithoutDaemonBinding_DeadDaemon_RefusesKillWithoutTmuxEnv(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)

	sockAmbient := filepath.Join(os.Getenv("TMUX_TMPDIR"), fmt.Sprintf("tmux-%d/default", os.Getuid()))
	// No daemon_pid: the shape every pre-fix daemon's record has.
	writeServerStateJSON(t, home, fmt.Sprintf(
		`{"mode":"dedicated","socket":%q,"unit":"agent-factory-tmux-server-old","at":%q}`,
		sockAmbient, time.Now().Format(time.RFC3339)))

	victim := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: "af_doctor-gone-session",
		tmux.EnvMarkerHome:    home,
	})

	opts := testOptionsWithHome(t, home, true, victim.PID) // Fix: true
	report, err := Run(opts)
	require.NoError(t, err)

	require.True(t, alive(victim),
		"`af doctor --fix` killed a process on the strength of a record that cannot account "+
			"for the daemon's socket generations (#6339)")
	for _, f := range findByCheck(report, "orphaned-process") {
		require.Empty(t, f.FixAction,
			"a record without daemon binding must not arm a kill for a process with no TMUX env: %s", f.Detail)
		require.False(t, f.Fixed)
	}
}

// TestRecordWrittenByLiveDaemon_GenerationMatch_StillKills is the
// no-regression direction for the generation gate: when the record's
// daemon_pid matches a daemon serving this home and every prior socket
// generation lists cleanly, a marked process with no TMUX env whose session is
// absent from every socket the daemon has used IS a proven orphan — --fix must
// still take it.
func TestRecordWrittenByLiveDaemon_GenerationMatch_StillKills(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)

	daemonProc := spawnWithEnv(t, "af", []string{"--daemon"}, nil)
	stubDaemonProcessArgv(t, func(pid int) []string {
		if pid == daemonProc.PID {
			return []string{"af", "--daemon"}
		}
		return nil
	})
	stubProcessHomes(t, map[int]string{daemonProc.PID: home})

	// The prior socket names a path no server ever took: its listing answers
	// tmux's definitive no-server empty, so it is accounted for.
	sockAmbient := filepath.Join(os.Getenv("TMUX_TMPDIR"), fmt.Sprintf("tmux-%d/default", os.Getuid()))
	deadPrior := testguard.SocketPath(t, "prior-dead")
	writeServerStateJSON(t, home, fmt.Sprintf(
		`{"mode":"dedicated","socket":%q,"unit":"agent-factory-tmux-server-new","at":%q,"daemon_pid":%d,"prior_sockets":[%q]}`,
		sockAmbient, time.Now().Format(time.RFC3339), daemonProc.PID, deadPrior))

	const deadName = "af_doctor-genuine-orphan"
	orphan := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: deadName,
		tmux.EnvMarkerHome:    home,
	})

	opts := testOptionsWithHome(t, home, true, daemonProc.PID, orphan.PID) // Fix: true
	report, err := Run(opts)
	require.NoError(t, err)

	findings := findByCheck(report, "orphaned-process")
	require.Len(t, findings, 1)
	require.NotEmpty(t, findings[0].FixAction,
		"a genuine orphan on a current, fully-accounted record must still arm the kill")
	require.True(t, findings[0].Fixed, "fix outcome: %v", findings[0].FixErr)
	require.Eventually(t, func() bool { return !alive(orphan) }, 5*time.Second, 25*time.Millisecond,
		"a genuine orphan whose record and prior sockets are all accounted for must still be killed")
}

// TestTruncatedSocketHistory_RecordOnlyProof_RefusesKill pins the bound's
// other half: the record says its own history is incomplete. prior_sockets is
// capped, and a socket generation evicted by the cap may still host the
// session a TMUX-less process's absence would otherwise convict — so even a
// current, daemon-stamped record with history_truncated set cannot corroborate
// a record-only death proof. --fix must report the process and holster the
// kill.
func TestTruncatedSocketHistory_RecordOnlyProof_RefusesKill(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)

	daemonProc := spawnWithEnv(t, "af", []string{"--daemon"}, nil)
	stubDaemonProcessArgv(t, func(pid int) []string {
		if pid == daemonProc.PID {
			return []string{"af", "--daemon"}
		}
		return nil
	})
	stubProcessHomes(t, map[int]string{daemonProc.PID: home})

	sockAmbient := filepath.Join(os.Getenv("TMUX_TMPDIR"), fmt.Sprintf("tmux-%d/default", os.Getuid()))
	deadPrior := testguard.SocketPath(t, "prior-dead")
	writeServerStateJSON(t, home, fmt.Sprintf(
		`{"mode":"dedicated","socket":%q,"unit":"agent-factory-tmux-server-new","at":%q,"daemon_pid":%d,"prior_sockets":[%q],"history_truncated":true}`,
		sockAmbient, time.Now().Format(time.RFC3339), daemonProc.PID, deadPrior))

	orphan := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: "af_doctor-maybe-evicted-session",
		tmux.EnvMarkerHome:    home,
	})

	opts := testOptionsWithHome(t, home, true, daemonProc.PID, orphan.PID) // Fix: true
	report, err := Run(opts)
	require.NoError(t, err)

	require.True(t, alive(orphan),
		"`af doctor --fix` killed a process on a record that ADMITS its socket history is "+
			"incomplete — a dropped generation may still host the session (#6339)")
	for _, f := range findByCheck(report, "orphaned-process") {
		require.Empty(t, f.FixAction,
			"a truncated socket history must not arm a record-only kill: %s", f.Detail)
		require.False(t, f.Fixed)
	}
}

// TestSameNamedSession_TwoSockets_BothHomesClaimed pins the union's
// (name, socket) coverage: session names derive from repo path and title, not
// AF home, so two socket generations can legitimately host a same-named
// session claiming DIFFERENT homes. The union must read the ownership marker
// for every pair — deduplicating by name would drop one session's claim, and
// that claim is the veto standing between a stale home and rm -rf.
func TestSameNamedSession_TwoSockets_BothHomesClaimed(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)

	claimedByPrior := testguard.SocketTempDir(t)
	claimedByCurrent := testguard.SocketTempDir(t)

	const name = "af_doctor-shared-name"
	sockA := testguard.SocketPath(t, "a")
	out, err := exec.Command("tmux", "-S", sockA, "new-session", "-d", "-s", name,
		"-e", tmux.EnvMarkerSession+"="+name, "-e", tmux.EnvMarkerHome+"="+claimedByPrior,
		"sleep", "300").CombinedOutput()
	require.NoError(t, err, "tmux new-session on server A: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", sockA, "kill-server").Run() })

	sockB := testguard.SocketPath(t, "b")
	out, err = exec.Command("tmux", "-S", sockB, "new-session", "-d", "-s", name,
		"-e", tmux.EnvMarkerSession+"="+name, "-e", tmux.EnvMarkerHome+"="+claimedByCurrent,
		"sleep", "300").CombinedOutput()
	require.NoError(t, err, "tmux new-session on server B: %s", out)
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", sockB, "kill-server").Run() })

	writeServerStateJSON(t, home, fmt.Sprintf(
		`{"mode":"dedicated","socket":%q,"unit":"agent-factory-tmux-server-new","at":%q,"prior_sockets":[%q]}`,
		sockB, time.Now().Format(time.RFC3339), sockA))

	ctx, err := newScanContext(testOptionsWithHome(t, home, false))
	require.NoError(t, err)
	homes, err := liveTmuxHomes(ctx)
	require.NoError(t, err)
	require.True(t, homes[normalizeHome(claimedByPrior)],
		"the session copy on the prior socket claims %s — dropping the pair loses that veto", claimedByPrior)
	require.True(t, homes[normalizeHome(claimedByCurrent)],
		"the session copy on the current socket claims %s", claimedByCurrent)
}

// TestProcessTmuxSocketUnlistable_AbsentServerPid_RefusesKill pins the
// corroboration rule for the per-process TMUX arm: the serverPID half of a
// process's TMUX env value can only prove the named server dead when the pid
// is present and no longer tmux's. A pid ABSENT from the snapshot — a live
// server hidden by /proc read failures or a pid namespace looks identical —
// is not death evidence, and when the socket the env value names cannot be
// listed either, nothing is proven: report, never kill.
func TestProcessTmuxSocketUnlistable_AbsentServerPid_RefusesKill(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)

	wedged := testguard.SocketPath(t, "wedged")
	victim := spawnWithEnv(t, "sh", nil, map[string]string{
		tmux.EnvMarkerSession: "af_doctor-hidden-server-session",
		tmux.EnvMarkerHome:    home,
		// The serverPID is absent from the snapshot — as a live server
		// outside doctor's view would be — and its socket cannot be listed.
		"TMUX": fmt.Sprintf("%s,%d,0", wedged, 1<<30),
	})

	opts := testOptionsWithHome(t, home, true, victim.PID) // Fix: true
	opts.Exec = cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error { return nil },
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			args := c.Args
			if len(args) > 3 && args[1] == "-S" && args[3] == "ls" && args[2] == wedged {
				return nil, fmt.Errorf("server on %s is wedged", wedged)
			}
			return []byte(""), nil
		},
	}

	report, err := Run(opts)
	require.NoError(t, err)

	require.True(t, alive(victim),
		"`af doctor --fix` killed a process whose TMUX env named a server doctor could not "+
			"reach and whose pid it could not observe — absence from the snapshot is not "+
			"death evidence (#6339)")
	for _, f := range findByCheck(report, "orphaned-process") {
		require.Empty(t, f.FixAction,
			"an unobservable server pid plus an unlistable socket must not arm a kill: %s", f.Detail)
		require.False(t, f.Fixed)
	}
}

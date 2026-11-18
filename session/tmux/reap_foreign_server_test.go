package tmux

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/stretchr/testify/require"
)

// spawnOrphanedMarkedCarrier starts a marked escapee REPARENTED ABOVE the
// test reaper — an intermediate sh backgrounds the reexec'd binary and exits,
// exactly the spawnMarkedProcess harness — so the carrier's ancestry ends at
// init and reaches the decision table the way #7469's detached carrier does:
// no protected placement, no tmux boundary. bin selects the carrier's
// executable identity (os.Args[0] for the plain case, an agent-named link for
// the AgentExe row); extraEnv carries the foreign server's TMUX tuple.
func spawnOrphanedMarkedCarrier(t *testing.T, name, home, gen, bin string, extraEnv ...string) proctree.Process {
	t.Helper()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "carrier.pid")
	// A CLEAN env, not os.Environ() — processEnvValue returns the FIRST match
	// and this test binary runs inside a real af session, so the ambient
	// AF_SESSION would win over the fixture's (spawnMarkedProcess, #7435 R2).
	env := []string{"PATH=" + os.Getenv("PATH"),
		EnvMarkerSession + "=" + name,
		EnvMarkerHome + "=" + home,
		EnvMarkerGeneration + "=" + gen,
	}
	env = append(env, extraEnv...)
	c := exec.Command("sh", "-c",
		"'"+bin+"' '"+markedEscapeeArg+"' '"+pidFile+"' </dev/null >/dev/null 2>&1 &")
	c.Env = env
	require.NoError(t, c.Run())
	waitForPIDFile(t, pidFile)
	marked := processFromPIDFile(t, pidFile)
	t.Cleanup(func() { _ = proctree.Signal(marked, syscall.SIGKILL) })
	require.NotEqual(t, os.Getpid(), marked.PPID,
		"the carrier must be reparented above the reaper — inside its subtree the exclusion verdict, not the server verdict, answers")
	// The scan reads /proc/<pid>/environ; wait until the marker is visible.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if env, err := proctree.Environ(marked.PID); err == nil {
			if v, ok := processEnvValue(env, EnvMarkerSession); ok && v == name {
				return marked
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Skipf("carrier %d environ never became readable; this fixture needs a marker-visible process", marked.PID)
	return proctree.Process{}
}

func foreignSocketFile(t *testing.T) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "user-tmux.sock")
	require.NoError(t, os.WriteFile(sock, nil, 0o600))
	return sock
}

func recordedOwnServer(t *testing.T) ServerIdentity {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "af-tmux.sock")
	require.NoError(t, os.WriteFile(sock, nil, 0o600))
	// PIDNS carries the real pid namespace — the shape a record has after
	// the stamp proved the pid holds the socket HERE (#7581 item 1). A
	// fixture without it would be an unproven record and every pid-derived
	// compare would read undecidable.
	ns, _ := proctree.PIDNamespaceID()
	return ServerIdentity{Socket: sock, PID: 4242, StartID: 987654321, SessionID: "$7", PIDNS: ns}
}

// #7469 acceptance: a DETACHED carrier whose TMUX env names a server that is
// not the recorded one — the user's own tmux server started inside the pane —
// is proven foreign: never signalled, named at WARNING, and NOT a blocking
// refusal, so teardown proceeds while it stays alive. The carrier reaches the
// sweep's unproven tier — no recorded pair, no membership — so the foreign
// arm itself is what excludes it; the admitted sibling alongside is what
// keeps the verdict non-blocking.
func TestVanishedSessionSweep_NeverSignalsAForeignServerCarrier(t *testing.T) {
	name := "af_foreign_server"
	home := t.TempDir()
	gen := name + "-gen"
	own := recordedOwnServer(t)

	carrier := spawnOrphanedMarkedCarrier(t, name, home, gen, os.Args[0],
		"TMUX="+foreignSocketFile(t)+",9999,3")
	// A second carrier today's rules already admit: its recorded pair makes
	// the sweep's answer "some members were reaped" — the verdict that lets
	// teardown proceed while the foreign carrier stays behind.
	sibling := spawnOrphanedMarkedCarrier(t, name, home, gen, os.Args[0])

	var warnings bytes.Buffer
	oldWarnings := log.WarningLog.Writer()
	log.WarningLog.SetOutput(&warnings)
	t.Cleanup(func() { log.WarningLog.SetOutput(oldWarnings) })

	membership := cohortMembership{
		Recorded:  map[int]uint64{sibling.PID: sibling.StartID},
		OwnServer: own,
	}
	marked, _, err := markedOrphanProcesses(
		[]proctree.Process{carrier, sibling}, name, home, fixtureCohort(gen), membership, nil, nil)
	require.NoError(t, err,
		"a positively-foreign carrier is attributed — it is named, not a blocking refusal")
	require.Equal(t, []proctree.Process{sibling}, marked,
		"the foreign-server carrier is never eligible for signals")
	require.Contains(t, warnings.String(), strconv.Itoa(carrier.PID))
	require.Contains(t, warnings.String(), "foreign tmux server")
	require.Contains(t, warnings.String(), "LEFT ALIVE")
	require.True(t, proctree.AliveSame(carrier), "the carrier survives the sweep")
	require.True(t, proctree.AliveSame(sibling), "the sweep only marks; nothing signals in a fixture")
}

// #7469: even a carrier that PASSES tier-2's executable identity (a
// claude-comm carrier, the #3699 look-alike) is refused when a foreign server
// spawned it — the allowlist may admit only what the server attribution does
// not place elsewhere. The carrier is UNPROVEN — no recorded pair — so its
// admission runs through tier 2's AgentExe arm, exactly where the foreign
// verdict must beat it.
func TestVanishedSessionSweep_ForeignBeatsAgentExeIdentity(t *testing.T) {
	name := "af_foreign_exe"
	home := t.TempDir()
	gen := name + "-gen"
	own := recordedOwnServer(t)

	carrier := spawnOrphanedMarkedCarrier(t, name, home, gen,
		agentNamedBinary(t, os.Args[0]),
		"TMUX="+foreignSocketFile(t)+",9999,3")
	sibling := spawnOrphanedMarkedCarrier(t, name, home, gen, os.Args[0])

	membership := cohortMembership{
		Recorded:  map[int]uint64{sibling.PID: sibling.StartID},
		Root:      proctree.Process{PID: sibling.PPID, StartID: 1},
		AgentExe:  "claude",
		OwnServer: own,
	}
	marked, _, err := markedOrphanProcesses(
		[]proctree.Process{carrier, sibling}, name, home, fixtureCohort(gen), membership, nil, nil)
	require.NoError(t, err)
	require.Equal(t, []proctree.Process{sibling}, marked,
		"an AgentExe-matching carrier on a foreign server is left alive, never signalled")
	require.True(t, proctree.AliveSame(carrier))
}

// #3699 must not regress: a detached carrier whose TMUX env is MISSING (the
// re-host scrubs tmux's own env) and that holds no server-mastered pty stays
// undecidable — which is today's verdict, so the arm changes nothing for it.
func TestVanishedSessionSweep_NoServerEvidenceKeepsTodaysVerdict(t *testing.T) {
	name := "af_undecidable"
	home := t.TempDir()
	gen := name + "-gen"

	// No TMUX env, no pty — undecidable. With no membership evidence at all
	// the gate refuses as before; with a recorded pair it admits as before.
	carrier := spawnOrphanedMarkedCarrier(t, name, home, gen, os.Args[0])

	marked, _, err := markedOrphanProcesses(
		[]proctree.Process{carrier}, name, home, fixtureCohort(gen),
		cohortMembership{OwnServer: recordedOwnServer(t)}, nil, nil)
	require.Error(t, err, "no kernel evidence at all still refuses every candidate (#7435 R2)")
	require.Empty(t, marked)
	require.True(t, proctree.AliveSame(carrier))

	marked, _, err = markedOrphanProcesses(
		[]proctree.Process{carrier}, name, home, fixtureCohort(gen),
		cohortMembership{
			Recorded:  map[int]uint64{carrier.PID: carrier.StartID},
			OwnServer: recordedOwnServer(t),
		}, nil, nil)
	require.NoError(t, err)
	require.Equal(t, []proctree.Process{carrier}, marked,
		"an undecidable carrier keeps its existing admission — undecidable is never foreign")
}

// #7469 R1: a STALE server record — one predating the session's current
// incarnation, the shape a respawn-onto-new-server leaves when its stamp
// fails — must never turn the session's own survivors foreign. The carrier
// here carries an own-TMUX tuple (the recorded socket, the recorded pid)
// against a stale record naming an older server pid on the same socket:
// with proven membership evidence it is reaped exactly as before.
func TestVanishedSessionSweep_StaleRecordCannotExcludeAProvenSurvivor(t *testing.T) {
	name := "af_stale_server"
	home := t.TempDir()
	gen := name + "-gen"

	sock := filepath.Join(t.TempDir(), "af-tmux.sock")
	require.NoError(t, os.WriteFile(sock, nil, 0o600))
	// The record a respawn left behind: server pid 1111 answered the previous
	// incarnation; the incumbent server answering the pane now is 2222.
	stale := ServerIdentity{Socket: sock, PID: 1111, StartID: 1, SessionID: "$7"}

	carrier := spawnOrphanedMarkedCarrier(t, name, home, gen, os.Args[0],
		"TMUX="+sock+",2222,3")

	marked, _, err := markedOrphanProcesses(
		[]proctree.Process{carrier}, name, home, fixtureCohort(gen),
		cohortMembership{
			Recorded:  map[int]uint64{carrier.PID: carrier.StartID},
			OwnServer: stale,
		}, nil, nil)
	require.NoError(t, err)
	require.Equal(t, []proctree.Process{carrier}, marked,
		"a kernel-proven member is never overridden by server attribution — stale or fresh (R4)")
	require.True(t, proctree.AliveSame(carrier), "the sweep only marks in a fixture")
}

// #7469 R1/R3: an own verdict NEVER promotes. A carrier that reads own
// against the recorded server — same socket, same server pid, same session
// index — but that today's evidence rules refuse is refused exactly as
// before: the gate still blocks, nothing is marked, the carrier stays
// alive. The fixture's verdict is asserted OWN first — a tuple whose
// session index missed the record would read undecidable and the refusal
// assertion would be vacuous (R3b).
func TestVanishedSessionSweep_OwnServerCarrierKeepsTodaysRefusal(t *testing.T) {
	name := "af_own_server"
	home := t.TempDir()
	gen := name + "-gen"
	own := recordedOwnServer(t)

	// Isolate the env channel: the carrier must not inherit a real pty a
	// dev box's own tmux server masters, or the pty channel could speak
	// foreign over the env's own.
	oldSlaves := ptySlaveIndices
	ptySlaveIndices = func(int) ([]proctree.PtyIndex, error) { return nil, nil }
	t.Cleanup(func() { ptySlaveIndices = oldSlaves })

	carrier := spawnOrphanedMarkedCarrier(t, name, home, gen, os.Args[0],
		"TMUX="+own.Socket+","+strconv.Itoa(own.PID)+",7")

	environ, err := proctree.Environ(carrier.PID)
	require.NoError(t, err)
	snap, err := proctree.Snapshot()
	require.NoError(t, err)
	require.Equal(t, serverVerdictOwn,
		carrierServerVerdict(carrier, environ, own, snap, &serverPtyOwners{}, own.PIDNS),
		"the fixture must read OWN — an undecidable verdict makes the refusal assertion vacuous")

	marked, _, err := markedOrphanProcesses(
		[]proctree.Process{carrier}, name, home, fixtureCohort(gen),
		cohortMembership{OwnServer: own}, nil, nil)
	require.Error(t, err,
		"an own verdict is not evidence — the gate still refuses a member it cannot prove")
	require.Empty(t, marked, "own never promotes a refused carrier into the reap set")
	require.True(t, proctree.AliveSame(carrier))
}

// #7469 R1a: retirement is keyed to "a new incarnation can exist" — the
// new-session issue inside start() — never to Start's success. A respawn
// that issues new-session and then fails leaves NO server evidence: a
// stale (socket, old server pid) surviving the failure would judge the
// replacement's own survivors foreign — named, left alive, worktree
// deleted under them. This goes red if the retire moves back to the
// success path: with it removed, the seeded record outlives the failed
// spawn.
func TestStartFailedAfterNewSession_RetiresServerIdentity(t *testing.T) {
	forceNewSessionEnvMarkers(t, false)
	ts := NewTmuxSessionWithDeps("retire-on-failed-spawn", "sh",
		answeredFailurePtyFactory{t: t, waitErr: errors.New("new-session refused the request")},
		cmd_test.MockCmdExec{
			RunFunc:    func(*exec.Cmd) error { return errors.New("session not found") },
			OutputFunc: func(*exec.Cmd) ([]byte, error) { return nil, nil },
		})
	ts.SeedServerEvidence("/tmp/tmux-1001/af", 1111, 987654321, "$7", "pid:[4026531836]")
	_, ok := ts.ServerEvidence()
	require.True(t, ok, "the seed stamps the previous incarnation's record")

	require.Error(t, ts.Start(t.TempDir()),
		"the staged launch command fails AFTER new-session issued")
	_, ok = ts.ServerEvidence()
	require.False(t, ok,
		"a spawn that issues new-session retires the record even when Start then fails — absent is undecidable")
	require.True(t, ts.ServerEvidenceRetired(),
		"the retired marker stays raised so the row writes absent until a stamp or seed lands")
}

// #7469 R1a/R3: an UNPROVEN carrier that reads OWN through the env channel
// still reaps at tier 2 — the outcome the stale-record bug inverts: under
// a stale (socket, old pid) record the same carrier reads foreign — named,
// left alive, worktree deleted under it — while a retired (absent) record
// reads undecidable and tier 2's AgentExe admission stands. The verdict is
// asserted own first so the outcome can never drift vacuous.
func TestVanishedSessionSweep_OwnServerCarrierStillReapedAtTier2(t *testing.T) {
	name := "af_own_tier2"
	home := t.TempDir()
	gen := name + "-gen"
	own := recordedOwnServer(t)

	// Isolate the env channel: the carrier must not inherit a real pty a
	// dev box's own tmux server masters, or the pty channel could speak
	// over the env's own.
	oldSlaves := ptySlaveIndices
	ptySlaveIndices = func(int) ([]proctree.PtyIndex, error) { return nil, nil }
	t.Cleanup(func() { ptySlaveIndices = oldSlaves })

	carrier := spawnOrphanedMarkedCarrier(t, name, home, gen,
		agentNamedBinary(t, os.Args[0]),
		"TMUX="+own.Socket+","+strconv.Itoa(own.PID)+",7")

	environ, err := proctree.Environ(carrier.PID)
	require.NoError(t, err)
	snap, err := proctree.Snapshot()
	require.NoError(t, err)
	require.Equal(t, serverVerdictOwn,
		carrierServerVerdict(carrier, environ, own, snap, &serverPtyOwners{}, own.PIDNS),
		"the fixture must read OWN — a foreign or undecidable verdict would make the outcome vacuous")

	marked, _, err := markedOrphanProcesses(
		[]proctree.Process{carrier}, name, home, fixtureCohort(gen),
		cohortMembership{
			Root:      proctree.Process{PID: carrier.PPID, StartID: 1},
			AgentExe:  "claude",
			OwnServer: own,
		}, nil, nil)
	require.NoError(t, err)
	require.Equal(t, []proctree.Process{carrier}, marked,
		"an unproven own-server carrier keeps its tier-2 admission — own never demotes either")
}

// #7581 item 1 (outcome): a carrier judged against a record whose pid was
// never proven in this namespace keeps master's verdict EXACTLY — the same
// admit-or-refuse answer an absent record produces. This is the only
// direction the namespace gate can move a verdict toward signalling: a
// cross-namespace pid mismatch was never a positive foreign proof, so the
// carrier goes back to whatever today's rules give it.
func TestVanishedSessionSweep_UnprovenPidKeepsMastersVerdict(t *testing.T) {
	name := "af_unproven_ns"
	home := t.TempDir()
	gen := name + "-gen"

	sock := filepath.Join(t.TempDir(), "af-tmux.sock")
	require.NoError(t, os.WriteFile(sock, nil, 0o600))
	unproven := ServerIdentity{Socket: sock, PID: 4242, StartID: 9, SessionID: "$7"} // PIDNS ""

	// The carrier's env names the recorded socket but a DIFFERENT server
	// pid — foreign under a proven record, undecidable under an unproven
	// one. Isolate the pty channel so the env arm is what speaks.
	oldSlaves := ptySlaveIndices
	ptySlaveIndices = func(int) ([]proctree.PtyIndex, error) { return nil, nil }
	t.Cleanup(func() { ptySlaveIndices = oldSlaves })
	carrier := spawnOrphanedMarkedCarrier(t, name, home, gen,
		agentNamedBinary(t, os.Args[0]), "TMUX="+sock+",9999,3")

	membership := cohortMembership{
		Root:     proctree.Process{PID: carrier.PPID, StartID: 1},
		AgentExe: "claude",
	}
	withRecord, _, errWith := markedOrphanProcesses(
		[]proctree.Process{carrier}, name, home, fixtureCohort(gen),
		cohortMembership{
			Root:      membership.Root,
			AgentExe:  membership.AgentExe,
			OwnServer: unproven,
		}, nil, nil)
	withoutRecord, _, errWithout := markedOrphanProcesses(
		[]proctree.Process{carrier}, name, home, fixtureCohort(gen), membership, nil, nil)
	require.Equal(t, errWithout != nil, errWith != nil,
		"the unproven record must answer exactly like no record at all")
	require.Equal(t, withoutRecord, withRecord,
		"master's verdict for this carrier is the tier-2 admit — the unproven record must not change it")
	require.Equal(t, []proctree.Process{carrier}, withRecord,
		"master's verdict: the agent-identical carrier is admitted and reaped")
	require.True(t, proctree.AliveSame(carrier), "the sweep only marks in a fixture")
}

// #3699 pinned: the re-hosted claude keeps converging. A claude-comm carrier
// under a recorded root — no TMUX env, no pty, no foreign anything — still
// passes tier 2's executable identity with a server record standing beside
// it, and is reaped.
func TestVanishedSessionSweep_RehostedAgentStillConverges(t *testing.T) {
	name := "af_rehost_3699"
	home := t.TempDir()
	gen := name + "-gen"

	carrier := spawnOrphanedMarkedCarrier(t, name, home, gen,
		agentNamedBinary(t, os.Args[0]))

	marked, _, err := markedOrphanProcesses(
		[]proctree.Process{carrier}, name, home, fixtureCohort(gen),
		cohortMembership{
			Root:      proctree.Process{PID: carrier.PPID, StartID: 1},
			AgentExe:  "claude",
			OwnServer: recordedOwnServer(t),
		}, nil, nil)
	require.NoError(t, err)
	require.Equal(t, []proctree.Process{carrier}, marked,
		"the #3699 re-hosted claude is reaped exactly as before")
}

// #7469 R1: the retirement itself — a stamped record drops to absent, and
// absent reads undecidable, never stale. Every spawn boundary calls this
// before the re-stamp, so a failed stamp leaves the cell empty rather than
// carrying the previous incarnation's server into the new one's survivors.
func TestRetireServerIdentity_DropsTheRecord(t *testing.T) {
	s := &TmuxSession{}
	s.SeedServerEvidence("/tmp/sock", 4242, 987654321, "$7", "pid:[4026531836]")
	_, ok := s.ServerEvidence()
	require.True(t, ok, "the seed stamps when the cell is empty")

	s.RetireServerIdentity()
	_, ok = s.ServerEvidence()
	require.False(t, ok, "retired is absent — a failed re-stamp leaves undecidable, not stale")

	// And a fresh stamp can still land after the retire — retirement is a
	// boundary, not a tombstone.
	s.SeedServerEvidence("/tmp/sock", 5555, 111, "$8", "pid:[4026531836]")
	got, ok := s.ServerEvidence()
	require.True(t, ok)
	require.Equal(t, 5555, got.PID)
}

// #7469 R3 — the real-tmux close shape, CI only: a marked carrier spawned
// inside a SECOND, user-owned tmux server, detached to init, still holding
// its pane's pty slave and that server's TMUX tuple. Both attribution
// channels run against REAL servers — real socket paths, real pids, a real
// pty master — and the outcome-level verdict must keep the foreign carrier
// outside the reap set and alive, while the session's own member converges.
func TestVanishedSessionSweep_RealTmuxForeignServerCarrier(t *testing.T) {
	if os.Getenv("CI") == "" {
		t.Skip("the real-tmux probe runs in CI only — it drives two private tmux servers")
	}
	testguard.IsolateTmux(t)

	name := "af_foreign_rt"
	home := t.TempDir()
	gen := name + "-gen"

	// The session's OWN server — the default socket IsolateTmux pins inside
	// the private dir; its real pid/socket_path become the recorded
	// identity, resolved to kernel fields exactly like the spawn stamp.
	sessName := "af_own_rt"
	require.NoError(t, exec.Command("tmux", "new-session", "-d", "-s",
		sessName, "sleep 300").Run())
	t.Cleanup(func() {
		_ = exec.Command("tmux", "kill-session", "-t", ExactTarget(sessName)).Run()
	})
	out, err := exec.Command("tmux", "display-message", "-p", "-t",
		ExactTarget(sessName), "#{pid} #{socket_path}").CombinedOutput()
	require.NoError(t, err, string(out))
	ownPID, ownSock, err := parseServerSocketReply(string(out))
	require.NoError(t, err)
	own := resolveServerIdentity(ServerIdentity{Socket: ownSock, PID: ownPID})
	require.True(t, own.valid(), "a real server answers with a complete identity")

	// The FOREIGN server — the user's own tmux started inside a pane, the
	// #7469 shape. Its pane exports the AF markers and spawns a carrier in a
	// self-exiting subshell — ( cmd & ) is the portable double-detach; there
	// is no setsid binary on darwin — so the carrier reparents to init while
	// keeping the pane's slave pty as its stdio and the server's TMUX tuple.
	// The pane then sleeps so the foreign masters stay held.
	dir := testguard.SocketTempDir(t)
	fsock := filepath.Join(dir, "f.sock")
	pidFile := filepath.Join(dir, "carrier.pid")
	pane := "export " + EnvMarkerSession + "=" + name +
		" " + EnvMarkerHome + "=" + home +
		" " + EnvMarkerGeneration + "=" + gen +
		"; ( sh -c 'echo $$ > " + pidFile + "; exec sleep 300' </dev/null & )" +
		"; exec sleep 300"
	require.NoError(t, exec.Command("tmux", "-S", fsock, "-f", "/dev/null",
		"new-session", "-d", "-s", "foreign", "sh", "-c", pane).Run())
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", fsock, "kill-server").Run() })
	waitForPIDFile(t, pidFile)
	carrier := processFromPIDFile(t, pidFile)
	t.Cleanup(func() { _ = proctree.Signal(carrier, syscall.SIGKILL) })
	require.Equal(t, 1, carrier.PPID,
		"the carrier must be init-reparented — the detached shape #7469 reports")

	// The attribution channels themselves, on real evidence — asserted
	// INDEPENDENTLY so the claim stays honest: the carrier's env tuple
	// names a socket that is not the recorded one, AND its held slave is
	// mastered by a server pid that is not the recorded one.
	environ, err := proctree.Environ(carrier.PID)
	require.NoError(t, err)
	snap, err := proctree.Snapshot()
	require.NoError(t, err)
	owners := &serverPtyOwners{}
	tmuxEnv, hasTmux := processEnvValue(environ, tmuxEnvVar)
	require.True(t, hasTmux, "the detached carrier still holds its pane's TMUX tuple")
	require.Equal(t, serverVerdictForeign,
		tmuxEnvServerVerdict(tmuxEnv, own, own.PIDNS),
		"env channel: a foreign socket in TMUX")
	require.Equal(t, serverVerdictForeign,
		ptyFdServerVerdict(carrier.PID, own, snap, owners, own.PIDNS),
		"pty channel: the carrier's slave is mastered by the foreign server")
	require.Equal(t, serverVerdictForeign,
		carrierServerVerdict(carrier, environ, own, snap, owners, own.PIDNS),
		"real evidence attributes the carrier to the foreign server")

	// And the outcome: the carrier is excluded and alive while a proven
	// member converges — foreign never blocks teardown.
	sibling := spawnOrphanedMarkedCarrier(t, name, home, gen, os.Args[0])
	marked, _, err := markedOrphanProcesses(
		[]proctree.Process{carrier, sibling}, name, home, fixtureCohort(gen),
		cohortMembership{
			Recorded:  map[int]uint64{sibling.PID: sibling.StartID},
			OwnServer: own,
		}, nil, nil)
	require.NoError(t, err, "a foreign carrier is named, never a blocking refusal")
	require.Equal(t, []proctree.Process{sibling}, marked)
	require.True(t, proctree.AliveSame(carrier), "the foreign carrier survives the sweep")
}

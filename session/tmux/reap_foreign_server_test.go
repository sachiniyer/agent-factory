package tmux

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/stretchr/testify/require"
)

// spawnMarkedCarrier starts a live marker-carrying sleeper with extra env
// pairs — the detached foreign-server carrier of #7469 carries the pane's
// inherited AF_* markers AND the nested server's TMUX tuple.
func spawnMarkedCarrier(t *testing.T, name, home, gen string, extraEnv ...string) proctree.Process {
	t.Helper()
	cmd := exec.Command("sleep", "300")
	cmd.Env = append(os.Environ(),
		EnvMarkerSession+"="+name,
		EnvMarkerHome+"="+home,
		EnvMarkerGeneration+"="+gen,
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	p, err := proctree.Lookup(cmd.Process.Pid)
	require.NoError(t, err)
	return p
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
	return ServerIdentity{Socket: sock, PID: 4242, StartID: 987654321, SessionID: "$7"}
}

// #7469 acceptance: a DETACHED carrier whose TMUX env names a server that is
// not the recorded one — the user's own tmux server started inside the pane —
// is proven foreign: never signalled, named at WARNING, and NOT a blocking
// refusal, so teardown proceeds while it stays alive.
func TestVanishedSessionSweep_NeverSignalsAForeignServerCarrier(t *testing.T) {
	name := "af_foreign_server"
	home := t.TempDir()
	gen := name + "-gen"
	own := recordedOwnServer(t)

	carrier := spawnMarkedCarrier(t, name, home, gen,
		"TMUX="+foreignSocketFile(t)+",9999,3")

	var warnings bytes.Buffer
	oldWarnings := log.WarningLog.Writer()
	log.WarningLog.SetOutput(&warnings)
	t.Cleanup(func() { log.WarningLog.SetOutput(oldWarnings) })

	membership := cohortMembership{
		Recorded:  map[int]uint64{carrier.PID: carrier.StartID},
		OwnServer: own,
	}
	marked, _, err := markedOrphanProcesses(
		[]proctree.Process{carrier}, name, home, fixtureCohort(gen), membership, nil, nil)
	require.NoError(t, err,
		"a positively-foreign carrier is attributed — it is named, not a blocking refusal")
	require.Empty(t, marked, "the foreign-server carrier is never eligible for signals")
	require.Contains(t, warnings.String(), strconv.Itoa(carrier.PID))
	require.Contains(t, warnings.String(), "foreign tmux server")
	require.Contains(t, warnings.String(), "LEFT ALIVE")
	require.True(t, proctree.AliveSame(carrier), "the carrier survives the sweep")
}

// #7469: even a carrier that PASSES tier-2's executable identity (a
// claude-comm carrier, the #3699 look-alike) is refused when a foreign server
// holds its launch — the allowlist may admit only what the server attribution
// does not place elsewhere. Staged here on the verdict the arm consumes: an
// anchor-eligible carrier with foreign server evidence never reaches the
// eligible set.
func TestVanishedSessionSweep_ForeignBeatsAgentExeIdentity(t *testing.T) {
	name := "af_foreign_exe"
	home := t.TempDir()
	gen := name + "-gen"
	own := recordedOwnServer(t)

	carrier := spawnMarkedCarrier(t, name, home, gen,
		"TMUX="+foreignSocketFile(t)+",9999,3")

	membership := cohortMembership{
		Recorded:  map[int]uint64{carrier.PID: carrier.StartID},
		Root:      proctree.Process{PID: carrier.PPID, StartID: 1},
		AgentExe:  "claude",
		OwnServer: own,
	}
	marked, _, err := markedOrphanProcesses(
		[]proctree.Process{carrier}, name, home, fixtureCohort(gen), membership, nil, nil)
	require.NoError(t, err)
	require.Empty(t, marked,
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
	// the gate refuses as before (the verdict changes nothing); with a
	// recorded pair it admits as before. Both directions pinned in one test.
	carrier := spawnMarkedCarrier(t, name, home, gen)

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

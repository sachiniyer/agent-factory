package tmux

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/proctree"
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

	carrier := spawnOrphanedMarkedCarrier(t, name, home, gen, os.Args[0],
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
// spawned it — the allowlist may admit only what the server attribution does
// not place elsewhere.
func TestVanishedSessionSweep_ForeignBeatsAgentExeIdentity(t *testing.T) {
	name := "af_foreign_exe"
	home := t.TempDir()
	gen := name + "-gen"
	own := recordedOwnServer(t)

	carrier := spawnOrphanedMarkedCarrier(t, name, home, gen,
		agentNamedBinary(t, os.Args[0]),
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

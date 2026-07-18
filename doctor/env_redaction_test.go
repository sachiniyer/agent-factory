package doctor

import (
	"os"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// These pin the rule that a DENIED environment read must never be acted on as a
// definite negative.
//
// darwin serves KERN_PROCARGS2's env section only for processes we own, and it
// does not report the refusal: the buffer comes back with argv and no env, which
// is byte-for-byte identical to a process started with an empty environment. So
// "this process names no AF home" and "I was not allowed to see whether it does"
// arrive looking the same.
//
// This once mattered most at the temp-home DELETE gate, whose predicate was "no
// process references this home" — a redacted read made an in-use home look
// unreferenced, so `af doctor --fix` would delete a home a live process was
// using. That gate is gone: the delete now rests on the daemon lock, a
// kernel-guaranteed fact, and process inspection is only a POSITIVE spare-signal
// (finding a process that names a home keeps it; not finding one authorises
// nothing — #1989). So the destructive tests those redactions drove are gone
// too; what remains here is the primitive-level rule that a denied read never
// fabricates an attribution, in EITHER direction — still load-bearing for the
// orphan-reaping path and for the positive spare-signal.

// unreadableEnvPID returns a pid whose environment this test cannot read.
//
// It SEARCHES for a process owned by another user rather than assuming pid 1 is
// root's. That assumption is true on a normal machine and false in our own test
// container, where pid 1 is the entrypoint running as the same unprivileged
// user as the tests — so the "refusal" never happened and the test failed for a
// reason unrelated to its subject.
//
// Skips honestly when the environment has no foreign process to refuse us.
func unreadableEnvPID(t *testing.T) int {
	t.Helper()
	self := os.Getuid()
	if self == 0 {
		t.Skip("running as root: root can read every environment, so no read can be refused here")
	}
	snap, err := proctree.Snapshot()
	require.NoError(t, err)
	pids := make([]int, 0, len(snap))
	for pid := range snap {
		pids = append(pids, pid)
	}
	sort.Ints(pids) // deterministic pick across runs
	for _, pid := range pids {
		if uid, ok := proctree.UID(pid); ok && uid != self {
			return pid
		}
	}
	t.Skip("no process owned by another user is visible here (a container running everything as one " +
		"uid), so no environment read can be refused")
	return 0
}

// TestRedactedEnvIsUnknownNotAbsent is the primitive-level regression: a
// process whose environment we may not read must report EnvUnknown, never
// EnvAbsent. EnvAbsent is a claim; this read never earned one.
func TestRedactedEnvIsUnknownNotAbsent(t *testing.T) {
	pid := unreadableEnvPID(t)

	_, status := proctree.LookupEnv(pid, "AGENT_FACTORY_HOME")
	require.Equal(t, proctree.EnvUnknown, status,
		"reading pid %d's environment is not permitted, so the answer must be UNKNOWN. "+
			"EnvAbsent here is a fabricated negative: it asserts the variable is unset on the "+
			"strength of a read that was denied", pid)
}

// TestUnreadableEnvYieldsNoHomeClaim: we could not read the environment, so we
// must not attribute a home to it. A redacted read must add nothing to the
// referenced set — which keeps the positive spare-signal honest: it can only
// spare a home it can genuinely see a process using, never one it guessed at.
func TestUnreadableEnvYieldsNoHomeClaim(t *testing.T) {
	pid := unreadableEnvPID(t)
	homes := processReferencedHomes(map[int]proctree.Process{pid: {PID: pid}})
	require.Empty(t, homes,
		"pid %d's environment is not readable, so no home may be attributed to it", pid)
}

// TestRedactedEnvOrphanIsNotKilled is the reaping half: a marked orphan whose
// AF_HOME cannot be read must be reported, never fixed. "I could not read its
// home" is not "its home is mine".
func TestRedactedEnvOrphanIsNotKilled(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)

	// A live process we cannot read: pid 1. It carries no marker we can see,
	// so it must never reach the killable set.
	pid := unreadableEnvPID(t)

	opts := Options{
		Fix:            true,
		ConfigDir:      home,
		TempDir:        t.TempDir(),
		Exec:           cmd.MakeExecutor(),
		MinTempHomeAge: time.Hour,
		killGrace:      50 * time.Millisecond,
		killTermWait:   50 * time.Millisecond,
		snapshot: func() (map[int]proctree.Process, error) {
			return map[int]proctree.Process{pid: {PID: pid, Comm: "init"}}, nil
		},
		remoteConfig: func() (*config.RemoteHooks, string, error) { return nil, "", nil },
	}
	t.Setenv("AGENT_FACTORY_HOME", home)

	report, err := Run(opts)
	require.NoError(t, err)

	for _, f := range report.Findings {
		require.Empty(t, f.FixAction,
			"a process whose environment could not be read must never carry a fix action: %s", f.Detail)
		require.False(t, f.Fixed, "nothing unreadable may be acted on: %s", f.Detail)
	}
	// And it is definitely still alive.
	require.NoError(t, func() error { _, err := os.FindProcess(pid); return err }())
}

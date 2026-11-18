package tmux

import (
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// #7509 item 1 — ONE attestation memo scopes the whole sweep. The sweep
// creates its scope once and the reap-entry protected recompute and the
// survivor report must both classify claimants inside it — a fresh memo
// per site re-prices every claimant's attestation (and every wedged
// claim's full budget) once per pass per site. The factory counter reads
// the threading directly: a call site that opens its own scope instead of
// using the caller's shows up as another allocation.
func TestOneAttestationScopeThreadsTheWholeSweep(t *testing.T) {
	var allocs atomic.Int32
	realNew := newExeAttestations
	newExeAttestations = func() *proctree.ExeAttestations {
		allocs.Add(1)
		return realNew()
	}
	t.Cleanup(func() { newExeAttestations = realNew })

	// The sweep's own scope — the one allocation the whole sweep pays.
	att := newExeAttestations()
	require.Equal(t, int32(1), allocs.Load())

	// A verified daemon claimant for the walks to classify: a real af
	// build under daemon argv, like TestReportableSurvivors uses.
	daemon := exec.Command(os.Args[0])
	daemon.Args = []string{"af", "--daemon"}
	daemon.Env = append(os.Environ(), afDaemonSleepEnv+"=1")
	daemon.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	require.NoError(t, daemon.Start())
	t.Cleanup(func() { _ = daemon.Process.Kill() })
	require.Eventually(t, func() bool {
		return proctree.ArgvIsAFDaemon(proctree.Argv(daemon.Process.Pid))
	}, 5*time.Second, 20*time.Millisecond, "the daemon argv must land before the fixture is read")

	snap, err := proctree.Snapshot()
	require.NoError(t, err)
	claimant, err := proctree.Lookup(daemon.Process.Pid)
	require.NoError(t, err)

	// The three sites the vanished sweep drives — the protected-set
	// recompute, the reap entry (nil protected → recompute), and the
	// survivor report — all classify inside the caller's scope. The
	// claimant is the real daemon itself: positively placed, so it is
	// spared by proof and the reap never signals it.
	protected := reapProtectedSet(snap, []proctree.Process{claimant}, att)
	require.Contains(t, protected, claimant.PID)
	remaining := reapSessionProcesses(reapOnRequest, "af_scope_thread",
		[]proctree.Process{claimant}, time.Millisecond, time.Millisecond, nil, att)
	require.Empty(t, remaining)
	require.True(t, proctree.AliveSame(claimant),
		"a positively-placed daemon is never signalled even inside the reap")
	reportableSurvivors(snap, []proctree.Process{claimant}, att)
	require.Equal(t, int32(1), allocs.Load(),
		"every classification site in the sweep shares ONE memo scope — none opens its own")

	// A caller with no sweep scope still gets a set-local memo.
	reapProtectedSet(snap, []proctree.Process{claimant}, nil)
	require.Equal(t, int32(2), allocs.Load(),
		"nil att allocates a scope local to that call — the single-call path is unchanged")
}

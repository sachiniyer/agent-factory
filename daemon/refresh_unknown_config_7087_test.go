package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// #7087 review, P3: refreshes run at daemon_poll_interval, which accepts any
// positive duration. A primed snapshot's freshness is counted in passes, so a
// row primed between two passes loads on the next one however much wall time
// that interval spans — a wall-clock bound shorter than the interval would
// stale every snapshot first and re-probe the row forever.
func TestAPrimedRowLoadsOnTheNextPassHoweverLongTheInterval(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	_ = captureWarnings(t)
	isolateDeferredConfigRows(t)
	unknownOnLoadForAll(t)
	seedRows(t, "repo-a", deferredRow("slow", "/projects/alpha"))
	key := daemonInstanceKey("repo-a", "slow")
	existing, _, _, _, err := refreshDaemonInstances(nil)
	require.NoError(t, err)

	expireAllDeferrals(t)
	var probe func()
	probes := 0
	startDeferredConfigProbe = func(p func()) { probes++; probe = p }
	existing, _, _, _, err = refreshDaemonInstances(existing)
	require.NoError(t, err)
	require.NotNil(t, probe, "precondition: the due row started its probe")

	primeRestoreContextForRefresh = func(string) (*session.RestoreContext, error) {
		return session.NewRestoreContext(), nil
	}
	probe()
	// A whole poll interval goes by before the next pass: an hour, which a
	// daemon_poll_interval may be.
	later := time.Now().Add(time.Hour)
	prevNow := deferredConfigNow
	deferredConfigNow = func() time.Time { return later }
	t.Cleanup(func() { deferredConfigNow = prevNow })

	loaded := 0
	fromInstanceDataWithContextForRefresh = func(d session.InstanceData, rc *session.RestoreContext) (*session.Instance, error) {
		loaded++
		return &session.Instance{ID: d.ID, Title: d.Title}, nil
	}
	refreshed, _, _, _, err := refreshDaemonInstances(existing)
	require.NoError(t, err)
	require.NotNil(t, refreshed[key], "the row loads on the pass after its probe")
	require.Equal(t, 1, loaded, "from the primed snapshot")
	require.Equal(t, 1, probes, "without being probed again")
}

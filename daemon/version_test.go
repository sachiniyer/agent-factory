package daemon

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// withVersion sets the recorded build version for one test.
func withVersion(t *testing.T, v string) {
	t.Helper()
	prev := Version()
	t.Cleanup(func() { SetVersion(prev) })
	SetVersion(v)
}

// Ping is the probe every client uses to learn the daemon's version, so it must
// carry it — an OK with no version is what a pre-#1044 daemon returns, and a
// client reads that as skew.
func TestPing_ReportsRecordedVersion(t *testing.T) {
	withVersion(t, "1.0.192")

	var resp PingResponse
	s := &controlServer{}
	require.NoError(t, s.Ping(PingRequest{}, &resp))
	require.True(t, resp.OK)
	require.Equal(t, "1.0.192", resp.Version)
}

// An unset version must stay empty rather than inventing a value: "" is the
// wire signal for "older than version reporting", and a placeholder here would
// make a genuinely stale daemon look current.
func TestPing_UnsetVersionStaysEmpty(t *testing.T) {
	withVersion(t, "")

	var resp PingResponse
	s := &controlServer{}
	require.NoError(t, s.Ping(PingRequest{}, &resp))
	require.True(t, resp.OK)
	require.Empty(t, resp.Version)
}

func TestSetVersion_RoundTrips(t *testing.T) {
	withVersion(t, "1.2.3")
	require.Equal(t, "1.2.3", Version())
}

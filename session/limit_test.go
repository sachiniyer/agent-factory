package session

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestSetLimitReached sets the LimitReached liveness + reset time and reports
// them through the accessors (#1146). The composed Status stays Ready (there is
// no legacy limit value), so render sites must key off LimitReached, not Status.
func TestSetLimitReached(t *testing.T) {
	reset := time.Date(2026, 7, 5, 14, 0, 0, 0, time.UTC)
	i := &Instance{}
	i.SetLimitReached(reset)

	require.True(t, i.LimitReached())
	require.Equal(t, LiveLimitReached, i.GetLiveness())
	require.Equal(t, Ready, i.GetStatus(), "LimitReached composes to Ready for legacy readers")

	got, ok := i.LimitResetAt()
	require.True(t, ok)
	require.True(t, got.Equal(reset))
}

// TestSetLimitReached_NoResetTime: a banner with no parseable reset time still
// blocks the session, but LimitResetAt reports no known time.
func TestSetLimitReached_NoResetTime(t *testing.T) {
	i := &Instance{}
	i.SetLimitReached(time.Time{})
	require.True(t, i.LimitReached())
	_, ok := i.LimitResetAt()
	require.False(t, ok, "a zero reset time must report as unknown")
}

// TestSetLimitReached_SkipsTransient: a row mid create/kill teardown must not be
// clobbered into LimitReached (mirrors SetStatusIfNotDeleting).
func TestSetLimitReached_SkipsTransient(t *testing.T) {
	for _, s := range []Status{Loading, Deleting} {
		i := &Instance{}
		i.SetStatus(s)
		i.SetLimitReached(time.Now())
		require.False(t, i.LimitReached(), "%v must not be overwritten by SetLimitReached", s)
		require.Equal(t, s, i.GetStatus())
	}
}

// TestClearLimitReached moves a limit-blocked instance back to Running and drops
// the reset time; it is a no-op on a non-limit instance.
func TestClearLimitReached(t *testing.T) {
	i := &Instance{}
	i.SetLimitReached(time.Now().Add(time.Hour))
	i.ClearLimitReached()
	require.False(t, i.LimitReached())
	require.Equal(t, LiveRunning, i.GetLiveness())
	_, ok := i.LimitResetAt()
	require.False(t, ok)

	// No-op on a Ready instance.
	r := &Instance{}
	r.SetStatus(Ready)
	r.ClearLimitReached()
	require.Equal(t, Ready, r.GetStatus())
}

// TestLimitResetAt_ClearedWhenNotLimit: once the liveness moves off LimitReached
// the reset time never surfaces, even if the field lingers in memory.
func TestLimitResetAt_ClearedWhenNotLimit(t *testing.T) {
	i := &Instance{}
	i.SetLimitReached(time.Now().Add(time.Hour))
	i.SetStatus(Ready) // shim moves liveness to LiveReady, limitResetAt lingers
	require.False(t, i.LimitReached())
	_, ok := i.LimitResetAt()
	require.False(t, ok, "a lingering reset time must not surface once off LimitReached")
}

// TestLimitReached_PersistRoundTrip: a limit-blocked instance serializes its
// liveness + reset time, and the InstanceData survives the JSON round-trip the
// daemon writes to disk / carries in the snapshot — so the badge survives a
// restart (#1146).
func TestLimitReached_PersistRoundTrip(t *testing.T) {
	reset := time.Date(2026, 7, 5, 14, 30, 0, 0, time.UTC)
	i, err := NewInstance(InstanceOptions{Title: "limited", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	i.SetLimitReached(reset)

	data := i.ToInstanceData()
	require.Equal(t, LiveLimitReached, data.Liveness)
	require.True(t, data.LimitResetAt.Equal(reset))

	// JSON round-trip (the on-disk + snapshot format).
	raw, err := json.Marshal(data)
	require.NoError(t, err)
	var back InstanceData
	require.NoError(t, json.Unmarshal(raw, &back))
	require.Equal(t, LiveLimitReached, back.Liveness)
	require.True(t, back.LimitResetAt.Equal(reset))

	// FromInstanceData maps the reset time onto the rebuilt instance's field.
	require.Equal(t, reset, back.LimitResetAt)
}

// TestToInstanceData_NoLimitResetForNormalSession: a Ready session never persists
// a reset time, so omitempty drops the field for every normal row.
func TestToInstanceData_NoLimitResetForNormalSession(t *testing.T) {
	i, err := NewInstance(InstanceOptions{Title: "normal", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	i.SetStatus(Ready)
	data := i.ToInstanceData()
	require.True(t, data.LimitResetAt.IsZero(), "a non-limit session must not carry a reset time")
}

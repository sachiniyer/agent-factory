package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

func stubProbe(t *testing.T, fn func(string) (session.HostCheck, error)) *[]string {
	t.Helper()
	prev := probeHostsFn
	var called []string
	probeHostsFn = func(_ context.Context, name string, _ map[string]config.HostConfig, _ string, _ time.Duration) (session.HostCheck, error) {
		called = append(called, name)
		return fn(name)
	}
	t.Cleanup(func() { probeHostsFn = prev })
	return &called
}

func TestProbeOne_ReportsTheProbeAndSkipsAnInFlightHost(t *testing.T) {
	called := stubProbe(t, func(string) (session.HostCheck, error) {
		return session.HostCheck{Reachable: true, Complete: true}, nil
	})
	r := probeOne(context.Background(), probeHostsFn, "one-probe", nil, "")
	assert.True(t, r.Probed && r.Reachable && r.Complete)
	assert.Equal(t, []string{"one-probe"}, *called)

	// An incomplete answer is carried through, never flattened to complete (#5841).
	stubProbe(t, func(string) (session.HostCheck, error) {
		return session.HostCheck{Reachable: true, Complete: false}, nil
	})
	r = probeOne(context.Background(), probeHostsFn, "incomplete-probe", nil, "")
	assert.True(t, r.Probed && r.Reachable)
	assert.False(t, r.Complete)

	require.True(t, daemonHostProbes.tryAcquire("busy-probe"))
	defer daemonHostProbes.release("busy-probe")
	r = probeOne(context.Background(), probeHostsFn, "busy-probe", nil, "")
	assert.False(t, r.Probed, "a second probe of a host mid-probe is skipped, not stacked")
	assert.Len(t, *called, 1)
}

func TestProbeOne_RefusalSurfacesAsErrorText(t *testing.T) {
	stubProbe(t, func(string) (session.HostCheck, error) {
		return session.HostCheck{}, errors.New("host \"x\" is not in the host list")
	})
	r := probeOne(context.Background(), probeHostsFn, "x-refused", nil, "")
	assert.True(t, r.Probed)
	assert.False(t, r.Reachable)
	assert.Contains(t, r.Error, "not in the host list")
}

func TestListedHostReadinessFoldsIntoTheCatalog(t *testing.T) {
	key := session.HostReadinessKey("cat-box")
	opt := HostOption{Name: "cat-box", Status: BackendAvailable}

	applyListedHostReadiness(&opt)
	assert.Equal(t, "unprobed", opt.Readiness)
	assert.NotEmpty(t, opt.Note)
	assert.Equal(t, BackendAvailable, opt.Status)

	session.RecordHostReadiness(key, errors.New("git is missing"))
	opt = HostOption{Name: "cat-box", Status: BackendAvailable}
	applyListedHostReadiness(&opt)
	assert.Equal(t, BackendUnavailable, opt.Status)
	assert.Contains(t, opt.Reason, "git is missing")
}

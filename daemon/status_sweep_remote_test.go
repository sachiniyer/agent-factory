package daemon

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// W4.1: one wedged off-box probe must not delay the others, nor the sweep
// beyond its wait, and a later sweep must not stack a second probe behind it.
func TestFanOutRemoteProbes_WedgedHostIsolated(t *testing.T) {
	oldWait := remoteProbeSweepWait
	remoteProbeSweepWait = 200 * time.Millisecond
	t.Cleanup(func() { remoteProbeSweepWait = oldWait })

	wedged, fast1, fast2 := &session.Instance{}, &session.Instance{}, &session.Instance{}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var wedgedCalls, fastDone atomic.Int32
	probe := func(_ string, inst *session.Instance) {
		if inst == wedged {
			wedgedCalls.Add(1)
			<-release
			return
		}
		fastDone.Add(1)
	}
	admitAll := func() (func(), bool) { return func() {}, true }
	targets := []worktreeProbeTarget{{instance: wedged}, {instance: fast1}, {instance: fast2}}

	start := time.Now()
	fanOutRemoteProbes(targets, probe, admitAll)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("sweep held by wedged probe for %v", d)
	}
	if got := fastDone.Load(); got != 2 {
		t.Fatalf("healthy sessions probed %d times, want 2", got)
	}

	fanOutRemoteProbes(targets, probe, admitAll)
	if got := wedgedCalls.Load(); got != 1 {
		t.Fatalf("wedged host probed %d times, want 1 (no stacking)", got)
	}
}

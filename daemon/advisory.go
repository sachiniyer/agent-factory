package daemon

import (
	"context"
	"sync"
)

// advisoryScope is the cancellation scope for ADVISORY background work: work
// whose result is best-effort and which nothing durable depends on (today, the
// host-readiness probes). Shutdown cancels it before anything else, so such work
// can never sit on the path to the final SaveInstancesForShutdown checkpoint — a
// stopper SIGKILLs a daemon that has not exited within stopDaemonGrace (5s), and
// a drain that waited out a 30s ssh probe skipped the save (#5839).
//
// The rule it encodes: a goroutine that can block on the network for longer than
// the shutdown grace must take a context from here and must not be joined by the
// drain waitgroup.
type advisoryScope struct {
	once   sync.Once
	ctx    context.Context
	cancel context.CancelFunc
}

func (a *advisoryScope) init() {
	a.once.Do(func() { a.ctx, a.cancel = context.WithCancel(context.Background()) })
}

// Context is cancelled when shutdown begins.
func (a *advisoryScope) Context() context.Context {
	a.init()
	return a.ctx
}

// Cancel marks shutdown as begun. Idempotent.
func (a *advisoryScope) Cancel() {
	a.init()
	a.cancel()
}

func (m *Manager) advisoryContext() context.Context {
	if m == nil {
		return context.Background()
	}
	return m.advisory.Context()
}

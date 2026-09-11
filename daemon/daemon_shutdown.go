package daemon

import (
	"sync"

	"github.com/sachiniyer/agent-factory/log"
)

// drainDaemon quiesces the daemon's externally reachable control planes and
// daemon-owned writers before the terminal checkpoint. It closes mutation
// admission, drains the HTTP and control servers (joining all dispatched
// handlers), re-closes web listeners to cover the ApplyConfig rebind window,
// then stops the background goroutines and joins every background writer.
//
// httpClosed and controlClosed are set via the supplied pointers so the
// deferred fallback-closers in runDaemon can skip a redundant close.
func drainDaemon(
	m *Manager,
	closeHTTP func() error,
	closeControl func() error,
	httpClosed *bool,
	controlClosed *bool,
	stopCh chan struct{},
	wg *sync.WaitGroup,
) {
	// Close mutation admission, then close and JOIN both externally reachable
	// control planes before taking the terminal snapshot. Closing only their
	// listeners is insufficient: an ArchiveSession (or any other writer) that
	// already passed admission can still mutate memory and block its targeted
	// persist behind SaveInstances' repo flock. The transport cleanup functions
	// wait for those dispatched handlers, so nothing user-driven can begin or
	// remain in flight across the checkpoint.
	m.lifecycle.markQuiescing()
	if closeHTTP != nil {
		*httpClosed = true
		if err := closeHTTP(); err != nil {
			log.WarningLog.Printf("failed to drain daemon HTTP server: %v", err)
		}
	}
	*controlClosed = true
	if err := closeControl(); err != nil {
		log.WarningLog.Printf("failed to drain daemon control server: %v", err)
	}
	// A control-socket ApplyConfig admitted before quiescing can rebind a TCP
	// listener after closeHTTP's sync.Once-guarded double close. Close once
	// more after every control handler has returned so no replacement listener
	// outlives shutdown.
	if m.webListeners != nil {
		if err := m.webListeners.close(); err != nil {
			log.WarningLog.Printf("failed to close web listeners after control drain: %v", err)
		}
	}

	// Stop and join daemon-owned writers too. The lifecycle gate above also
	// refuses any scheduler/watcher delivery that reaches the closed transports
	// during their deferred teardown.
	close(stopCh)
	wg.Wait()
	// The poll loop is out, so no further root-agent create can be launched; wait
	// for one that already is (#3721). JOINED, never cancelled — a create torn
	// down mid-provision is the half-created session the always-ensure loop has no
	// way to reconcile — and BEFORE the final SaveInstances below, so a create
	// that lands late is persisted rather than overwritten by a save that predates
	// it. This is also what the poll goroutine's own wg.Wait did while the create
	// still ran on it.
	m.waitRootAgentCreatesForShutdown()
	// Root-program drift inspections also run off the poll goroutine. Join them
	// after the poll has stopped launching new ones, so neither a normal resolver
	// tail nor a deliberately single-flighted stalled read outlives its Manager.
	m.waitRootProgramDriftInspectionsForShutdown()
	// RPCs and the poll are gone, and root creates (which can launch a final
	// conversation capture) are joined. No detached durable writer may now be
	// admitted; let pre-destructive and permanently stalled work stand down, and
	// join any mutation already admitted through its targeted persist.
	m.stopAndWaitBackgroundMutationsForShutdown()
}

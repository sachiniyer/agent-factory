package daemon

import (
	"errors"
	"fmt"
	"sync"

	"github.com/sachiniyer/agent-factory/log"
)

// stopFor is the package-test convenience form. Production lifecycle paths use
// stopForInstance with the stable id; an empty id is intentionally a wildcard
// only for tests that construct marker servers without persisted identity.
func (v *vscodeSupervisor) stopFor(key string) {
	_ = v.stopForInstance(key, "")
}

// stopForInstance tears down the editor belonging to exactly one stable session,
// including a previous daemon's process when this supervisor's map is empty.
func (v *vscodeSupervisor) stopForInstance(key, instanceID string) error {
	v.mu.Lock()
	if !v.reserveReconcileLocked(key) {
		v.mu.Unlock()
		return fmt.Errorf("VS Code editor admission or teardown for session id %q is already in progress", instanceID)
	}
	server := v.servers[key]
	if server != nil && (instanceID == "" || server.instanceID == instanceID) {
		delete(v.servers, key)
	} else {
		server = nil
	}
	v.mu.Unlock()
	defer v.releaseReconcile(key)

	// Stop OUTSIDE the lock: it blocks for up to the stop grace, and holding the
	// supervisor lock across it would stall every unrelated session's editor. The
	// same-key reservation stays held through both in-memory and durable cleanup,
	// so no replacement can enter between them.
	var memoryErr error
	if server != nil {
		memoryErr = server.stop()
	}
	if instanceID != "" {
		// A prior failed reaper or an older daemon can leave additional sidecars
		// even when an in-memory editor existed. Teardown succeeds only after every
		// owner for this stable instance has been reconciled.
		matched, persistedErr := v.reconcilePersistedForInstance(key, instanceID)
		if memoryErr == nil {
			return persistedErr
		}
		if matched && persistedErr == nil {
			// The sidecar is the stable retry authority for a failed reaper. Finding
			// and conclusively removing it supersedes the cached in-memory UNKNOWN.
			return nil
		}
		memoryErr = errors.Join(memoryErr, persistedErr)
	}
	if memoryErr != nil {
		// Keep the in-memory identity as a retry handle when no matching durable
		// owner conclusively replaced it. Admission cannot race into the key while
		// the reservation is held, so a nil slot is still ours.
		v.mu.Lock()
		if v.servers[key] == nil {
			v.servers[key] = server
		}
		v.mu.Unlock()
		return memoryErr
	}
	return nil
}

// Stop tears down every editor and refuses further spawns, so daemon shutdown
// leaves no orphaned code-server behind. It mirrors watcherSupervisor.Stop:
// snapshot under the lock, then stop outside it.
func (v *vscodeSupervisor) Stop() {
	v.mu.Lock()
	v.stopped = true
	// A key reservation covers work outside mu, including persisted-owner reads
	// and signals. Wait until every such operation has completed before the
	// global sidecar scan; otherwise shutdown can act on the same owner twice.
	for len(v.reconciling) > 0 {
		v.reconcileCond.Wait()
	}
	servers := make([]*vscodeServer, 0, len(v.servers))
	for _, s := range v.servers {
		servers = append(servers, s)
	}
	v.servers = make(map[string]*vscodeServer)
	v.mu.Unlock()

	var wg sync.WaitGroup
	stopErrs := make(chan error, len(servers))
	for _, s := range servers {
		wg.Add(1)
		go func(s *vscodeServer) {
			defer wg.Done()
			if err := s.stop(); err != nil {
				stopErrs <- err
			}
		}(s)
	}
	wg.Wait()
	close(stopErrs)
	for err := range stopErrs {
		log.WarningLog.Printf("vscode: graceful shutdown could not confirm live editor teardown: %v", err)
	}
	if err := v.stopAllPersisted(); err != nil {
		log.WarningLog.Printf("vscode: graceful shutdown could not determine or complete persisted editor teardown: %v", err)
	}
}

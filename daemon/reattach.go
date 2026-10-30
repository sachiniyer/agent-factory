package daemon

import (
	"errors"
	"sync"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// reattachConcurrency bounds how many restored sandbox rows dial their
// transport at once, so a daemon with many off-box sessions does not open every
// ssh connection in the same instant.
const reattachConcurrency = 8

// reattachRestoredRuntimes re-binds every restored ssh/sandbox row that
// persisted a runtime identity to its still-running agent-server (#5571 W1.5).
// It runs in the background after the instances are published: a slow or dead
// host must not hold up daemon readiness, and a row that cannot be reached
// simply stays as the loader left it (inert, Lost) — today's behavior — until
// the unreachable-state work (W1.3) projects it properly.
//
// It must be called only AFTER sweepStartupOrphanSandboxTunnels: that sweep kills
// every tunnel bearing this AF home's ownership marker, which would catch the
// tunnels reattachment has just opened. Each attempt runs under the session's
// operation lock — the same one restore and Lost recovery hold — and re-verifies
// the row is still the registered instance before probing.
func (m *Manager) reattachRestoredRuntimes() {
	type row struct {
		key  string
		inst *session.Instance
	}
	m.mu.Lock()
	rows := make([]row, 0, len(m.instances))
	for key, inst := range m.instances {
		if inst != nil {
			rows = append(rows, row{key, inst})
		}
	}
	m.mu.Unlock()
	go func() {
		sem := make(chan struct{}, reattachConcurrency)
		var wg sync.WaitGroup
		for _, r := range rows {
			wg.Add(1)
			sem <- struct{}{}
			go func(key string, inst *session.Instance) {
				defer wg.Done()
				defer func() { <-sem }()
				// Ineligible rows (local, docker, hook, archived, tombstoned, or written
				// before the runtime identity existed) return immediately.
				opLock, _, lockErr := m.lockSessionOperationWithin(key, "reattach", inst.Title)
				if lockErr != nil {
					log.WarningLog.Printf("daemon: skipped reattaching session %q: %v", inst.Title, lockErr)
					return
				}
				defer opLock.Unlock()
				m.mu.Lock()
				current := m.instances[key] == inst
				m.mu.Unlock()
				if !current {
					return
				}
				// A W1.4 restart records its runtime identity (PID, address, the
				// adoption it still owes) durably before it spawns and before it
				// adopts: a daemon that exits mid-restart must reload a row that
				// names the server it left running.
				repoID, _ := splitDaemonInstanceKey(key)
				inst.SetRuntimeIdentityPersist(func(i *session.Instance) error {
					return m.persistInstanceErr(repoID, i)
				})
				err := inst.ReattachSandbox()
				inst.SettleAttentionAfterReattach()
				// An adopted restart may have respawned the agent: the session
				// already ended an in-flight task run (before observations
				// resumed); the owning task's status is stamped here.
				if inst.ConsumeTaskRunInterrupted() {
					stampTaskRunInterrupted(inst, m.taskRunPeerSnapshot())
					m.persistInstance(repoID, inst)
				}
				if err != nil && !errors.Is(err, session.ErrReattachIneligible) {
					log.WarningLog.Printf("daemon: could not reattach session %q to its sandbox after restart: %v", inst.Title, err)
				}
			}(r.key, r.inst)
		}
		wg.Wait()
	}()
}

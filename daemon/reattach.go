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
func (m *Manager) reattachRestoredRuntimes(instances map[string]*session.Instance) {
	rows := make([]*session.Instance, 0, len(instances))
	for _, inst := range instances {
		if inst != nil {
			rows = append(rows, inst)
		}
	}
	go func() {
		sem := make(chan struct{}, reattachConcurrency)
		var wg sync.WaitGroup
		for _, inst := range rows {
			wg.Add(1)
			sem <- struct{}{}
			go func(inst *session.Instance) {
				defer wg.Done()
				defer func() { <-sem }()
				// Ineligible rows (local, docker, hook, archived, tombstoned, or written
				// before the runtime identity existed) return immediately.
				err := inst.ReattachSandbox()
				if err != nil && !errors.Is(err, session.ErrReattachIneligible) {
					log.WarningLog.Printf("daemon: could not reattach session %q to its sandbox after restart: %v", inst.Title, err)
				}
			}(inst)
		}
		wg.Wait()
	}()
}

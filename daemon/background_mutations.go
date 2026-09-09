package daemon

// launchBackgroundMutation registers a detached durable-state writer before it
// becomes runnable. Once shutdown closes admission it refuses new writers, and
// the WaitGroup is safe to wait because no later Add can cross that same lock.
func (m *Manager) launchBackgroundMutation(run func(stop <-chan struct{})) bool {
	m.backgroundMutationMu.Lock()
	defer m.backgroundMutationMu.Unlock()
	if m.backgroundMutationsStopped {
		return false
	}
	if m.backgroundMutationStop == nil {
		m.backgroundMutationStop = make(chan struct{})
	}
	stop := m.backgroundMutationStop
	m.backgroundMutationWG.Add(1)
	go func() {
		defer m.backgroundMutationWG.Done()
		run(stop)
	}()
	return true
}

// stopAndWaitBackgroundMutationsForShutdown is called only after external RPCs,
// the poll loop, and root creates have been joined, so every possible launcher
// has stopped. Closing stop lets pre-destructive task workers and ghost retries
// stand down; a descriptor worker or writer already mutating is joined through
// its persist before this returns.
func (m *Manager) stopAndWaitBackgroundMutationsForShutdown() {
	m.backgroundMutationMu.Lock()
	if !m.backgroundMutationsStopped {
		m.backgroundMutationsStopped = true
		if m.backgroundMutationStop == nil {
			m.backgroundMutationStop = make(chan struct{})
		}
		close(m.backgroundMutationStop)
	}
	m.backgroundMutationMu.Unlock()
	m.backgroundMutationWG.Wait()
}

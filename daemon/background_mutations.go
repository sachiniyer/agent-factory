package daemon

// launchBackgroundMutation registers a detached durable-state writer before it
// becomes runnable. Once shutdown closes admission it refuses new writers, and
// the WaitGroup is safe to wait because no later Add can cross that same lock.
func (m *Manager) launchBackgroundMutation(run func(stop <-chan struct{})) bool {
	stop, admitted := m.beginBackgroundMutation()
	if !admitted {
		return false
	}
	go func() {
		defer m.backgroundMutationWG.Done()
		run(stop)
	}()
	return true
}

// beginBackgroundMutation is the synchronous admission half for a writer that
// already owns its goroutine. The caller must call backgroundMutationWG.Done.
// Keeping that Add behind the same mutex as shutdown's stop makes an admitted
// checkpoint callback joinable without letting a late callback race Wait.
func (m *Manager) beginBackgroundMutation() (<-chan struct{}, bool) {
	m.backgroundMutationMu.Lock()
	defer m.backgroundMutationMu.Unlock()
	if m.backgroundMutationsStopped {
		return nil, false
	}
	if m.backgroundMutationStop == nil {
		m.backgroundMutationStop = make(chan struct{})
	}
	m.backgroundMutationWG.Add(1)
	return m.backgroundMutationStop, true
}

// stopAndWaitBackgroundMutationsForShutdown is called only after external RPCs,
// the poll loop, and root creates have been joined, so every possible launcher
// has stopped. Closing stop lets pre-destructive task workers, ghost retries,
// and consumers of permanently stalled descriptor workers stand down. A writer
// already admitted is joined through its persist before this returns.
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

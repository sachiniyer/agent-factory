package daemon

import "time"

// watcherLimitBackpressurePoll bounds how quickly a watch reader resumes after
// the drainer makes room in a usage-limit-protected queue. It is a package var
// so tests can exercise the wait without real sleeps.
var watcherLimitBackpressurePoll = 100 * time.Millisecond

// waitForLimitQueueCapacity backpressures the watch subprocess once its durable
// usage-limit backlog reaches the ordinary queue cap. This is what makes a
// multi-day limit park both lossless and bounded: AF stops reading stdout, so
// the subprocess blocks on the pipe instead of AF dropping distinct events or
// growing its own queue without limit. A stop always breaks the wait so watcher
// reload and daemon shutdown remain bounded.
func (w *taskWatcher) waitForLimitQueueCapacity() bool {
	for w.queue != nil && w.queue.limitParkedAtCapacity() {
		select {
		case <-w.stopCh:
			return false
		case <-time.After(watcherLimitBackpressurePoll):
		}
	}
	return !w.stopRequested()
}

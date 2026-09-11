package daemon

import (
	"bufio"
	"bytes"
	"time"
)

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

// persistBufferedLimitEvents saves every complete event bufio already pulled
// from the subprocess pipe before a stop interrupted capacity backpressure.
// Those bytes are no longer recoverable from the producer after restart. The
// queue is necessarily limit-protected when this path runs, so appending the
// reader's bounded buffer cannot evict older events; an unterminated suffix is
// still not an event and remains intentionally discarded.
func (w *taskWatcher) persistBufferedLimitEvents(br *bufio.Reader, tail *tailBuffer) {
	buffered, err := br.Peek(br.Buffered())
	if err != nil || len(buffered) == 0 {
		return
	}
	lastNewline := bytes.LastIndexByte(buffered, '\n')
	if lastNewline < 0 {
		return
	}
	for _, line := range bytes.Split(buffered[:lastNewline], []byte{'\n'}) {
		w.enqueueEvent(string(bytes.TrimRight(line, "\r")), tail, true)
	}
}

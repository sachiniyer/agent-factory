package daemon

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/log"
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
// reload and daemon shutdown remain bounded. Writer shutdown breaks it too:
// once no process can add bytes, the finite pipe must be drained even though
// no queue capacity became available, or runOnce would wait on a reader that
// can never reach the EOF already sitting behind this pre-read gate. A stop
// below capacity needs the same drain whenever the queue is limit-protected:
// capacity controls when reads pause, but ownership of bytes already accepted
// by the pipe does not depend on whether the disk backlog reached that bound.
func (w *taskWatcher) waitForLimitQueueCapacity(stdoutWritersStopped <-chan struct{}) (proceed, drainFinitePipe bool) {
	for w.queue != nil && w.queue.limitParkedAtCapacity() {
		select {
		case <-w.stopCh:
			return false, true
		case <-stdoutWritersStopped:
			return false, true
		case <-time.After(watcherLimitBackpressurePoll):
		}
	}
	if !w.stopRequested() {
		return true, false
	}
	return false, w.queue != nil && w.queue.retainLimitParked()
}

// targetLimitRequiresRetention applies the fail-closed side of queue safety:
// an absent observation is never permission to expire or oldest-evict distinct
// events. The supervisor's production observer orders the positive/negative
// answer against limit publication; tests inject the same point-in-time answer.
func (w *taskWatcher) targetLimitRequiresRetention() bool {
	limitParked, err := w.sup.observeTargetLimit(w.taskID)
	if err == nil {
		return limitParked
	}
	log.WarningLog.Printf("watch task %s: cannot observe target usage-limit state; protecting backlog until delivery succeeds: %v", w.taskID, err)
	return true
}

// persistRemainingLimitEvents saves complete events accepted before a stop
// interrupted capacity backpressure. The caller waits until the process group
// has been killed, so this drains both bufio-prefetched bytes and the now-finite
// kernel pipe without reopening production. The queue is limit-protected, so
// these already-emitted events may cross its ordinary cap without eviction.
func (w *taskWatcher) persistRemainingLimitEvents(br *bufio.Reader, tail *tailBuffer) {
	for {
		chunk, err := br.ReadSlice('\n')
		switch {
		case err == nil:
			w.enqueueEvent(strings.TrimRight(string(chunk), "\r\n"), tail, true)
		case errors.Is(err, bufio.ErrBufferFull):
			line := string(chunk)
			discarded := 0
			var tailErr error
			for {
				var more []byte
				more, tailErr = br.ReadSlice('\n')
				discarded += len(more)
				if !errors.Is(tailErr, bufio.ErrBufferFull) {
					break
				}
			}
			if tailErr != nil {
				tail.add(line)
				return
			}
			log.WarningLog.Printf("watch task %s: stdout line exceeded %d bytes during stop drain; truncated (%d bytes discarded)", w.taskID, maxWatchLineBytes, discarded)
			w.enqueueEvent(line, tail, true)
		case errors.Is(err, io.EOF):
			if len(chunk) > 0 {
				log.WarningLog.Printf("watch task %s: discarding %d bytes of unterminated stdout output during stop drain", w.taskID, len(chunk))
				tail.add(string(chunk))
			}
			return
		default:
			return
		}
	}
}

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
// reload and daemon shutdown remain bounded. Once queue state is known, writer
// shutdown breaks it too: no process can add bytes, so the finite pipe may drain
// beyond the ordinary cap. Unknown state is different — enqueue must refuse it,
// so the finite pipe remains the only lossless buffer until recovery. A stop
// below capacity needs the same drain whenever the queue is limit-protected:
// capacity controls when reads pause, but ownership of bytes already accepted
// by the pipe does not depend on whether the disk backlog reached that bound.
func (w *taskWatcher) waitForLimitQueueCapacity(stdoutWritersStopped <-chan struct{}) (proceed, drainFinitePipe bool) {
	writersStopped := stdoutWritersStopped
	writerFinished := false
	for w.queue != nil {
		blocked, unknown := w.queue.limitBackpressureState()
		if !blocked {
			break
		}
		if writerFinished && !unknown {
			return false, true
		}
		select {
		case <-w.stopCh:
			return false, true
		case <-writersStopped:
			if !unknown {
				return false, true
			}
			// The writer is finished, so leaving its pipe unread is bounded by
			// the finite output already present. Disable the closed channel and
			// keep retrying recovery: draining while state is unknown only hands
			// those bytes to an enqueue that is required to refuse them.
			writerFinished = true
			writersStopped = nil
		case <-time.After(watcherLimitBackpressurePoll):
		}
	}
	if !w.stopRequested() {
		return true, false
	}
	return false, w.retainFinitePipeOnStop()
}

// retainFinitePipeOnStop decides whether complete events already accepted by
// stdout belong in protected storage. It waits for a delivery already in the
// drainer's limit-publication critical section, then consults both durable queue
// state and the current target. Holding the same fence prevents a later drainer
// from starting after this decision: it will observe stop and decline delivery.
func (w *taskWatcher) retainFinitePipeOnStop() bool {
	if w.queue == nil {
		return false
	}
	w.limitPublicationMu.Lock()
	defer w.limitPublicationMu.Unlock()
	if w.queue.retainLimitParked() {
		return true
	}
	return w.targetLimitRequiresRetention()
}

// deliverQueuedEventPublishingLimit performs the only drainer delivery that
// can establish limit retention. The marker lands before the publication fence
// opens. If stop won the fence, no delivery is attempted: the stdout reader has
// already made (or is about to make) its final decision from settled state.
func (w *taskWatcher) deliverQueuedEventPublishingLimit(ev queuedEvent, cursor eventQueueCursor) (err error, attempted bool) {
	w.limitPublicationMu.Lock()
	defer w.limitPublicationMu.Unlock()
	if w.stopRequested() {
		return nil, false
	}
	err = w.deliverQueuedEvent(ev, cursor)
	if errors.Is(err, errTargetLimitReached) {
		if markErr := w.queue.markLimitParked(); markErr != nil {
			log.ErrorLog.Printf("watch task %s: failed to protect usage-limit backlog from retention bounds: %v", w.taskID, markErr)
		}
	}
	return err, true
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

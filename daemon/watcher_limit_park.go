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
// reload and daemon shutdown remain bounded, and the finite pipe it leaves
// drains into protected storage then — once, at teardown.
//
// Writer shutdown does NOT break the wait. An exited process's kernel pipe is
// already finite — every byte it will ever emit is staged there — so the pipe
// itself is the bounded buffer the cap advertises. Draining it into the queue
// on exit would let a command that repeatedly exits after crashWindow append a
// fresh pipeful on every restart and grow the durable backlog without bound
// (#4226 review). The staged lines drain at queue pace instead: EOF reaches
// consumeLines only once the backlog has room. Unknown state waits the same
// way, with a harder reason — enqueue must refuse it, so the finite pipe is
// the only lossless buffer until recovery. A stop below capacity needs the
// same drain whenever the queue is limit-protected: capacity controls when
// reads pause, but ownership of bytes already accepted by the pipe does not
// depend on whether the disk backlog reached that bound.
func (w *taskWatcher) waitForLimitQueueCapacity(stdoutWritersStopped <-chan struct{}) (proceed, drainFinitePipe bool) {
	writersStopped := stdoutWritersStopped
	writerFinished := false
	stopping := false
	stopCh := w.stopCh
	for w.queue != nil {
		blocked, unknown := w.queue.limitBackpressureState()
		if !blocked {
			break
		}
		if stopping && (writerFinished || writersStopped == nil) {
			// Stop arrived while queue state was unreadable. It is still
			// unreadable and there is no live writer left to wait on, so the
			// pipe is finite; drain it. The drain itself re-checks state and
			// keeps lines in the run tail if the queue never recovered.
			return false, true
		}
		select {
		case <-stopCh:
			if !unknown {
				return false, true
			}
			// Queue state is unreadable, so handing the pipe's complete events
			// to the queue now can only produce refused enqueues. Keep recovering
			// until the writers are actually gone — the stop watchdog bounds that
			// wait — so a queue that heals during teardown still receives them.
			stopping = true
			stopCh = nil
		case <-writersStopped:
			// The writer is finished and its pipe is finite, so the bytes it
			// already emitted stage in the kernel buffer at no further cost.
			// Draining them into the queue here would append past the cap once
			// per restart forever; keep waiting for queue space instead.
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
//
// The destination is chosen at drain time behind one unthrottled load attempt.
// A queue that healed during teardown takes the enqueue path and keeps the
// events durable. One whose state is still unreadable must refuse every
// enqueue, and holding stop until storage recovers would hang reload and
// shutdown, so these events are lost. The loss is counted in dropped_events
// and logged once with the lines the run tail still holds. A stop returns
// without the failure summary that would otherwise print that tail (#4226
// review).
func (w *taskWatcher) persistRemainingLimitEvents(br *bufio.Reader, tail *tailBuffer) {
	emit := func(line string) { w.enqueueEvent(line, tail, true) }
	if w.queue != nil && w.queue.loadFailedFresh() {
		lost := 0
		emit = func(line string) {
			tail.add(line)
			lost++
			w.recordEventDrop()
		}
		defer func() {
			if lost > 0 {
				log.ErrorLog.Printf("watch task %s: event queue still unreadable at stop; %d complete event(s) from the stopped command could not be retained and were counted as dropped%s", w.taskID, lost, tail.logSuffix())
			}
		}()
	}
	for {
		chunk, err := br.ReadSlice('\n')
		switch {
		case err == nil:
			emit(strings.TrimRight(string(chunk), "\r\n"))
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
			emit(line)
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

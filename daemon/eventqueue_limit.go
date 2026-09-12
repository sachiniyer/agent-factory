package daemon

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
)

// loadLimitParkedLocked recovers the queue-level retention marker. A marker
// without pending events is residue from a crash after the final cursor advance;
// remove it before the next event can inherit a park that no longer exists.
func (q *eventQueue) loadLimitParkedLocked() error {
	info, err := os.Lstat(q.limitPath)
	if err != nil {
		if os.IsNotExist(err) {
			q.limitParked = false
			q.parkedStatusSeq = 0
			return nil
		}
		return fmt.Errorf("inspect usage-limit queue marker: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return config.RefuseManagedFileSymlink(q.limitPath)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("usage-limit queue marker %s is not a regular file", q.limitPath)
	}
	raw, err := os.ReadFile(q.limitPath)
	if err != nil {
		return fmt.Errorf("read usage-limit queue marker: %w", err)
	}
	q.limitParked = true
	q.parkedStatusSeq = 0
	fields := strings.Fields(string(raw))
	if len(fields) == 2 && fields[0] == "parked" {
		if seq, parseErr := strconv.ParseInt(fields[1], 10, 64); parseErr == nil && seq > 0 {
			q.parkedStatusSeq = seq
		}
	}
	if q.pending == 0 {
		return q.clearLimitParkedLocked()
	}
	return nil
}

func (q *eventQueue) markLimitParkedLocked() error {
	return q.persistLimitParkedLocked(q.parkedStatusSeq)
}

func (q *eventQueue) persistLimitParkedLocked(statusSeq int64) error {
	data := []byte("parked\n")
	if statusSeq > 0 {
		data = []byte("parked " + strconv.FormatInt(statusSeq, 10) + "\n")
	}
	if err := config.AtomicWriteFileRefusingLink(q.limitPath, data, 0644); err != nil {
		return err
	}
	q.limitParked = true
	q.parkedStatusSeq = statusSeq
	return nil
}

func (q *eventQueue) markLimitParked() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := q.retryLoadNowLocked(); err != nil {
		return err
	}
	if q.limitParked {
		return nil
	}
	return q.markLimitParkedLocked()
}

func (q *eventQueue) clearLimitParkedLocked() error {
	if err := config.RemoveFileRefusingLink(q.limitPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	q.limitParked = false
	q.parkedStatusSeq = 0
	return nil
}

// parkedStatusRecorded reports whether cursor still names the queue head whose
// parked task-store occurrence was already committed. The head sequence is the
// identity; LastRunStatus is only presentation and may be replaced by watcher
// lifecycle reporting while this event remains queued.
func (q *eventQueue) parkedStatusRecorded(cursor eventQueueCursor) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	_ = q.retryLoadLocked()
	return q.loadErr == nil && q.offset == cursor.offset && q.parkedStatusSeq == cursor.seq
}

// recordParkedStatus binds a successfully committed parked status to the exact
// queue head that caused it. A concurrent head move makes the cursor stale and
// leaves the new head unmarked, so its distinct occurrence is recorded later.
func (q *eventQueue) recordParkedStatus(cursor eventQueueCursor) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := q.retryLoadNowLocked(); err != nil {
		return false, err
	}
	if q.pending == 0 || q.offset != cursor.offset {
		return false, nil
	}
	ev, n, err := q.readEventAtLocked(q.offset)
	if err != nil {
		return false, err
	}
	if ev.Seq != cursor.seq || n != cursor.length {
		return false, nil
	}
	if q.limitParked && q.parkedStatusSeq == cursor.seq {
		return true, nil
	}
	if err := q.persistLimitParkedLocked(cursor.seq); err != nil {
		// The task-store write already committed. Remember that fact in this
		// process even if its durable queue annotation failed, so a transient
		// marker fault cannot revive the ten-second rewrite loop. A restart may
		// conservatively repeat the write once, then retry persistence.
		q.parkedStatusSeq = cursor.seq
		return false, err
	}
	return true, nil
}

// headParkedStatusRecorded reports whether the current queue head is the exact
// occurrence whose parked task status was committed. It is intentionally
// narrower than retainLimitParked: a generic protection marker may precede the
// first delivery attempt, and an old recorded sequence may remain while later
// events drain.
func (q *eventQueue) headParkedStatusRecorded() (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := q.retryLoadLocked(); err != nil {
		return false, err
	}
	if q.pending == 0 || q.parkedStatusSeq == 0 {
		return false, nil
	}
	ev, _, err := q.readEventAtLocked(q.offset)
	if err != nil {
		return false, err
	}
	return ev.Seq == q.parkedStatusSeq, nil
}

// retainLimitParked reports whether the current backlog belongs to a known
// usage-limit episode. The marker is durable, so a daemon restart cannot turn a
// six-day park into an ordinary 72-hour outage expiry.
func (q *eventQueue) retainLimitParked() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	_ = q.retryLoadLocked()
	return q.loadErr == nil && q.limitParked
}

// limitParkedAtCapacity tells the stdout reader to stop consuming bytes once a
// protected backlog reaches the ordinary queue bound. The watch subprocess then
// blocks on its pipe, so AF retains bounded disk use without dropping distinct
// events. One final record may cross the byte cap; event lines are themselves
// bounded, and the reader checks again before reading another.
func (q *eventQueue) limitParkedAtCapacity() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	_ = q.retryLoadLocked()
	if q.loadErr != nil || !q.limitParked {
		return false
	}
	return q.pending >= watcherQueueMaxEvents || q.size-q.offset >= watcherQueueMaxBytes
}

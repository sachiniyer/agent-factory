package daemon

import (
	"fmt"
	"os"

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
	q.limitParked = true
	if q.pending == 0 {
		return q.clearLimitParkedLocked()
	}
	return nil
}

func (q *eventQueue) markLimitParkedLocked() error {
	if err := config.AtomicWriteFileRefusingLink(q.limitPath, []byte("parked\n"), 0644); err != nil {
		return err
	}
	q.limitParked = true
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
	return nil
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

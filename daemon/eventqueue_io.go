package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"github.com/sachiniyer/agent-factory/config"
)

// The event queue's on-disk record primitives: directory durability, record
// boundaries, one record read, and the cursor writes. eventqueue.go next door
// owns the queue's state and policy — recovery, enqueue, peek/advance, the caps
// and compaction — and calls these. Split from it when the file crossed the
// 1000-line limit (#1145); nothing else moved.

func syncEventQueueDirectory(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open %s: %w", dir, err)
	}
	if err := handle.Sync(); err != nil {
		_ = handle.Close()
		return fmt.Errorf("fsync %s: %w", dir, err)
	}
	if err := handle.Close(); err != nil {
		return fmt.Errorf("close %s after fsync: %w", dir, err)
	}
	return nil
}

// offsetIsRecordBoundaryLocked reports whether q.offset begins a record in the
// open queue file f. Offset 0 and offset>=size are boundaries by definition;
// any interior offset is a boundary iff the byte before it is the record
// terminator '\n'. A ReadAt failure is an error, not "not a boundary" (#3242):
// conflating them would reset a valid cursor over a transient read fault and
// redeliver the whole delivered prefix. ReadAt leaves f's seek position
// untouched. Callers hold q.mu.
func (q *eventQueue) offsetIsRecordBoundaryLocked(f *os.File) (bool, error) {
	if q.offset <= 0 || q.offset >= q.size {
		return true, nil
	}
	var b [1]byte
	if _, err := f.ReadAt(b[:], q.offset-1); err != nil {
		return false, err
	}
	return b[0] == '\n', nil
}

// readEventAtLocked reads and parses one JSONL record at the given offset,
// returning the record and its length including the newline. Callers hold q.mu.
//
// The returned length distinguishes the two failure modes so peek can self-heal
// (#1634): a CORRUPT but newline-terminated record returns its byte length with
// the error (there is a boundary to skip past), while a TRUNCATED record with no
// terminating newline returns length 0 (no boundary — the drainer parks).
func (q *eventQueue) readEventAtLocked(off int64) (queuedEvent, int64, error) {
	f, err := os.Open(q.path)
	if err != nil {
		return queuedEvent{}, 0, err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(off, 0); err != nil {
		return queuedEvent{}, 0, err
	}
	br := bufio.NewReaderSize(f, 64*1024)
	raw, err := br.ReadBytes('\n')
	if err != nil {
		return queuedEvent{}, 0, fmt.Errorf("truncated event record at offset %d: %w", off, err)
	}
	var ev queuedEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return queuedEvent{}, int64(len(raw)), fmt.Errorf("corrupt event record at offset %d: %w", off, err)
	}
	return ev, int64(len(raw)), nil
}

// persistCursorLocked writes the current cursor (q.offset). Atomic
// (write+rename) so a torn write can never yield a cursor pointing mid-record.
// Callers hold q.mu.
func (q *eventQueue) persistCursorLocked() error {
	return q.persistCursorValueLocked(q.offset)
}

// persistCursorValueLocked durably writes an explicit cursor value. Compaction
// uses it to record the post-rewrite offset (0) BEFORE the rename that shrinks
// the file (#1537). Callers hold q.mu.
//
// The cursor REFUSES a symlinked path (#3672). It is a byte offset into the
// jsonl file beside it — a pair af creates, advances and deletes together — so
// the two must stay in the same directory: following a link would leave the
// queue's own compaction (which fsyncs the cursor's directory before renaming
// the queue file) fencing the wrong directory, and replacing one would discard
// an arrangement nobody has a reason to have made.
func (q *eventQueue) persistCursorValueLocked(off int64) error {
	return config.AtomicWriteFileRefusingLink(q.curPath, []byte(strconv.FormatInt(off, 10)), 0644)
}

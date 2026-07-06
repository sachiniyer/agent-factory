package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// Durable per-task event queue (#1129). When a watch-task event's delivery
// fails — e.g. every event fired while sessions were unreachable during a tmux
// outage (#1104/#1122) — the event is appended here instead of dropped, and a
// stop-aware drainer replays the backlog in emission order once deliveries
// succeed again. The healthy path never touches the queue: the first delivery
// attempt stays synchronous on the watcher's reader goroutine, preserving the
// backpressure/ordering contract.
//
// Layout: <AF home>/events/<taskID>.jsonl holds one JSON event per line;
// <taskID>.cursor holds the byte offset of the first undelivered event, so a
// pop is a cursor advance, not a file rewrite. Both files are removed whenever
// the queue fully drains. The cursor is written AFTER the delivery it
// acknowledges, so a daemon crash mid-replay redelivers at most one event —
// at-least-once by design; exactly-once machinery is not worth building for a
// prompt-delivery system.
//
// Only the owning daemon touches these files (one daemon per AF home), and
// within it the reader goroutine enqueues while the drainer dequeues — q.mu
// serializes them, no cross-process file lock needed.

const (
	// watcherQueueMaxEvents/watcherQueueMaxBytes bound one task's undelivered
	// backlog across a long outage. On overflow the OLDEST events are dropped:
	// after an outage the newest events are the actionable ones, and the
	// oldest are the most likely to have been re-swept by the script's next
	// poll. Drops are logged with the same one-warning-per-minute pattern as
	// the rate limiter, with a running counter.
	watcherQueueMaxEvents = 500
	watcherQueueMaxBytes  = 256 * 1024

	// watcherQueueMaxAge is the retention bound (#1129 PR 4): a queued event
	// older than this is expired at drain time instead of delivered — a
	// prompt about a three-day-old notification is noise, and the sources
	// worth watching re-emit on their next poll. Expiries are logged with a
	// count, never silent.
	watcherQueueMaxAge = 72 * time.Hour
)

// watcherQueueCompactBytes caps the delivered prefix a queue file may
// accumulate before it is compacted (the pending suffix rewritten to a fresh
// file). The pending backlog is bounded by watcherQueueMaxBytes, but during a
// long partial drain with live enqueues the delivered prefix would otherwise
// grow without limit. Package var so tests can shrink it.
var watcherQueueCompactBytes = int64(1024 * 1024)

// queuedEvent is the on-disk record: the raw stdout line plus a per-queue
// sequence number and enqueue timestamp for diagnostics (and the PR-4
// age-based retention).
type queuedEvent struct {
	Seq  int64     `json:"seq"`
	TS   time.Time `json:"ts"`
	Line string    `json:"line"`
}

// eventQueueCursor binds a peeked event to the exact queue position and record
// identity observed at peek time. Live enqueues may overflow-drop old events
// while replay delivery is in progress, so advance must validate this token
// before consuming the current head.
type eventQueueCursor struct {
	offset int64
	length int64
	seq    int64
}

// eventQueue is one task's durable backlog. Zero pending events is the steady
// state: no files on disk, every field zero.
type eventQueue struct {
	taskID  string
	path    string // <dir>/<taskID>.jsonl
	curPath string // <dir>/<taskID>.cursor

	mu      sync.Mutex
	offset  int64 // byte offset of the first undelivered event
	size    int64 // total bytes in the jsonl file
	pending int   // undelivered event count
	seq     int64 // last sequence number handed out

	dropped     int // events dropped to the overflow caps, for the drop log
	lastDropLog time.Time

	// now stamps each event's enqueue timestamp; a seam so tests can backdate
	// events into the past and exercise age-based expiry deterministically,
	// with no real-time sleep. Defaults to time.Now in production.
	now func() time.Time
}

// eventQueueDir resolves the queue directory, creating it on first use.
func eventQueueDir() (string, error) {
	configDir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(configDir, "events")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	return dir, nil
}

// newEventQueue opens (or initializes) the queue for taskID under dir,
// recovering offset/pending from any files a previous daemon left behind —
// which is what lets a backlog survive a daemon restart or reload.
func newEventQueue(dir, taskID string) *eventQueue {
	q := &eventQueue{
		taskID:  taskID,
		path:    filepath.Join(dir, taskID+".jsonl"),
		curPath: filepath.Join(dir, taskID+".cursor"),
		now:     time.Now,
	}
	q.load()
	return q
}

// load recovers the queue state from disk. A missing file is an empty queue; a
// corrupt cursor resets to 0 (redelivering pending events — at-least-once,
// never silent loss).
func (q *eventQueue) load() {
	q.mu.Lock()
	defer q.mu.Unlock()

	info, err := os.Stat(q.path)
	if err != nil {
		return // no queue file: empty queue
	}
	q.size = info.Size()

	if raw, err := os.ReadFile(q.curPath); err == nil {
		if off, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64); err == nil && off >= 0 && off <= q.size {
			q.offset = off
		} else {
			log.WarningLog.Printf("watch task %s: corrupt event-queue cursor; replaying the queue from the start", q.taskID)
		}
	}

	// Count the pending events and recover the sequence counter. The pending
	// suffix is bounded by watcherQueueMaxBytes, so this scan is cheap. A
	// bufio.Reader (not a Scanner) on purpose: JSON-escaping can inflate a
	// maxWatchLineBytes line severalfold, and a Scanner's token cap would make
	// the count silently stop at the first oversized record — losing exactly
	// the events durability promised to keep.
	f, err := os.Open(q.path)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(q.offset, 0); err != nil {
		return
	}
	br := bufio.NewReaderSize(f, 64*1024)
	scanned := q.offset
	for {
		raw, err := br.ReadBytes('\n')
		if err != nil {
			if len(raw) > 0 {
				// A trailing record with no newline is a torn append (daemon
				// died mid-write). It was never fully enqueued; truncate it
				// away so the next append starts on a record boundary instead
				// of gluing two records into one corrupt line.
				log.WarningLog.Printf("watch task %s: discarding %d bytes of torn trailing event-queue record", q.taskID, len(raw))
				if terr := os.Truncate(q.path, scanned); terr != nil {
					log.WarningLog.Printf("watch task %s: failed to truncate torn event-queue tail: %v", q.taskID, terr)
				} else {
					q.size = scanned
				}
			}
			return
		}
		scanned += int64(len(raw))
		q.pending++
		var ev queuedEvent
		if uerr := json.Unmarshal(raw, &ev); uerr == nil && ev.Seq > q.seq {
			q.seq = ev.Seq
		}
	}
}

// pendingCount reports how many undelivered events the queue holds.
func (q *eventQueue) pendingCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.pending
}

// enqueue appends one event and enforces the overflow caps by dropping oldest
// pending events past them.
func (q *eventQueue) enqueue(line string) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.seq++
	rec, err := json.Marshal(queuedEvent{Seq: q.seq, TS: q.now(), Line: line})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(q.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	rec = append(rec, '\n')
	n, err := f.Write(rec)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		// A short write would leave a torn record that corrupts the NEXT
		// append; truncate back to the last record boundary so the file stays
		// record-aligned (the caller drops this event — same degradation as
		// enqueue failing outright).
		if n > 0 {
			if terr := os.Truncate(q.path, q.size); terr != nil {
				log.WarningLog.Printf("watch task %s: failed to truncate short-written event record: %v", q.taskID, terr)
				q.size += int64(n)
			}
		}
		return err
	}
	q.size += int64(n)
	q.pending++

	return q.dropOldestOverCapsLocked()
}

// dropOldestOverCapsLocked advances the cursor past the oldest pending events
// until the backlog fits the count/byte caps. Callers hold q.mu.
func (q *eventQueue) dropOldestOverCapsLocked() error {
	droppedNow := 0
	for q.pending > watcherQueueMaxEvents || q.size-q.offset > watcherQueueMaxBytes {
		if q.pending <= 1 {
			// Always retain the newest event, even one over the byte cap on
			// its own (enqueue callers cap lines at maxWatchLineBytes).
			break
		}
		_, n, err := q.readEventAtLocked(q.offset)
		if err != nil {
			return err
		}
		q.offset += n
		q.pending--
		droppedNow++
	}
	if droppedNow == 0 {
		return nil
	}
	q.dropped += droppedNow
	if now := time.Now(); now.Sub(q.lastDropLog) >= time.Minute {
		q.lastDropLog = now
		// One warning per window, not per drop — mirroring the rate limiter's
		// log discipline; the counter keeps the exact total.
		log.WarningLog.Printf("watch task %s: event queue over its cap (%d events / %d bytes); dropped %d oldest queued events (%d dropped total)",
			q.taskID, watcherQueueMaxEvents, watcherQueueMaxBytes, droppedNow, q.dropped)
	}
	return q.persistCursorLocked()
}

// peek returns the oldest undelivered event and an advance cursor without
// consuming it. ok is false when the queue is empty.
func (q *eventQueue) peek() (ev queuedEvent, cursor eventQueueCursor, ok bool, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.pending == 0 {
		return queuedEvent{}, eventQueueCursor{}, false, nil
	}
	ev, n, err := q.readEventAtLocked(q.offset)
	if err != nil {
		return queuedEvent{}, eventQueueCursor{}, false, err
	}
	return ev, eventQueueCursor{offset: q.offset, length: n, seq: ev.Seq}, true, nil
}

// advance consumes the oldest event AFTER its successful delivery: cursor
// forward by the cursor peek reported, files removed once fully drained, and
// the delivered prefix compacted away once it outgrows its cap. If another
// queue mutation moved the head since peek, advance returns false without
// consuming anything; callers should re-peek.
func (q *eventQueue) advance(cursor eventQueueCursor) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.pending == 0 {
		return false, nil
	}
	if cursor.length <= 0 {
		return false, fmt.Errorf("invalid event-queue cursor length %d", cursor.length)
	}
	if q.offset != cursor.offset {
		return false, nil
	}
	ev, n, err := q.readEventAtLocked(q.offset)
	if err != nil {
		return false, err
	}
	if ev.Seq != cursor.seq || n != cursor.length {
		return false, nil
	}
	q.offset += cursor.length
	q.pending--
	if q.pending == 0 {
		// Fully drained: reclaim the delivered prefix by removing both files —
		// the steady healthy state is no queue files at all.
		q.offset, q.size = 0, 0
		if err := os.Remove(q.path); err != nil && !os.IsNotExist(err) {
			return false, err
		}
		if err := os.Remove(q.curPath); err != nil && !os.IsNotExist(err) {
			return false, err
		}
		return true, nil
	}
	if q.offset > watcherQueueCompactBytes {
		if err := q.compactLocked(); err != nil {
			// Compaction is an optimization; a failure must not lose the
			// event that was just consumed. Fall through to the cursor write.
			log.WarningLog.Printf("watch task %s: event-queue compaction failed (will retry later): %v", q.taskID, err)
		}
	}
	return true, q.persistCursorLocked()
}

// compactLocked rewrites the queue file to just its pending suffix, dropping
// the delivered prefix: copy suffix → temp file → rename over the original,
// then reset the in-memory offset (the caller persists the cursor). Crash
// safety is rename-based: a crash before the rename leaves the old
// consistent (file, cursor) pair; a crash after the rename but before the
// cursor write leaves a stale cursor pointing past the smaller new file,
// which load()'s off<=size validation rejects — resetting to 0 and
// redelivering the pending suffix (at-least-once, never loss). Callers hold
// q.mu.
func (q *eventQueue) compactLocked() error {
	src, err := os.Open(q.path)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	if _, err := src.Seek(q.offset, 0); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(q.path), q.taskID+".compact-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	n, err := io.Copy(tmp, src)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, q.path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	q.offset, q.size = 0, n
	return nil
}

// readEventAtLocked reads and parses one JSONL record at the given offset,
// returning the record and its length including the newline. Callers hold
// q.mu. A corrupt or truncated record is an error — the drainer logs and
// parks rather than guessing at boundaries.
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
		return queuedEvent{}, 0, fmt.Errorf("corrupt event record at offset %d: %w", off, err)
	}
	return ev, int64(len(raw)), nil
}

// persistCursorLocked writes the cursor file. Atomic (write+rename) so a torn
// write can never yield a cursor pointing mid-record. Callers hold q.mu.
func (q *eventQueue) persistCursorLocked() error {
	return config.AtomicWriteFile(q.curPath, []byte(strconv.FormatInt(q.offset, 10)), 0644)
}

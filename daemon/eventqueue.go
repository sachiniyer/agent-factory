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
	remove  func(string) error

	// appendRecord/truncate are the append and truncate seams. Production wires
	// them to os.Truncate and a real O_APPEND write; tests inject them to
	// simulate the partial-write + truncate-failure path that #1634 wedged on
	// (a real short write is otherwise impossible to force on a local FS).
	appendRecord func(path string, rec []byte) (int, error)
	truncate     func(path string, size int64) error

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
		taskID:       taskID,
		path:         filepath.Join(dir, taskID+".jsonl"),
		curPath:      filepath.Join(dir, taskID+".cursor"),
		remove:       os.Remove,
		appendRecord: appendRecordToFile,
		truncate:     os.Truncate,
		now:          time.Now,
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

	// Defense-in-depth (#1537): a cursor that clears the off<=size bound can
	// still point into the MIDDLE of a record — e.g. a crash during compaction
	// that shrank the file but left a pre-compaction offset behind. Records
	// always end in '\n', so a real interior boundary has '\n' immediately
	// before it; if q.offset isn't one, replay from the start rather than
	// parking the drainer forever on an unreadable head (redelivering a few
	// already-delivered events is at-least-once, a wedged queue is not).
	if !q.offsetIsRecordBoundaryLocked(f) {
		log.WarningLog.Printf("watch task %s: event-queue cursor %d is not on a record boundary; replaying the queue from the start", q.taskID, q.offset)
		q.offset = 0
	}
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

	if err := q.resetCursorBeforeFreshAppendLocked(); err != nil {
		return err
	}
	q.seq++
	rec, err := json.Marshal(queuedEvent{Seq: q.seq, TS: q.now(), Line: line})
	if err != nil {
		return err
	}
	rec = append(rec, '\n')
	n, err := q.appendRecord(q.path, rec)
	if err != nil {
		// A short write leaves a torn record that would corrupt the NEXT append:
		// O_APPEND writes at the real end of file, so the next record glues onto
		// the torn bytes into one invalid line. Truncate back to the last record
		// boundary so the file stays record-aligned (the caller drops this event
		// — same degradation as enqueue failing outright).
		if n > 0 {
			if terr := q.truncate(q.path, q.size); terr != nil {
				q.recoverTornRecordLocked(n, terr)
			}
		}
		return err
	}
	q.size += int64(n)
	q.pending++

	return q.dropOldestOverCapsLocked()
}

// appendRecordToFile is the production append seam: one O_APPEND write of the
// whole record, returning the byte count written so a short write can be
// recovered by the caller.
func appendRecordToFile(path string, rec []byte) (int, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return 0, err
	}
	n, err := f.Write(rec)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return n, err
}

// recoverTornRecordLocked salvages a short write whose truncate ALSO failed, so
// the torn bytes (n of them, no trailing newline) are stuck on disk. Left as-is
// they would permanently wedge the queue (#1634): the next O_APPEND merges its
// record onto the torn bytes into one invalid line that no restart could clear.
// Best-effort recovery: append a single newline to force a record boundary, so
// the torn fragment becomes its own (unparseable) line that peek() drops and
// skips past instead of merging into the next event. The fragment is counted as
// pending to keep the in-memory count equal to the on-disk line count — else a
// later good event would be miscounted and silently lost when the head is
// skipped. If even the newline append fails, the reader's skip path is the final
// backstop (a merged line is skippable too, only losing the following event).
// Callers hold q.mu.
func (q *eventQueue) recoverTornRecordLocked(n int, truncErr error) {
	f, ferr := os.OpenFile(q.path, os.O_WRONLY|os.O_APPEND, 0644)
	if ferr != nil {
		log.WarningLog.Printf("watch task %s: failed to truncate short-written event record (%v); could not reopen to re-align it: %v", q.taskID, truncErr, ferr)
		return
	}
	_, werr := f.Write([]byte{'\n'})
	if closeErr := f.Close(); werr == nil {
		werr = closeErr
	}
	if werr != nil {
		log.WarningLog.Printf("watch task %s: failed to truncate short-written event record (%v); could not re-align it with a newline: %v", q.taskID, truncErr, werr)
		return
	}
	q.size += int64(n) + 1
	q.pending++
	log.WarningLog.Printf("watch task %s: failed to truncate short-written event record (%v); re-aligned the torn %d bytes with a trailing newline so the queue stays readable", q.taskID, truncErr, n)
}

// resetCursorBeforeFreshAppendLocked removes or zeroes any leftover cursor
// before a brand-new queue file is created. A stale nonzero cursor beside a
// fresh jsonl file could make a later daemon skip bytes on reload.
func (q *eventQueue) resetCursorBeforeFreshAppendLocked() error {
	if q.pending != 0 || q.offset != 0 || q.size != 0 {
		return nil
	}
	if err := q.remove(q.curPath); err != nil && !os.IsNotExist(err) {
		if resetErr := config.AtomicWriteFile(q.curPath, []byte("0"), 0644); resetErr != nil {
			return fmt.Errorf("failed to reset stale event-queue cursor before enqueue: remove failed: %v; reset failed: %w", err, resetErr)
		}
		log.WarningLog.Printf("watch task %s: failed to remove stale event-queue cursor before enqueue; reset it to 0: %v", q.taskID, err)
	}
	return nil
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
//
// Self-heal (#1634): an unreadable head record that is nonetheless record-
// aligned (a corrupt line with a terminating newline — e.g. a torn fragment
// re-aligned by recoverTornRecordLocked, or a merged line from a truncate
// failure) is DROPPED and skipped rather than parking the drainer forever. On-
// disk corruption survives every daemon restart, so parking is a permanent
// wedge; dropping the one bad record and advancing keeps the valid events after
// it reachable. Only a record with no boundary to skip to (an unterminated torn
// tail) is surfaced as an error for the drainer to park on.
func (q *eventQueue) peek() (ev queuedEvent, cursor eventQueueCursor, ok bool, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for q.pending > 0 {
		ev, n, rerr := q.readEventAtLocked(q.offset)
		if rerr == nil {
			return ev, eventQueueCursor{offset: q.offset, length: n, seq: ev.Seq}, true, nil
		}
		if n <= 0 {
			// No record boundary found (unterminated torn tail): nothing safe to
			// skip to. Surface the error; the drainer parks until the next reload.
			return queuedEvent{}, eventQueueCursor{}, false, rerr
		}
		log.WarningLog.Printf("watch task %s: dropping unreadable queued event at offset %d (%d bytes): %v", q.taskID, q.offset, n, rerr)
		q.offset += n
		q.pending--
		if q.pending == 0 {
			if _, derr := q.removeDrainedFilesLocked(); derr != nil {
				return queuedEvent{}, eventQueueCursor{}, false, derr
			}
			return queuedEvent{}, eventQueueCursor{}, false, nil
		}
		if perr := q.persistCursorLocked(); perr != nil {
			return queuedEvent{}, eventQueueCursor{}, false, perr
		}
	}
	return queuedEvent{}, eventQueueCursor{}, false, nil
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
		return q.removeDrainedFilesLocked()
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

// removeDrainedFilesLocked reclaims queue storage after the final event is
// delivered. The in-memory delivered-prefix state must stay intact until the
// jsonl file is gone; otherwise a later append can make already-delivered bytes
// look pending and silently lose the appended event (#1433).
func (q *eventQueue) removeDrainedFilesLocked() (bool, error) {
	if err := q.remove(q.path); err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if err := q.remove(q.curPath); err != nil && !os.IsNotExist(err) {
		if resetErr := config.AtomicWriteFile(q.curPath, []byte("0"), 0644); resetErr != nil {
			q.offset, q.size = 0, 0
			return false, fmt.Errorf("failed to reset drained event-queue cursor: remove failed: %v; reset failed: %w", err, resetErr)
		}
		log.WarningLog.Printf("watch task %s: failed to remove drained event-queue cursor; reset it to 0: %v", q.taskID, err)
	}
	q.offset, q.size = 0, 0
	return true, nil
}

// compactLocked rewrites the queue file to just its pending suffix, dropping
// the delivered prefix: copy suffix → temp file → persist cursor 0 → rename
// over the original → reset the in-memory offset. Crash safety comes from the
// cursor-before-rename ordering (#1537): the post-compaction cursor is 0, and
// it is made durable BEFORE the rename shrinks the file, so no crash can leave
// the small compacted file beside a stale offset that points mid-record. Every
// crash point leaves a record-aligned (file, cursor) pair; the worst case
// redelivers an already-delivered prefix (at-least-once, never loss). Callers
// hold q.mu.
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
	// Persist the post-compaction cursor (0) durably BEFORE the rename shrinks
	// the file, closing the crash window that used to wedge the queue (#1537).
	// The old ordering renamed first and left the caller to persist the cursor
	// after, so a crash in between paired the small compacted file with a stale
	// pre-compaction offset pointing mid-record. Cursor-before-rename makes every
	// crash point consistent and record-aligned: before this write, old file +
	// old offset; after it but before the rename, old file + cursor 0 (redelivers
	// the delivered prefix); after the rename, compacted file + cursor 0.
	if err := q.persistCursorValueLocked(0); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, q.path); err != nil {
		_ = os.Remove(tmpName)
		// The file is still the old (large) one but the cursor is now 0; restore
		// the real offset so the on-disk pair stays consistent (a crash before
		// this restore just redelivers the delivered prefix — at-least-once).
		if rerr := q.persistCursorLocked(); rerr != nil {
			return fmt.Errorf("compaction rename failed (%v); cursor restore also failed: %w", err, rerr)
		}
		return err
	}
	q.offset, q.size = 0, n
	return nil
}

// offsetIsRecordBoundaryLocked reports whether q.offset begins a record in the
// open queue file f. Offset 0 and offset>=size are boundaries by definition;
// any interior offset is a boundary iff the byte before it is the record
// terminator '\n'. ReadAt leaves f's seek position untouched. Callers hold q.mu.
func (q *eventQueue) offsetIsRecordBoundaryLocked(f *os.File) bool {
	if q.offset <= 0 || q.offset >= q.size {
		return true
	}
	var b [1]byte
	if _, err := f.ReadAt(b[:], q.offset-1); err != nil {
		return false
	}
	return b[0] == '\n'
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
func (q *eventQueue) persistCursorValueLocked(off int64) error {
	return config.AtomicWriteFile(q.curPath, []byte(strconv.FormatInt(off, 10)), 0644)
}

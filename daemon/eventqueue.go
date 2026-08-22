package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
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

// eventQueueLoadRetryInterval bounds how often a failed load is re-attempted.
// Retries are driven by hot callers — pendingCount on every stdout line, the
// snapshot RPC's alarm projection on every TUI poll — and each attempt is real
// disk I/O under q.mu, where a Stat against a hung mount would stall every
// queue entry point behind it. One attempt per interval bounds that cost
// while keeping recovery latency small; the drainer's own backoff paces its
// retries beyond it. Package var so tests can shrink it.
var eventQueueLoadRetryInterval = 5 * time.Second

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

	// appendRecord/appendBoundary/truncate are the write seams. Production wires
	// both appends to a real O_APPEND write and truncate to os.Truncate; tests
	// inject them to simulate short writes, truncate failures, and deferred close
	// errors that a local filesystem cannot force deterministically.
	appendRecord   func(path string, rec []byte) (int, error)
	appendBoundary func(path string, rec []byte) (int, error)
	truncate       func(path string, size int64) error
	syncDirectory  func(string) error

	mu      sync.Mutex
	offset  int64 // byte offset of the first undelivered event
	size    int64 // total bytes in the jsonl file
	pending int   // undelivered event count
	seq     int64 // last sequence number handed out

	// loadErr records a failed load (#3242). A Stat/Open/Seek/scan failure
	// leaves the on-disk state UNKNOWN, which is not the same as empty: the
	// file may still hold undelivered records that were never enumerated.
	// While set, offset/size/pending are zeroed and meaningless, and every
	// entry point refuses to trust or mutate the queue — enqueue and peek
	// first retry the load (healing once storage recovers), advance refuses
	// outright. Trusting a fabricated zero here once let a single replayed
	// event "drain" the queue and unlink the file with its backlog inside.
	// lastLoadRetry throttles re-attempts to eventQueueLoadRetryInterval.
	loadErr       error
	lastLoadRetry time.Time

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
// which is what lets a backlog survive a daemon restart or reload. A failed
// recovery still returns a queue, but one that knows its state is unknown
// (loadFailed) and refuses to act until a retried load succeeds (#3242).
func newEventQueue(dir, taskID string) *eventQueue {
	q := &eventQueue{
		taskID:         taskID,
		path:           filepath.Join(dir, taskID+".jsonl"),
		curPath:        filepath.Join(dir, taskID+".cursor"),
		remove:         os.Remove,
		appendRecord:   appendRecordToFile,
		appendBoundary: appendRecordToFile,
		truncate:       os.Truncate,
		syncDirectory:  syncEventQueueDirectory,
		now:            time.Now,
	}
	q.load()
	return q
}

// errEventQueueLoadFailed marks every refusal caused by unknown on-disk state
// after a failed load (#3242). Callers that must distinguish "the queue cannot
// be read right now" (the transient-outage shape — keep retrying) from record
// corruption (park until reload) classify with errors.Is on the error they
// already hold: re-reading queue state after the fact races a concurrent heal
// from another entry point's retry, and a stale read there would misclassify
// the outage as corruption and park a recovered backlog permanently.
var errEventQueueLoadFailed = errors.New("event-queue state unknown after a failed load")

// load recovers the queue state from disk. A missing file is an empty queue; a
// corrupt or unreadable cursor resets to 0 (redelivering pending events —
// at-least-once, never silent loss). Any other failure marks the state UNKNOWN
// via loadErr (#3242): unlike a missing file, an unreadable one may hold
// undelivered records, so nothing may treat the queue as empty or mutate it
// until a later load succeeds.
func (q *eventQueue) load() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.loadLocked()
}

// loadLocked runs one load attempt, zeroing the recovered state first so a
// failure can never leave a half-populated count behind. q.seq is deliberately
// not reset: sequence numbers must never move backward. Logging is gated on
// transitions (#1910 discipline — retries repeat per enqueue/peek/stdout line
// while storage stays broken, and unbounded repetition of a known state is a
// flood): the ERROR logs once on entering failure, and the scan's cursor and
// boundary notices are emitted only when the attempt succeeded (they describe
// a reset that really happens) or alongside that first failure. Callers hold
// q.mu.
func (q *eventQueue) loadLocked() {
	prevErr := q.loadErr
	q.offset, q.size, q.pending, q.loadErr = 0, 0, 0, nil
	var warns []string
	err := q.scanDiskStateLocked(&warns)
	if err != nil {
		q.offset, q.size, q.pending = 0, 0, 0
		q.loadErr = err
		// A failed attempt arms the retry throttle wherever it started — the
		// constructor's included: without this, the run loop's first
		// pendingCount would repeat the same blocking disk I/O back to back at
		// startup, doubling the stall on a hung filesystem.
		q.lastLoadRetry = time.Now()
	}
	if err == nil || prevErr == nil {
		for _, w := range warns {
			log.WarningLog.Printf("watch task %s: %s", q.taskID, w)
		}
	}
	if err != nil && prevErr == nil {
		log.ErrorLog.Printf("watch task %s: failed to load event queue; treating its state as unknown, not empty: %v", q.taskID, err)
	}
}

// retryLoadNowLocked re-attempts recovery unconditionally after a failed load;
// a no-op once a load has succeeded. The event-carrying entry points use it —
// enqueue and the live FIFO gate (pendingCountFresh) — because each such call
// stands for one real event already bounded by the delivery rate window, and
// each is a designed recovery point (#3242) where a stale answer drops the
// event or routes it around a readable backlog. Autonomous callers (snapshot
// polling, the drainer's peek) take the throttled flavor instead: they carry
// no event, so deferring their I/O costs nothing but latency. Callers hold
// q.mu.
func (q *eventQueue) retryLoadNowLocked() error {
	if q.loadErr == nil {
		return nil
	}
	q.lastLoadRetry = time.Now()
	q.loadLocked()
	if q.loadErr == nil {
		log.InfoLog.Printf("watch task %s: event-queue storage recovered; %d pending event(s) restored", q.taskID, q.pending)
	}
	return q.loadErr
}

// retryLoadLocked is the throttled flavor for pure observers — pendingCount,
// which the snapshot RPC polls — at most one disk attempt per
// eventQueueLoadRetryInterval, answering the recorded error between attempts so
// hot read-only callers stay cheap during an outage. A failed constructor load
// arms the throttle too (loadLocked), so startup performs one blocking disk
// attempt, not two back to back. Callers hold q.mu.
func (q *eventQueue) retryLoadLocked() error {
	if q.loadErr == nil {
		return nil
	}
	if time.Since(q.lastLoadRetry) < eventQueueLoadRetryInterval {
		return q.loadErr
	}
	return q.retryLoadNowLocked()
}

// scanDiskStateLocked reads the on-disk queue state into memory, returning an
// error whenever the state could not be fully enumerated. Cursor/boundary
// notices are appended to warns rather than logged, so loadLocked can gate
// them on transitions instead of repeating them every retry. Callers hold
// q.mu.
func (q *eventQueue) scanDiskStateLocked(warns *[]string) error {
	info, err := os.Stat(q.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no queue file: empty queue
		}
		return fmt.Errorf("stat event queue: %w", err)
	}
	q.size = info.Size()

	if raw, err := os.ReadFile(q.curPath); err == nil {
		if off, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64); err == nil && off >= 0 && off <= q.size {
			q.offset = off
		} else {
			*warns = append(*warns, "corrupt event-queue cursor; replaying the queue from the start")
		}
	} else if !os.IsNotExist(err) {
		// An unreadable cursor gets the corrupt-cursor treatment, not loadErr:
		// replaying from 0 redelivers the delivered prefix (at-least-once, never
		// loss), while parking the queue on a single bad cursor inode would
		// wedge it — the next persist rewrites the cursor via rename anyway.
		*warns = append(*warns, fmt.Sprintf("cannot read event-queue cursor (%v); replaying the queue from the start", err))
	}

	// Count the pending events and recover the sequence counter. The pending
	// suffix is bounded by watcherQueueMaxBytes, so this scan is cheap. A
	// bufio.Reader (not a Scanner) on purpose: JSON-escaping can inflate a
	// maxWatchLineBytes line severalfold, and a Scanner's token cap would make
	// the count silently stop at the first oversized record — losing exactly
	// the events durability promised to keep.
	f, err := os.Open(q.path)
	if err != nil {
		if os.IsNotExist(err) {
			// Removed between Stat and Open: an empty queue after all.
			q.offset, q.size = 0, 0
			return nil
		}
		return fmt.Errorf("open event queue: %w", err)
	}
	defer func() { _ = f.Close() }()

	// Defense-in-depth (#1537): a cursor that clears the off<=size bound can
	// still point into the MIDDLE of a record — e.g. a crash during compaction
	// that shrank the file but left a pre-compaction offset behind. Records
	// always end in '\n', so a real interior boundary has '\n' immediately
	// before it; if q.offset isn't one, replay from the start rather than
	// parking the drainer forever on an unreadable head (redelivering a few
	// already-delivered events is at-least-once, a wedged queue is not).
	boundary, err := q.offsetIsRecordBoundaryLocked(f)
	if err != nil {
		return fmt.Errorf("check event-queue record boundary at offset %d: %w", q.offset, err)
	}
	if !boundary {
		*warns = append(*warns, fmt.Sprintf("event-queue cursor %d is not on a record boundary; replaying the queue from the start", q.offset))
		q.offset = 0
	}
	if _, err := f.Seek(q.offset, 0); err != nil {
		return fmt.Errorf("seek event queue to offset %d: %w", q.offset, err)
	}
	br := bufio.NewReaderSize(f, 64*1024)
	scanned := q.offset
	for {
		raw, err := br.ReadBytes('\n')
		if err != nil {
			if !errors.Is(err, io.EOF) {
				// A mid-scan read error is NOT a torn tail: the bytes past this
				// point exist but could not be enumerated, and the torn-tail
				// truncate below would delete them (#3242). Unknown state.
				return fmt.Errorf("scan event queue at offset %d: %w", scanned, err)
			}
			if len(raw) > 0 {
				// A trailing record with no newline is a torn append (daemon
				// died mid-write). It was never fully enqueued; truncate it
				// away so the next append starts on a record boundary instead
				// of gluing two records into one corrupt line.
				log.WarningLog.Printf("watch task %s: discarding %d bytes of torn trailing event-queue record", q.taskID, len(raw))
				if terr := q.truncate(q.path, scanned); terr != nil {
					// q.size already includes the torn bytes discovered during load;
					// recoverTornRecordLocked accounts only for the new boundary byte.
					q.recoverTornRecordLocked(len(raw), terr, true)
				} else {
					q.size = scanned
				}
			}
			return nil
		}
		scanned += int64(len(raw))
		q.pending++
		var ev queuedEvent
		if uerr := json.Unmarshal(raw, &ev); uerr == nil && ev.Seq > q.seq {
			q.seq = ev.Seq
		}
	}
}

// pendingCount reports how many undelivered events the queue holds. After a
// failed load it first retries recovery, so a healed filesystem restores the
// real count (and FIFO routing) on the next live event; while storage stays
// unreadable it reports 0, and callers that must distinguish empty from
// unknown check loadFailed — the mutating entry points never trust this zero.
func (q *eventQueue) pendingCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	_ = q.retryLoadLocked()
	return q.pending
}

// pendingCountFresh is pendingCount with an unthrottled recovery attempt, for
// the live FIFO gate (#3242): a live event is a real recovery point, and a
// stale throttled zero there would route it AROUND a backlog that is already
// readable again — breaking FIFO for no gain, since the very same event's
// failed delivery would retry the load in enqueue anyway.
func (q *eventQueue) pendingCountFresh() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	_ = q.retryLoadNowLocked()
	return q.pending
}

// pendingState reports the undelivered count and whether that count is UNKNOWN
// as one atomic read (#3242): fetching them through separate calls races a
// concurrent heal into "0 pending, known" — the exact fabricated banner the
// unknown flag exists to prevent. The throttled retry keeps snapshot polling
// cheap during an outage.
func (q *eventQueue) pendingState() (pending int, unknown bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	_ = q.retryLoadLocked()
	return q.pending, q.loadErr != nil
}

// replayNeeded reports, in one atomic read, whether the drainer has work: a
// recovered backlog, or UNKNOWN state whose first peek must recover or park
// (#3242). Split count/state calls race a concurrent snapshot heal into
// "0, known" — skipping the drainer and stranding the healed backlog until
// the next reload.
func (q *eventQueue) replayNeeded() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	_ = q.retryLoadLocked()
	return q.pending > 0 || q.loadErr != nil
}

// loadFailed reports whether the queue's on-disk state is currently unknown
// because its last load attempt failed (#3242).
func (q *eventQueue) loadFailed() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.loadErr != nil
}

// enqueue appends one event and enforces the overflow caps by dropping oldest
// pending events past them.
func (q *eventQueue) enqueue(line string) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	// Unknown state admits no safe mutation (#3242): the in-memory size/offset
	// are fabricated zeros, so the fresh-queue cursor reset below could destroy
	// a live cursor, and a torn-write recovery would truncate to the wrong
	// boundary. Retry the load; refuse (caller drops the event, logged) only
	// while the state stays unknown.
	if err := q.retryLoadNowLocked(); err != nil {
		return fmt.Errorf("%w; refusing to append: %w", errEventQueueLoadFailed, err)
	}
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
		if n >= len(rec) {
			// The record landed IN FULL and only the close/flush failed (#2107) —
			// a real POSIX case, since close surfaces deferred writeback errors.
			// Truncating here would destroy a complete event: "close failed" is
			// not "write torn". The truncate below exists solely because O_APPEND
			// would glue the next record onto TORN bytes, and a complete record
			// leaves none to glue onto, so the file is already record-aligned.
			// Keep it, and account for it — the on-disk record must be counted in
			// memory or a later torn write would truncate back to a stale size and
			// delete it after the fact. Same philosophy as compactLocked, whose
			// temp-file+rename leaves the original intact when its close fails.
			q.size += int64(n)
			q.pending++
			log.WarningLog.Printf("watch task %s: event-queue append succeeded but close failed; kept the fully-written record: %v", q.taskID, err)
			// Enforce the caps as the success path does: a persistently failing
			// close must not let the backlog outgrow its bounds. The close error is
			// the one worth returning, so a cap failure only gets logged.
			if capErr := q.dropOldestOverCapsLocked(); capErr != nil {
				log.WarningLog.Printf("watch task %s: failed to enforce event-queue caps after a close failure: %v", q.taskID, capErr)
			}
			return err
		}
		// A short write leaves a torn record that would corrupt the NEXT append:
		// O_APPEND writes at the real end of file, so the next record glues onto
		// the torn bytes into one invalid line. Truncate back to the last record
		// boundary so the file stays record-aligned (the caller drops this event
		// — same degradation as enqueue failing outright).
		if n > 0 {
			if terr := q.truncate(q.path, q.size); terr != nil {
				q.recoverTornRecordLocked(n, terr, false)
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

// recoverTornRecordLocked salvages torn bytes whose truncate ALSO failed. This
// handles both a short write in enqueue and a torn tail found by load after a
// restart. Left as-is, the next O_APPEND merges its record onto the torn bytes
// into one invalid line that no restart could clear.
// Best-effort recovery: append a single newline to force a record boundary, so
// the torn fragment becomes its own (unparseable) line that peek() drops and
// skips past instead of merging into the next event. The fragment is counted as
// pending to keep the in-memory count equal to the on-disk line count — else a
// later good event would be miscounted and silently lost when the head is
// skipped. If even the newline append fails, the reader's skip path is the final
// backstop (a merged line is skippable too, only losing the following event).
// sizeIncludesTorn distinguishes load (q.size came from stat and already counts
// the fragment) from enqueue (q.size is still the last good boundary). Callers
// hold q.mu.
func (q *eventQueue) recoverTornRecordLocked(n int, truncErr error, sizeIncludesTorn bool) {
	written, werr := q.appendBoundary(q.path, []byte{'\n'})
	if written < 1 {
		if werr == nil {
			werr = io.ErrShortWrite
		}
		log.WarningLog.Printf("watch task %s: failed to truncate torn event-queue record (%v); could not re-align it with a newline: %v", q.taskID, truncErr, werr)
		return
	}
	if !sizeIncludesTorn {
		q.size += int64(n)
	}
	q.size++ // the recovery newline
	q.pending++
	if werr != nil {
		// The full boundary byte is visible and must be counted, just like a
		// fully-written event record whose Close reports deferred writeback
		// failure. Its crash durability is unknown, so keep that warning explicit;
		// a restart will inspect and recover the tail again if it did not persist.
		log.WarningLog.Printf("watch task %s: failed to truncate torn event-queue record (%v); re-aligned the torn %d bytes, but could not confirm the recovery newline on close: %v", q.taskID, truncErr, n, werr)
		return
	}
	log.WarningLog.Printf("watch task %s: failed to truncate torn event-queue record (%v); re-aligned the torn %d bytes with a trailing newline so the queue stays readable", q.taskID, truncErr, n)
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
	// A failed load must surface as an ERROR here, never as a drained-empty
	// queue (#3242): the drainer holds on it and keeps retrying, instead of
	// concluding there is nothing to replay. Throttled on purpose: the drain
	// loop retries on its backoff cadence with no event in hand, so its first
	// peek after a failed constructor load must not repeat the blocking I/O
	// the constructor just performed — recovery defers to the retry interval
	// (the production drain backoff is longer anyway).
	if err := q.retryLoadLocked(); err != nil {
		return queuedEvent{}, eventQueueCursor{}, false, fmt.Errorf("%w: %w", errEventQueueLoadFailed, err)
	}
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
	// Belt-and-braces, not a live path today: loadErr is only ever set before
	// the first successful load, and every real cursor comes from a successful
	// peek on this same instance, so a production advance cannot currently see
	// it. The guard stays because advance is the method that consumes and
	// deletes — the #3242 invariant is that nothing may remove state that was
	// never enumerated, and a future change to the load lifecycle must land on
	// this refusal, not on a silent (false, nil) "re-peek" answer.
	if q.loadErr != nil {
		return false, fmt.Errorf("%w; refusing to advance: %w", errEventQueueLoadFailed, q.loadErr)
	}
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
// the delivered prefix: persist cursor 0 → sync its directory entry → create,
// copy, sync, and close the temp file → rename over the original → commit the
// matching in-memory offset → sync the containing directory. Crash safety comes
// from both halves of that publication: the cursor reset and compacted contents
// reach stable storage before the queue rename, and the queue's new directory
// entry does afterward. Creating the temp only after the cursor fence prevents
// that fence from also making an abandoned compact-file name durable. The
// cursor-before-rename ordering (#1537) separately ensures no crash can leave
// the small compacted file beside a stale offset that points mid-record. Every
// crash point therefore preserves the pending suffix; the worst case redelivers
// an already-delivered prefix (at-least-once, never loss). Callers hold q.mu.
func (q *eventQueue) compactLocked() error {
	src, err := os.Open(q.path)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	if _, err := src.Seek(q.offset, 0); err != nil {
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
		return err
	}
	// AtomicWriteFile's directory sync is intentionally best-effort for its
	// general callers. Compaction needs a stronger cross-file guarantee: cursor
	// 0 must be durable before the shortened queue can become durable, or a
	// crash could pair the new queue with the old nonzero cursor and skip events.
	// Fence it before creating the compact temp so this sync cannot also persist
	// an abandoned temp-file name if the daemon crashes before the queue rename.
	if err := q.syncDirectory(filepath.Dir(q.curPath)); err != nil {
		return fmt.Errorf("sync event-queue cursor directory before compaction: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(q.path), q.taskID+".compact-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	n, err := io.Copy(tmp, src)
	if err == nil {
		if syncErr := tmp.Sync(); syncErr != nil {
			err = fmt.Errorf("sync compacted event queue: %w", syncErr)
		}
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
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
	// Rename is already visible, so commit the matching in-memory cursor before
	// attempting the directory sync. If that sync fails, advance's optimization
	// fallback persists q.offset again; it must write 0 beside the compacted file,
	// never the pre-compaction offset that points into a different byte layout.
	q.offset, q.size = 0, n
	if err := q.syncDirectory(filepath.Dir(q.path)); err != nil {
		return fmt.Errorf("sync event-queue directory after compaction: %w", err)
	}
	return nil
}

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
func (q *eventQueue) persistCursorValueLocked(off int64) error {
	return config.AtomicWriteFile(q.curPath, []byte(strconv.FormatInt(off, 10)), 0644)
}

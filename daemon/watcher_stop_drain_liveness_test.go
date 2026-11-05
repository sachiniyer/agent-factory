package daemon

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestPersistRemainingLimitEventsReportsReaderActivity verifies the primary fix:
// the teardown drain updates the reader-liveness signals (readerBusy/
// readerProgress) so readerWorking() reports true while the drain is actively
// emitting. Without these bumps, awaitWatchReaders misclassifies the drain as
// idle and closes the stdout read end mid-drain, silently discarding events
// still staged in the kernel pipe (#4321 review).
func TestPersistRemainingLimitEventsReportsReaderActivity(t *testing.T) {
	queue := newEventQueue(t.TempDir(), "drain-liveness")
	for i := 0; i < watcherQueueMaxEvents; i++ {
		if err := queue.enqueue(fmt.Sprintf("old-%03d", i), true); err != nil {
			t.Fatalf("seed event %d: %v", i, err)
		}
	}

	stopCh := make(chan struct{})
	close(stopCh)
	w := &taskWatcher{taskID: "drain-liveness", sup: newWatcherSupervisor(), queue: queue, stopCh: stopCh}

	// Block the first append so the drain is mid-emit when inspected.
	appendStarted := make(chan struct{}, 1)
	releaseAppend := make(chan struct{})
	queue.appendRecord = func(path string, rec []byte) (int, error) {
		select {
		case appendStarted <- struct{}{}:
		default:
		}
		<-releaseAppend
		return appendRecordToFile(path, rec)
	}

	br := bufio.NewReaderSize(strings.NewReader("drain-1\ndrain-2\ndrain-3\n"), maxWatchLineBytes)
	drainDone := make(chan struct{})
	go func() {
		w.persistRemainingLimitEvents(br, &tailBuffer{})
		close(drainDone)
	}()

	select {
	case <-appendStarted:
	case <-time.After(time.Second):
		t.Fatal("drain did not start appending the first event")
	}

	// While the drain is mid-emit, the reader-liveness signals must report
	// activity so awaitWatchReaders keeps waiting.
	if w.readerBusy.Load() <= 0 {
		t.Fatalf("readerBusy = %d during drain emit, want > 0", w.readerBusy.Load())
	}
	working := w.readerWorking()
	if !working() {
		t.Fatal("readerWorking() returned false while the drain is actively emitting")
	}

	close(releaseAppend)
	select {
	case <-drainDone:
	case <-time.After(time.Second):
		t.Fatal("drain did not finish after release")
	}

	// All three lines were enqueued past the ordinary cap (limit-protected).
	if got := queue.pendingCount(); got != watcherQueueMaxEvents+3 {
		t.Fatalf("enqueued = %d, want %d (seed + 3 drain lines)", got, watcherQueueMaxEvents+3)
	}
	if w.dropped != 0 {
		t.Fatalf("drain counted drops: %d", w.dropped)
	}
}

// TestPersistRemainingLimitEventsReportsActivityWhenQueueUnreadable verifies the
// loadFailedFresh drain branch also reports liveness: the drain is still
// actively reading and accounting events even when the queue refuses every
// enqueue, so awaitWatchReaders must wait for it too.
func TestPersistRemainingLimitEventsReportsActivityWhenQueueUnreadable(t *testing.T) {
	dir := t.TempDir()
	seed := newEventQueue(dir, "drain-unreadable")
	if err := seed.enqueue("parked-before-stop", true); err != nil {
		t.Fatalf("seed protected event: %v", err)
	}
	realQueuePath := seed.path
	backupPath := realQueuePath + ".readable"
	if err := os.Rename(realQueuePath, backupPath); err != nil {
		t.Fatalf("move queue behind unreadable fixture: %v", err)
	}
	if err := os.Mkdir(realQueuePath, 0o755); err != nil {
		t.Fatalf("replace queue file with unreadable directory: %v", err)
	}
	queue := newEventQueue(dir, "drain-unreadable")
	if !queue.loadFailed() {
		t.Fatal("fixture did not make queue state unreadable")
	}

	stopCh := make(chan struct{})
	close(stopCh)
	s := newWatcherSupervisor()
	// Block the first drop persist so the drain is mid-emit when inspected.
	dropStarted := make(chan struct{}, 1)
	releaseDrop := make(chan struct{})
	s.recordDrops = func(_ string, _ int, _ time.Time) error {
		select {
		case dropStarted <- struct{}{}:
		default:
		}
		<-releaseDrop
		return nil
	}
	w := &taskWatcher{taskID: "drain-unreadable", sup: s, queue: queue, stopCh: stopCh}

	br := bufio.NewReaderSize(strings.NewReader("drain-1\ndrain-2\n"), maxWatchLineBytes)
	drainDone := make(chan struct{})
	go func() {
		w.persistRemainingLimitEvents(br, &tailBuffer{})
		close(drainDone)
	}()

	select {
	case <-dropStarted:
	case <-time.After(time.Second):
		t.Fatal("drain did not start accounting the first dropped event")
	}

	if w.readerBusy.Load() <= 0 {
		t.Fatalf("readerBusy = %d during unreadable-queue drain emit, want > 0", w.readerBusy.Load())
	}
	working := w.readerWorking()
	if !working() {
		t.Fatal("readerWorking() returned false during unreadable-queue drain emit")
	}

	close(releaseDrop)
	select {
	case <-drainDone:
	case <-time.After(time.Second):
		t.Fatal("drain did not finish after release")
	}

	if w.dropped != 2 {
		t.Fatalf("unreadable-queue drain dropped = %d, want 2", w.dropped)
	}
}

// TestStopDrainKeepsReadEndOpenDuringSlowDrain verifies the composition at the
// heart of the bug: consumeLines parks on limit backpressure, stop requests the
// teardown drain, and awaitWatchReaders runs concurrently with readerWorking().
// With the fix, the drain's liveness signals keep awaitWatchReaders from
// closing the stdout read end mid-drain.
func TestStopDrainKeepsReadEndOpenDuringSlowDrain(t *testing.T) {
	queue := newEventQueue(t.TempDir(), "stop-drain-bound")
	for i := 0; i < watcherQueueMaxEvents; i++ {
		if err := queue.enqueue(fmt.Sprintf("old-%03d", i), true); err != nil {
			t.Fatalf("seed event %d: %v", i, err)
		}
	}

	// Block the first append so the drain is mid-emit when awaitWatchReaders
	// checks the liveness predicate.
	appendStarted := make(chan struct{}, 1)
	releaseAppend := make(chan struct{})
	queue.appendRecord = func(path string, rec []byte) (int, error) {
		select {
		case appendStarted <- struct{}{}:
		default:
		}
		<-releaseAppend
		return appendRecordToFile(path, rec)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer w.Close()

	const drainLines = 5
	var payload strings.Builder
	for i := 0; i < drainLines; i++ {
		payload.WriteString(fmt.Sprintf("drain-line-%02d\n", i))
	}
	if _, err := w.WriteString(payload.String()); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	stopCh := make(chan struct{})
	close(stopCh)
	writersStopped := make(chan struct{})
	close(writersStopped)

	s := newWatcherSupervisor()
	s.stopGrace = 20 * time.Millisecond
	watcher := &taskWatcher{taskID: "stop-drain-bound", sup: s, queue: queue, stopCh: stopCh}

	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		defer r.Close()
		watcher.consumeLines(r, &tailBuffer{}, writersStopped)
	}()

	select {
	case <-appendStarted:
	case <-time.After(time.Second):
		t.Fatal("drain did not start appending")
	}

	// The drain is now mid-emit (blocked in appendRecord). Run
	// awaitWatchReaders with a short grace — it must NOT close the read end
	// because readerWorking() reports true.
	awaitDone := make(chan struct{})
	go func() {
		awaitWatchReaders("stop-drain-bound", true, s.stopGrace, []*os.File{r}, watcher.readerWorking(), readerDone)
		close(awaitDone)
	}()

	// Wait well past the grace. Without the fix, awaitWatchReaders would have
	// closed r by now.
	time.Sleep(3 * s.stopGrace)

	if _, err := r.Stat(); err != nil {
		t.Fatalf("awaitWatchReaders closed the read end mid-drain (liveness predicate did not report activity): %v", err)
	}

	// Release the drain and close the write end so the reader reaches EOF.
	close(releaseAppend)
	if err := w.Close(); err != nil {
		t.Fatalf("close write end: %v", err)
	}

	select {
	case <-readerDone:
	case <-time.After(time.Second):
		t.Fatal("reader did not finish after release")
	}
	select {
	case <-awaitDone:
	case <-time.After(time.Second):
		t.Fatal("awaitWatchReaders did not return")
	}

	if got := queue.pendingCount(); got != watcherQueueMaxEvents+drainLines {
		t.Fatalf("enqueued = %d, want %d (seed + %d drain lines)", got, watcherQueueMaxEvents+drainLines, drainLines)
	}
	if watcher.dropped != 0 {
		t.Fatalf("drain counted drops: %d", watcher.dropped)
	}
}

// TestStopDrainPreservesAllStagedEventsWithSlowQueue is the end-to-end
// reproduction: a limit-parked task whose pipe holds more than bufio's 64KB
// buffer, a slow queue whose per-line persist cost outlasts stopGrace, and the
// full runOnce wiring (consumeLines + awaitWatchReaders with readerWorking).
// Without the fix, awaitWatchReaders closes the read end mid-drain and the
// events still in the kernel pipe are silently discarded. With the fix, all
// staged events are enqueued.
func TestStopDrainPreservesAllStagedEventsWithSlowQueue(t *testing.T) {
	queue := newEventQueue(t.TempDir(), "stop-drain-e2e")
	for i := 0; i < watcherQueueMaxEvents; i++ {
		if err := queue.enqueue(fmt.Sprintf("old-%03d", i), true); err != nil {
			t.Fatalf("seed event %d: %v", i, err)
		}
	}

	// Slow append so the drain outlasts stopGrace: each line takes a
	// millisecond, so hundreds of lines cross the grace boundary.
	queue.appendRecord = func(path string, rec []byte) (int, error) {
		time.Sleep(time.Millisecond)
		return appendRecordToFile(path, rec)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	// 128KB of 64-byte lines (2048 lines): fills the 64KB kernel pipe and
	// overflows into bufio's 64KB buffer once the reader drains the pipe.
	const lineCount = 2048
	const lineLen = 64
	var payload strings.Builder
	for i := 0; i < lineCount; i++ {
		line := fmt.Sprintf("event-%06d", i)
		if pad := lineLen - len(line) - 1; pad > 0 {
			line += strings.Repeat("x", pad)
		}
		line += "\n"
		payload.WriteString(line)
	}
	payloadBytes := []byte(payload.String())
	if len(payloadBytes) != lineCount*lineLen {
		t.Fatalf("payload = %d bytes, want %d", len(payloadBytes), lineCount*lineLen)
	}

	// Writer goroutine: writes 128KB, blocks when the 64KB pipe is full.
	// The drain reading from the pipe unblocks the writer so the remaining
	// 64KB lands in the kernel pipe — the data the bug silently discards.
	writerDone := make(chan struct{})
	var writerErr error
	go func() {
		defer close(writerDone)
		_, writerErr = w.Write(payloadBytes)
		_ = w.Close()
	}()

	stopCh := make(chan struct{})
	close(stopCh)
	writersStopped := make(chan struct{})
	close(writersStopped)

	s := newWatcherSupervisor()
	s.stopGrace = 30 * time.Millisecond
	watcher := &taskWatcher{taskID: "stop-drain-e2e", sup: s, queue: queue, stopCh: stopCh}

	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		defer r.Close()
		watcher.consumeLines(r, &tailBuffer{}, writersStopped)
	}()

	// Mirror runOnce: awaitWatchReaders runs after writersStopped, with
	// readerWorking() and the configured grace.
	awaitWatchReaders("stop-drain-e2e", true, s.stopGrace, []*os.File{r}, watcher.readerWorking(), readerDone)

	select {
	case <-writerDone:
	case <-time.After(10 * time.Second):
		t.Fatal("writer did not finish after the drain")
	}

	if writerErr != nil {
		t.Fatalf("writer error: %v", writerErr)
	}

	got := queue.pendingCount()
	want := watcherQueueMaxEvents + lineCount
	if got != want {
		t.Fatalf("enqueued = %d, want %d (seed + %d drain lines); lost %d events",
			got, want, lineCount, want-got)
	}
	if watcher.dropped != 0 {
		t.Fatalf("drain counted drops: %d", watcher.dropped)
	}
}

// errorAfterData delivers its data, then returns a non-EOF error on the next
// Read — simulating the stdout read end being closed under the drain.
type errorAfterData struct {
	data []byte
	pos  int
	sent bool
}

func (r *errorAfterData) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, fmt.Errorf("read end closed under the drain")
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

// TestPersistRemainingLimitEventsLogsNonEOFReadError verifies the defense-in-
// depth: a non-EOF read error during the drain (e.g. the read end closed under
// it) is logged and any partial chunk is kept in the run tail, rather than
// being silently discarded.
func TestPersistRemainingLimitEventsLogsNonEOFReadError(t *testing.T) {
	queue := newEventQueue(t.TempDir(), "drain-read-err")
	for i := 0; i < watcherQueueMaxEvents; i++ {
		if err := queue.enqueue(fmt.Sprintf("old-%03d", i), true); err != nil {
			t.Fatalf("seed event %d: %v", i, err)
		}
	}

	stopCh := make(chan struct{})
	close(stopCh)
	w := &taskWatcher{taskID: "drain-read-err", sup: newWatcherSupervisor(), queue: queue, stopCh: stopCh}

	// "first-line\n" is a complete event; "second-line-partial" has no newline
	// and the reader returns a non-EOF error after the data is exhausted.
	br := bufio.NewReaderSize(&errorAfterData{data: []byte("first-line\nsecond-line-partial")}, maxWatchLineBytes)
	tail := &tailBuffer{}
	w.persistRemainingLimitEvents(br, tail)

	// The first complete line was enqueued.
	if got := queue.pendingCount(); got != watcherQueueMaxEvents+1 {
		t.Fatalf("enqueued = %d, want %d (seed + first complete line)", got, watcherQueueMaxEvents+1)
	}
	// The partial second line was kept in the tail (not silently discarded).
	if !strings.Contains(tail.logSuffix(), "second-line-partial") {
		t.Fatalf("partial chunk was not kept in the run tail: %q", tail.logSuffix())
	}
}

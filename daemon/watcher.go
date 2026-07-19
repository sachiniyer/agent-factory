package daemon

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/task"
)

// The watcher supervisor hosts watch tasks (#782 phase 2): for every enabled
// task with a watch_cmd it keeps `$SHELL -c watch_cmd` running, turns each
// newline-terminated stdout line into one event, and delivers the rendered
// prompt through the same path cron fires use (create a session, or send into
// target_session). The script contract:
//
//   - each newline-terminated stdout line = one event; lines over 64KB are
//     truncated with a logged note
//   - stderr appends to ~/.agent-factory/logs/task-<id>.log, size-capped by
//     the same log_max_size_mb/log_max_backups rotation as the main log (#1062)
//   - env: AF_TASK_ID, AF_TASK_NAME, AF_PROJECT_PATH
//   - exit 0 = intentional stop (status "stopped"; re-armed by the next
//     reload or re-enable); non-zero = restart with exponential backoff
//   - ≥5 non-zero exits within 10 minutes = crash loop (status "errored",
//     restarts stop until the next reload)
//   - events above 10/min per task are dropped with a logged warning
//   - a failed delivery queues the event durably and a stop-aware drainer
//     replays the backlog in order — before newer live events — once
//     deliveries succeed again, rate-limited by the same 10/min window;
//     the backlog is bounded (oldest dropped past 500 events / 256KB, with
//     a logged count), survives daemon restarts, and events older than 72h
//     are expired at replay time with a logged count (#1129)
//   - each run buffers its recent output (last 10 lines / 2KB: stdout lines
//     that did not become delivered events, plus stderr). Non-zero exits log
//     that tail, and the crash-loop breaker persists "errored: <exit>:
//     <first line>" into last_run_status so `af tasks list` and the TUI
//     show why the task errored (#797)
const (
	// maxWatchLineBytes caps how much of a single stdout line becomes an
	// event; the rest of the line is discarded with a logged note.
	maxWatchLineBytes = 64 * 1024

	watcherEventsPerMinute = 10
	watcherBaseBackoff     = time.Second
	watcherMaxBackoff      = 5 * time.Minute
	watcherCrashWindow     = 10 * time.Minute
	watcherCrashMaxExits   = 5

	// watcherStopGrace bounds how long a stop request waits after SIGTERM
	// before escalating to a process-group SIGKILL. Mirrors
	// sigtermFallbackGrace on the daemon-shutdown path.
	watcherStopGrace = 5 * time.Second

	// watcherTailMaxLines/watcherTailMaxBytes bound the per-run buffer of
	// recent script output kept for failure diagnostics (#797): stdout
	// lines that did not become delivered events, plus stderr. The byte cap
	// applies both per line and to the buffer total.
	watcherTailMaxLines = 10
	watcherTailMaxBytes = 2 * 1024

	// watcherStatusSummaryMax caps the failure summary the crash-loop
	// breaker persists into the task's last_run_status, so tasks.json and
	// the TUI detail row stay readable.
	watcherStatusSummaryMax = 256

	// watcherDrainBaseBackoff/watcherDrainMaxBackoff pace the event-queue
	// drainer's retries after a failed replay delivery (#1129): 10s doubling
	// to 5m, then settling at the 5m cadence for as long as the failure
	// persists — the #1128 never-give-up discipline, because an outage is
	// indistinguishable from a broken target while it lasts.
	watcherDrainBaseBackoff = 10 * time.Second
	watcherDrainMaxBackoff  = 5 * time.Minute
)

// truncateRunes returns s limited to at most maxBytes bytes, cut on a UTF-8
// rune boundary so the result is always valid UTF-8. Slicing a string by a
// raw byte index can split a multi-byte rune, and encoding/json persists the
// half rune as U+FFFD ("�"), corrupting non-ASCII failure diagnostics in
// tasks.json (#863, a regression of #797/#799). Callers append any ellipsis
// themselves so the cap covers only the retained content.
func truncateRunes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	// maxBytes may land inside a rune; back up to the start of that rune so
	// the cut falls on a boundary and s[:end] holds only whole runes.
	end := maxBytes
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end]
}

// watcherSupervisor owns the running watch-task processes. Reload reconciles
// them against tasks.json the same way taskScheduler.Reload reconciles cron
// entries; the ReloadTasks RPC drives both so CLI/TUI edits take effect live.
type watcherSupervisor struct {
	mu       sync.Mutex
	watchers map[string]*taskWatcher // task ID → supervised watcher

	// Injection points for tests: loadTasks substitutes fixture task lists,
	// deliver observes events without spawning sessions, setStatus observes
	// lifecycle statuses without a task store, and queueDir redirects the
	// durable event queues to a scratch directory.
	loadTasks func() ([]task.Task, error)
	deliver   func(taskID, line string) error
	setStatus func(taskID, status string)
	logPath   func(taskID string) (string, error)
	queueDir  func() (string, error)

	shell            string
	baseBackoff      time.Duration
	maxBackoff       time.Duration
	crashWindow      time.Duration
	crashMaxExits    int
	eventsPerMinute  int
	stopGrace        time.Duration
	drainBaseBackoff time.Duration
	drainMaxBackoff  time.Duration
	queueMaxAge      time.Duration
}

func newWatcherSupervisor() *watcherSupervisor {
	return &watcherSupervisor{
		watchers:         make(map[string]*taskWatcher),
		loadTasks:        task.LoadTasks,
		deliver:          deliverWatchEvent,
		setStatus:        persistWatcherStatus,
		logPath:          watcherLogPath,
		queueDir:         eventQueueDir,
		shell:            watcherShell(),
		baseBackoff:      watcherBaseBackoff,
		maxBackoff:       watcherMaxBackoff,
		crashWindow:      watcherCrashWindow,
		crashMaxExits:    watcherCrashMaxExits,
		eventsPerMinute:  watcherEventsPerMinute,
		stopGrace:        watcherStopGrace,
		drainBaseBackoff: watcherDrainBaseBackoff,
		drainMaxBackoff:  watcherDrainMaxBackoff,
		queueMaxAge:      watcherQueueMaxAge,
	}
}

// Reload re-reads tasks.json and reconciles the running watcher set: enabled
// watch tasks without a live watcher are started — including ones whose
// script previously exited or crash-looped, so a reload (or re-enable) is the
// re-arm path. Watchers whose task was disabled or removed are stopped, and a
// watcher whose process-defining fields changed is restarted with the new
// config. Delivery-only fields (prompt, target_session, program) are not part
// of that signature: deliverWatchEvent re-loads the task per event, so editing
// them takes effect without killing a long-lived watch script.
func (s *watcherSupervisor) Reload() error {
	tasks, err := s.loadTasks()
	if err != nil {
		return err
	}

	desired := make(map[string]task.Task)
	for _, t := range tasks {
		if !t.Enabled || !t.IsWatch() {
			continue
		}
		// The ID flows into the stderr log path; reject hand-edited IDs the
		// same way RunTask does before any filesystem path is built.
		if err := task.ValidateTaskID(t.ID); err != nil {
			log.WarningLog.Printf("not watching task with invalid id %q: %v", t.ID, err)
			continue
		}
		desired[t.ID] = t
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var stale []*taskWatcher
	for id, w := range s.watchers {
		t, ok := desired[id]
		if ok && watcherSignature(t) == w.sig && !w.finished() {
			continue
		}
		stale = append(stale, w)
		delete(s.watchers, id)
	}
	// Wait for stale watchers to die before starting replacements so two
	// processes for the same task never overlap. Bounded by stopGrace via
	// the per-watcher SIGKILL escalation.
	stopWatchers(stale)

	for id, t := range desired {
		if _, running := s.watchers[id]; running {
			continue
		}
		w := s.newTaskWatcher(t)
		s.watchers[id] = w
		go w.run()
	}

	// Queue files for tasks that no longer exist at all are removed — a
	// deleted task's backlog must not replay into a recreated namesake. A
	// merely-disabled task keeps its backlog for re-enable (#1129). Runs after
	// stopWatchers so no stale drainer is mid-replay on a file being removed.
	s.cleanOrphanQueues(tasks)
	return nil
}

// cleanOrphanQueues removes event-queue files whose task ID is absent from
// tasks.json entirely.
func (s *watcherSupervisor) cleanOrphanQueues(tasks []task.Task) {
	dir, err := s.queueDir()
	if err != nil {
		return
	}
	known := make(map[string]struct{}, len(tasks))
	for _, t := range tasks {
		known[t.ID] = struct{}{}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		id := strings.TrimSuffix(strings.TrimSuffix(name, ".jsonl"), ".cursor")
		if id == name { // neither suffix matched
			continue
		}
		if _, ok := known[id]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			log.WarningLog.Printf("failed to remove orphan event-queue file %s: %v", name, err)
		}
	}
}

// Stop terminates every watcher: SIGTERM to each process group, group SIGKILL
// after the grace. Blocks until all watcher goroutines have returned.
func (s *watcherSupervisor) Stop() {
	s.mu.Lock()
	stale := make([]*taskWatcher, 0, len(s.watchers))
	for _, w := range s.watchers {
		stale = append(stale, w)
	}
	s.watchers = make(map[string]*taskWatcher)
	s.mu.Unlock()
	stopWatchers(stale)
}

// watchingTaskIDs returns the IDs with a live (not yet finished) watcher, for
// tests and status reporting.
func (s *watcherSupervisor) watchingTaskIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.watchers))
	for id, w := range s.watchers {
		if !w.finished() {
			ids = append(ids, id)
		}
	}
	return ids
}

// droppedEvents returns the rate-limit drop counter for a task's watcher, or
// 0 if no watcher is registered.
func (s *watcherSupervisor) droppedEvents(taskID string) int {
	s.mu.Lock()
	w := s.watchers[taskID]
	s.mu.Unlock()
	if w == nil {
		return 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dropped
}

func (s *watcherSupervisor) newTaskWatcher(t task.Task) *taskWatcher {
	w := &taskWatcher{
		taskID:        t.ID,
		name:          t.Name,
		cmdStr:        t.WatchCmd,
		dir:           t.ProjectPath,
		sig:           watcherSignature(t),
		targetSession: t.TargetSession,
		sup:           s,
		stopCh:        make(chan struct{}),
		doneCh:        make(chan struct{}),
	}
	// Resolve the repo the task belongs to so a delivery alarm can be scoped to
	// the right repo's snapshot (#1238). A resolution failure only costs the
	// alarm its repo scope — it never disables the watcher — so it is logged and
	// left empty rather than propagated.
	if repo, err := config.RepoFromPath(t.ProjectPath); err != nil {
		log.WarningLog.Printf("watch task %s: cannot resolve repo for delivery-alarm scope: %v", t.ID, err)
	} else {
		w.repoID = repo.ID
	}
	// Recover any backlog a previous watcher/daemon left behind (#1129); the
	// run loop starts the drainer if the queue is non-empty. A queue-dir
	// failure disables durability, never the watcher itself.
	if dir, err := s.queueDir(); err != nil {
		log.WarningLog.Printf("watch task %s: event queue unavailable (failed deliveries will be dropped): %v", t.ID, err)
	} else {
		w.queue = newEventQueue(dir, t.ID)
	}
	return w
}

// watcherSignature captures the fields that define the watch process itself;
// a change to any of them restarts the script on reload.
func watcherSignature(t task.Task) string {
	return t.WatchCmd + "\x00" + t.ProjectPath + "\x00" + t.Name
}

func stopWatchers(ws []*taskWatcher) {
	var wg sync.WaitGroup
	for _, w := range ws {
		wg.Add(1)
		go func(w *taskWatcher) {
			defer wg.Done()
			w.stop()
		}(w)
	}
	wg.Wait()
}

// tailBuffer and its failure-summary helpers live in tailbuffer.go (extracted
// to keep watcher.go under its file-length ceiling, #1145).

// taskWatcher supervises one watch task: it restarts the script with backoff
// on failure and feeds its stdout lines to the supervisor's deliver hook.
// Deliveries are serialized in emission order — the single reader goroutine
// delivers synchronously, so a slow delivery backpressures the script's
// stdout pipe rather than reordering events.
type taskWatcher struct {
	taskID string
	name   string
	cmdStr string
	dir    string
	sig    string
	// repoID/targetSession are captured at construction to label a delivery
	// alarm (#1238) without disk I/O on the snapshot hot path. repoID scopes
	// the alarm to a repo's snapshot; targetSession names where events are
	// failing to land (e.g. "root"). Both come from the task's delivery fields,
	// which are not part of the watcher signature — a target_session edit is
	// picked up by deliverWatchEvent's per-event reload, so a stale label here
	// is at worst momentary and never affects delivery itself.
	repoID        string
	targetSession string

	sup *watcherSupervisor

	// queue is the task's durable event backlog (#1129); nil when the queue
	// directory is unavailable, in which case failed deliveries fall back to
	// the pre-queue drop-with-log behavior.
	queue *eventQueue

	stopCh   chan struct{}
	stopOnce sync.Once
	doneCh   chan struct{}
	// wg counts the drainer goroutine so stop() can join it; drainers spawn
	// lazily via ensureDrainer.
	wg sync.WaitGroup

	mu          sync.Mutex
	dropped     int
	eventTimes  []time.Time
	lastDropLog time.Time
	// draining marks a live drainLoop goroutine, so at most one drains the
	// queue at a time and replay order is preserved.
	draining bool

	// Delivery-failure alarm state (#1238), guarded by mu. deliverFailSince is
	// the start of the current run of consecutive failed deliveries (zero when
	// the last delivery succeeded); deliverFailCount is how many back-to-back
	// attempts have failed; deliverFailErr is the most recent delivery error.
	// A single success clears all three, so a task in the map with a non-zero
	// deliverFailSince is exactly one whose pipeline is currently down.
	deliverFailSince time.Time
	deliverFailCount int
	deliverFailErr   string
}

// stop requests termination and blocks until the run goroutine returns. The
// drainer is joined too: Reload starts a replacement watcher for the same task
// only after stop returns, so two drainers can never interleave one task's
// replay.
func (w *taskWatcher) stop() {
	w.stopOnce.Do(func() { close(w.stopCh) })
	<-w.doneCh
	w.wg.Wait()
}

func (w *taskWatcher) finished() bool {
	select {
	case <-w.doneCh:
		return true
	default:
		return false
	}
}

func (w *taskWatcher) stopRequested() bool {
	select {
	case <-w.stopCh:
		return true
	default:
		return false
	}
}

// run is the supervision loop: spawn the script, restart on non-zero exit
// with exponential backoff, stop for good on exit 0 ("stopped") or on a
// crash loop ("errored").
func (w *taskWatcher) run() {
	defer close(w.doneCh)

	// A backlog recovered from disk (daemon restart, reload, or a prior
	// crash-looped run) starts replaying immediately, independent of the
	// script's own lifecycle (#1129).
	if w.queue != nil && w.queue.pendingCount() > 0 {
		w.ensureDrainer()
	}

	backoff := w.sup.baseBackoff
	var failures []time.Time

	for {
		if w.stopRequested() {
			return
		}

		started := time.Now()
		tail, runErr := w.runOnce()
		if w.stopRequested() {
			if runErr != nil {
				log.InfoLog.Printf("watch task %s: watch command terminated during stop/reload (%v); not a failure", w.taskID, runErr)
			}
			return
		}

		if runErr == nil {
			log.InfoLog.Printf("watch task %s: watch command exited cleanly; stopped until the next reload or re-enable", w.taskID)
			w.sup.setStatus(w.taskID, "stopped")
			return
		}

		// A SIGTERM-shaped death is often daemon shutdown or a unit restart
		// reaching the child before the stop request reaches this loop — unit
		// managers signal the whole control group, not just the daemon. Give
		// the stop channel a grace before treating it as a script failure, so
		// shutdown doesn't log spurious failed/restarting warnings or count
		// toward the crash-loop breaker.
		if exitedFromSignal(runErr, syscall.SIGTERM) {
			select {
			case <-w.stopCh:
				log.InfoLog.Printf("watch task %s: watch command terminated during stop/reload; not a failure", w.taskID)
				return
			case <-time.After(w.sup.stopGrace):
			}
		}

		now := time.Now()
		failures = append(failures, now)
		cut := 0
		for cut < len(failures) && now.Sub(failures[cut]) > w.sup.crashWindow {
			cut++
		}
		failures = failures[cut:]
		if len(failures) >= w.sup.crashMaxExits {
			log.ErrorLog.Printf("watch task %s: %d failures within %s (last: %v); giving up until the next reload or re-enable%s", w.taskID, len(failures), w.sup.crashWindow, runErr, tail.logSuffix())
			w.sup.setStatus(w.taskID, failureSummary(runErr, tail))
			return
		}

		// A run that stayed healthy for a whole crash window restarts the
		// backoff chain at baseBackoff; an unhealthy run keeps doubling toward
		// the cap. The healthy reset must NOT also advance the backoff this
		// cycle — otherwise the next quick failure would wait 2*baseBackoff
		// instead of restarting the documented 1s→2s→4s… chain at baseBackoff
		// (#1005).
		healthy := now.Sub(started) >= w.sup.crashWindow
		wait, next := nextBackoff(backoff, w.sup.baseBackoff, w.sup.maxBackoff, healthy)
		log.WarningLog.Printf("watch task %s: watch command failed (%v); restarting in %s%s", w.taskID, runErr, wait, tail.logSuffix())
		select {
		case <-w.stopCh:
			return
		case <-time.After(wait):
		}
		backoff = next
	}
}

// nextBackoff computes the delay before the next restart and the backoff to
// carry into the following cycle. A run that stayed healthy for a whole crash
// window resets the chain: it waits baseBackoff and leaves the carried backoff
// at baseBackoff, so the next failure restarts the documented 1s→2s→4s…
// sequence from the base rather than from 2*baseBackoff (#1005). An unhealthy
// run waits the current backoff and doubles it toward maxBackoff for next time.
func nextBackoff(current, base, max time.Duration, healthy bool) (wait, next time.Duration) {
	if healthy {
		return base, base
	}
	next = current * 2
	if next > max {
		next = max
	}
	return current, next
}

// exitedFromSignal reports whether err is an exec exit caused by the given
// signal (as opposed to a non-zero exit code or a start failure).
func exitedFromSignal(err error, sig syscall.Signal) bool {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled() && ws.Signal() == sig
}

// runOnce spawns the script once and consumes its stdout until the shell
// exits. Returns nil on exit 0, the exit/start error otherwise, plus the
// run's output tail (never nil) for the caller's failure logging.
func (w *taskWatcher) runOnce() (*tailBuffer, error) {
	tail := &tailBuffer{}
	cmd := exec.Command(w.sup.shell, "-c", w.cmdStr)
	cmd.Dir = w.dir
	cmd.Env = append(os.Environ(),
		"AF_TASK_ID="+w.taskID,
		"AF_TASK_NAME="+w.name,
		"AF_PROJECT_PATH="+w.dir,
	)
	// Own process group so the whole tree — including grandchildren the
	// script backgrounded with `&` or `disown` — can be signaled together,
	// mirroring the post-worktree hook runner (#610, #769).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// stderr appends to the per-task log, size-capped by the same rotation
	// policy as the main log (#1062): NewRotatingFile rotates on open when the
	// file already exceeds the cap and again on the write path, which is what
	// bounds a continuous watch task whose log this run holds open for weeks.
	// A logging failure must not take the watcher down — the failure tail
	// below still captures stderr for this run even when the file can't be
	// opened.
	var stderrLog io.WriteCloser
	var stderrLogPath string
	if logPath, err := w.sup.logPath(w.taskID); err != nil {
		log.WarningLog.Printf("watch task %s: cannot resolve stderr log path: %v", w.taskID, err)
	} else if lw, err := log.NewRotatingFile(logPath, 0644); err != nil {
		log.WarningLog.Printf("watch task %s: cannot open stderr log: %v", w.taskID, err)
	} else {
		stderrLog = lw
		stderrLogPath = logPath
		defer lw.Close()
	}

	// Hand the child the write end of a pipe we own instead of using
	// cmd.StdoutPipe(): Wait must not manage the pipe, because backgrounded
	// grandchildren inherit the write end and we want to (a) keep reading
	// their lines while the shell lives and (b) get a guaranteed EOF once the
	// group-kill below reaps them.
	r, pw, err := os.Pipe()
	if err != nil {
		return tail, fmt.Errorf("failed to create stdout pipe: %w", err)
	}
	cmd.Stdout = pw

	// stderr flows through a pipe we own for the same reasons, teed to the
	// per-task log and into the failure tail (#797). Pipe failure degrades to
	// the pre-#797 direct-to-file wiring rather than failing the run.
	var stderrR, stderrW *os.File
	if er, ew, perr := os.Pipe(); perr != nil {
		log.WarningLog.Printf("watch task %s: cannot create stderr pipe (stderr won't appear in failure logs): %v", w.taskID, perr)
		// Degrade to the pre-#797 direct-to-file wiring. cmd.Stderr must be a
		// real *os.File here: for any other writer os/exec inserts a copy
		// goroutine that Wait blocks on, and a backgrounded grandchild holding
		// stderr open would wedge Wait forever — the group SIGKILL that
		// guarantees EOF only runs after Wait returns. Child writes bypass the
		// rotating writer's size accounting, so on this path growth is bounded
		// by NewRotatingFile's rotate-on-open in the next runOnce instead.
		if stderrLog != nil {
			_ = stderrLog.Close()
			stderrLog = nil
			if f, ferr := os.OpenFile(stderrLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); ferr != nil {
				log.WarningLog.Printf("watch task %s: cannot reopen stderr log: %v", w.taskID, ferr)
			} else {
				cmd.Stderr = f
				defer f.Close()
			}
		}
	} else {
		stderrR, stderrW = er, ew
		cmd.Stderr = stderrW
	}

	if err := cmd.Start(); err != nil {
		_ = r.Close()
		_ = pw.Close()
		if stderrR != nil {
			_ = stderrR.Close()
			_ = stderrW.Close()
		}
		return tail, err
	}
	_ = pw.Close() // the child holds its own dup
	if stderrW != nil {
		_ = stderrW.Close()
	}

	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		defer r.Close()
		w.consumeLines(r, tail)
	}()

	stderrDone := make(chan struct{})
	if stderrR == nil {
		close(stderrDone)
	} else {
		var sink io.Writer
		if stderrLog != nil {
			sink = stderrLog
		}
		go func() {
			defer close(stderrDone)
			defer stderrR.Close()
			w.consumeStderr(stderrR, sink, tail)
		}()
	}

	// Watchdog for stop requests: SIGTERM the group so the script can clean
	// up, escalate to a group SIGKILL after the grace.
	waitDone := make(chan struct{})
	go func(pgid int) {
		select {
		case <-w.stopCh:
			_ = syscall.Kill(-pgid, syscall.SIGTERM)
			select {
			case <-waitDone:
			case <-time.After(w.sup.stopGrace):
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			}
		case <-waitDone:
		}
	}(cmd.Process.Pid)

	waitErr := cmd.Wait()
	close(waitDone)

	// Group-kill on every exit path (#769): backgrounded grandchildren must
	// not outlive the watcher. This also closes any inherited stdout/stderr
	// write ends, so both reader goroutines are guaranteed to reach EOF.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	<-readerDone
	<-stderrDone

	return tail, waitErr
}

// consumeLines turns newline-terminated stdout lines into events. Lines over
// maxWatchLineBytes are truncated to the cap and the remainder discarded with
// a logged note; unterminated trailing output at EOF is not an event but is
// kept in the failure tail — it is often the script's death rattle (#797).
func (w *taskWatcher) consumeLines(r io.Reader, tail *tailBuffer) {
	br := bufio.NewReaderSize(r, maxWatchLineBytes)
	for {
		chunk, err := br.ReadSlice('\n')
		switch {
		case err == nil:
			w.handleEvent(strings.TrimRight(string(chunk), "\r\n"), tail)
		case errors.Is(err, bufio.ErrBufferFull):
			// ReadSlice's buffer filled before a newline: keep the first
			// maxWatchLineBytes as the event and discard the rest of the line.
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
				// The stream ended before the line did — never
				// newline-terminated, so not an event.
				tail.add(line)
				return
			}
			log.WarningLog.Printf("watch task %s: stdout line exceeded %d bytes; truncated (%d bytes discarded)", w.taskID, maxWatchLineBytes, discarded)
			w.handleEvent(line, tail)
		case errors.Is(err, io.EOF):
			if len(chunk) > 0 {
				log.WarningLog.Printf("watch task %s: discarding %d bytes of unterminated stdout output (events must be newline-terminated lines)", w.taskID, len(chunk))
				tail.add(string(chunk))
			}
			return
		default:
			return
		}
	}
}

// consumeStderr tees the script's stderr to the per-task log file (when one
// could be opened) and keeps complete lines in the failure tail, so a script
// whose own redirections starve the log file is still diagnosable from the
// daemon log on failure (#797).
func (w *taskWatcher) consumeStderr(r io.Reader, logFile io.Writer, tail *tailBuffer) {
	br := bufio.NewReaderSize(r, 4*1024)
	atLineStart := true
	for {
		chunk, err := br.ReadSlice('\n')
		if len(chunk) > 0 {
			if logFile != nil {
				_, _ = logFile.Write(chunk)
			}
			if atLineStart {
				tail.add(string(chunk))
			}
		}
		switch {
		case err == nil:
			atLineStart = true
		case errors.Is(err, bufio.ErrBufferFull):
			// Only an overlong line's first chunk lands in the tail (add caps
			// it anyway); the rest still reaches the log file above.
			atLineStart = false
		default:
			return
		}
	}
}

// handleEvent routes one stdout line: with a backlog pending it is queued
// behind it (delivering directly would reorder the stream, #1129); otherwise
// it is rate-limited and delivered synchronously exactly as before, with a
// failed delivery queued for replay instead of dropped. Called from the single
// reader goroutine, so direct deliveries stay serialized in order. Lines that
// do not become a delivered event this run — rate-dropped, failed, or queued —
// go to the failure tail (#797).
func (w *taskWatcher) handleEvent(line string, tail *tailBuffer) {
	// FIFO gating: once a backlog exists, every newer event goes through the
	// queue until it drains. Backlogged enqueues bypass the rate limiter —
	// the limiter gates deliveries to the target (the drainer reserves a slot
	// per replayed event); the queue's own count/byte caps bound the backlog.
	if w.queue != nil && w.queue.pendingCount() > 0 {
		w.enqueueEvent(line, tail)
		return
	}

	if !w.tryReserveEventSlot() {
		now := time.Now()
		w.mu.Lock()
		w.dropped++
		dropped := w.dropped
		logIt := now.Sub(w.lastDropLog) >= time.Minute
		if logIt {
			w.lastDropLog = now
		}
		w.mu.Unlock()
		// One warning per window, not per drop — a flooding script must not
		// also flood the daemon log. The counter keeps the exact total.
		// Rate-dropped events are deliberately NOT queued: the limiter is
		// protective policy against a chatty script, not an outage signal.
		if logIt {
			log.WarningLog.Printf("watch task %s: event rate exceeded %d/min; dropping excess events (%d dropped so far)", w.taskID, w.sup.eventsPerMinute, dropped)
		}
		tail.add(line)
		return
	}

	err := w.sup.deliver(w.taskID, line)
	w.recordDeliveryResult(time.Now(), err)
	if err != nil {
		switch {
		case errors.Is(err, errTargetBusy):
			// Not a failure: a TUI is attached to the target, so the event is
			// held and retried after detach rather than pasted into live typing
			// (#1586). A deferral delivers nothing, so refund the rate slot it
			// reserved above — otherwise the live attempt AND the drainer's replay
			// would each spend one, double-charging the target's per-minute budget
			// and eventually dropping events. Queue it (preserving FIFO) and log
			// quietly.
			w.releaseEventSlot()
			log.InfoLog.Printf("watch task %s: target session attached; deferring event until detach (#1586)", w.taskID)
		case errors.Is(err, errAtConcurrencyLimit):
			// Not a failure either: the task is at its max_concurrent_runs cap
			// (#1892), so nothing was created. Refund the rate slot for the same
			// reason a deferral does — the event will be delivered by the drainer,
			// which spends its own slot then, and burning one per refusal would
			// charge the cap's whole parked period against the per-minute budget.
			// Queueing it preserves FIFO and is what makes the cap queue rather than
			// drop; the drainer retries until a session finishes.
			w.releaseEventSlot()
			log.InfoLog.Printf("watch task %s: at its max_concurrent_runs limit; queueing event until a session finishes (#1892)", w.taskID)
		default:
			// A genuine failure that never got as far as the target delivered
			// nothing either, so it refunds too (#2102) — otherwise the live
			// attempt AND every drainer retry each spend a slot, leaking the
			// per-minute budget exactly when an outage needs it for recovery. Only
			// pre-flight failures qualify: a failed create or send may have landed
			// (see errNotAttempted), and a delivered event must stay charged.
			if errors.Is(err, errNotAttempted) {
				w.releaseEventSlot()
			}
			log.ErrorLog.Printf("watch task %s: failed to deliver event: %v", w.taskID, err)
		}
		w.enqueueEvent(line, tail)
	}
}

// tryReserveEventSlot applies the per-task delivery rate limit: prune the
// sliding window, then reserve one slot if the window has room. Live
// deliveries and the drainer's replays reserve through the same window, so
// combined delivery pressure on the target session never exceeds
// eventsPerMinute — a burst replay after an outage trickles in.
//
// This rate limit and the max_concurrent_runs cap (#1892) are orthogonal and do
// not double-limit, so neither needs to know about the other. This one is
// protective policy against a chatty script and DROPS excess events by design;
// the cap is a resource bound and QUEUES them, never dropping. They compose
// without any reconciliation because of handleEvent's FIFO gate above: the first
// event the cap parks creates a backlog, and from then on every new event is
// enqueued without ever consulting this limiter. So the moment concurrency is the
// binding constraint, the rate limiter stops dropping — the two are never the
// binding constraint at the same time.
func (w *taskWatcher) tryReserveEventSlot() bool {
	now := time.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	cut := 0
	for cut < len(w.eventTimes) && now.Sub(w.eventTimes[cut]) >= time.Minute {
		cut++
	}
	w.eventTimes = w.eventTimes[cut:]
	if len(w.eventTimes) >= w.sup.eventsPerMinute {
		return false
	}
	w.eventTimes = append(w.eventTimes, now)
	return true
}

// releaseEventSlot refunds a rate slot reserved by tryReserveEventSlot when the
// attempt did not actually deliver — a deferral (errTargetBusy, #1586) sends
// nothing, so it must not spend the target's per-minute budget. It drops the
// newest reservation; the live path and the drainer share the window, but the
// limiter counts reservations rather than tracking identity, so removing one
// per refunded attempt keeps the count exactly right regardless of which
// goroutine's timestamp is dropped.
func (w *taskWatcher) releaseEventSlot() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if n := len(w.eventTimes); n > 0 {
		w.eventTimes = w.eventTimes[:n-1]
	}
}

// enqueueEvent appends the line to the durable backlog and wakes the drainer.
// The line also lands in the run's failure tail — it did not become a
// delivered event this run (#797). When the queue is unavailable or the append
// fails, this degrades to the pre-#1129 behavior: logged and dropped.
func (w *taskWatcher) enqueueEvent(line string, tail *tailBuffer) {
	tail.add(line)
	if w.queue == nil {
		return
	}
	if err := w.queue.enqueue(line); err != nil {
		log.ErrorLog.Printf("watch task %s: failed to queue event for replay; dropping it: %v", w.taskID, err)
		return
	}
	w.ensureDrainer()
}

// ensureDrainer starts the drain goroutine unless one is live or a stop is in
// flight. Callers: enqueueEvent (an event just queued) and run (a backlog
// recovered from disk).
func (w *taskWatcher) ensureDrainer() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.draining || w.stopRequested() {
		return
	}
	w.draining = true
	w.wg.Add(1)
	go w.drainLoop()
}

// stopDraining clears the draining flag so a later enqueue can start a fresh
// drainer.
func (w *taskWatcher) stopDraining() {
	w.mu.Lock()
	w.draining = false
	w.mu.Unlock()
}

// deliverWatchEvent is the production delivery hook: it re-loads the task (so
// prompt/target_session edits apply without restarting the script), renders
// {{line}}, and routes through the same delivery path cron fires use, then
// records the run status (#664 path).
func deliverWatchEvent(taskID, line string) error {
	// The three pre-flight checks below fail before anything is created or sent,
	// so they are tagged notAttempted and the caller refunds their rate slot
	// (#2102). Everything past them can fail with the delivery already in
	// flight, and stays charged.
	t, err := task.GetTask(taskID)
	if err != nil {
		return notAttempted(fmt.Errorf("failed to load task: %w", err))
	}
	if !t.Enabled {
		return notAttempted(fmt.Errorf("task %s is disabled", taskID))
	}
	prompt := task.RenderWatchPrompt(t.Prompt, line)
	if strings.TrimSpace(prompt) == "" {
		return notAttempted(fmt.Errorf("event rendered an empty prompt (line %q)", line))
	}
	status, err := deliverTaskPrompt(t, prompt, true)
	if err != nil {
		return err
	}
	if status == StatusDeferredAttached {
		// A TUI is attached full-screen to the target session; the delivery was
		// held so it can't paste into and submit the user's in-progress input
		// (#1586). Signal the caller (handleEvent / drainLoop) to re-queue and
		// retry after detach — the event is neither lost nor delivered into live
		// typing. errTargetBusy is exempt from the delivery-failure alarm and
		// logged quietly, since a deferral is expected, not an outage.
		return errTargetBusy
	}
	now := time.Now()
	if err := task.UpdateTaskStatus(taskID, &now, status); err != nil {
		log.ErrorLog.Printf("failed to update task status: %v", err)
	}
	return nil
}

// persistWatcherStatus records a watcher lifecycle status on the task:
// "stopped", or "errored: <exit>: <first output line>" from the crash-loop
// breaker (#797). LastRunAt is preserved — it tracks event deliveries, not
// supervision changes. Passing nil for lastRunAt tells UpdateTaskStatus to
// leave LastRunAt untouched: reading it here (outside the file lock) and
// writing it back would revert a newer timestamp a concurrent deliverWatchEvent
// committed in the gap — the TOCTOU race in #1215. UpdateTaskStatus skips
// Program enum validation so legacy task records still receive status bumps
// (#664).
func persistWatcherStatus(taskID, status string) {
	if err := task.UpdateTaskStatus(taskID, nil, status); err != nil {
		log.WarningLog.Printf("failed to record watcher status %q on task %s: %v", status, taskID, err)
	}
}

// watcherLogPath resolves (and creates the directory for) the per-task
// stderr log, ~/.agent-factory/logs/task-<id>.log.
func watcherLogPath(taskID string) (string, error) {
	configDir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(configDir, "logs")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	return filepath.Join(dir, "task-"+taskID+".log"), nil
}

// watcherShell returns the shell watch commands run under: the user's $SHELL,
// falling back to sh when unset (e.g. under a supervised unit missing it).
func watcherShell() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	return "sh"
}

//go:build linux

package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const (
	eventQueueDurabilityHelperEnv  = "AF_TEST_EVENT_QUEUE_COMPACTION_DURABILITY"
	eventQueueDurabilityVerdictEnv = "AF_TEST_EVENT_QUEUE_COMPACTION_VERDICT"
	eventQueueDurabilityTaskID     = "ab313300"

	// The helper writes one of these to the file named by
	// eventQueueDurabilityVerdictEnv. An absent file is a third state: the
	// helper never got to say anything.
	eventQueueDurabilityPassed = "passed"
	eventQueueDurabilityFailed = "failed"
)

// TestEventQueue_CompactionPublishesDurably observes the actual Linux write
// barriers around compaction. A rewritten inode must be synced before it is
// renamed over the durable queue, and the containing directory must be synced
// after that rename publishes the new entry.
//
// The helper runs under strace, so two processes report on this run and only
// one of them is the code under test. The helper records its own verdict to a
// file strace neither writes nor owns; strace's exit status is diagnostic
// context, never a verdict (#3751). See evaluateCompactionDurability.
func TestEventQueue_CompactionPublishesDurably(t *testing.T) {
	if os.Getenv(eventQueueDurabilityHelperEnv) == "1" {
		runEventQueueDurabilityHelper(t)
		return
	}

	strace, err := exec.LookPath("strace")
	if err != nil {
		t.Skip("strace is required to observe compaction durability barriers")
	}
	dir := t.TempDir()
	verdictPath := filepath.Join(t.TempDir(), "helper-verdict")
	cmd := exec.Command(
		strace,
		"-f", "-qq", "-yy", "-s", "4096",
		"-e", "trace=fsync,fdatasync,rename,renameat,renameat2",
		os.Args[0], "-test.run=^TestEventQueue_CompactionPublishesDurably$",
	)
	cmd.Env = append(os.Environ(),
		eventQueueDurabilityHelperEnv+"=1",
		eventQueueDurabilityVerdictEnv+"="+verdictPath,
		"AF_TEST_EVENT_QUEUE_DIR="+dir,
	)
	// The tracer's exit status is captured but deliberately not consulted as a
	// pass/fail signal; CombinedOutput's error is handed on as context only.
	trace, tracerErr := cmd.CombinedOutput()

	verdict := readEventQueueDurabilityVerdict(verdictPath)
	if problem := evaluateCompactionDurability(verdict, tracerErr, string(trace), dir); problem != "" {
		t.Fatalf("%s\ntrace:\n%s", problem, trace)
	}
}

// readEventQueueDurabilityVerdict reads what the helper said about itself.
// A missing or unreadable file reports the empty verdict — "the helper did not
// get to speak" — which is distinct from it having reported a failure.
func readEventQueueDurabilityVerdict(path string) string {
	recorded, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(recorded))
}

// evaluateCompactionDurability judges one traced run and returns why it failed,
// or "" when the durability contract was observed to hold.
//
// The tracer's exit status is not an input to that judgement (#3751). strace is
// the observation apparatus, not the code under test: on linux/arm64 the Go
// runtime's async-preemption SIGURG put a traced thread into a group-stop,
// strace's PTRACE_LISTEN to resume it failed with EIO, and strace exited 1 —
// after the traced helper had already run every barrier and printed PASS.
// Reading that status as the helper's verdict reddened master on a run where
// the contract held and the trace proves it.
//
// So the two facts that can fail this test are the helper's own verdict, which
// it records where strace cannot reach, and the barriers, which are read out of
// the trace regardless of how strace exited. A tracer that died is appended as
// context to those failures — a truncated trace is the likeliest reason a
// barrier is missing from one — and says nothing on its own.
func evaluateCompactionDurability(verdict string, tracerErr error, trace, dir string) string {
	switch verdict {
	case eventQueueDurabilityPassed:
	case "":
		return "compaction helper recorded no verdict: it did not reach the end of its run" +
			tracerDiagnostic(tracerErr)
	default:
		return fmt.Sprintf("compaction helper failed its own assertions (verdict: %q)", verdict)
	}

	tempSync, queueRename, directorySync := parseCompactionDurabilityTrace(
		joinStraceResumedLines(strings.Split(trace, "\n")), dir)
	switch {
	case tempSync < 0:
		return "compaction renamed an unsynced temporary queue file" + tracerDiagnostic(tracerErr)
	case queueRename < 0:
		return "trace did not contain the compacted queue rename" + tracerDiagnostic(tracerErr)
	case tempSync > queueRename:
		return "temporary queue sync happened after publication" + tracerDiagnostic(tracerErr)
	case directorySync < 0:
		return "compaction did not sync " + dir + " after publishing the queue entry" +
			tracerDiagnostic(tracerErr)
	}
	return ""
}

// tracerDiagnostic renders a tracer-side failure as context for a barrier the
// trace does not show. It is only ever appended to an existing failure: strace
// exiting non-zero is not itself a defect in the code under test.
func tracerDiagnostic(tracerErr error) string {
	if tracerErr == nil {
		return ""
	}
	return fmt.Sprintf(" — note that strace also exited non-zero (%v), "+
		"so this trace may be truncated rather than the barrier absent", tracerErr)
}

// joinStraceResumedLines merges strace's split "<unfinished ...>" /
// "<... syscall resumed>" pairs back into one logical line per syscall.
//
// With -f, strace multiplexes every traced thread into one stream: whenever
// another event must be printed while a syscall is between entry and exit, the
// in-flight call is emitted as "fsync(args <unfinished ...>" and its completion
// later as "<... fsync resumed>) = 0". The Go runtime makes that interleaving
// routine — its SIGURG preemption signals land between fsync entry and exit —
// so the compact-file sync can arrive split (#3292: the failing release-lane
// trace shows exactly this, the sync succeeding before the rename). A parser
// that requires the fd path and "= 0" on one physical line then reports a
// barrier missing that actually held. Joining is keyed by pid (one syscall can
// be in flight per thread), and the joined line takes the RESUMED line's
// position, i.e. the syscall's completion order — the order the barrier
// assertions are about.
var (
	stracePidPrefix   = regexp.MustCompile(`^\[pid\s+(\d+)\]\s*`)
	straceResumedMark = regexp.MustCompile(`<\.\.\.\s+\S+\s+resumed>`)
)

func joinStraceResumedLines(lines []string) []string {
	pending := make(map[string]string)
	joined := make([]string, 0, len(lines))
	for _, line := range lines {
		pid := ""
		if m := stracePidPrefix.FindStringSubmatch(line); m != nil {
			pid = m[1]
		}
		if idx := strings.Index(line, "<unfinished ...>"); idx >= 0 {
			pending[pid] = strings.TrimRight(line[:idx], " ")
			continue
		}
		if loc := straceResumedMark.FindStringIndex(line); loc != nil {
			if head, ok := pending[pid]; ok {
				delete(pending, pid)
				joined = append(joined, head+line[loc[1]:])
				continue
			}
		}
		joined = append(joined, line)
	}
	return joined
}

// parseCompactionDurabilityTrace scans joined trace lines for the three
// write-barrier events and returns their indexes (-1 = not seen): the
// successful sync of the compact temp file, the rename publishing it over the
// queue, and the directory sync after that rename.
func parseCompactionDurabilityTrace(lines []string, dir string) (tempSync, queueRename, directorySync int) {
	tempSync, queueRename, directorySync = -1, -1, -1
	compactName := eventQueueDurabilityTaskID + ".compact-"
	queueName := eventQueueDurabilityTaskID + ".jsonl"
	for index, line := range lines {
		isSync := strings.Contains(line, "fsync(") || strings.Contains(line, "fdatasync(")
		syscallSucceeded := strings.HasSuffix(strings.TrimSpace(line), "= 0")
		syncSucceeded := isSync && syscallSucceeded
		if syncSucceeded && strings.Contains(line, compactName) {
			tempSync = index
		}
		if syscallSucceeded && strings.Contains(line, "rename") && strings.Contains(line, compactName) && strings.Contains(line, queueName) {
			queueRename = index
		}
		if queueRename >= 0 && index > queueRename && syncSucceeded && strings.Contains(line, "<"+dir+">") {
			directorySync = index
			break
		}
	}
	return tempSync, queueRename, directorySync
}

// TestCompactionDurabilityTraceParser_SplitSyscallLines replays the shape of
// the #3292 failure — the compact-file fsync split by a sibling thread's
// SIGURG print, completing before the rename — and requires the parser to see
// all three barriers with their real ordering. Against the pre-#3292 parser
// (no join), tempSync stays -1 on this input and the durability gate reports
// a sync that in fact happened.
func TestCompactionDurabilityTraceParser_SplitSyscallLines(t *testing.T) {
	const dir = "/tmp/queue-dir"
	raw := []string{
		"[pid 11] fsync(8</tmp/queue-dir/" + eventQueueDurabilityTaskID + ".compact-42> <unfinished ...>",
		"[pid 10] --- SIGURG {si_signo=SIGURG, si_code=SI_TKILL, si_pid=10, si_uid=1001} ---",
		"[pid 11] <... fsync resumed>)        = 0",
		"[pid 11] renameat(AT_FDCWD</src>, \"/tmp/queue-dir/" + eventQueueDurabilityTaskID + ".compact-42\", " +
			"AT_FDCWD</src>, \"/tmp/queue-dir/" + eventQueueDurabilityTaskID + ".jsonl\") = 0",
		"[pid 11] fsync(8</tmp/queue-dir>) = 0",
	}
	tempSync, queueRename, directorySync := parseCompactionDurabilityTrace(joinStraceResumedLines(raw), dir)
	if tempSync < 0 {
		t.Fatal("split compact-file fsync was not recognized: the joiner must reunite unfinished/resumed pairs")
	}
	if queueRename < 0 || directorySync < 0 {
		t.Fatalf("rename/directory barriers not recognized: rename=%d dirSync=%d", queueRename, directorySync)
	}
	if tempSync > queueRename || directorySync < queueRename {
		t.Fatalf("barrier order misread: tempSync=%d rename=%d dirSync=%d", tempSync, queueRename, directorySync)
	}
}

// eventQueueDurabilityFixtureDir is the queue directory in the captured trace
// below, in the shape `t.TempDir()` produces.
const eventQueueDurabilityFixtureDir = "/tmp/TestEventQueue_CompactionPublishesDurably2544418973/001"

// eventQueueTracerDiedTrace reproduces the master build that #3751 is about
// (linux/arm64, run 33712122606): all three barriers present, successful and in
// order, the traced binary printing PASS — and then strace failing to resume a
// thread the Go runtime's SIGURG had group-stopped, and exiting 1.
//
// The strace diagnostic is on this stream because the tracer and the tracee
// share it, which is exactly how the real run looked; the parser must read past
// it. Keeping that stream shared is deliberate: `strace -o <file>` would split
// them, but it also switches strace's per-line prefix from "[pid N]" to a bare
// pid column, which joinStraceResumedLines keys on — silently undoing #3294.
func eventQueueTracerDiedTrace(dir string) string {
	compact := dir + "/" + eventQueueDurabilityTaskID + ".compact-3993619722"
	queue := dir + "/" + eventQueueDurabilityTaskID + ".jsonl"
	return strings.Join([]string{
		"[pid 46700] fsync(8<" + compact + ">) = 0",
		"[pid 46700] renameat(AT_FDCWD</src>, \"" + compact + "\", AT_FDCWD</src>, \"" + queue + "\") = 0",
		"[pid 46700] fsync(8<" + dir + ">) = 0",
		"PASS",
		"[pid 46695] --- stopped by SIGURG ---",
		"/usr/bin/strace: ptrace(PTRACE_LISTEN,pid:46695,sig:0): Input/output error",
		"",
	}, "\n")
}

// TestEventQueue_CompactionPublishesDurablyOutlivesTheTracer pins #3751: the
// tracer dying does not make the helper wrong. Against the previous oracle —
// `if err := cmd.CombinedOutput(); err != nil { t.Fatalf(...) }`, where cmd is
// strace — this input fails before the barriers are ever parsed, which is how
// master went red on a run whose trace shows the contract holding.
func TestEventQueue_CompactionPublishesDurablyOutlivesTheTracer(t *testing.T) {
	dir := eventQueueDurabilityFixtureDir
	tracerDied := errors.New("exit status 1")

	if problem := evaluateCompactionDurability(
		eventQueueDurabilityPassed, tracerDied, eventQueueTracerDiedTrace(dir), dir); problem != "" {
		t.Fatalf("strace's exit status was read as the helper's verdict: %s", problem)
	}
}

// TestEventQueue_CompactionPublishesDurablyFailsOnAMissingBarrier is the other
// half: ignoring the tracer's exit must not soften the durability assertion.
// A trace without the post-rename directory sync fails whether or not strace
// survived, and when it did not, its exit is named as context.
func TestEventQueue_CompactionPublishesDurablyFailsOnAMissingBarrier(t *testing.T) {
	dir := eventQueueDurabilityFixtureDir
	full := eventQueueTracerDiedTrace(dir)
	directorySync := "[pid 46700] fsync(8<" + dir + ">) = 0\n"
	withoutDirectorySync := strings.Replace(full, directorySync, "", 1)
	if withoutDirectorySync == full {
		t.Fatalf("fixture no longer contains the directory sync line %q", directorySync)
	}

	problem := evaluateCompactionDurability(eventQueueDurabilityPassed, nil, withoutDirectorySync, dir)
	if problem == "" {
		t.Fatal("a trace missing the post-rename directory sync must fail: that is the #3147 contract")
	}
	if strings.Contains(problem, "strace") {
		t.Fatalf("a healthy tracer must not be blamed for a missing barrier: %s", problem)
	}

	withTracerDead := evaluateCompactionDurability(
		eventQueueDurabilityPassed, errors.New("exit status 1"), withoutDirectorySync, dir)
	if withTracerDead == "" {
		t.Fatal("a missing barrier must still fail when strace also exited non-zero")
	}
	if !strings.Contains(withTracerDead, "strace") {
		t.Fatalf("a dead tracer must be reported as context for the missing barrier: %s", withTracerDead)
	}
}

// TestEventQueue_CompactionPublishesDurablyHeedsTheHelperVerdict keeps the
// helper's own assertions load-bearing: they are the fact the old oracle was
// reaching for through the wrong process, so they must still fail the test —
// and so must a helper that never reported at all.
func TestEventQueue_CompactionPublishesDurablyHeedsTheHelperVerdict(t *testing.T) {
	dir := eventQueueDurabilityFixtureDir
	trace := eventQueueTracerDiedTrace(dir)

	if problem := evaluateCompactionDurability(eventQueueDurabilityFailed, nil, trace, dir); problem == "" {
		t.Fatal("the helper failing its own assertions must fail the test, clean trace or not")
	}
	if problem := evaluateCompactionDurability("", nil, trace, dir); problem == "" {
		t.Fatal("a helper that recorded no verdict must fail the test")
	}
}

func runEventQueueDurabilityHelper(t *testing.T) {
	verdictPath := os.Getenv(eventQueueDurabilityVerdictEnv)
	if verdictPath == "" {
		t.Fatalf("%s is empty: the helper has no channel of its own to report through",
			eventQueueDurabilityVerdictEnv)
	}
	// Registered before every other cleanup so it runs after them (cleanups are
	// LIFO): the recorded verdict then accounts for a failure raised by any of
	// them too. This file is the helper's only voice — strace owns the exit
	// status of this process, and on a bad day reports its own troubles there.
	t.Cleanup(func() {
		verdict := eventQueueDurabilityPassed
		if t.Failed() {
			verdict = eventQueueDurabilityFailed
		}
		if err := os.WriteFile(verdictPath, []byte(verdict), 0o600); err != nil {
			t.Errorf("record helper verdict: %v", err)
		}
	})

	dir := os.Getenv("AF_TEST_EVENT_QUEUE_DIR")
	if dir == "" {
		t.Fatal("AF_TEST_EVENT_QUEUE_DIR is empty")
	}
	previousThreshold := watcherQueueCompactBytes
	watcherQueueCompactBytes = 1
	t.Cleanup(func() { watcherQueueCompactBytes = previousThreshold })

	q := newEventQueue(dir, eventQueueDurabilityTaskID)
	for index := 0; index < 2; index++ {
		if err := q.enqueue(fmt.Sprintf("event-%d", index)); err != nil {
			t.Fatalf("enqueue %d: %v", index, err)
		}
	}
	_, cursor, ok, err := q.peek()
	if err != nil || !ok {
		t.Fatalf("peek: ok=%v err=%v", ok, err)
	}
	advanced, err := q.advance(cursor)
	if err != nil || !advanced {
		t.Fatalf("advance through compaction: advanced=%v err=%v", advanced, err)
	}
	if q.pendingCount() != 1 {
		t.Fatalf("pending after compaction = %d, want 1", q.pendingCount())
	}
}

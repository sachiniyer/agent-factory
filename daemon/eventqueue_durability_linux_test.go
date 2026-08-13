//go:build linux

package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

const (
	eventQueueDurabilityHelperEnv = "AF_TEST_EVENT_QUEUE_COMPACTION_DURABILITY"
	eventQueueDurabilityTaskID    = "ab313300"
)

// TestEventQueue_CompactionPublishesDurably observes the actual Linux write
// barriers around compaction. A rewritten inode must be synced before it is
// renamed over the durable queue, and the containing directory must be synced
// after that rename publishes the new entry.
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
	cmd := exec.Command(
		strace,
		"-f", "-qq", "-yy", "-s", "4096",
		"-e", "trace=fsync,fdatasync,rename,renameat,renameat2",
		os.Args[0], "-test.run=^TestEventQueue_CompactionPublishesDurably$",
	)
	cmd.Env = append(os.Environ(),
		eventQueueDurabilityHelperEnv+"=1",
		"AF_TEST_EVENT_QUEUE_DIR="+dir,
	)
	trace, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("trace compaction helper: %v\n%s", err, trace)
	}

	tempSync, queueRename, directorySync := parseCompactionDurabilityTrace(
		joinStraceResumedLines(strings.Split(string(trace), "\n")), dir)
	if tempSync < 0 {
		t.Fatalf("compaction renamed an unsynced temporary queue file; trace:\n%s", trace)
	}
	if queueRename < 0 {
		t.Fatalf("trace did not contain the compacted queue rename; trace:\n%s", trace)
	}
	if tempSync > queueRename {
		t.Fatalf("temporary queue sync happened after publication; trace:\n%s", trace)
	}
	if directorySync < 0 {
		t.Fatalf("compaction did not sync %s after publishing the queue entry; trace:\n%s", dir, trace)
	}
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

func runEventQueueDurabilityHelper(t *testing.T) {
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

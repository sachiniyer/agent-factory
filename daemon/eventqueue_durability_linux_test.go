//go:build linux

package daemon

import (
	"fmt"
	"os"
	"os/exec"
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

	lines := strings.Split(string(trace), "\n")
	tempSync, queueRename, directorySync := -1, -1, -1
	compactName := eventQueueDurabilityTaskID + ".compact-"
	queueName := eventQueueDurabilityTaskID + ".jsonl"
	for index, line := range lines {
		isSync := strings.Contains(line, "fsync(") || strings.Contains(line, "fdatasync(")
		if isSync && strings.Contains(line, compactName) {
			tempSync = index
		}
		if strings.Contains(line, "rename") && strings.Contains(line, compactName) && strings.Contains(line, queueName) {
			queueRename = index
		}
		if queueRename >= 0 && index > queueRename && isSync && strings.Contains(line, "<"+dir+">") {
			directorySync = index
			break
		}
	}
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

package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/task"
)

// A watch backlog written before task generations existed lives under the
// task-ID-only stem (<id>.jsonl / <id>.cursor). When the daemon's task load
// backfills a generation onto that row (#4224 review), the row's queue stem
// moves to the generation-qualified form. Nothing would reopen the old files,
// and cleanOrphanQueues would delete them as a removed generation's backlog.
// This adopts them instead.
//
// Only a backfilled generation adopts, because only that row is the pre-field
// row that wrote the legacy files. A row minted by an add is a new incarnation,
// and a removed namesake's backlog must never replay into it.

// errLegacyEventQueueConflict means both the legacy file and its
// generation-qualified replacement exist as different files. Supported flows
// never produce this: adoption runs before any watcher of the backfilled
// generation opens its queue, and nothing but a pre-field row writes the legacy
// stem. The replacement is the queue that watcher has been using, so it stays
// authoritative, and the legacy file is left for the operator.
var errLegacyEventQueueConflict = errors.New("legacy event-queue file conflicts with its generation's queue")

// adoptLegacyEventQueue renames the legacy queue files of taskID to the stem of
// its backfilled generationID. It is idempotent and safe to repeat after a
// crash: the log is renamed before its cursor, each rename is atomic, and a
// file already moved is simply absent, so a restart finishes the move and the
// cursor still lands beside the log it indexes. A conflict on either file moves
// neither, because a legacy cursor paired with the generation's log would point
// into a different file.
func adoptLegacyEventQueue(dir, taskID, generationID string) error {
	if !task.IsBackfilledGeneration(generationID) {
		return nil
	}
	legacy := eventQueueStem(taskID, "")
	owned := eventQueueStem(taskID, generationID)
	type move struct{ from, to string }
	var moves []move
	var conflicts []string
	for _, suffix := range []string{".jsonl", ".cursor"} {
		from := filepath.Join(dir, legacy+suffix)
		to := filepath.Join(dir, owned+suffix)
		if _, err := os.Lstat(from); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return fmt.Errorf("inspect legacy event queue %s: %w", from, err)
		}
		if _, err := os.Lstat(to); err == nil {
			conflicts = append(conflicts, from+" and "+to)
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("inspect event queue %s: %w", to, err)
		}
		moves = append(moves, move{from: from, to: to})
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("%w: %s both exist; the generation's queue is kept, and the legacy files are left in place for inspection",
			errLegacyEventQueueConflict, strings.Join(conflicts, "; "))
	}
	for _, m := range moves {
		if err := os.Rename(m.from, m.to); err != nil {
			return fmt.Errorf("move legacy event queue %s to %s: %w", m.from, m.to, err)
		}
	}
	if len(moves) == 0 {
		return nil
	}
	return syncEventQueueDirectory(dir)
}

// openTaskEventQueue opens the durable queue for one watcher, first adopting a
// legacy backlog the upgrade left behind. It returns nil when the queue must not
// be opened, with the reason already logged: a failed adoption leaves the legacy
// files where they are, and opening the generation's queue then would start a
// second backlog that a later adoption could not merge.
func openTaskEventQueue(dir string, t task.Task) *eventQueue {
	if err := adoptLegacyEventQueue(dir, t.ID, t.GenerationID); err != nil {
		if !errors.Is(err, errLegacyEventQueueConflict) {
			log.WarningLog.Printf("watch task %s: could not move its pre-upgrade event backlog to its task generation; the backlog is kept and retried when the watcher restarts, and failed deliveries are dropped until then: %v", t.ID, err)
			return nil
		}
		log.WarningLog.Printf("watch task %s: %v", t.ID, err)
	}
	return newEventQueueForGeneration(dir, t.ID, t.GenerationID)
}

// ownsQueueStem reports whether stem names a queue file the task row with
// generationID owns: its generation's queue, or, for a backfilled generation,
// the legacy backlog it has not adopted yet. A disabled backfilled row keeps
// that backlog for re-enable, exactly as it keeps its own (#1129).
func ownsQueueStem(stem, taskID, generationID string) bool {
	if stem == eventQueueStem(taskID, generationID) {
		return true
	}
	return stem == eventQueueStem(taskID, "") && task.IsBackfilledGeneration(generationID)
}

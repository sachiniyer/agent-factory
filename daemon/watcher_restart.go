package daemon

import (
	"fmt"
	"sync"

	"github.com/sachiniyer/agent-factory/task"
)

// restart replaces one enabled watch task even when its process-defining
// configuration is unchanged. It is the explicit path for re-reading an edited
// script at the same watch_cmd path. The old watcher is fully joined before the
// replacement can start, so their process trees and queue drainers never
// overlap.
func (s *watcherSupervisor) restart(t task.Task) error {
	if err := task.ValidateTaskID(t.ID); err != nil {
		return err
	}
	if !t.Enabled {
		return fmt.Errorf("task %q is disabled; enable it before restarting its watch command", t.ID)
	}
	if !t.IsWatch() {
		return fmt.Errorf("task %q is not a watch task", t.ID)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return fmt.Errorf("watch task supervisor is shutting down")
	}
	if current := s.watchers[t.ID]; current != nil {
		delete(s.watchers, t.ID)
		// The flush total crosses only between incarnations: a watcher stopped
		// for a replaced task must not seed its drops onto the row the reused
		// ID now names (#4224 review).
		dropped := current.stop()
		if current.generationID == t.GenerationID {
			if dropped > t.DroppedEvents {
				t.DroppedEvents = dropped
			}
		} else {
			// The generation rebound under the reused ID: the persisted total
			// on the row is the predecessor incarnation's evidence just as
			// the flush is, so the replacement starts clean (#4224). The
			// durable row needs the same reset or the stale count keeps
			// reporting and reseeding across restarts.
			t.DroppedEvents = 0
			s.clearReboundDropSeed(t)
		}
	}

	replacement := s.newTaskWatcher(t)
	s.watchers[t.ID] = replacement
	go replacement.run()
	select {
	case <-replacement.startedCh:
		return nil
	case <-replacement.doneCh:
		select {
		case <-replacement.startedCh:
			return nil
		default:
			return fmt.Errorf("watch task %q stopped before its replacement process could start; inspect the task log for the command error", t.ID)
		}
	}
}

func watcherSignature(t task.Task) string {
	return t.GenerationID + "\x00" + t.WatchCmd + "\x00" + t.ProjectPath + "\x00" + t.Name
}

// flushedDrop is one stopped watcher's final drop total tagged with the task
// incarnation that produced it. The tag is what makes the transfer between a
// stopped watcher and the replacement row safe: a reused task ID must not
// inherit drops — or the "dropped" last-run status — its predecessor earned
// (#4224 review).
type flushedDrop struct {
	generationID string
	drops        int
}

func stopWatchers(ws []*taskWatcher) map[string]flushedDrop {
	var wg sync.WaitGroup
	totals := make([]int, len(ws))
	for i, w := range ws {
		wg.Add(1)
		go func(i int, w *taskWatcher) {
			defer wg.Done()
			totals[i] = w.stop()
		}(i, w)
	}
	wg.Wait()
	dropped := make(map[string]flushedDrop, len(ws))
	for i, w := range ws {
		if totals[i] > dropped[w.taskID].drops {
			dropped[w.taskID] = flushedDrop{generationID: w.generationID, drops: totals[i]}
		}
	}
	return dropped
}

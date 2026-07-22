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
		current.stop()
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

package daemon

import "github.com/sachiniyer/agent-factory/task"

// renderedWatchPrompt returns line rendered through taskID's watch-prompt
// template, mirroring the pre-flight `strings.TrimSpace(prompt) == ""` rule
// deliverWatchEventWithOptions applies before any send (daemon/watcher.go:915),
// so the durable-queue boundary can decide deliverability with the same
// yardstick the drainer's replay will. The supervisor's loader is the same
// source deliverWatchEventWithOptions reloads per event, so a prompt edit
// between enqueue and replay is reflected at whichever arm reads it last;
// whichever arm first sees a trimspace-empty render drops the event, and the
// survivor behaves identically. A loader that is unset (supervisor built by
// a focused test) or fails, or a task deleted between scheduling and this
// enqueue, has no template to render through: return the raw line and let the
// caller's blankness check fall back to the raw line, which never false-keeps
// a blank line.
func (s *watcherSupervisor) renderedWatchPrompt(taskID, line string) string {
	if s.loadTasks == nil {
		return line
	}
	tasks, err := s.loadTasks()
	if err != nil {
		return line
	}
	for i := range tasks {
		if tasks[i].ID == taskID {
			return task.RenderWatchPrompt(tasks[i].Prompt, line)
		}
	}
	return line
}

package daemon

import "github.com/sachiniyer/agent-factory/task"

// renderedWatchPrompt returns line rendered through taskID's watch-prompt
// template plus whether that render is dependable, mirroring the pre-flight
// `strings.TrimSpace(prompt) == ""` rule deliverWatchEventWithOptions applies
// before any send (daemon/watcher.go:920), so the durable-queue boundary can
// decide deliverability with the same yardstick the drainer's replay will.
// The supervisor's loader is the same source deliverWatchEventWithOptions
// reloads per event, so a prompt edit between enqueue and replay is reflected
// at whichever arm reads it last; whichever arm first sees a trimspace-empty
// render drops the event, and the survivor behaves identically.
//
// The render is dependable only when the task was actually loaded: a loader
// that is unset (supervisor built by a focused test), fails, or finds the task
// gone leaves the prompt UNKNOWN, and equating unknown with the default prompt
// would permanently discard an event whose real template may yet render a
// deliverable prompt once the store recovers. The caller therefore fails OPEN
// on ok=false (retains the event) and lets the drainer retry, so the discard
// only fires on a dependable empty render — never on a default-prompt
// assumption that could lose a recoverable event. A fail-open of a genuinely
// blank line parks at worst until the drainer re-renders; the drain path's
// errEmptyPrompt advance (daemon/watcher_drain.go) bounds that park to one
// replay, so the fail-open cannot strand the queue.
func (s *watcherSupervisor) renderedWatchPrompt(taskID, line string) (string, bool) {
	if s.loadTasks == nil {
		return line, false
	}
	tasks, err := s.loadTasks()
	if err != nil {
		return line, false
	}
	for i := range tasks {
		if tasks[i].ID == taskID {
			return task.RenderWatchPrompt(tasks[i].Prompt, line), true
		}
	}
	return line, false
}

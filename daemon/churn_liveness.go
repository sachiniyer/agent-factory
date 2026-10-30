package daemon

import (
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// settleChurnedLiveness records the liveness for a tick whose pane changed.
// Churn normally means Running, but a frame where the agent itself says it gave
// up (codex "Reconnect failed", whose elapsed timer repaints forever) is positive
// evidence of NOT working, and churn alone must not override it (#6055). Such a
// frame settles like an idle pane — Ready, the waiting-for-you dot — instead of
// a Running that never ends. captured guards against settling off an empty read.
func (m *Manager) settleChurnedLiveness(instance *session.Instance, captured bool, content string, epoch uint64) {
	if captured && task.AgentStoppedContent(content, instance.ResolvedAgent()) {
		m.resolveIdleLiveness(instance, content, epoch)
		return
	}
	agent := instance.ResolvedAgent()
	running := session.ObserveLiveness(session.LiveRunning).AtEpoch(epoch)
	if !task.PublishesWorkingContent(agent) {
		_ = instance.Transition(running)
		return
	}
	// The agent publishes its own in-turn indicator, so churn is not the evidence
	// of work (a typed draft repaints the composer too): the liveness moves, but
	// an attention request is answered only by the indicator itself.
	_ = instance.Transition(running.ChurnOnly())
	if captured && task.IsWorkingContent(content, agent) {
		m.answerAttentionByWork(instance)
	}
}

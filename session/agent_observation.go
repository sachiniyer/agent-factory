package session

import "time"

// SnapshotAgent serializes pane observation with prompt delivery. The caller
// supplies the runtime handle it paired with its state epoch; that epoch is the
// apply fence after this method returns and RecordPromptAttempt advances it.
func (i *Instance) SnapshotAgent(server AgentServer) (Observation, error) {
	i.agentObservationMu.Lock()
	defer i.agentObservationMu.Unlock()
	return server.Snapshot()
}

// SendPromptWithEvidence starts the attempt timestamp only after any pane
// snapshot already in flight has completed, then records the delivery verdict
// before another snapshot can begin.
func (i *Instance) SendPromptWithEvidence(prompt string, now func() time.Time) (PromptDeliveryStatus, error) {
	i.agentObservationMu.Lock()
	defer i.agentObservationMu.Unlock()
	attemptedAt := now()
	status, err := SendPromptWithStatus(i.AgentServer(), prompt)
	i.RecordPromptAttempt(status, attemptedAt)
	return status, err
}

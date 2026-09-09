package session

import "fmt"

// WithPostStopCapture attaches the transaction's branch capture without changing
// the preflighted command. The backend must call CaptureAfterStop after confirmed
// teardown and before launching any replacement process.
func (p AgentSwapPlan) WithPostStopCapture(capture func() error) AgentSwapPlan {
	p.afterStop = capture
	return p
}

// CaptureAfterStop freezes the outgoing work at the stop/start boundary. A
// teardown refusal must not call it; a capture error must prevent replacement.
func (p AgentSwapPlan) CaptureAfterStop() error {
	if p.afterStop != nil {
		return p.afterStop()
	}
	return nil
}

// CaptureHandoffBrief uses the same branch summary as account handoffs, after
// teardown. The provisional record retains the outgoing identity even though
// Program already names the target. Update the rollback token too, so a later
// launch failure can still remove precisely this transaction's ledger entry.
func (i *Instance) CaptureHandoffBrief(swap *HandoffSwap, override string) (MissionBrief, error) {
	brief := i.BuildMissionBrief(swap.To, override, swap.Reason)
	brief.From = swap.From.Agent
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.inFlightOp != OpReplacing || len(i.Tabs) == 0 || len(i.Tabs[0].Handoffs) == 0 {
		return MissionBrief{}, fmt.Errorf("session %q has no handoff to capture", i.Title)
	}
	n := len(i.Tabs[0].Handoffs)
	if i.Tabs[0].Handoffs[n-1] != swap.AgentHandoff {
		return MissionBrief{}, fmt.Errorf("session %q: the last handoff is not the one being captured", i.Title)
	}
	swap.HeadSHA = brief.Work.HeadSHA
	i.Tabs[0].Handoffs[n-1] = swap.AgentHandoff
	i.touchLocked()
	return brief, nil
}

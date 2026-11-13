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

// WithBeforeStart attaches a step the backend runs once the outgoing runtime is
// confirmed stopped and its post-stop capture is done, immediately before the
// incoming runtime starts: the last point at which a refusal still leaves
// nothing running. A handoff makes its incoming record durable there (#7215),
// as an account swap makes its new identity durable before its spawn. A step
// error refuses the start. A backend that cannot bracket the start, because
// the replacement starts inside a sandbox's own agent-server, never runs it.
func (p AgentSwapPlan) WithBeforeStart(step func() error) AgentSwapPlan {
	p.beforeStart = step
	return p
}

// RunBeforeStart runs the WithBeforeStart step, if any. A backend calls it after
// CaptureAfterStop and immediately before starting the incoming runtime.
func (p AgentSwapPlan) RunBeforeStart() error {
	if p.beforeStart != nil {
		return p.beforeStart()
	}
	return nil
}

// CaptureHandoffBrief uses the same branch summary as account handoffs, after
// teardown. The provisional record retains the outgoing identity even though
// Program already names the target. Update the rollback token too, so a later
// launch failure can still remove precisely this transaction's ledger entry.
func (i *Instance) CaptureHandoffBrief(swap *HandoffSwap, override string) (MissionBrief, error) {
	// The brief's To is the effective agent — the one the resolved command
	// launches — so Render's same-agent check is judged on the same value
	// admission was. An unclassifiable command has no effective answer; the
	// enum is still the honest display label there. But sameness is a
	// two-sided decision admission made, not a value Render can re-derive from
	// From == To: a program_overrides redirect can make the resolved running
	// identity (From) equal the target enum (To) for a handoff admission
	// classified as cross-agent. The CrossAgent flag carries admission's
	// verdict so Render's same-agent branch honors it instead of collapsing
	// on that coincidence (#4430 review).
	to := swap.effectiveTo
	if to == "" {
		to = swap.To
	}
	brief := i.BuildMissionBrief(to, override, swap.Reason)
	brief.From = swap.From.Agent
	brief.CrossAgent = swap.crossAgent
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

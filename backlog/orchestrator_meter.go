package backlog

import "maps"

// Cost sums the orchestrator's metered transcript costs: its conversation from
// when it was bound, across every session that orchestrated the group.
func (o *Orchestrator) Cost() float64 { return RunRef{Meter: o.Meter}.Cost() }

// SetOrchestratorMeters stores per-file meters on the orchestrators of the
// given sessions. It returns whether anything changed.
func (b *Backlog) SetOrchestratorMeters(meters map[string]map[string]Meter) bool {
	changed := false
	for i := range b.Items {
		o := b.Items[i].Orchestrator
		if o == nil || o.SessionID == "" {
			continue
		}
		if m, ok := meters[o.SessionID]; ok && !maps.Equal(o.Meter, m) {
			o.Meter = maps.Clone(m)
			changed = true
		}
	}
	return changed
}

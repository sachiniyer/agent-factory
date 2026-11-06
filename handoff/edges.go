package handoff

// edges.go is the edge-trigger discipline the shadow log requires (design
// R14): a decision emits on signature change — a parked session produces one
// row, not one per tick — and an unchanged verdict re-announces at most once
// per heartbeat interval.

import (
	"sort"
	"strings"
	"time"
)

// EdgeTracker dedupes decision events per session. It is in-memory by design:
// a daemon restart re-announces the standing verdict once, which is itself a
// true edge (a fresh process's first sight of the session).
type EdgeTracker struct {
	last map[string]edgeState
}

type edgeState struct {
	sig string
	at  time.Time
}

// NewEdgeTracker returns an empty tracker.
func NewEdgeTracker() *EdgeTracker {
	return &EdgeTracker{last: map[string]edgeState{}}
}

// Clone returns a tracker copy for two-phase emission: a pass may filter on
// the clone and commit it only after the events it released are durably
// written, so a failed write never suppresses the verdict's retry (#6817
// review).
func (t *EdgeTracker) Clone() *EdgeTracker {
	c := &EdgeTracker{last: make(map[string]edgeState, len(t.last))}
	for k, v := range t.last {
		c.last[k] = v
	}
	return c
}

// Filter returns the events worth logging for sessionKey: all of them when
// the session's verdict signature changed (or the re-announce interval
// elapsed), none when it is unchanged. An empty event set is itself a
// signature — the noop verdict — and silence is never re-announced; the
// transition INTO silence still records, so a later signal fires a real edge.
func (t *EdgeTracker) Filter(sessionKey string, events []Event, now time.Time, reannounce time.Duration) []Event {
	sig := eventSignature(events)
	prev, seen := t.last[sessionKey]
	if seen && prev.sig == sig && (sig == "" || now.Sub(prev.at) < reannounce) {
		return nil
	}
	t.last[sessionKey] = edgeState{sig: sig, at: now}
	return events
}

// Prune drops edge state for sessions no longer in the governed set, so a
// session that leaves and returns re-announces once rather than suppressing a
// genuinely new verdict under a stale signature.
func (t *EdgeTracker) Prune(live map[string]bool) {
	for key := range t.last {
		if !live[key] {
			delete(t.last, key)
		}
	}
}

// signatureParams are the Event.Params keys that define a verdict's IDENTITY
// — stable codes and booleans that name WHAT the verdict is, not the volatile
// evidence numbers that describe how bad it is right now. Only these keys
// participate in the dedup signature; the rest (percent, the observed window
// name, the move count) are evidence that drifts between ticks for a
// semantically unchanged verdict and must NOT re-trigger emission (design
// R14: one row per verdict change, one heartbeat re-announce). The full
// values stay on the row for the operator — the signature is just cheaper
// than the row.
var signatureParams = map[string]bool{
	"confirmed":   true, // wall confirmation verdict identity
	"trigger":     true, // which trigger fired (wall vs idle)
	"skipped":     true, // which candidates were skipped and why
	"halt_reason": true, // the mover halt reason
	"halted_at":   true, // the halt episode timestamp (new episode = new edge)
	"until":       true, // the refusal-backoff deadline (new backoff = new edge)
}

// eventSignature renders the decision's event set canonically: order and
// parameter map iteration cannot divide one verdict into two signatures.
// Only stable params (signatureParams) participate — volatile evidence
// (percent, window name, move count) stays on the row but is excluded so a
// standing verdict does not re-emit on every usage-percent jitter.
func eventSignature(events []Event) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString(string(e.Kind))
		b.WriteByte('|')
		b.WriteString(e.Reason)
		b.WriteByte('|')
		b.WriteString(e.From)
		b.WriteByte('|')
		b.WriteString(e.To)
		b.WriteByte('|')
		keys := make([]string, 0, len(e.Params))
		for k := range e.Params {
			if signatureParams[k] {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString(k)
			b.WriteByte('=')
			b.WriteString(e.Params[k])
			b.WriteByte(';')
		}
		b.WriteByte('\n')
	}
	return b.String()
}

package backlog

import "fmt"

type State string

const (
	StateDone     State = "done"
	StateDropped  State = "dropped"
	StateNeedsYou State = "needs_you"
	StateInReview State = "in_review"
	StateWorking  State = "working"
	StateStarting State = "starting"
	StateStopped  State = "stopped"
	StateWaiting  State = "waiting"
	StateQueued   State = "queued"
	StateDraft    State = "draft"
)

var stateLabels = map[State]string{
	StateDone:     "Done",
	StateDropped:  "Dropped",
	StateNeedsYou: "Needs you",
	StateInReview: "In review",
	StateWorking:  "Working",
	StateStarting: "Starting",
	StateStopped:  "Stopped",
	StateWaiting:  "Waiting",
	StateQueued:   "Queued",
	StateDraft:    "Draft",
}

// Label is the state as the UI shows it.
func (s State) Label() string { return stateLabels[s] }

// ParseState accepts a state value or its label.
func ParseState(s string) (State, error) {
	for st, label := range stateLabels {
		if s == string(st) || s == label {
			return st, nil
		}
	}
	return "", fmt.Errorf("unknown state %q — use one of draft, queued, waiting, starting, working, stopped, in_review, needs_you, done, dropped", s)
}

// SessionFacts is what the caller knows about one session.
type SessionFacts struct {
	// Limited marks a session parked at a usage limit; Reason says until when.
	Limited     bool
	Live        bool
	RunActive   bool
	Lost        bool
	AutoRestore bool
	Reason      string
}

// Facts looks up a session by id. A nil Facts means no session is known.
type Facts func(sessionID string) SessionFacts

// Derived is an item's shown state.
type Derived struct {
	State  State  `json:"state"`
	Reason string `json:"reason,omitempty"`
	// HoldsSlot is whether the item counts against the runner's max_running.
	HoldsSlot bool `json:"-"`
}

// rollupRank orders the states a parent takes from its children when no child
// needs the user or is working.
var rollupRank = map[State]int{
	StateDraft:    1,
	StateWaiting:  2,
	StateQueued:   3,
	StateStopped:  4,
	StateInReview: 5,
}

// Derive returns the shown state of item id, or a zero Derived when id is
// unknown.
func (b *Backlog) Derive(id int, facts Facts) Derived {
	return newDeriver(b, facts).derive(id)
}

// DeriveAll returns the shown state of every item.
func (b *Backlog) DeriveAll(facts Facts) map[int]Derived {
	d := newDeriver(b, facts)
	out := make(map[int]Derived, len(b.Items))
	for _, it := range b.Items {
		out[it.ID] = d.derive(it.ID)
	}
	return out
}

type deriver struct {
	b        *Backlog
	facts    Facts
	memo     map[int]Derived
	children map[int][]int
	busy     map[int]bool
}

func newDeriver(b *Backlog, facts Facts) *deriver {
	if facts == nil {
		facts = func(string) SessionFacts { return SessionFacts{} }
	}
	d := &deriver{b: b, facts: facts, memo: map[int]Derived{}, children: map[int][]int{}, busy: map[int]bool{}}
	for _, it := range b.Items {
		if it.Parent != 0 {
			d.children[it.Parent] = append(d.children[it.Parent], it.ID)
		}
	}
	return d
}

func (d *deriver) derive(id int) Derived {
	if r, ok := d.memo[id]; ok {
		return r
	}
	it := d.b.Item(id)
	if it == nil || d.busy[id] {
		return Derived{}
	}
	d.busy[id] = true
	var r Derived
	if len(d.children[id]) > 0 {
		r = d.rollup(it)
	} else {
		r = d.leaf(it)
	}
	delete(d.busy, id)
	d.memo[id] = r
	return r
}

func (d *deriver) leaf(it *Item) Derived {
	switch {
	case it.Status == StatusDone:
		return Derived{State: StateDone}
	case it.Status == StatusDropped:
		return Derived{State: StateDropped}
	case it.OpenQuestion() != nil:
		return Derived{State: StateNeedsYou}
	case it.Status == StatusReview:
		return Derived{State: StateInReview}
	case it.Status == StatusRunning:
		return d.running(it)
	case it.Status == StatusQueued:
		if reason := d.waitReason(it); reason != "" {
			return Derived{State: StateWaiting, Reason: reason}
		}
		return Derived{State: StateQueued}
	}
	return Derived{State: StateDraft}
}

func (d *deriver) running(it *Item) Derived {
	run := it.lastRun()
	if run == nil {
		return Derived{State: StateStopped, Reason: "no session"}
	}
	if run.State == RunStarting {
		return Derived{State: StateStarting, HoldsSlot: true}
	}
	if run.SessionID == "" {
		return Derived{State: StateStopped, Reason: "no session"}
	}
	f := d.facts(run.SessionID)
	if f.RunActive || it.answerPending() {
		// An answer on its way to the session is work in progress even while its
		// run is not armed yet: delivery arms the run immediately before it sends,
		// and a held delivery (a client attached) has no open completion gate.
		return Derived{State: StateWorking, HoldsSlot: true}
	}
	reason := f.Reason
	if reason == "" {
		reason = "no active run"
	}
	return Derived{State: StateStopped, Reason: reason, HoldsSlot: f.Lost && f.AutoRestore}
}

// waitReason walks the item and its ancestors: an item waits on what any
// ancestor waits on, and on earlier siblings under a sequential parent.
func (d *deriver) waitReason(it *Item) string {
	seen := map[int]bool{}
	for n := it; n != nil && !seen[n.ID]; n = d.b.Item(n.Parent) {
		seen[n.ID] = true
		for _, w := range n.WaitsOn {
			target := d.b.Item(w)
			if target == nil {
				continue
			}
			switch d.derive(w).State {
			case StateDone:
			case StateDropped:
				return fmt.Sprintf("Waiting on %s (dropped)", FormatID(w))
			default:
				return "Waiting on " + FormatID(w)
			}
		}
		if p := d.b.Item(n.Parent); p != nil && p.Sequential {
			for _, sib := range d.children[p.ID] {
				if sib == n.ID {
					break
				}
				if s := d.derive(sib).State; s != StateDone && s != StateDropped {
					return fmt.Sprintf("Waiting on %s (runs in order)", FormatID(sib))
				}
			}
		}
	}
	return ""
}

func (d *deriver) rollup(it *Item) Derived {
	kids := d.children[it.ID]
	var needs, working, done, dropped int
	best := Derived{}
	for _, c := range kids {
		r := d.derive(c)
		switch r.State {
		case StateNeedsYou:
			needs++
		case StateWorking, StateStarting:
			working++
		case StateDone:
			done++
		case StateDropped:
			dropped++
		}
		if rollupRank[r.State] > rollupRank[best.State] {
			best = r
		}
	}
	reason := fmt.Sprintf("%d of %d done", done, len(kids))
	switch {
	case needs > 0 || it.OpenQuestion() != nil:
		return Derived{State: StateNeedsYou, Reason: reason}
	case working > 0:
		return Derived{State: StateWorking, Reason: reason}
	case dropped == len(kids):
		return Derived{State: StateDropped, Reason: reason}
	case done+dropped == len(kids):
		return Derived{State: StateDone, Reason: reason}
	}
	if best.State == StateWaiting {
		reason = best.Reason
	}
	return Derived{State: best.State, Reason: reason}
}

package layout

// Ring is the focus ring (RFC §2.3): an ordered list of focusable ids with
// one active entry. The canonical order is tree → pane A → pane B (if
// split) → automations → projects; regions hidden by the degradation ladder are marked
// hidden and skipped when cycling.
type Ring struct {
	ids    []string
	hidden map[string]bool
	active int
}

// NewRing builds a ring over ids in cycling order. The first visible id is
// active.
func NewRing(ids ...string) *Ring {
	return &Ring{
		ids:    append([]string(nil), ids...),
		hidden: make(map[string]bool),
	}
}

// SetHidden marks an id hidden (skipped by cycling and unfocusable) or
// visible again. Hiding the active id makes the next visible id active.
func (r *Ring) SetHidden(id string, hidden bool) { r.hidden[id] = hidden }

// SetIDs replaces the ring's id set in cycling order — the N-pane ring
// changes shape as panes open, close, and auto-hide (#1088). The active id
// is preserved when it survives the change; otherwise the first visible id
// becomes active. Hidden flags carry over for ids present in both sets;
// flags for dropped ids are discarded so a re-added id starts visible.
func (r *Ring) SetIDs(ids ...string) {
	activeID := ""
	if r.normalize() {
		activeID = r.ids[r.active]
	}
	hidden := make(map[string]bool, len(ids))
	for _, id := range ids {
		if r.hidden[id] {
			hidden[id] = true
		}
	}
	r.ids = append([]string(nil), ids...)
	r.hidden = hidden
	r.active = 0
	for i, id := range r.ids {
		if id == activeID {
			r.active = i
			break
		}
	}
}

// Active returns the currently focused id, or "" when every id is hidden.
// If the active id has been hidden, focus moves forward to the next visible
// id.
func (r *Ring) Active() string {
	if !r.normalize() {
		return ""
	}
	return r.ids[r.active]
}

// Next advances focus to the next visible id, wrapping around, and returns
// it. Returns "" when every id is hidden.
func (r *Ring) Next() string { return r.step(1) }

// Prev moves focus to the previous visible id, wrapping around, and returns
// it. Returns "" when every id is hidden.
func (r *Ring) Prev() string { return r.step(-1) }

// Focus moves focus directly to id. It reports whether it did; unknown and
// hidden ids are refused and leave focus unchanged.
func (r *Ring) Focus(id string) bool {
	for i, s := range r.ids {
		if s == id && !r.hidden[s] {
			r.active = i
			return true
		}
	}
	return false
}

// normalize ensures active points at a visible id, scanning forward from
// the current position. It reports whether any id is visible.
func (r *Ring) normalize() bool {
	n := len(r.ids)
	for i := 0; i < n; i++ {
		idx := (r.active + i) % n
		if !r.hidden[r.ids[idx]] {
			r.active = idx
			return true
		}
	}
	return false
}

func (r *Ring) step(delta int) string {
	n := len(r.ids)
	if n == 0 || !r.normalize() {
		return ""
	}
	for i := 1; i <= n; i++ {
		idx := ((r.active+delta*i)%n + n) % n
		if !r.hidden[r.ids[idx]] {
			r.active = idx
			return r.ids[idx]
		}
	}
	return r.ids[r.active] // single visible id: stay put
}

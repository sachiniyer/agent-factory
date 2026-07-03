// Package zones is the hand-rolled mouse hit-testing registry for the
// multi-pane TUI rewrite (docs/design/tui-rewrite.md §2.5, epic #1024) —
// deliberately not bubblezone, which post-processes ANSI markers out of
// every frame; this registry works directly on the layout Rect model.
//
// During View() each pane registers a rect per interactive target (zone ids
// like "tree:instance:api-fix" or "pane:A:header"). The root model resolves
// every tea.MouseMsg through Resolve and dispatches to the owning pane with
// pane-local coordinates. The registry is rebuilt every frame: Reset at the
// top of View, Register while rendering.
package zones

import "github.com/sachiniyer/agent-factory/ui/layout"

type zone struct {
	id   string
	rect layout.Rect
}

// Registry maps screen rects to zone ids for mouse hit-testing. It is not
// goroutine-safe: registration and resolution both happen on the bubbletea
// event loop.
type Registry struct {
	zones []zone
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{} }

// Register records that rect belongs to zone id for the current frame.
// Registration order is paint order: when zones overlap, the most recently
// registered one is "on top" and wins hit-testing. Empty rects are accepted
// but can never be hit.
func (r *Registry) Register(id string, rect layout.Rect) {
	r.zones = append(r.zones, zone{id: id, rect: rect})
}

// Resolve hit-tests a screen coordinate. It returns the topmost zone
// containing the point and the point in zone-local coordinates; ok is false
// (with empty id) on a miss.
func (r *Registry) Resolve(x, y int) (id string, local layout.Point, ok bool) {
	p := layout.Point{X: x, Y: y}
	for i := len(r.zones) - 1; i >= 0; i-- {
		if z := r.zones[i]; z.rect.Contains(p) {
			return z.id, z.rect.Local(p), true
		}
	}
	return "", layout.Point{}, false
}

// Reset clears all registrations for a new frame, retaining capacity.
func (r *Registry) Reset() { r.zones = r.zones[:0] }

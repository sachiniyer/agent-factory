package ui

import (
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
	"github.com/sachiniyer/agent-factory/ui/tree"
)

// registerZones records the mouse hit-test rects for this frame (#1024 R4):
// the pane background, the section header, and — for every row inside the
// rendered window — the instance/tab row plus the instance's ▸/▾ arrow cell.
// Coordinates mirror String()'s exact line budget (the leading blank + title
// chrome, then the "▲ N more" indicator when the window is scrolled), and
// every zone is clipped to the pane rect since the final ClampToRect clips
// any partially fitting last row the same way.
func (s *Sidebar) registerZones(heights []int, start, end int, topIndicator bool) {
	if s.zones == nil || s.rect.Empty() {
		return
	}
	s.zones.Register(zones.TreeBG, s.rect)

	instances := s.proj.GetInstances()
	y := s.rect.Y + chromeLines
	if topIndicator {
		y++
	}
	bottom := s.rect.Bottom()
	for i := start; i < end && y < bottom; i++ {
		item := s.visibleItems[i]
		h := heights[i]
		if y+h > bottom {
			h = bottom - y
		}
		row := layout.Rect{X: s.rect.X, Y: y, W: s.rect.W, H: h}
		switch {
		case item.IsHeader:
			// Distinct zone per folder so a click toggles the right one (#1028).
			if item.Kind == SectionArchived {
				s.zones.Register(zones.TreeHeaderArchived, row)
			} else {
				s.zones.Register(zones.TreeHeader, row)
			}
		case isInstanceRow(item) && item.ItemIndex >= 0 && item.ItemIndex < len(instances):
			inst := instances[item.ItemIndex]
			switch {
			case item.IsTab:
				s.zones.Register(zones.TreeTab(inst.Title, item.TabIndex), row)
			default:
				// Instance row (live or archived, #1028): a title-keyed select
				// zone. Archived rows are flat (no tab children), so they get no
				// expand/collapse arrow — only live, expandable rows do.
				s.zones.Register(zones.TreeInstance(inst.Title), row)
				if item.Kind == SectionInstances {
					if ax, ay, ok := tree.ArrowCell(s.contentWidth()); ok && tree.Expandable(inst) && ay < h {
						s.zones.Register(zones.TreeArrow(inst.Title),
							layout.Rect{X: s.rect.X + ax, Y: y + ay, W: 1, H: 1})
					}
				}
			}
		}
		y += heights[i]
	}
}

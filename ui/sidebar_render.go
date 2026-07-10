package ui

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/tree"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// String renders the sidebar. The item list is windowed around the selection
// (#787): lipgloss.Place pads short content but never truncates, so rendering
// every row would overflow the allocation once enough instances exist and push
// the menu/error box below the fold. A blind clamp (the #700 fix for the
// content pane) would be wrong here because the selected row can sit below the
// fold while navigating — instead the rendered slice scrolls so the selection
// is always visible, with "▲/▼ N more" rows marking hidden items.
// chromeLines is the fixed chrome above the sidebar's item list: one leading
// blank line plus the title row. Shared by String()'s window budget and the
// zone registration's y origin so the two can't drift.
const chromeLines = 2

func (s *Sidebar) String() string {
	s.syncFromStore()

	// Render every visible row up front and measure real heights: instance
	// rows are multi-line (title + branch, plus an optional PR line) while tab
	// rows are single-line, so the window math cannot assume one line per item.
	rows := make([]string, len(s.visibleItems))
	heights := make([]int, len(s.visibleItems))
	totalLines := 0
	for i, item := range s.visibleItems {
		isSelected := i == s.selectedIdx
		if item.IsHeader {
			rows[i] = s.renderHeader(item.Kind, isSelected)
		} else {
			switch item.Kind {
			case SectionInstances:
				if item.IsTab {
					rows[i] = s.renderTabRow(item, isSelected)
				} else {
					rows[i] = s.renderInstance(item.ItemIndex, isSelected)
				}
			case SectionArchived:
				// Archived rows are flat instance rows (#1028) — no tab children.
				rows[i] = s.renderInstance(item.ItemIndex, isSelected)
			}
		}
		heights[i] = lipgloss.Height(rows[i])
		totalLines += heights[i]
	}

	avail := s.height - chromeLines
	start, end := 0, len(rows)
	hiddenAbove, hiddenBelow := 0, 0
	if s.height > 0 && totalLines > avail {
		s.scrollToSelection(heights, avail)
		start = s.scrollOffset
		end, _, _ = fitWindow(heights, start, avail)
		hiddenAbove = start
		hiddenBelow = len(rows) - end
	} else {
		s.scrollOffset = 0
	}

	s.registerZones(heights, start, end, hiddenAbove > 0)

	var b strings.Builder
	b.WriteString("\n")

	// Title bar. Clamp the chip width to the allocation and truncate the text
	// to the chip: lipgloss.Place pads but never clips, so at ultra-narrow
	// widths the 15-cell " Agent Factory " (or the padded chip itself) would
	// otherwise push the row past s.width — the same #646 overflow class the
	// section headers hit. The chip doubles as the tree's focus-ring indicator:
	// the accent background recedes to gray when focus is elsewhere.
	titleWidth := s.contentWidth() + 2
	if s.width > 0 && titleWidth > s.width {
		titleWidth = s.width
	}
	titleChip := mainTitle
	if !s.focused {
		titleChip = blurredTitle
	}
	titleText := s.titleText()
	if !s.autoyes {
		b.WriteString(lipgloss.Place(
			titleWidth, 1, lipgloss.Left, lipgloss.Bottom,
			titleChip.Render(fitTitleText(titleText, titleWidth))))
	} else {
		title := lipgloss.Place(
			titleWidth/2, 1, lipgloss.Left, lipgloss.Bottom,
			titleChip.Render(fitTitleText(titleText, titleWidth/2)))
		autoYes := lipgloss.Place(
			titleWidth-(titleWidth/2), 1, lipgloss.Right, lipgloss.Bottom,
			autoYesStyle.Render(fitTitleText(" auto-yes ", titleWidth-(titleWidth/2))))
		b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, title, autoYes))
	}

	if hiddenAbove > 0 {
		b.WriteString("\n")
		b.WriteString(s.renderWindowIndicator("▲", hiddenAbove))
	}
	for i := start; i < end; i++ {
		b.WriteString("\n")
		b.WriteString(rows[i])
	}
	if hiddenBelow > 0 {
		b.WriteString("\n")
		b.WriteString(s.renderWindowIndicator("▼", hiddenBelow))
	}

	out := lipgloss.Place(s.width, s.height, lipgloss.Left, lipgloss.Top, b.String())
	// Safety clamp to the exact allocation via PR 1's shared helper:
	// lipgloss.Place pads but never truncates in either dimension, fitWindow
	// force-includes the first windowed row even when it alone exceeds the
	// height budget (e.g. a tall PR row at a tiny height), and rare row shapes
	// (double-digit index prefixes at ultra-narrow widths) can still exceed
	// the width after the per-row truncation (#646/#700 class).
	if s.width > 0 && s.height > 0 {
		out = layout.ClampToRect(out, layout.Rect{W: s.width, H: s.height})
	} else if s.height > 0 {
		if lines := strings.Split(out, "\n"); len(lines) > s.height {
			out = strings.Join(lines[:s.height], "\n")
		}
	}
	return out
}

// scrollToSelection clamps scrollOffset and moves it minimally so the selected
// row is fully inside the rendered window. For tab rows, the parent instance
// title is treated as the top anchor when it fits, preserving the rendered
// group header even though vertical nav stops on tabs only. Only called when
// the list overflows the allocation.
func (s *Sidebar) scrollToSelection(heights []int, avail int) {
	n := len(heights)
	if s.scrollOffset > n-1 {
		s.scrollOffset = n - 1
	}
	if s.scrollOffset < 0 {
		s.scrollOffset = 0
	}
	anchor := s.scrollAnchorIndex()
	if anchor < s.scrollOffset {
		s.scrollOffset = anchor
	}
	for s.scrollOffset < s.selectedIdx {
		end, _, _ := fitWindow(heights, s.scrollOffset, avail)
		if s.selectedIdx < end {
			break
		}
		s.scrollOffset++
	}
	// Fill from the bottom: when the tail of the list fits starting one row
	// earlier (items were removed or the selection sits at the end), scroll
	// up to use the slack rather than render dead space below the last item.
	for s.scrollOffset > 0 {
		if end, _, _ := fitWindow(heights, s.scrollOffset-1, avail); end < n {
			break
		}
		s.scrollOffset--
	}
}

func (s *Sidebar) scrollAnchorIndex() int {
	if s.selectedIdx < 0 || s.selectedIdx >= len(s.visibleItems) {
		return s.selectedIdx
	}
	sel := s.visibleItems[s.selectedIdx]
	if sel.Kind != SectionInstances || !sel.IsTab {
		return s.selectedIdx
	}
	for i := s.selectedIdx - 1; i >= 0; i-- {
		item := s.visibleItems[i]
		if item.Kind == SectionInstances && !item.IsHeader && !item.IsTab &&
			item.ItemIndex == sel.ItemIndex {
			if i > 0 && s.visibleItems[i-1].IsHeader &&
				s.visibleItems[i-1].Kind == SectionInstances {
				return i - 1
			}
			return i
		}
		if isInstanceRow(item) && item.ItemIndex != sel.ItemIndex {
			break
		}
	}
	return s.selectedIdx
}

// fitWindow returns the exclusive end index of the rows that fit when
// rendering starts at offset within avail lines, plus whether "▲"/"▼"
// indicator rows (one line each) are needed. heights[i] is the rendered line
// count of visibleItems[i].
func fitWindow(heights []int, offset, avail int) (end int, topInd, botInd bool) {
	n := len(heights)
	topInd = offset > 0
	for {
		budget := avail
		if topInd {
			budget--
		}
		if botInd {
			budget--
		}
		end = offset
		used := 0
		for end < n && used+heights[end] <= budget {
			used += heights[end]
			end++
		}
		if end == offset && offset < n {
			// Force at least one row so scrolling can always reach the
			// selection; String()'s final clamp bounds any overflow.
			end = offset + 1
		}
		if end == n || botInd {
			return end, topInd, botInd
		}
		// Rows remain below: reserve a line for the "▼" indicator and refit.
		// Shrinking the budget can only shrink end, so this converges in one
		// extra pass.
		botInd = true
	}
}

// renderWindowIndicator renders the one-line "▲ N more" / "▼ N more" marker
// shown in place of rows scrolled out of the window.
func (s *Sidebar) renderWindowIndicator(arrow string, hidden int) string {
	w := s.contentWidth()
	text := fmt.Sprintf("%s %d more", arrow, hidden)
	if w > 0 && runewidth.StringWidth(text) > w {
		// Same narrow-width handling as renderHeader: drop the "..." tail when
		// it would itself overflow, since lipgloss.Place won't clip oversize
		// content.
		tail := "..."
		if w < runewidth.StringWidth(tail) {
			tail = ""
		}
		text = runewidth.Truncate(text, w, tail)
	}
	return windowIndicatorStyle.Padding(0, narrowAwarePad(w)).Render(
		lipgloss.Place(w, 1, lipgloss.Left, lipgloss.Center, text))
}

// narrowAwarePad returns the horizontal padding for a one-line sidebar row
// rendered at effective content width w. The 2-cell Padding(0,1) sits OUTSIDE
// the width the row text is truncated to, so at ultra-narrow allocations the
// padded row overflows the sidebar (the #646 overflow class; reproduced by
// T-Rex at SetSize(9,18) on the section header). Drop the padding at the same
// w <= 9 threshold the instance and tab rows already use.
func narrowAwarePad(w int) int {
	if w <= 9 {
		return 0
	}
	return 1
}

// titleText is the title-chip label: the active project's name (#1461) when one
// is set, otherwise the default " Agent Factory " brand. Both are wrapped in the
// same single-space padding the chip has always rendered.
func (s *Sidebar) titleText() string {
	if s.projectName != "" {
		return " " + s.projectName + " "
	}
	return " Agent Factory "
}

// fitTitleText truncates a title-bar chip's text to w cells so lipgloss.Place
// (which pads but never clips) can't push the row past the sidebar allocation
// at ultra-narrow widths.
func fitTitleText(text string, w int) string {
	if w > 0 && runewidth.StringWidth(text) > w {
		return runewidth.Truncate(text, w, "")
	}
	return text
}

func (s *Sidebar) renderHeader(kind SidebarSectionKind, selected bool) string {
	var expanded bool
	for _, sec := range s.sections {
		if sec.Kind == kind {
			expanded = sec.Expanded
			break
		}
	}

	arrow := "▶ "
	if expanded {
		arrow = "▼ "
	}

	var label string
	live, archived := partitionByArchived(s.proj.GetInstances())
	switch kind {
	case SectionInstances:
		// Count live sessions only — archived ones live under their own folder.
		label = fmt.Sprintf("Instances (%d)", len(live))
	case SectionArchived:
		label = fmt.Sprintf("Archived (%d)", len(archived))
	}

	style := sectionHeaderStyle
	if selected {
		style = sectionHeaderSelectedStyle
	}

	w := s.contentWidth()
	text := arrow + label
	if w > 0 && runewidth.StringWidth(text) > w {
		// Drop the "..." tail when the container is too narrow to fit it,
		// otherwise runewidth.Truncate returns content wider than w and
		// lipgloss.Place won't clip the overflow.
		tail := "..."
		if w < runewidth.StringWidth(tail) {
			tail = ""
		}
		text = runewidth.Truncate(text, w, tail)
	}
	return style.Padding(0, narrowAwarePad(w)).Render(
		lipgloss.Place(w, 1, lipgloss.Left, lipgloss.Center, text))
}

func (s *Sidebar) renderInstance(idx int, selected bool) string {
	instances := s.proj.GetInstances()
	if idx < 0 || idx >= len(instances) {
		return ""
	}
	inst := instances[idx]
	return s.renderer.Render(inst, idx+1, selected, s.proj.NumRepos() > 1, s.instanceExpanded(inst))
}

// renderTabRow renders one tab child row of an expanded instance. The label
// set comes from tree.TabLabels — the same slots the tab bar shows — and the
// row whose slot matches the store's active tab carries the "*" marker.
func (s *Sidebar) renderTabRow(item SidebarItem, selected bool) string {
	instances := s.proj.GetInstances()
	if item.ItemIndex < 0 || item.ItemIndex >= len(instances) {
		return ""
	}
	labels := tree.TabLabels(instances[item.ItemIndex])
	if item.TabIndex < 0 || item.TabIndex >= len(labels) {
		return ""
	}
	return s.renderer.RenderTab(labels[item.TabIndex], item.TabIndex+1,
		item.TabIndex == len(labels)-1, selected, s.proj.ActiveTab() == item.TabIndex)
}

package ui

import (
	"fmt"
	"sort"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
	"github.com/sachiniyer/agent-factory/ui/store"
	"github.com/sachiniyer/agent-factory/ui/tree"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// SidebarSectionKind identifies the type of sidebar section. Since the layout
// cutover (#1024 PR 4) the left rail is the instances tree only — tasks moved
// to the automations strip and hooks behind an overlay — so Instances is the
// sole section; the kind survives on SidebarItem so selection reads stay
// structured.
type SidebarSectionKind int

const (
	SectionInstances SidebarSectionKind = iota
	// SectionArchived holds archived sessions (#1028) in a folder pinned at the
	// bottom of the rail, collapsed by default. Its rows are instance rows like
	// the Instances section's (ItemIndex is a projection index), but flat — an
	// archived session has no live tabs and is not attachable. The header is
	// hidden entirely when no session is archived, so the folder only appears
	// once there is something in it.
	SectionArchived
)

// SidebarItem represents one visible row in the sidebar. Within the Instances
// section a non-header row is either an instance row or — the tree dimension
// added by #1024 PR 3 — one of the instance's tab children (IsTab set,
// TabIndex the 0-based tab slot). For tab rows ItemIndex still identifies the
// parent instance, so "which instance is selected" reads uniformly off any
// non-header row.
type SidebarItem struct {
	Kind      SidebarSectionKind
	IsHeader  bool
	ItemIndex int // index within the section's children (instances/tasks)
	IsTab     bool
	TabIndex  int // 0-based tab slot; meaningful only when IsTab
}

// SidebarSection holds state for one collapsible section.
type SidebarSection struct {
	Kind     SidebarSectionKind
	Title    string
	Expanded bool
}

// isInstanceRow reports whether item is a selectable instance row — a non-header
// row in either the Instances or Archived section (#1028). Both carry a
// projection ItemIndex; the difference is only which folder they render under.
func isInstanceRow(item SidebarItem) bool {
	return !item.IsHeader && (item.Kind == SectionInstances || item.Kind == SectionArchived)
}

// partitionByArchived splits projection indices into live and archived (#1028).
// Live rows keep the projection's stable order (root-first, then oldest-first by
// CreatedAt — see LessInstanceOrder). Archived rows are re-sorted NEWEST-created
// first (#1605): the archive folder reads as a most-recent-on-top history, the
// inverse of the live tree, rather than inheriting the oldest-first projection
// order. Only the archived group is reordered; the live partition is untouched.
// Title breaks a CreatedAt tie so the order is total and never jitters.
func partitionByArchived(instances []*session.Instance) (live, archived []int) {
	for i, inst := range instances {
		if inst != nil && inst.ShownArchived() {
			archived = append(archived, i)
		} else {
			live = append(live, i)
		}
	}
	sort.SliceStable(archived, func(a, b int) bool {
		ia, ib := instances[archived[a]], instances[archived[b]]
		if !ia.CreatedAt.Equal(ib.CreatedAt) {
			return ia.CreatedAt.After(ib.CreatedAt)
		}
		return ia.Title < ib.Title
	})
	return live, archived
}

var sectionHeaderStyle = lipgloss.NewStyle().
	Bold(true).
	Foreground(activeTheme.Foreground)

var sectionHeaderSelectedStyle = lipgloss.NewStyle().
	Bold(true).
	Background(activeTheme.SelectionBackground).
	Foreground(activeTheme.SelectionForeground)

var windowIndicatorStyle = lipgloss.NewStyle().
	Foreground(activeTheme.ForegroundMuted)

var mainTitle = lipgloss.NewStyle().
	Background(AccentColor).
	Foreground(activeTheme.Background)

// blurredTitle is the title chip with tree focus elsewhere (#1024 PR 4 focus
// ring): same shape, receded color.
var blurredTitle = lipgloss.NewStyle().
	Background(activeTheme.ForegroundDim).
	Foreground(activeTheme.ForegroundStrong)

var autoYesStyle = lipgloss.NewStyle().
	Background(activeTheme.SelectionBackground).
	Foreground(activeTheme.SelectionForeground)

// Sidebar is the unified left navigation pane with collapsible sections. It is
// a VIEW over the store.Projection (#1024 PR 2): the instance/task/hook data —
// and the cross-pane selection — live in the store, written by the snapshot
// reconcile and the session-control handlers. The sidebar owns only its local
// UI state: the flattened row list, the cursor over it, expansion state, and
// the scroll window.
//
// Since #1024 PR 3 the Instances section is a TREE: each instance row carries
// its tabs as expandable children (the same slots the tab bar shows, via
// tree.TabLabels), and the cursor can rest on an instance row OR a tab row.
// Expansion policy: the display-selected instance auto-expands and every other
// instance collapses, keeping the row count ≈ instances + selected-instance
// tabs; h/← collapses the selected instance explicitly (treeCollapsed) until
// l/→ re-expands it or the selection moves on. Landing the cursor on a tab row
// drives the store's active tab, which is what retargets the content pane.
type Sidebar struct {
	proj *store.Projection

	sections     []SidebarSection
	visibleItems []SidebarItem
	selectedIdx  int

	// seenSig is the structure signature (structureSig) visibleItems was last
	// derived from. The store is written directly by event-loop handlers
	// without notifying the sidebar, so every public method lazily re-derives
	// via syncFromStore before touching the row list. A plain store version is
	// no longer enough (pre-PR 3 seenVersion): the tree's row STRUCTURE also
	// depends on per-instance state mutated in place without a version bump —
	// the selected instance's tab slots (an out-of-band tab create/close
	// reconciles tabs onto the same pointer) and its transient-ness (a kill
	// flips it to Deleting, force-collapsing it) — so those inputs are folded
	// into the signature. seenSelSeq tracks the store's SelectInstance
	// assertions the cursor has honored.
	seenSig    string
	seenSelSeq uint64

	// treeCollapsed is the title of the display-selected instance whose tab
	// children the user explicitly collapsed (h/←). Cleared when the selection
	// moves to a different instance, so every newly selected instance starts
	// auto-expanded (collapse-by-default applies to non-selected instances).
	treeCollapsed string

	// lastCursorTitle/lastCursorTab remember the identity of the last
	// instance-section row the cursor rested on (tab -1 = the instance row
	// itself). The reconcile's #969 re-pin asserts only the instance; this memo
	// restores the tab SUB-selection too, so a snapshot arriving while the
	// cursor sits on a tab row doesn't yank it up to the instance row.
	lastCursorTitle string
	lastCursorTab   int

	// scrollOffset is the index into visibleItems of the first rendered row
	// when the list is too tall for the allocation. It is adjusted lazily in
	// String() (rebuilds can shift indices between renders), scrolling
	// minimally so the selected row stays visible.
	scrollOffset int

	// Rendering
	renderer *tree.InstanceRenderer
	autoyes  bool
	height   int
	width    int
	focused  bool

	// projectName is the active project's display name (repo basename), shown
	// in the title chip so the rail always names which project is in view after
	// an in-place project switch (#1461). Empty falls back to "Agent Factory".
	projectName string

	// rect is the pane's screen rect (SetRect); zone registration needs the
	// absolute origin, not just the size.
	rect layout.Rect
	// zones is the shared mouse hit-test registry (#1024 R4). String()
	// registers a rect per interactive row every frame; nil (ui unit tests
	// that never wire a registry) skips registration.
	zones *zones.Registry
}

// NewSidebar creates a new sidebar rendering the given projection.
func NewSidebar(autoYes bool, proj *store.Projection) *Sidebar {
	s := &Sidebar{
		proj: proj,
		sections: []SidebarSection{
			{Kind: SectionInstances, Title: "Instances", Expanded: true},
			// Archived (#1028): pinned last, collapsed by default. Rendered only
			// when it holds archived sessions (rebuildVisibleItems skips the empty
			// folder).
			{Kind: SectionArchived, Title: "Archived", Expanded: false},
		},
		renderer:      tree.NewInstanceRenderer(),
		autoyes:       autoYes,
		lastCursorTab: -1,
	}
	s.rebuildVisibleItems()
	s.seenSig = s.structureSig()
	return s
}

// SetSize sets the display dimensions.
func (s *Sidebar) SetSize(width, height int) {
	s.width = width
	s.height = height
	s.renderer.SetWidth(s.contentWidth())
}

// contentWidth is the effective row width inside the sidebar allocation: the
// full width minus the 2-cell row padding the row styles add. This replaces
// the pre-cutover AdjustPreviewWidth 0.9 buffer (#1024 PR 4) — the tree gets
// its whole rect.
func (s *Sidebar) contentWidth() int {
	w := s.width - 2
	if w < 0 {
		w = 0
	}
	return w
}

// SetRect implements layout.Pane.
func (s *Sidebar) SetRect(r layout.Rect) {
	s.rect = r
	s.SetSize(r.W, r.H)
}

// SetZoneRegistry wires the shared mouse hit-test registry (#1024 R4).
func (s *Sidebar) SetZoneRegistry(reg *zones.Registry) {
	s.zones = reg
}

// Focused implements layout.Pane.
func (s *Sidebar) Focused() bool { return s.focused }

// Focus implements layout.Pane.
func (s *Sidebar) Focus() { s.focused = true }

// Blur implements layout.Pane.
func (s *Sidebar) Blur() { s.focused = false }

// SetProjectName sets the active project's display name shown in the title chip
// (#1461). Empty restores the default "Agent Factory" label.
func (s *Sidebar) SetProjectName(name string) { s.projectName = name }

// HandleKey implements layout.Pane. Tree navigation stays routed through the
// root model's global bindings in PR 4 (the keys also work when the workspace
// pane has focus); per-pane routing arrives with the split (#1024 PR 5).
func (s *Sidebar) HandleKey(tea.KeyMsg) (tea.Cmd, bool) { return nil, false }

// HandleMouse implements layout.Pane. Mouse dispatch is zone-id-based at the
// root (#1024 R4): String() registers per-row zones and the root router
// calls the click primitives (SelectTabRow, ToggleInstanceTree, …) directly,
// so the pane-local fallback consumes nothing.
func (s *Sidebar) HandleMouse(tea.MouseMsg, layout.Point) tea.Cmd { return nil }

// View implements layout.Pane.
func (s *Sidebar) View() string { return s.String() }

// structureSig fingerprints every input the flattened row list is derived
// from: the store version (instance list, tasks, selection binding) plus the
// tree-structural per-instance state that mutates in place without a version
// bump — the selected instance's identity, tab-slot count, and expandability —
// and the explicit collapse override. See the seenSig field.
func (s *Sidebar) structureSig() string {
	selTitle := ""
	slots := 0
	expandable := false
	if inst := s.proj.GetSelectedInstance(); inst != nil {
		selTitle = inst.Title
		slots = len(tree.TabLabels(inst))
		expandable = tree.Expandable(inst)
	}
	// Fold in the archived partition (#1028): a status flip to/from Archived
	// changes which folder a row belongs to but does NOT bump the projection
	// Version (it mutates an existing instance in place), so without this the
	// sidebar would not re-partition until the next structural change. The
	// archived index set — stable within a projection order — uniquely
	// identifies the partition, so it catches both a single archive/restore and
	// a same-count swap.
	_, archived := partitionByArchived(s.proj.GetInstances())
	return fmt.Sprintf("%d|%s|%d|%t|%s|%v",
		s.proj.Version(), selTitle, slots, expandable, s.treeCollapsed, archived)
}

// instanceExpanded reports whether inst's tab children are currently shown:
// the display-selected instance auto-expands (matched by title so a #765
// same-title swap keeps the subtree open) unless it is transient or the user
// explicitly collapsed it. Everything else is collapsed — the keep-row-count-
// manageable default for non-selected instances.
func (s *Sidebar) instanceExpanded(inst *session.Instance) bool {
	sel := s.proj.GetSelectedInstance()
	if sel == nil || inst == nil || sel.Title != inst.Title {
		return false
	}
	return tree.Expandable(inst) && s.treeCollapsed != inst.Title
}

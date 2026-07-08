package app

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/store"
	"github.com/sachiniyer/agent-factory/ui/tree"
)

type paneBinding struct {
	instance *session.Instance
	tab      int
}

type panePreviewTxn struct {
	ownerPaneID int
	original    paneBinding
	target      paneBinding
	seq         uint64
}

type panePreviewSuppression struct {
	original paneBinding
	target   paneBinding
}

func samePaneBinding(a, b paneBinding) bool {
	return a.instance == b.instance && a.tab == b.tab
}

func (m *home) updatePanePreview(selected *session.Instance, targetTab int, tabSpecific bool, attachedNow bool) {
	if attachedNow || m.interactive || selected == nil {
		m.cancelPanePreview(false)
		return
	}
	owner := m.previewOwnerPane()
	if owner == nil {
		m.cancelPanePreview(false)
		return
	}
	if m.panePreviewTxn != nil && m.panePreviewTxn.ownerPaneID != owner.ID() {
		m.cancelPanePreview(false)
	}
	w := m.paneWindows[owner.ID()]
	if w == nil {
		m.cancelPanePreview(false)
		return
	}
	original := paneBinding{instance: owner.Instance(), tab: owner.Tab()}
	if m.panePreviewTxn != nil && m.panePreviewTxn.ownerPaneID == owner.ID() {
		original = m.panePreviewTxn.original
	}
	target := paneBinding{instance: selected, tab: targetTab}
	// The preview target must match the selected/action tab. An instance row
	// whose active tab is Terminal still names the full (instance, tab)
	// target, so preview and commit cannot diverge (#1415, #1289 class).
	if (!tabSpecific && original.instance == target.instance) ||
		(tabSpecific && samePaneBinding(original, target)) {
		m.cancelPanePreview(false)
		return
	}
	if m.isPanePreviewSuppressed(original, target) {
		m.cancelPanePreview(false)
		return
	}
	changed := m.panePreviewTxn == nil ||
		m.panePreviewTxn.ownerPaneID != owner.ID() ||
		!samePaneBinding(m.panePreviewTxn.original, original) ||
		!samePaneBinding(m.panePreviewTxn.target, target)

	if owner == m.livePane {
		m.closeLiveTermPane()
		m.enforceInteractiveInvariant()
	}

	seq := w.SetPreview(target.instance, target.tab, paneBindingLabel(original))
	m.panePreviewTxn = &panePreviewTxn{
		ownerPaneID: owner.ID(),
		original:    original,
		target:      target,
		seq:         seq,
	}
	if changed {
		w.InvalidateContent(target.instance, target.tab, "Loading preview...")
		m.lastPaneCapture[owner.ID()] = time.Time{}
	}
}

func (m *home) cancelPanePreview(focusOwner bool) {
	txn := m.panePreviewTxn
	if txn == nil {
		return
	}
	m.panePreviewTxn = nil
	if w := m.paneWindows[txn.ownerPaneID]; w != nil {
		w.ClearPreview()
		w.InvalidateContent(txn.original.instance, txn.original.tab, "Loading pane...")
		m.lastPaneCapture[txn.ownerPaneID] = time.Time{}
	}
	if focusOwner {
		if p := m.openPaneByID(txn.ownerPaneID); p != nil {
			m.focusRegion(layout.PaneRegion(p.ID()))
		}
	}
}

func (m *home) suppressActivePanePreview() {
	if txn := m.panePreviewTxn; txn != nil {
		m.suppressPanePreview(txn.original, txn.target)
	}
}

func (m *home) suppressPanePreviewForSelection(owner *store.OpenPane) {
	if owner == nil {
		return
	}
	selected := m.store.GetSelectedInstance()
	if selected == nil {
		return
	}
	m.suppressPanePreview(
		paneBinding{instance: owner.Instance(), tab: owner.Tab()},
		paneBinding{instance: selected, tab: m.store.ActiveTab()},
	)
}

func (m *home) suppressPanePreview(original, target paneBinding) {
	if original.instance == nil || target.instance == nil || samePaneBinding(original, target) {
		return
	}
	m.panePreviewSuppression = &panePreviewSuppression{
		original: original,
		target:   target,
	}
}

func (m *home) isPanePreviewSuppressed(original, target paneBinding) bool {
	suppressed := m.panePreviewSuppression
	if suppressed == nil {
		return false
	}
	if !samePaneBinding(suppressed.target, target) {
		m.panePreviewSuppression = nil
		return false
	}
	return samePaneBinding(suppressed.original, original)
}

func (m *home) commitPanePreviewReplace() tea.Cmd {
	txn := m.panePreviewTxn
	if txn == nil {
		return nil
	}
	if err := previewCommitError(txn.target.instance); err != nil {
		return m.handleError(err)
	}
	owner := m.openPaneByID(txn.ownerPaneID)
	if owner == nil {
		m.cancelPanePreview(false)
		return nil
	}
	if existing := m.store.FindOpenPane(txn.target.instance, txn.target.tab); existing != nil && existing != owner {
		m.cancelPanePreview(false)
		m.store.TouchOpenPane(existing)
		m.relayout()
		m.focusRegion(layout.PaneRegion(existing.ID()))
		return m.panesRefresh(m.attached.Load())
	}
	w := m.paneWindows[owner.ID()]
	m.panePreviewTxn = nil
	if w != nil {
		w.ClearPreview()
		w.InvalidateContent(txn.target.instance, txn.target.tab, "Loading pane...")
	}
	if !m.store.RebindOpenPane(owner, txn.target.instance, txn.target.tab) {
		return nil
	}
	m.store.TouchOpenPane(owner)
	m.lastPaneCapture[owner.ID()] = time.Time{}
	m.relayout()
	m.focusRegion(layout.PaneRegion(owner.ID()))
	return m.panesRefresh(m.attached.Load())
}

func (m *home) commitPanePreviewAlongside() tea.Cmd {
	txn := m.panePreviewTxn
	if txn == nil {
		return nil
	}
	if err := previewCommitError(txn.target.instance); err != nil {
		return m.handleError(err)
	}
	if m.openPaneByID(txn.ownerPaneID) == nil {
		m.cancelPanePreview(false)
		return nil
	}
	target := txn.target
	m.suppressActivePanePreview()
	m.cancelPanePreview(false)
	_, cmd := m.openOrFocusPane(target.instance, target.tab)
	return cmd
}

func previewCommitError(inst *session.Instance) error {
	if inst == nil {
		return fmt.Errorf("no preview to commit")
	}
	if inst.HasInFlightOp() {
		return fmt.Errorf("cannot commit preview for %q: session operation in flight", inst.Title)
	}
	switch inst.GetLiveness() {
	case session.LiveDead:
		return fmt.Errorf("cannot commit preview for %q: session no longer running", inst.Title)
	case session.LiveLost:
		return fmt.Errorf("cannot commit preview for %q: session was lost", inst.Title)
	}
	return nil
}

func (m *home) renderBindingForPane(p *store.OpenPane) (paneBinding, uint64) {
	w := m.paneWindows[p.ID()]
	if m.panePreviewTxn != nil && m.panePreviewTxn.ownerPaneID == p.ID() {
		return m.panePreviewTxn.target, m.panePreviewTxn.seq
	}
	if w == nil {
		return paneBinding{instance: p.Instance(), tab: p.Tab()}, 0
	}
	return paneBinding{instance: p.Instance(), tab: p.Tab()}, w.ContentSeq()
}

func (m *home) previewOwnerPane() *store.OpenPane {
	if p := m.visiblePaneByID(m.lastFocusedPaneID); p != nil {
		return p
	}
	if p := m.focusedOpenPane(); p != nil {
		return p
	}
	if len(m.visiblePanes) > 0 {
		return m.visiblePanes[0]
	}
	return nil
}

func (m *home) visiblePaneByID(id int) *store.OpenPane {
	if id == 0 {
		return nil
	}
	for _, p := range m.visiblePanes {
		if p.ID() == id {
			return p
		}
	}
	return nil
}

func (m *home) openPaneByID(id int) *store.OpenPane {
	if id == 0 {
		return nil
	}
	for _, p := range m.store.OpenPanes() {
		if p.ID() == id {
			return p
		}
	}
	return nil
}

func paneBindingLabel(binding paneBinding) string {
	if binding.instance == nil {
		return "no session"
	}
	label := ""
	labels := tree.TabLabels(binding.instance)
	if binding.tab >= 0 && binding.tab < len(labels) {
		label = labels[binding.tab]
	}
	if label == "" {
		return binding.instance.Title
	}
	return fmt.Sprintf("%s · %s", binding.instance.Title, label)
}

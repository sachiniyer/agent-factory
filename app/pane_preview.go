package app

import (
	"fmt"
	"time"

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

func samePaneBinding(a, b paneBinding) bool {
	return a.instance == b.instance && a.tab == b.tab
}

func (m *home) updatePanePreview(selected *session.Instance, attachedNow bool) {
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
	target := paneBinding{instance: selected, tab: 0}
	// #1321 is an instance preview, not a tab preview: selecting a different
	// tab row on the pane's original instance keeps the #1289 divergence header.
	if original.instance == target.instance {
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

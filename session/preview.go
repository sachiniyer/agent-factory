package session

import "fmt"

// PreviewTab captures the detached content of the tab currently at idx. The
// ordinal form exists for legacy callers that never supplied a stable tab id.
// An out-of-range ordinal is an explicit error: returning ("", nil) would claim
// that a nonexistent pane was merely blank (#2200).
func (i *Instance) PreviewTab(idx int) (string, error) {
	i.mu.RLock()
	if idx < 0 || idx >= len(i.Tabs) {
		i.mu.RUnlock()
		return "", fmt.Errorf("session %q tab %d: %w", i.Title, idx, ErrTabIndexOutOfRange)
	}
	started := i.started
	ts := i.tabTmuxAtLocked(idx)
	i.mu.RUnlock()
	if !started || ts == nil {
		return "", nil
	}
	return ts.CapturePaneContent()
}

// PreviewTabFullHistory is PreviewTab's full-scrollback counterpart. It keeps
// the same explicit out-of-range refusal.
func (i *Instance) PreviewTabFullHistory(idx int) (string, error) {
	i.mu.RLock()
	if idx < 0 || idx >= len(i.Tabs) {
		i.mu.RUnlock()
		return "", fmt.Errorf("session %q tab %d: %w", i.Title, idx, ErrTabIndexOutOfRange)
	}
	started := i.started
	ts := i.tabTmuxAtLocked(idx)
	i.mu.RUnlock()
	if !started || ts == nil {
		return "", nil
	}
	return ts.CapturePaneContentWithOptions("-", "-")
}

// PreviewTabByID captures the tab named by stable id without ever converting
// that identity into an ordinal used by a later live-list lookup. The instance
// lock resolves the id directly to the target tmux pointer and stays held through
// the bounded non-agent capture. A concurrent close/reorder therefore waits; it
// cannot redirect the capture to a sibling or a same-name replacement (#2200).
//
// The agent tab retains the backend-specific preview path. It is pinned at slot
// zero and cannot be closed or reordered, so snapshotting its backend under the
// same lock preserves both its identity and the formatting contract.
func (i *Instance) PreviewTabByID(tabID string, full bool) (string, error) {
	i.mu.RLock()
	idx, exists := i.tabIndexByIDLocked(tabID)
	if !exists {
		i.mu.RUnlock()
		return "", fmt.Errorf("session %q tab id %q: %w", i.Title, tabID, ErrTabGone)
	}
	tab := i.Tabs[idx]
	if !i.started {
		i.mu.RUnlock()
		return "", nil
	}
	if tab.Kind != TabKindAgent {
		// Keep the roster read lock through the bounded tmux capture. Besides
		// preventing an ordinal shift, this prevents a close+recreate from reusing
		// the old tmux name between target selection and capture.
		defer i.mu.RUnlock()
		ts := tab.tmux
		if ts == nil {
			return "", nil
		}
		if full {
			return ts.CapturePaneContentWithOptions("-", "-")
		}
		return ts.CapturePaneContent()
	}

	// Backend preview methods re-enter i.mu, so the pinned agent target snapshots
	// the backend under the lock and performs the call after releasing it.
	backend := i.backend
	i.mu.RUnlock()
	if backend == nil {
		return "", fmt.Errorf("session %q has no preview backend", i.Title)
	}
	if full {
		return backend.PreviewFullHistory(i)
	}
	return backend.Preview(i)
}

// previewByIDAsOrdinal is the compatibility bridge for a remote agent-server
// whose private wire protocol is ordinal-shaped. The daemon-side Instance owns
// the authoritative roster, so it holds that roster's read lock from stable-id
// resolution until the bounded remote capture returns. A close or reorder
// therefore cannot shift a new target under the ordinal in between (#2200).
//
// Local capture must not use this helper: its capture path re-enters i.mu. It has
// PreviewTabByID above, which selects the tmux pointer directly instead.
func (i *Instance) previewByIDAsOrdinal(tabID string, capture func(int) (string, error)) (string, error) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	idx, exists := i.tabIndexByIDLocked(tabID)
	if !exists {
		return "", fmt.Errorf("session %q tab id %q: %w", i.Title, tabID, ErrTabGone)
	}
	return capture(idx)
}

package session

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/terminal"
)

// PreviewSnapshot keeps a captured terminal grid and the ownership-affecting
// modes observed for that same target in one value. HasModes is explicit: the
// zero-value Modes is a valid primary-screen/no-mouse observation, not an
// invitation for a client to guess. Runtimes that cannot report modes leave it
// false and scrolling remains unavailable until an authoritative snapshot lands.
type PreviewSnapshot struct {
	Content  string
	Modes    terminal.Modes
	HasModes bool
}

func previewSnapshotWithModes(content string, ts *tmux.TmuxSession) PreviewSnapshot {
	snapshot := PreviewSnapshot{Content: content}
	if ts == nil {
		return snapshot
	}
	state, err := ts.ReadTerminalState()
	if err != nil {
		return snapshot
	}
	snapshot.Modes = state.Modes
	snapshot.HasModes = true
	return snapshot
}

// PreviewTab captures the detached content of the tab currently at idx. The
// ordinal form exists for legacy callers that never supplied a stable tab id.
// An out-of-range ordinal is an explicit error: returning ("", nil) would claim
// that a nonexistent pane was merely blank (#2200).
func (i *Instance) PreviewTab(idx int) (string, error) {
	snapshot, err := i.PreviewTabSnapshot(idx, false)
	return snapshot.Content, err
}

// PreviewTabSnapshot is PreviewTab with an authoritative terminal-mode
// observation when the selected runtime exposes a tmux pane.
func (i *Instance) PreviewTabSnapshot(idx int, full bool) (PreviewSnapshot, error) {
	i.mu.RLock()
	if idx < 0 || idx >= len(i.Tabs) {
		i.mu.RUnlock()
		return PreviewSnapshot{}, fmt.Errorf("session %q tab %d: %w", i.Title, idx, ErrTabIndexOutOfRange)
	}
	started := i.started
	ts := i.tabTmuxAtLocked(idx)
	i.mu.RUnlock()
	if !started || ts == nil {
		return PreviewSnapshot{}, nil
	}

	var content string
	var err error
	if full {
		content, err = ts.CapturePaneContentWithOptions("-", "-")
	} else {
		content, err = ts.CapturePaneContent()
	}
	if err != nil {
		return PreviewSnapshot{}, err
	}
	return previewSnapshotWithModes(content, ts), nil
}

// PreviewTabFullHistory is PreviewTab's full-scrollback counterpart. It keeps
// the same explicit out-of-range refusal.
func (i *Instance) PreviewTabFullHistory(idx int) (string, error) {
	snapshot, err := i.PreviewTabSnapshot(idx, true)
	return snapshot.Content, err
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
	snapshot, err := i.PreviewTabSnapshotByID(tabID, full)
	return snapshot.Content, err
}

// PreviewTabSnapshotByID binds content and terminal modes to one stable tab
// identity. A mode read can fail without losing a valid capture; HasModes=false
// makes that uncertainty explicit to routing clients.
func (i *Instance) PreviewTabSnapshotByID(tabID string, full bool) (PreviewSnapshot, error) {
	i.mu.RLock()
	idx, exists := i.tabIndexByIDLocked(tabID)
	if !exists {
		i.mu.RUnlock()
		return PreviewSnapshot{}, fmt.Errorf("session %q tab id %q: %w", i.Title, tabID, ErrTabGone)
	}
	tab := i.Tabs[idx]
	if !i.started {
		i.mu.RUnlock()
		return PreviewSnapshot{}, nil
	}
	if tab.Kind != TabKindAgent {
		// Keep the roster read lock through the bounded tmux capture. Besides
		// preventing an ordinal shift, this prevents a close+recreate from reusing
		// the old tmux name between target selection and capture.
		defer i.mu.RUnlock()
		ts := tab.tmux
		if ts == nil {
			return PreviewSnapshot{}, nil
		}
		var (
			content string
			err     error
		)
		if full {
			content, err = ts.CapturePaneContentWithOptions("-", "-")
		} else {
			content, err = ts.CapturePaneContent()
		}
		if err != nil {
			return PreviewSnapshot{}, err
		}
		return previewSnapshotWithModes(content, ts), nil
	}

	// Backend preview methods re-enter i.mu, so the pinned agent target snapshots
	// the backend under the lock and performs the call after releasing it.
	backend := i.backend
	ts := tab.tmux
	i.mu.RUnlock()
	if backend == nil {
		return PreviewSnapshot{}, fmt.Errorf("session %q has no preview backend", i.Title)
	}
	var (
		content string
		err     error
	)
	if full {
		content, err = backend.PreviewFullHistory(i)
	} else {
		content, err = backend.Preview(i)
	}
	if err != nil {
		return PreviewSnapshot{}, err
	}
	return previewSnapshotWithModes(content, ts), nil
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

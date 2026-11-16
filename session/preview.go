package session

import (
	"errors"
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
	// LinesAbove is how many scrollback lines sit ABOVE the captured region, and
	// LinesAboveKnown says whether anyone measured (#3169).
	//
	// Two fields rather than one, because 0 and "unmeasured" are different answers
	// and collapsing them is the bug being fixed: a visible-screen capture reported
	// as having nothing above it reads as COMPLETE. A remote sandbox does not carry
	// the count over its REST preview, so unknown is a real state, not a defensive
	// one — and it must render as "not measured", never as "nothing above".
	LinesAbove      int
	LinesAboveKnown bool
}

// captureVisibleWithCount captures the visible screen and, when it can, the lines
// above it in ONE tmux command, so a completeness claim is bound to the bytes it
// describes (#3169 review).
//
// ADDITIVE, with a fallback, and that is the correction to my first attempt: making
// the combined command shape MANDATORY broke every caller whose producer answers the
// plain capture shape — the TUI's tab panes and ordinal preview resolution share this
// path, and they lost scroll mode, the session-gone fallback and ordinal resolution
// because an unparseable answer failed the capture outright instead of degrading. A
// marker is an enhancement to what preview REPORTS; it must never become a new
// requirement on what preview can CAPTURE.
//
// The distinction that makes the fallback safe: only ErrScrollbackCaptureUnparseable
// degrades. A vanished session or a wedged server still propagates, because those are
// exactly the failures callers act on — the session-gone fallback depends on seeing
// them, and swallowing one to obtain a count would trade a real signal for a nicety.
//
// A degraded capture reports the count as UNKNOWN, which renders "not measured"
// rather than "nothing above" — so losing the count never fabricates completeness.
func captureVisibleWithCount(ts *tmux.TmuxSession, rows bool) (string, int, bool, error) {
	capture, plainCapture := ts.CaptureVisibleWithScrollback, ts.CapturePaneContent
	if rows {
		capture, plainCapture = ts.CaptureVisibleRowsWithScrollback, ts.CaptureVisiblePaneGrid
	}
	content, size, err := capture()
	switch {
	case err == nil:
		return content, size, true, nil
	case !errors.Is(err, tmux.ErrScrollbackCaptureUnparseable):
		// A real failure: gone, wedged, or otherwise. Surface it unchanged.
		return "", 0, false, err
	}
	// This producer does not answer the combined shape. Capture exactly as before and
	// leave the count unknown.
	plain, plainErr := plainCapture()
	if plainErr != nil {
		return "", 0, false, plainErr
	}
	return plain, 0, false, nil
}

// captureFullHistory captures the whole scrollback, as drawn rows when rows is
// set (#7406).
func captureFullHistory(ts *tmux.TmuxSession, rows bool) (string, error) {
	if rows {
		return ts.CapturePaneRowsWithOptions("-", "-")
	}
	return ts.CapturePaneContentWithOptions("-", "-")
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

// PreviewTabSnapshot captures the detached content of the tab currently at idx
// along with an authoritative terminal-mode observation when the selected runtime
// exposes a tmux pane.
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

	if !full {
		content, above, known, err := captureVisibleWithCount(ts, false)
		if err != nil {
			return PreviewSnapshot{}, err
		}
		snapshot := previewSnapshotWithModes(content, ts)
		snapshot.LinesAbove, snapshot.LinesAboveKnown = above, known
		return snapshot, nil
	}
	content, err := ts.CapturePaneContentWithOptions("-", "-")
	if err != nil {
		return PreviewSnapshot{}, err
	}
	return previewSnapshotWithModes(content, ts), nil
}

// PreviewTabSnapshotByID binds content and terminal modes to one stable tab
// identity. A mode read can fail without losing a valid capture; HasModes=false
// makes that uncertainty explicit to routing clients.
func (i *Instance) PreviewTabSnapshotByID(tabID string, full bool) (PreviewSnapshot, error) {
	return i.previewTabSnapshotByID(tabID, full, false)
}

// PreviewTabRowsSnapshotByID is PreviewTabSnapshotByID for a renderer: a local
// pane is captured as the rows it draws, never joining a soft-wrapped row onto
// the row below it (#7406). Detectors and `af sessions preview` keep the joined
// capture, where a match must not break at a wrap. An off-box sandbox's content
// arrives over REST already joined, as before.
func (i *Instance) PreviewTabRowsSnapshotByID(tabID string, full bool) (PreviewSnapshot, error) {
	return i.previewTabSnapshotByID(tabID, full, true)
}

func (i *Instance) previewTabSnapshotByID(tabID string, full, rows bool) (PreviewSnapshot, error) {
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
		// Read the floor BEFORE the capture, so a clear-history afterwards cannot turn
		// this partial capture into a measured zero (#3169 review).
		if !full {
			content, above, known, err := captureVisibleWithCount(ts, rows)
			if err != nil {
				return PreviewSnapshot{}, err
			}
			snapshot := previewSnapshotWithModes(content, ts)
			snapshot.LinesAbove, snapshot.LinesAboveKnown = above, known
			return snapshot, nil
		}
		content, err := captureFullHistory(ts, rows)
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
	// A LOCAL visible capture goes through the atomic tmux command so its
	// completeness claim describes the bytes returned (#3169 review).
	// LocalBackend.Preview is exactly ts.CapturePaneContent() with the same flags, so
	// the content is unchanged; what is added is the count, from the same invocation.
	//
	// ts == nil is an off-box sandbox, whose content arrives over REST and whose count
	// nothing measures — so it stays unknown, which renders as "not measured". That is
	// the honest answer, and it is why unknown is a real state rather than a default.
	if !full && ts != nil {
		content, above, known, err := captureVisibleWithCount(ts, rows)
		if err != nil {
			return PreviewSnapshot{}, err
		}
		snapshot := previewSnapshotWithModes(content, ts)
		snapshot.LinesAbove, snapshot.LinesAboveKnown = above, known
		return snapshot, nil
	}
	var (
		content string
		err     error
	)
	switch {
	case full && rows && ts != nil:
		// LocalBackend.PreviewFullHistory is this capture joined.
		content, err = captureFullHistory(ts, rows)
	case full:
		content, err = backend.PreviewFullHistory(i)
	default:
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
// PreviewTabSnapshotByID above, which selects the tmux pointer directly instead.
func (i *Instance) previewByIDAsOrdinal(tabID string, capture func(int) (string, error)) (string, error) {
	// The roster lock is NOT held across the capture: a REST capture against a
	// wedged host can take its whole budget, and an RLock held that long queues
	// every writer (and, through Go's writer preference, every reader) behind it
	// (W4.1). Instead the ordinal is resolved before and re-verified after, and a
	// capture that raced a close or reorder is discarded and retried.
	for attempt := 0; attempt < previewOrdinalAttempts; attempt++ {
		i.mu.RLock()
		idx, exists := i.tabIndexByIDLocked(tabID)
		i.mu.RUnlock()
		if !exists {
			return "", fmt.Errorf("session %q tab id %q: %w", i.Title, tabID, ErrTabGone)
		}
		content, err := capture(idx)
		i.mu.RLock()
		now, stillThere := i.tabIndexByIDLocked(tabID)
		i.mu.RUnlock()
		if !stillThere {
			return "", fmt.Errorf("session %q tab id %q: %w", i.Title, tabID, ErrTabGone)
		}
		if now == idx {
			return content, err
		}
	}
	return "", fmt.Errorf("session %q tab id %q: roster kept changing during capture: %w", i.Title, tabID, ErrTabGone)
}

// previewOrdinalAttempts bounds the resolve-capture-verify retry above.
const previewOrdinalAttempts = 3

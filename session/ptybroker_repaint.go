package session

// The capture → repaint family for the WS PTY broker: what a clientless
// channel's Snapshot reports (PaneSnapshot), how the broker turns it into
// repaint bytes (buildRepaint/buildRepaintSnapshot), when a snapshot is worth
// repainting at all (snapshotHasRepaintState), and how its measured pane
// dimensions become the broker's authoritative size (adoptSnapshotSize).
// Split out of ptybroker.go when it crossed the file-length lint (#1145) —
// same package, so nothing here is a new boundary.

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/terminal"
)

// PaneSnapshot is a fresh-subscriber repaint source: the pane's current visible
// screen (with escapes) plus the pane cursor position. CursorRow/CursorCol are
// 0-based; they are meaningful only when HasCursor is true.
//
// Screen MUST be GRID-form — one line per PHYSICAL pane row, NOT -J-joined logical
// lines. buildRepaint places each line at its own absolute row, so line index i is
// taken to be pane row i; feeding it -J-joined lines (where one logical line spans
// several pane rows) would mis-map the rows. The local tmux channel captures grid
// form (CaptureVisiblePaneGrid). The remote channel's REST preview is -J-joined and
// carries no cursor (HasCursor=false) — a known screen-only best-effort limitation,
// see remoteClientlessChannel.Snapshot.
type PaneSnapshot struct {
	Screen    []byte
	CursorRow int
	CursorCol int
	HasCursor bool
	// Modes are the ownership-affecting terminal modes that were already active
	// before this subscriber existed. HasModes distinguishes a truthful all-off
	// primary-screen snapshot from a source that cannot report modes.
	Modes    terminal.Modes
	HasModes bool
	// Rows/Cols are the pane's REAL dimensions measured by the capture — the one
	// place a pane nobody ever drove exposes its geometry: tmux spawns it at the
	// server's default-size (80x24 stock, but user-configurable — e.g. 200x60),
	// and with no driving surface no RESIZE frame ever reports the truth
	// (#4480). HasSize distinguishes a measured pane from a source that cannot
	// report dimensions (the remote REST preview), which leaves them zero; when
	// set, both fit a uint16.
	Rows    uint16
	Cols    uint16
	HasSize bool
}

// repaintSnapshot is one atomic broker event: the grid repaint plus the terminal
// modes captured with it. The daemon emits the modes immediately before Data,
// and Data also restores them as DEC sequences for terminal-only clients.
type repaintSnapshot struct {
	data       []byte
	modes      terminal.Modes
	hasModes   bool
	provenance PTYRepaintProvenance
}

// adoptSnapshotSize records the pane's measured dimensions as the broker's
// authoritative size. The capture path is the ONLY way a pane nobody ever drove
// gets its geometry known: tmux spawns it at the server's default-size (80x24
// stock, but user-configurable), and with no driving surface no RESIZE frame
// ever teaches subscribers the truth — the snapshot's measurement is the only
// place it exists (#4480). genBefore is resizeGen sampled before the Snapshot
// exec began; a resize frame that landed while tmux was being queried is newer
// authority than dims captured before it, so that stale observation is dropped.
// Adopting never resizes the pane — the broker's belief is catching up to tmux,
// not another surface driving it.
func (b *ptyBroker) adoptSnapshotSize(snap PaneSnapshot, genBefore uint64) {
	if !snap.HasSize {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.resizeGen != genBefore {
		return
	}
	if b.hasSize && b.rows == snap.Rows && b.cols == snap.Cols {
		return
	}
	b.rows, b.cols = snap.Rows, snap.Cols
	b.hasSize = true
	b.resizeGen++
	b.wakeAllLocked()
}

// buildRepaint turns a GRID-form pane snapshot (see PaneSnapshot) into bytes that
// reconstruct the screen when written to the emulator: clear the screen, then place
// each captured row at its OWN absolute line — CSI row;1 H, erase-to-EOL, then the
// row's content — and finally restore the cursor to the pane's real position (1-based
// CSI H) when the snapshot carries one.
//
// The explicit per-row positioning is the #1688 fix. The old form wrote the whole
// screen as one CRLF-joined blob and let the emulator RE-WRAP it by the emulator's
// OWN width, then issued an absolute cursor move to the pane's cursor_y. That is only
// correct when the client width == pane width: under a mismatch (multi-writer
// last-resize-wins — e.g. a browser subscriber opening at a different size than the
// pane) the re-wrap shifts the rows, so the absolute cursor row named the wrong line
// and Claude's relative-cursor status-block redraw corrupted the frame. Pinning each
// pane row at its own absolute line decouples the layout from the emulator's width:
// row i lands on line i whether the emulator is wider or narrower than the pane, so
// cursor_y names the same row it named in the pane. A row that overflows a narrower
// emulator wraps, but the next row's absolute CSI H + erase overwrites the overflow,
// so rows never accumulate a drift — correct by construction at any width.
//
// The cursor restore also fixes the earlier duplicated-prompt artifact (#1676):
// writing the screen leaves the emulator cursor at the bottom (past the trailing
// blank rows), but the pane program's cursor is wherever it really is (row 0 for a
// just-started shell). Without the restore, the pane's next relative-positioned
// redraw (a shell's SIGWINCH prompt redraw, which uses CR to return to the current
// line) renders at the bottom while a stale copy sits at the top. The restore lands
// the emulator cursor on the real position so that redraw overwrites in place.
func buildRepaint(snap PaneSnapshot) []byte {
	var out []byte
	if snap.HasModes {
		out = append(out, snap.Modes.RestoreSequence()...)
	}
	out = append(out, []byte("\x1b[2J")...)
	// capture-pane emits ONE trailing "\n" after the last row and strips trailing
	// blank rows, so that final "\n" is a row SEPARATOR, not a real empty row.
	// Splitting without trimming it would yield a phantom trailing "" element and emit
	// an out-of-range CSI (N+1);1 H + erase — which, in an emulator clamped to the pane
	// height, clamps onto the real bottom row and WIPES it (Claude's input/status
	// line). Trim exactly that one separator; a genuinely-blank last row is impossible
	// here because capture-pane strips it. TrimSuffix is a no-op when there is none.
	screen := strings.TrimSuffix(string(snap.Screen), "\n")
	for i, line := range strings.Split(screen, "\n") {
		out = append(out, []byte(fmt.Sprintf("\x1b[%d;1H\x1b[K", i+1))...)
		out = append(out, line...)
	}
	if snap.HasCursor {
		out = append(out, []byte(fmt.Sprintf("\x1b[%d;%dH", snap.CursorRow+1, snap.CursorCol+1))...)
	}
	return out
}

func buildRepaintSnapshot(snap PaneSnapshot) repaintSnapshot {
	return repaintSnapshot{
		data:     buildRepaint(snap),
		modes:    snap.Modes,
		hasModes: snap.HasModes,
	}
}

// snapshotHasRepaintState keeps authoritative metadata from disappearing merely
// because the grid is blank. A fresh primary-screen pane can have no printable
// cells while its all-false mode snapshot is exactly what resolves ownership.
func snapshotHasRepaintState(snap PaneSnapshot) bool {
	return len(snap.Screen) > 0 || snap.HasCursor || snap.HasModes
}

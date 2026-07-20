package ui

import "github.com/charmbracelet/bubbles/viewport"

// ScrollOwner identifies the subsystem that can truthfully satisfy a scroll
// request for a pane. HostHistory is the captured-preview implementation; the
// other values define the routing seam for child-owned history and surfaces
// that cannot scroll.
type ScrollOwner uint8

const (
	ScrollOwnerNone ScrollOwner = iota
	ScrollOwnerHostHistory
	ScrollOwnerChildApplication
)

// ScrollIntent is a semantic vertical displacement. Negative lines move toward
// older content; positive lines move toward newer content. Mouse and keyboard
// input are normalized to this type before the controller sees them.
type ScrollIntent struct {
	Lines int
}

var (
	scrollOneLineUp   = ScrollIntent{Lines: -1}
	scrollOneLineDown = ScrollIntent{Lines: 1}
)

// ScrollController is the pane-level ownership and transition contract. It is
// deliberately independent of keys and mouse events: callers submit semantic
// intents, and the implementation decides whether to apply immediately or hold
// them across an asynchronous transition.
//
// Methods are called while TabPane.mu is held; implementations do not lock.
type ScrollController interface {
	Owner() ScrollOwner
	Active() bool
	Scroll(*viewport.Model, ScrollIntent)
	Resize(*viewport.Model, int, int)
	Reset(*viewport.Model)
}

// historyScrollController extends the owner contract with the asynchronous
// fill lifecycle needed by tmux/host scrollback. Keeping this behind the base
// interface leaves child-application implementations free of capture phases.
type historyScrollController interface {
	ScrollController
	AwaitingHistory() bool
	NeedsFill(viewportHeight int) bool
	BeginFill()
	FillGeneration() uint64
	FillIsCurrent(uint64) bool
	RearmFill()
	CompleteFill(uint64, *viewport.Model, string) bool
}

type historyScrollPhase uint8

const (
	historyScrollIdle historyScrollPhase = iota
	historyScrollLoading
	historyScrollReady
)

// hostHistoryScrollController owns all state that used to be distributed over
// TabPane's isScrolling/fill-pending/generation booleans. phase is the single
// state-machine axis. pendingIntents is intentionally retained while capture
// runs, so input latency cannot erase or reorder user intent (#2192).
type hostHistoryScrollController struct {
	phase          historyScrollPhase
	pendingIntents []ScrollIntent
	fillGen        uint64
	dispatchedGen  uint64
}

var _ historyScrollController = (*hostHistoryScrollController)(nil)

func newHostHistoryScrollController() historyScrollController {
	return &hostHistoryScrollController{}
}

func (*hostHistoryScrollController) Owner() ScrollOwner {
	return ScrollOwnerHostHistory
}

func (c *hostHistoryScrollController) Active() bool {
	return c.phase != historyScrollIdle
}

func (c *hostHistoryScrollController) AwaitingHistory() bool {
	return c.phase == historyScrollLoading
}

func (c *hostHistoryScrollController) NeedsFill(viewportHeight int) bool {
	return c.phase == historyScrollLoading && viewportHeight > 0 &&
		c.dispatchedGen != c.fillGen
}

func (c *hostHistoryScrollController) BeginFill() {
	if c.phase == historyScrollLoading {
		c.dispatchedGen = c.fillGen
	}
}

func (c *hostHistoryScrollController) FillGeneration() uint64 {
	return c.fillGen
}

func (c *hostHistoryScrollController) FillIsCurrent(gen uint64) bool {
	return c.phase == historyScrollLoading && c.fillGen == gen
}

// Scroll either starts host-history acquisition, queues intent while that
// acquisition is pending, or applies intent to the ready viewport. The first
// request is recorded before the viewport is emptied: entering the mode is not
// a substitute for performing the requested scroll.
func (c *hostHistoryScrollController) Scroll(v *viewport.Model, intent ScrollIntent) {
	if intent.Lines == 0 {
		return
	}
	switch c.phase {
	case historyScrollIdle:
		v.SetContent("")
		v.GotoTop()
		c.phase = historyScrollLoading
		c.pendingIntents = append(c.pendingIntents, intent)
		c.fillGen++
	case historyScrollLoading:
		c.pendingIntents = append(c.pendingIntents, intent)
	case historyScrollReady:
		applyScrollIntent(v, intent)
	}
}

// CompleteFill publishes only the capture generation that is still owed. The
// target offset is derived in one transition: seed the viewport's newest valid
// offset, then apply every intent accumulated during the capture. There is no
// final unconditional GotoBottom that can overwrite those requests (#2192).
func (c *hostHistoryScrollController) CompleteFill(
	gen uint64,
	v *viewport.Model,
	content string,
) bool {
	if !c.FillIsCurrent(gen) {
		return false
	}
	v.SetContent(content)
	// Seed from the ready content and current geometry, then replay in order so
	// boundary clamping is identical to input received after the fill. In
	// particular, down-at-bottom followed by up must still move up.
	v.SetYOffset(viewportBottomOffset(v))
	for _, intent := range c.pendingIntents {
		applyScrollIntent(v, intent)
	}
	c.pendingIntents = nil
	c.phase = historyScrollReady
	c.fillGen++
	return true
}

// Resize keeps a ready history viewport at the same distance from its newest
// row. Loading uses the eventual geometry when CompleteFill lands; idle has no
// scroll position to preserve.
func (c *hostHistoryScrollController) Resize(v *viewport.Model, width, height int) {
	distanceFromBottom := 0
	if c.phase == historyScrollReady {
		distanceFromBottom = max(0, viewportBottomOffset(v)-v.YOffset)
	}
	v.Width = width
	v.Height = height
	if c.phase == historyScrollReady {
		v.SetYOffset(viewportBottomOffset(v) - distanceFromBottom)
	}
}

// RearmFill preserves pending intent but gives the retry a fresh generation.
// A capture that could not publish must not either lose input or leave the pane
// permanently masked as in-flight (#1709).
func (c *hostHistoryScrollController) RearmFill() {
	if c.phase == historyScrollLoading {
		c.fillGen++
	}
}

func (c *hostHistoryScrollController) Reset(v *viewport.Model) {
	c.phase = historyScrollIdle
	c.pendingIntents = nil
	c.fillGen++
	v.SetContent("")
	v.GotoTop()
}

func applyScrollIntent(v *viewport.Model, intent ScrollIntent) {
	switch {
	case intent.Lines < 0:
		v.LineUp(-intent.Lines)
	case intent.Lines > 0:
		v.LineDown(intent.Lines)
	}
}

func viewportBottomOffset(v *viewport.Model) int {
	return max(0, v.TotalLineCount()-v.Height)
}

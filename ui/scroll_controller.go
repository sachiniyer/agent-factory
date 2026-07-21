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
	MarkFillDispatched()
	ClaimFill() (scrollFillToken, bool)
	FillIsCurrent(scrollFillToken) bool
	RearmFill(scrollFillToken)
	ResolveHost()
	CompleteFill(scrollFillToken, *viewport.Model, string) bool
}

// scrollFillToken is the controller-issued capability for one asynchronous
// history capture. A generation number alone could order completions but could
// not stop periodic refreshes from launching duplicate full captures. ClaimFill
// makes that wrong state unrepresentable: only one caller can hold the current
// lifecycle's token.
type scrollFillToken struct {
	generation uint64
}

type historyScrollPhase uint8

const (
	historyScrollIdle historyScrollPhase = iota
	historyScrollLoading
	historyScrollReady
)

// captureHistoryScrollController owns all state that used to be distributed
// over TabPane's isScrolling/fill-pending/generation booleans. It begins either
// as an ownership probe or as established HostHistory; ResolveHost promotes a
// probe without replacing it, which preserves intent queued while terminal
// modes were in flight. phase is the single state-machine axis.
type captureHistoryScrollController struct {
	owner          ScrollOwner
	phase          historyScrollPhase
	pendingIntents []ScrollIntent
	fillGen        uint64
	dispatchedGen  uint64
	fillInFlight   bool
}

var _ historyScrollController = (*captureHistoryScrollController)(nil)

func newHostHistoryScrollController() historyScrollController {
	return &captureHistoryScrollController{owner: ScrollOwnerHostHistory}
}

// newOwnershipProbeScrollController preserves input for a capture-backed target
// whose terminal modes have not arrived yet. Its full capture resolves the same
// controller to HostHistory, or TabPane replaces it with a non-host owner without
// ever painting the captured buffer.
func newOwnershipProbeScrollController() historyScrollController {
	return &captureHistoryScrollController{owner: ScrollOwnerNone}
}

// inactiveScrollController is the shared no-host-capture behavior for the two
// non-host owners. Concrete owner types embed it so no constructor can pair this
// behavior with HostHistory or an invalid enum value.
type inactiveScrollController struct{}

func (*inactiveScrollController) Active() bool { return false }
func (*inactiveScrollController) Scroll(*viewport.Model, ScrollIntent) {
}
func (*inactiveScrollController) Resize(v *viewport.Model, width, height int) {
	v.Width = width
	v.Height = height
}
func (*inactiveScrollController) Reset(v *viewport.Model) {
	v.SetContent("")
	v.GotoTop()
}

// childApplicationScrollController prevents AF from entering capture history
// while a fullscreen application owns the conversation. Input routing can still
// forward a tracked wheel directly to the child.
type childApplicationScrollController struct{ inactiveScrollController }

func (*childApplicationScrollController) Owner() ScrollOwner {
	return ScrollOwnerChildApplication
}

// unavailableScrollController is used by fresh live streams while they wait for
// an authoritative repaint, and by surfaces with no truthful implementation.
// Capture-backed previews use captureHistoryScrollController while resolving so
// an initiating gesture can survive the asynchronous ownership snapshot.
type unavailableScrollController struct{ inactiveScrollController }

func (*unavailableScrollController) Owner() ScrollOwner { return ScrollOwnerNone }

var (
	_ ScrollController = (*childApplicationScrollController)(nil)
	_ ScrollController = (*unavailableScrollController)(nil)
)

func newChildApplicationScrollController() ScrollController {
	return &childApplicationScrollController{}
}

func newUnavailableScrollController() ScrollController {
	return &unavailableScrollController{}
}

func (c *captureHistoryScrollController) Owner() ScrollOwner {
	return c.owner
}

func (c *captureHistoryScrollController) Active() bool {
	return c.phase != historyScrollIdle
}

func (c *captureHistoryScrollController) AwaitingHistory() bool {
	return c.phase == historyScrollLoading
}

func (c *captureHistoryScrollController) NeedsFill(viewportHeight int) bool {
	return c.phase == historyScrollLoading && viewportHeight > 0 &&
		c.dispatchedGen != c.fillGen
}

func (c *captureHistoryScrollController) MarkFillDispatched() {
	if c.phase == historyScrollLoading {
		c.dispatchedGen = c.fillGen
	}
}

func (c *captureHistoryScrollController) ClaimFill() (scrollFillToken, bool) {
	if c.phase != historyScrollLoading || c.fillInFlight {
		return scrollFillToken{}, false
	}
	c.dispatchedGen = c.fillGen
	c.fillInFlight = true
	return scrollFillToken{generation: c.fillGen}, true
}

func (c *captureHistoryScrollController) FillIsCurrent(token scrollFillToken) bool {
	return c.phase == historyScrollLoading && c.fillInFlight &&
		c.fillGen == token.generation
}

// Scroll either starts host-history acquisition, queues intent while that
// acquisition is pending, or applies intent to the ready viewport. The first
// request is recorded before the viewport is emptied: entering the mode is not
// a substitute for performing the requested scroll.
func (c *captureHistoryScrollController) Scroll(v *viewport.Model, intent ScrollIntent) {
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
		c.fillInFlight = false
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
func (c *captureHistoryScrollController) CompleteFill(
	token scrollFillToken,
	v *viewport.Model,
	content string,
) bool {
	if !c.FillIsCurrent(token) {
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
	c.fillInFlight = false
	return true
}

// Resize keeps a ready history viewport at the same distance from its newest
// row. Loading uses the eventual geometry when CompleteFill lands; idle has no
// scroll position to preserve.
func (c *captureHistoryScrollController) Resize(v *viewport.Model, width, height int) {
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
func (c *captureHistoryScrollController) RearmFill(token scrollFillToken) {
	if c.FillIsCurrent(token) {
		c.fillGen++
		c.fillInFlight = false
	}
}

func (c *captureHistoryScrollController) ResolveHost() {
	c.owner = ScrollOwnerHostHistory
}

func (c *captureHistoryScrollController) Reset(v *viewport.Model) {
	c.phase = historyScrollIdle
	c.pendingIntents = nil
	c.fillGen++
	c.fillInFlight = false
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

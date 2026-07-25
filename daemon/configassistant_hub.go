package daemon

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// The web config-assistant stream owner (#2467). The config assistant is a bare
// tmux session with no Instance (configagent.go), so it is reachable through
// neither the session roster nor the m.instances-keyed PTY stream route. This hub
// is the parallel owner for the web surface: it spawns ONE shared assistant on
// demand, streams it to N browser tabs through a session.BareSessionStreamer, and
// reaps it when nobody has been attached for a grace window.
//
// Why a hub and not "one agent per socket": the web terminal reconnects with
// exponential backoff (web/src/terminal.ts, 500ms→10s), so a single browser tab
// drops and re-dials its socket on every transient blip. Tying the assistant's
// life to one socket would kill it on the first dropped keepalive; and spawning a
// second assistant per tab would burn a second agent giving conflicting config
// advice. So the assistant is shared and multi-writer (the broker already fans a
// pane to N subscribers), and its lifetime is decoupled from any one socket:
//
//   - POST spawns-or-reuses; a POST during the grace window keeps the assistant warm.
//   - the stream route subscribes; a new subscriber cancels a pending idle reap.
//   - DELETE reaps immediately (the web analog of the TUI's handleConfigAgentDone).
//   - when the LAST subscriber leaves, a grace timer arms; if it expires with still
//     nobody attached, the assistant is reaped. The window absorbs the reconnect
//     ladder so a flapping tab does not reap the agent out from under itself.
//
// A normal session's broker reaps its CAPTURE on last-detach but never the SESSION
// (a user-owned session persists); a config assistant is ephemeral and costs a
// tmux process plus a live agent that may be burning API tokens, so this hub adds
// the session-level idle reaper the normal path deliberately omits.

// configAssistantGraceWindow is how long the assistant stays warm after its last
// subscriber leaves before the idle reaper tears it down. Long enough to ride the
// web client's 500ms→10s reconnect ladder across a transient drop, short enough
// that a chat abandoned by closing the tab does not linger burning tokens. A field
// on the hub (not a const) so tests can shrink it.
const configAssistantGraceWindow = 60 * time.Second

// errNoConfigAssistant is the stream route's "nothing to attach to" answer: no
// assistant has been spawned, or it was reaped. It maps to a 404 so the browser
// terminal settles on MsgExit and stops reconnecting rather than looping against a
// session that does not exist.
var errNoConfigAssistant = errors.New("no config assistant is running")

// errConfigAssistantUnavailable is returned when the request builder was never
// wired (a build with no config-assistant support, or the daemon launched without
// the commands/ injection). Distinct from a spawn failure: the feature is absent,
// not broken.
var errConfigAssistantUnavailable = errors.New("config assistant is not available in this build")

// errConfigAssistantSpawnAborted is returned by ensure() (the POST path) when a
// create was aborted BEFORE it could be stored — a concurrent DELETE cancelled the
// cold spawn, or the just-spawned session vanished before it could be wrapped. It is
// deliberately DISTINCT from errNoConfigAssistant: a POST that raced a DELETE is
// RETRYABLE (a second POST would succeed), so it maps to a 409, whereas
// errNoConfigAssistant is the STREAM route's settle-and-stop 404. Conflating them
// (the pre-fix behavior) made a POST that lost the spawn race answer 404, which a
// client following the documented 404-means-stop contract reads as "give up" even
// though retrying would work (#2467 review round 2).
var errConfigAssistantSpawnAborted = errors.New("config assistant creation was aborted; retry")

// configAssistantRequestBuilder builds the spawn request (resolved program +
// briefing) for the web config assistant. It is INJECTED at daemon startup from
// commands/ (SetConfigAssistantRequestBuilder), because the briefing lives in the
// configagent package, which imports this one — the daemon cannot import it back.
// The browser sends nothing that becomes a command: the daemon builds the whole
// request from its own config, so an authenticated POST cannot smuggle an
// arbitrary program in (the route names no session and carries no body). nil until
// wired, in which case a POST is errConfigAssistantUnavailable, never a spawn with
// no briefing.
var configAssistantRequestBuilder func() (SpawnConfigAgentRequest, error)

// SetConfigAssistantRequestBuilder wires the injected briefing/program builder.
// Called once, before RunDaemon, from the daemon-launch command.
func SetConfigAssistantRequestBuilder(f func() (SpawnConfigAgentRequest, error)) {
	configAssistantRequestBuilder = f
}

// bareStreamer is the hub's view of a bare-session PTY stream — the subset of
// session.BareSessionStreamer the hub drives. An interface so the hub's whole
// lifecycle (reuse, subscriber counting, grace reap, #1632 refusal) is testable
// with a fake streamer and no tmux.
type bareStreamer interface {
	Subscribe(since session.Seq) (session.PTYSubscription, error)
	Input(b []byte) error
	Resize(rows, cols uint16) error
	Close()
}

// configAssistant is one live shared assistant: its tmux session name (for reap)
// and the streamer fanning its pane to subscribers.
type configAssistant struct {
	name     string
	streamer bareStreamer
}

// configAssistantHub owns the single shared assistant. See the file comment.
type configAssistantHub struct {
	graceWindow time.Duration

	// spawn creates a fresh shared assistant, blocking through readiness +
	// trust-dismissal + briefing (up to ~60s). reapFn tears one down by name.
	// Injected in tests; wired to the manager in newConfigAssistantHub.
	spawn  func(ctx context.Context) (name string, streamer bareStreamer, err error)
	reapFn func(name string) error

	// spawnMu serializes spawn-or-reuse so a burst of POSTs produces ONE assistant,
	// the rest reusing it. Held across the ~60s spawn; state mutations take mu, not
	// spawnMu, so a concurrent stream/DELETE is never blocked by an in-flight spawn.
	spawnMu sync.Mutex

	mu      sync.Mutex
	current *configAssistant
	active  int
	grace   *time.Timer
	// gen invalidates a grace timer whose fire escaped Stop: every arm/reset bumps
	// it, and onGraceExpired ignores a fire whose captured gen is stale.
	gen uint64
	// reapEpoch is bumped by every reap()/stop(). ensure() captures it before a cold
	// spawn and re-checks after: a DELETE that lands mid-spawn (while current is still
	// nil, so reap() sees nothing to tear down) is not silently lost — ensure tears
	// down what it spawned instead of storing an assistant the user asked to remove.
	reapEpoch uint64
	// spawnCancel cancels an in-flight cold spawn so a DELETE mid-spawn aborts the
	// ~60s wait rather than blocking on it; nil when no spawn is in flight. The
	// reapEpoch re-check is the correctness guarantee even when a spawn finished
	// before the cancel could land; this only makes the abort prompt.
	spawnCancel context.CancelFunc
	// afterSpawnHook fires in ensure() after a spawn returns success and BEFORE the
	// reapEpoch re-check. No-op in production; a test injects a reap() here to pin the
	// spawn-done-but-not-yet-stored race deterministically.
	afterSpawnHook func()
}

// newConfigAssistantHub wires the hub to the manager: spawn builds the request
// from the injected builder, spawns a real config agent, and wraps its bare tmux
// session in a BareSessionStreamer; reap tears the tmux session down.
func newConfigAssistantHub(m *Manager) *configAssistantHub {
	h := &configAssistantHub{graceWindow: configAssistantGraceWindow}
	h.spawn = func(ctx context.Context) (string, bareStreamer, error) {
		build := configAssistantRequestBuilder
		if build == nil {
			return "", nil, errConfigAssistantUnavailable
		}
		req, err := build()
		if err != nil {
			return "", nil, err
		}
		name, _, err := m.SpawnConfigAgent(ctx, req)
		if err != nil {
			return "", nil, err
		}
		ts := m.configAgents.session(name)
		if ts == nil {
			// The session was reaped between spawn and lookup (a racing DELETE, or
			// daemon shutdown). Best-effort re-reap so nothing leaks, and report it as an
			// aborted create (retryable) rather than "no assistant" (settle-and-stop).
			_ = m.ReapConfigAgent(ReapConfigAgentRequest{SessionName: name})
			return "", nil, errConfigAssistantSpawnAborted
		}
		return name, session.NewBareSessionStreamer(ts), nil
	}
	h.reapFn = func(name string) error {
		return m.ReapConfigAgent(ReapConfigAgentRequest{SessionName: name})
	}
	return h
}

// ensure spawns the shared assistant or reuses the running one. Idempotent: a
// burst of POSTs yields a single assistant. Blocks through the spawn's readiness
// wait on a cold start.
//
// A POST that arrives during the idle grace window keeps the assistant warm by
// RE-ARMING the reaper — it does NOT cancel it: an assistant with no subscriber is
// always on a timer, so a bare POST (spawn, then never open the stream — a WS dial
// that fails, a tab closed in the ~60s gap, a scripted curl) still gets reaped after
// the window rather than leaking a permission-skipping agent for the daemon's life.
func (h *configAssistantHub) ensure(ctx context.Context) (string, error) {
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()

	h.mu.Lock()
	if h.current != nil {
		name := h.current.name
		h.refreshIdleTimerLocked() // reuse: re-arm the window (no subscriber) or leave it off (has one)
		h.mu.Unlock()
		return name, nil
	}
	// Capture the reap epoch and publish a cancel for this spawn, so a DELETE landing
	// during the ~60s spawn is honored: it bumps reapEpoch and cancels the wait.
	epoch := h.reapEpoch
	sctx, cancel := context.WithCancel(ctx)
	h.spawnCancel = cancel
	h.mu.Unlock()

	name, streamer, err := h.spawn(sctx)

	h.mu.Lock()
	h.spawnCancel = nil
	aborted := h.reapEpoch != epoch // a DELETE ran during the spawn
	h.mu.Unlock()
	cancel() // release the context regardless of outcome

	if err != nil {
		// A spawn that failed BECAUSE a concurrent DELETE cancelled its context is an
		// aborted create (retryable), not a server error — classify it so, rather than
		// surfacing the raw context.Canceled. A failure with no racing DELETE is a real
		// spawn error and propagates unchanged.
		if aborted {
			return "", errConfigAssistantSpawnAborted
		}
		return "", err
	}

	if h.afterSpawnHook != nil {
		h.afterSpawnHook()
	}

	h.mu.Lock()
	if h.reapEpoch != epoch {
		// A DELETE ran during the spawn (it saw current==nil and had nothing to tear
		// down). Honor it: reap what we just spawned rather than storing an assistant
		// the user asked to remove — which, with no subscriber, would never be reaped.
		// Aborted create → retryable, NOT the stream route's settle-and-stop 404.
		h.mu.Unlock()
		streamer.Close()
		_ = h.reapSession(name)
		return "", errConfigAssistantSpawnAborted
	}
	h.current = &configAssistant{name: name, streamer: streamer}
	h.active = 0
	h.refreshIdleTimerLocked() // no subscriber yet → arm the idle reaper (the P1 fix)
	h.mu.Unlock()
	return name, nil
}

// subscribe opens one subscriber against the current assistant. It returns the
// (counted) subscription AND the streamer the connection drives input/resize on,
// so servePTYStream addresses the SAME assistant for the whole connection even if
// a reap swaps current mid-stream. errNoConfigAssistant when none is running.
func (h *configAssistantHub) subscribe(since session.Seq) (session.PTYSubscription, bareStreamer, error) {
	h.mu.Lock()
	cur := h.current
	if cur == nil {
		h.mu.Unlock()
		return nil, nil, errNoConfigAssistant
	}
	h.active++
	h.refreshIdleTimerLocked() // a live subscriber cancels any pending idle reap
	streamer := cur.streamer
	h.mu.Unlock()

	sub, err := streamer.Subscribe(since)
	if err != nil {
		h.subscriberGone(cur)
		return nil, nil, err
	}
	return &countedSubscription{PTYSubscription: sub, hub: h, assistant: cur}, streamer, nil
}

// subscriberGone decrements the live-subscriber count for the assistant the
// subscription was counted against, and re-arms the idle reaper if that assistant is
// now idle. It IGNORES a decrement whose assistant is no longer current (#2467 P3):
// a subscription that outlives a reap must not decrement — or arm the reaper against
// — the NEXT assistant, which would kill a live browser terminal mid-conversation.
// Called exactly once per successful subscribe, from the counted subscription's Close.
func (h *configAssistantHub) subscriberGone(assistant *configAssistant) {
	h.mu.Lock()
	if h.current != assistant {
		h.mu.Unlock()
		return // stale: this subscription belonged to an already-reaped assistant
	}
	if h.active > 0 {
		h.active--
	}
	h.refreshIdleTimerLocked()
	h.mu.Unlock()
}

// refreshIdleTimerLocked is the single arm/reset decision: a live assistant with NO
// subscriber is always on the idle reaper's clock; one with a subscriber, or none at
// all, has no timer. Every state change (spawn, reuse, subscribe, detach) routes
// through this, so no path can leave an idle assistant with no timer (the #2467 P1
// leak) or a subscribed one on a timer. mu held.
func (h *configAssistantHub) refreshIdleTimerLocked() {
	if h.current != nil && h.active == 0 {
		h.armGraceLocked()
	} else {
		h.resetGraceLocked()
	}
}

// reap tears the current assistant down immediately (the DELETE path). A no-op when
// none is running. Bumping reapEpoch and cancelling any in-flight spawn is what makes
// a DELETE that races a cold spawn count: ensure() re-checks the epoch and tears down
// what it spawned rather than silently dropping the DELETE (#2467 P2).
func (h *configAssistantHub) reap() error {
	h.mu.Lock()
	h.reapEpoch++
	if h.spawnCancel != nil {
		h.spawnCancel() // abort an in-flight cold spawn's ~60s wait
	}
	cur := h.current
	h.current = nil
	h.active = 0
	h.resetGraceLocked()
	h.mu.Unlock()
	if cur == nil {
		return nil
	}
	cur.streamer.Close()
	return h.reapSession(cur.name)
}

// stop tears the assistant's stream down at daemon shutdown so the clientless
// capture goroutine ends. It does NOT reap the tmux session — configAgents.Stop()
// owns that; this only closes the streamer (idempotent with the tmux teardown). Like
// reap it bumps the epoch and cancels an in-flight spawn, so a spawn racing shutdown
// does not store an assistant nothing will ever reap.
func (h *configAssistantHub) stop() {
	h.mu.Lock()
	h.reapEpoch++
	if h.spawnCancel != nil {
		h.spawnCancel()
	}
	cur := h.current
	h.current = nil
	h.active = 0
	h.resetGraceLocked()
	h.mu.Unlock()
	if cur != nil {
		cur.streamer.Close()
	}
}

// reapSession indirects through the injected reaper (m.ReapConfigAgent in
// production).
func (h *configAssistantHub) reapSession(name string) error {
	if h.reapFn == nil {
		return nil
	}
	return h.reapFn(name)
}

// armGraceLocked schedules the idle reaper. mu held.
func (h *configAssistantHub) armGraceLocked() {
	h.resetGraceLocked()
	gen := h.gen
	h.grace = time.AfterFunc(h.graceWindow, func() { h.onGraceExpired(gen) })
}

// resetGraceLocked cancels any pending timer and invalidates a fire that already
// escaped Stop (by bumping gen). mu held.
func (h *configAssistantHub) resetGraceLocked() {
	if h.grace != nil {
		h.grace.Stop()
		h.grace = nil
	}
	h.gen++
}

// onGraceExpired reaps the assistant if it is still idle and this fire is current.
func (h *configAssistantHub) onGraceExpired(gen uint64) {
	h.mu.Lock()
	if gen != h.gen || h.active != 0 || h.current == nil {
		h.mu.Unlock()
		return
	}
	cur := h.current
	h.current = nil
	h.gen++
	h.mu.Unlock()

	cur.streamer.Close()
	if err := h.reapSession(cur.name); err != nil {
		log.WarningLog.Printf("config assistant: idle reap of %s failed: %v", cur.name, err)
	}
}

// countedSubscription decrements the hub's subscriber count exactly once when the
// connection's subscription closes, re-arming the idle reaper when it was the last.
// It pins the assistant it was counted against so a decrement that outlives a reap is
// ignored rather than applied to the next assistant (#2467 P3).
type countedSubscription struct {
	session.PTYSubscription
	hub       *configAssistantHub
	assistant *configAssistant
	once      sync.Once
}

func (c *countedSubscription) Close() error {
	err := c.PTYSubscription.Close()
	c.once.Do(func() { c.hub.subscriberGone(c.assistant) })
	return err
}

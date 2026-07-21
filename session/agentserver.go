package session

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/sachiniyer/agent-factory/terminal"
)

// ErrTabGone reports that a stable tab id (#1738) names no live tab — it was
// closed, or it never existed. It is the REFUSAL the id-addressed data plane
// returns instead of falling back to a positional tab: once a client addresses a
// tab by its stable id, silently serving whatever tab now sits at some ordinal is
// the misroute the id exists to prevent (#1779). Callers map it to a 404/gone.
var ErrTabGone = errors.New("no tab with that id")

// ErrTabClosed ends a PTY subscription whose TAB was closed (#2136), as opposed
// to the session-wide teardown that ends every tab's stream at once (Kill). It is
// the end-of-stream error CloseTab hands the closed tab's subscribers so the WS
// writer can name the cause instead of leaving them blocked until the keepalive
// gives up.
//
// It WRAPS io.EOF deliberately: every consumer of a subscription already treats
// io.EOF as "this stream is over" (daemon/ws_pty.go, the attach clients, the
// tests), and a tab close IS that — only with a known cause. Wrapping means the
// distinction is opt-in for the one caller that renders it, and no existing
// errors.Is(err, io.EOF) check has to learn about tabs.
var ErrTabClosed = fmt.Errorf("tab closed: %w", io.EOF)

// TabAddressableServer is implemented by an agent-server whose data plane can be
// addressed by a tab's STABLE id (#1738) instead of a shifting ordinal. It is the
// id-native half of AgentServer's ordinal data plane: the ordinal methods stay for
// legacy clients that never supplied a ?tab_id=, while a client that DID supply one
// binds through here, so the id is resolved exactly ONCE — atomically, at the
// moment the operation binds — and never round-trips through an ordinal that a
// concurrent close/reorder can shift underneath it (#1779).
//
// The local runtime implements it (its brokers are already keyed by stable id, so
// id-addressing is strictly simpler than the ordinal round-trip it replaces). A
// runtime whose wire protocol is ordinal-shaped — the remote agent-server — does
// not. Its roster is fixed (TabManagement=false), so the handler's compatibility
// bridge cannot race a close/reorder; mutable remote tabs must add this id-native
// plane before that capability is enabled.
type TabAddressableServer interface {
	// SubscribeTab is Subscribe addressed by stable tab id. ErrTabGone when the id
	// names no live tab.
	SubscribeTab(tabID string, since Seq) (PTYSubscription, error)
	// InputTab is Input addressed by stable tab id. ErrTabGone when the id names no
	// live tab.
	InputTab(tabID string, b []byte) error
	// ResizeTab is Resize addressed by stable tab id. ErrTabGone when the id names
	// no live tab.
	ResizeTab(tabID string, rows, cols uint16) error
}

// AgentServer is the uniform contract the daemon speaks to a session's runtime,
// regardless of where that runtime physically lives (#1592 Phase 2 — the
// OpenHands-style agent-server seam). The daemon's observation and delivery
// paths depend ONLY on this interface; the tmux mechanism is an internal detail
// of the LOCAL in-process implementation (agentserver_local.go), no longer
// visible on the daemon's path. A Phase-4 runtime (container/ssh) implements the
// same interface over a native PTY behind an authed URL, and the daemon code
// above it does not change.
//
// This is the locality leak the epic set out to remove: before PR4 the daemon
// called tmux-shaped Backend methods (HasUpdated/IsAlive/
// SendPromptCommand/Preview) directly, baking "the session is local tmux" into
// the orchestrator. Those methods now live behind the agent-server.
//
// MULTI-WRITER (locked #1592): the data plane has NO lease. Subscribe is
// read-write for every subscriber, Input/Resize are accepted from any of them,
// and the PTY size is last-resize-wins. There is deliberately no mode argument —
// af is single-owner, so gating typing would be needless machinery. A lease is
// additive/reversible later if the rare two-active-clients resize-flap ever
// bites.
type AgentServer interface {
	// Provision establishes WHERE the agent runs — the local git worktree for the
	// local runtime, an off-box workspace for a future remote/container runtime.
	// It is the first half of Phase-1's Start split (backend provision phase).
	Provision(firstTimeSetup bool) error
	// Launch starts WHAT runs in the provisioned workspace — the agent process and
	// its tabs. It is the second half of Phase-1's Start split (backend launch
	// phase). firstTimeSetup mirrors Provision: a fresh create materializes the
	// worktree and spawns; a restore reconnects.
	Launch(firstTimeSetup bool) error
	// Expose returns where the session's data plane is reachable. For the local
	// in-process agent-server it is an in-process handle; a Phase-4 runtime returns
	// an authed URL. The WS PTY broker (PR5) consumes it.
	Expose() (StreamEndpoint, error)

	// Snapshot returns the current non-interactive observation the daemon's
	// liveness poll reads each tick, dismissing any pending trust/permission
	// prompt as a side effect (the poll always did both, in that order). See
	// Observation. The local implementation never errors; the error is for a future
	// remote runtime whose observation channel can fail.
	Snapshot() (Observation, error)
	// Preview returns tab `tab`'s visible output; full=true returns the entire
	// scrollback history. tab 0 is the agent tab (the backend preview); tab>0 is a
	// shell/process tab. This is the daemon's SOLE capture path for content the TUI
	// can't stream live (remote/hook sessions, scroll-mode scrollback, the transient
	// preview target) — the TUI no longer captures tmux itself (#1592 Phase 2 PR6).
	Preview(tab int, full bool) (PreviewSnapshot, error)
	// PreviewByID is Preview addressed by the tab's stable identity. Implementations
	// must either bind directly to the identified capture target or keep identity
	// resolution and the ordinal capture in one critical section; resolving first
	// and using that ordinal against a later roster can expose another tab (#2200).
	// ErrTabGone reports that the exact target no longer exists.
	PreviewByID(tabID string, full bool) (PreviewSnapshot, error)
	// Alive reports whether the underlying session process is still running, and
	// whether the probe could be ANSWERED at all. Kept separate from Snapshot
	// (rather than folded into Observation) so the daemon probes liveness ONLY on
	// the idle branch, exactly as before — folding it in would add a liveness
	// probe to every non-idle tick.
	//
	// A non-nil error means UNKNOWN, not dead: the probe itself failed. For the
	// remote runtime that is a REST call to the sandbox's agent-server that never
	// completed — a dropped ssh forward, a docker-proxy hiccup, a blackholed
	// route. The local runtime probes in-process and never errors.
	//
	// The distinction is load-bearing, which is why the error is on the signature
	// rather than swallowed into a bare bool. "Unreachable" and "reachable, and
	// the agent is gone" are the same `false` but demand OPPOSITE responses: the
	// first may be a transient blip that must be waited out, the second is an
	// authoritative answer that may be acted on at once. Collapsing them is what
	// let a single transport blip re-provision a live sandbox and destroy its
	// unpushed work (#1794) — so callers that act destructively on `false` MUST
	// branch on the error. Callers for whom both cases warrant the same response
	// may ignore it.
	Alive() (bool, error)

	// SendPrompt delivers a prompt over the reliable command path (tmux send-keys
	// for the local runtime) — the path automated/scheduled deliveries use, which
	// survives a PTY that is not currently attached. This is the daemon's delivery
	// primitive; interactive per-keystroke input is Input, on the data plane.
	SendPrompt(prompt string) error
	// Subscribe returns a fan-out read of tab `tab`'s PTY stream from cursor
	// `since` (0 = from the ring-buffer tail / live), so a reconnecting client
	// replays the gap it missed. tab 0 is the agent tab; tab>0 is a shell/process
	// tab — each tab has its own bounded ring buffer and clientless capture (#1592
	// Phase 2 PR6, tab-aware). The local agent-server drives a clientless tmux
	// channel — pipe-pane for output capture — and fans the bytes to every
	// subscriber; a subscriber that falls behind or dies is dropped without touching
	// the PTY (§6). Read-write: Input/Resize below are accepted from every
	// subscriber (multi-writer, no lease).
	Subscribe(tab int, since Seq) (PTYSubscription, error)
	// Input writes raw bytes to tab `tab`'s PTY (the multi-writer input path that
	// subsumes the old tmux-shaped SendKeys). For the local runtime it is a
	// clientless tmux send-keys, accepted from any subscriber.
	Input(tab int, b []byte) error
	// Resize sets tab `tab`'s PTY size; last-resize-wins across subscribers. The
	// local runtime drives a clientless tmux resize-window and broadcasts an
	// authoritative size echo to every subscriber so their emulators reflow (§6.2).
	Resize(tab int, rows, cols uint16) error

	// Kill terminates the session and releases its backing resources.
	Kill() error

	// Archive makes the workspace durable before its sandbox is torn down (#1592
	// Phase 4 PR6): it commits any uncommitted work and pushes the session branch
	// to origin (GitHub is the durable workspace store, epic decision 4),
	// returning the pushed branch so the orchestrator can clone it back on
	// restore. It is the primitive the disposable sandbox runtimes (docker/ssh)
	// archive through — the daemon calls it over the wire, and the in-sandbox
	// local agent-server pushes the branch it owns. The local in-process runtime
	// implements it too (a plain commit+push of its worktree), but the daemon
	// never drives a LOCAL session's archive through here — a local session
	// archives by relocating its worktree (§5.1), so this stays dormant for it.
	Archive() (string, error)
}

// Seq is a monotonic cursor into a session's PTY output ring buffer, used by
// Subscribe(since) to replay the gap after a reconnect (#1592 Phase 2). Defined
// here so the data-plane signatures are stable; the ring buffer that mints these
// lands with the WS PTY broker in PR5.
type Seq uint64

// StreamEndpoint identifies where a session's data plane is reachable. For the
// local in-process agent-server it is an in-process handle (Local=true); a
// Phase-4 remote runtime returns an authed URL. Auth-ready-by-shape for Phase 3.
type StreamEndpoint struct {
	// Local marks an in-process endpoint with no network hop — the only kind in
	// Phase 2 (local runtime).
	Local bool
	// URL is the authed endpoint a remote/container runtime exposes (empty for a
	// local in-process endpoint). Filled in Phase 4.
	URL string
}

// Observation is the non-interactive snapshot the daemon's liveness poll
// reads each tick (#1592 Phase 2): whether the pane changed since the last probe,
// whether the program is showing a prompt awaiting input, and the raw captured
// pane content so the usage-limit detector (#1146) can inspect it without a
// second capture. It replaces the daemon reading the tmux-shaped HasUpdated probe
// directly.
type Observation struct {
	// Updated is true if the session output changed since the last probe.
	Updated bool
	// HasPrompt is true if the program is showing a yes/no prompt awaiting input.
	HasPrompt bool
	// Content is the raw captured pane content, handed back so the idle branch can
	// run the usage-limit detector without a second capture (#1146). Empty for a
	// runtime with no live pane.
	Content string
}

// PTYSubscription is one subscriber's read side of a session's PTY stream
// (#1592 Phase 2 PR5), fanned out from the local agent-server's per-session ring
// buffer. It is event-oriented rather than a bare byte reader so a single
// consumer goroutine (the daemon's WS writer) can multiplex the two things that
// travel to a client on one connection — raw output bytes and the authoritative
// resize echo — without a second concurrent writer on the socket.
type PTYSubscription interface {
	// NextEvent blocks until the next stream event (output bytes or a resize
	// echo), ctx cancellation, or Close. It returns io.EOF once the stream ends
	// (the session's PTY vanished or the broker closed), or ErrTabClosed — which
	// wraps io.EOF — when the end came from THIS tab being closed (#2136). A client
	// that reconnects resumes from Seq() via Subscribe(since).
	NextEvent(ctx context.Context) (PTYEvent, error)
	// Seq reports the cursor of the next output byte this subscriber will read, so
	// a client that reconnects can resume the gap with Subscribe(since).
	Seq() Seq
	io.Closer
}

// PTYEventKind discriminates a PTYEvent between output bytes and a resize echo.
type PTYEventKind int

const (
	// PTYData carries verbatim PTY output bytes (Data), mapped to an OpPTYOut wire
	// frame by the WS broker.
	PTYData PTYEventKind = iota
	// PTYResize carries the authoritative last-resize-wins size (Rows/Cols),
	// mapped to a resize control frame so every subscriber's emulator reflows.
	PTYResize
	// PTYRepaint carries a one-shot initial screen repaint (Data) for a fresh
	// subscriber, mapped to an OpRepaint frame — rendered like output but NOT
	// counted toward the client's replay cursor (it is not part of the ring seq).
	PTYRepaint
	// PTYCursor carries this subscription's authoritative cursor (Seq) after the
	// SERVER moved it non-contiguously — a ring eviction, or the #1840 recovery
	// discard, fast-forwarded the subscriber over bytes that no longer exist. A
	// client derives its own replay cursor as start + bytes-received, which silently
	// desyncs across such a jump: it would then reconnect with a ?since BELOW the
	// broker's base, get clamped back up, and be re-sent bytes it already rendered
	// (duplicated output). Mapped to an OpHello frame — the same in-band cursor seed
	// the subscription opens with — so the client re-seeds instead of counting on.
	// Carries no PTY bytes and is not itself part of the ring seq.
	PTYCursor
)

// PTYRepaintProvenance distinguishes a fresh/reconnect snapshot from the
// recovery barrier repaint that intentionally covers the immediately following
// cursor jump. Defaulting to Fresh is fail-closed for callers that construct an
// event without provenance.
type PTYRepaintProvenance uint8

const (
	PTYRepaintFresh PTYRepaintProvenance = iota
	PTYRepaintRecovery
)

// PTYEvent is one event delivered to a subscriber: output bytes, the authoritative
// resize echo, a screen repaint, or a cursor re-seed, selected by Kind.
type PTYEvent struct {
	Kind PTYEventKind
	// Data is the verbatim PTY output, valid only when Kind == PTYData.
	Data []byte
	// Rows/Cols are the authoritative size, valid only when Kind == PTYResize.
	Rows uint16
	Cols uint16
	// Seq is the subscription's authoritative output cursor, valid only when
	// Kind == PTYCursor.
	Seq Seq
	// Modes accompany PTYRepaint when HasModes is true. They are snapshot
	// metadata, not ring bytes, and therefore do not advance Seq.
	Modes             terminal.Modes
	HasModes          bool
	RepaintProvenance PTYRepaintProvenance
}

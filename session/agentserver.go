package session

import (
	"errors"
	"io"
)

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
// called tmux-shaped Backend methods (HasUpdated/TapEnter/IsAlive/
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
	// liveness/AutoYes poll reads each tick, dismissing any pending trust/permission
	// prompt as a side effect (the poll always did both, in that order). See
	// Observation. The local implementation never errors; the error is for a future
	// remote runtime whose observation channel can fail.
	Snapshot() (Observation, error)
	// Preview returns the session's visible output; full=true returns the entire
	// scrollback history.
	Preview(full bool) (string, error)
	// Alive reports whether the underlying session process is still running. Kept
	// separate from Snapshot (rather than folded into Observation) so the daemon
	// probes liveness ONLY on the idle branch, exactly as before — folding it in
	// would add a liveness probe to every non-idle tick.
	Alive() bool

	// SendPrompt delivers a prompt over the reliable command path (tmux send-keys
	// for the local runtime) — the path automated/scheduled deliveries use, which
	// survives a PTY that is not currently attached. This is the daemon's delivery
	// primitive; interactive per-keystroke input is Input, on the data plane.
	SendPrompt(prompt string) error
	// TapEnter sends a bare Enter keystroke — the AutoYes accept. A no-op unless
	// the session has AutoYes enabled. An input helper that routes through the same
	// underlying channel as SendPrompt.
	TapEnter()

	// Subscribe returns a fan-out read of the raw PTY byte stream from cursor
	// `since` (0 = from the buffer tail), so a reconnecting client replays the gap.
	// Data plane — wired by the WS PTY broker in PR5; the local agent-server returns
	// ErrDataPlaneUnwired until then.
	Subscribe(since Seq) (PTYSubscription, error)
	// Input writes raw bytes to the PTY (the multi-writer input path that subsumes
	// the old tmux-shaped SendKeys). Data plane — wired in PR5.
	Input(b []byte) error
	// Resize sets the PTY size; last-resize-wins across subscribers. Data plane —
	// wired in PR5.
	Resize(rows, cols uint16) error

	// Kill terminates the session and releases its backing resources.
	Kill() error
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

// Observation is the non-interactive snapshot the daemon's liveness/AutoYes poll
// reads each tick (#1592 Phase 2): whether the pane changed since the last probe,
// whether the program is showing a prompt awaiting input, and the raw captured
// pane content so the usage-limit detector (#1146) can inspect it without a
// second capture. It replaces the daemon reading the tmux-shaped HasUpdated probe
// directly.
type Observation struct {
	// Updated is true if the session output changed since the last probe.
	Updated bool
	// HasPrompt is true if the program is showing a yes/no prompt awaiting input
	// (the AutoYes trigger).
	HasPrompt bool
	// Content is the raw captured pane content, handed back so the idle branch can
	// run the usage-limit detector without a second capture (#1146). Empty for a
	// runtime with no live pane.
	Content string
}

// PTYSubscription is the read side of a session's raw PTY byte stream plus a
// sequence cursor for replay (#1592 Phase 2). The WS PTY broker (PR5) implements
// it; the local agent-server returns ErrDataPlaneUnwired from Subscribe until
// then, so there is no concrete implementer in this PR.
type PTYSubscription interface {
	io.ReadCloser
	// Seq reports the cursor of the next byte to be read, so a client that
	// reconnects can resume with Subscribe(since).
	Seq() Seq
}

// ErrDataPlaneUnwired is returned by the local agent-server's data-plane methods
// (Subscribe/Input/Resize) in Phase 2 PR4: the interface is defined and the
// observation/delivery paths route through it, but the raw-PTY streaming plane is
// built on top in PR5 (the WS broker + clientless tmux fan-out). It is a distinct
// sentinel so PR5 can assert callers handle it and delete it once the plane is
// wired.
var ErrDataPlaneUnwired = errors.New("agent-server data plane not wired yet (Phase 2 PR5)")

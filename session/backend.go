package session

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// WorkspaceKind describes where a backend's workspace physically lives, so
// callers reason about locality without asking "is this the remote type"
// (#1592 Phase 1). New runtimes (ssh/container) pick the kind that matches
// where the git worktree lands.
type WorkspaceKind int

const (
	// WorkspaceLocalWorktree: a git worktree on the daemon's own machine, driven
	// by tmux (LocalBackend). Zero value — a backend-less instance reads
	// as a local workspace.
	WorkspaceLocalWorktree WorkspaceKind = iota
	// WorkspaceRemote: the workspace lives off-box; there is no local worktree or
	// tmux to drive (docker, SSH, and remote-hook runtimes).
	WorkspaceRemote
)

// Capabilities is a backend's self-description: which optional session
// operations it can service. The daemon and UI branch on these instead of on
// Type()=="remote" (#1592 Phase 1), so a NEW backend declares what it supports
// rather than every call-site learning its name. The end state is full parity —
// every backend implements every capability — but the descriptor stays so a
// surface can gray out an op a given runtime hasn't wired up yet.
type Capabilities struct {
	// Workspace records where the workspace lives (local worktree vs off-box).
	Workspace WorkspaceKind

	// There is deliberately no Attach bit (#1860). Attach is not an optional
	// capability any more: every runtime attaches client-side over the WS PTY
	// stream, so the bit was unconditionally true for every backend and no
	// dispatch ever read it. A capability that cannot be false gates nothing —
	// it only invites a future attach gate to branch on a constant.

	// Archive: the session can be archived/restored (local-worktree relocation
	// today; push/pull the branch once every backend clones from GitHub).
	Archive bool
	// Recover: a Lost session can be reconnected / re-spawned in place.
	Recover bool
	// TabManagement: the user can add/close tabs that RUN A PROCESS — a shell or
	// process tab, which needs a PTY in the daemon-side worktree. It does not
	// govern metadata-only tabs (a web tab is a name and a URL): those spawn
	// nothing, so gating them on this bit refused them for a reason that did not
	// apply to them (#3053). Ask TabKindRequires what a kind needs, and
	// RefuseTabKind whether this backend can serve it.
	TabManagement bool
	// TerminalTab: an interactive terminal surface is available. Off-box runtimes
	// provide it through their AgentServer stream rather than a daemon-local tmux.
	TerminalTab bool
	// InteractiveInput: raw key / prompt injection works — SendKeys/
	// SendPrompt drive a live PTY rather than returning "not supported".
	InteractiveInput bool
	// Handoff: the session's agent program can be swapped in place, keeping the
	// same workspace and branch (#2013). Local-worktree only for now — a sandbox
	// backend would have to re-launch the agent inside the provisioned sandbox
	// rather than re-provision it, which is a separate lifecycle.
	Handoff bool
}

// RefuseTabKind reports why this backend cannot serve a tab of this kind, or nil
// if it can. It is the single gate for "may this session gain THIS tab", and it
// asks what the KIND needs rather than whether the session is off-box (#3053):
// the old form refused every kind on off-box sessions and explained each refusal
// as a missing worktree, which is untrue of a tab that spawns nothing.
//
// target is the web tab's resolved URL and is ignored for every other kind. It
// is a parameter because a web tab's blockers depend on WHERE it points: only a
// loopback target is reverse-proxied, so only a loopback target is affected by
// the daemon-host routing gap. Naming that blocker for an external URL would
// state a requirement the caller's tab does not have — the exact defect this
// function exists to remove, one level down.
//
// Each refusal names the requirement that is actually unmet, because a user told
// "not supported on this backend" cannot tell that from a kind that could work.
func (c Capabilities) RefuseTabKind(kind TabKind, target string) error {
	switch TabKindRequires(kind) {
	case TabNeedsMetadataOnly:
		// A web tab spawns nothing and reads nothing, so the KIND needs nothing
		// from the workspace. What the daemon cannot yet do is SERVE one off-box,
		// and that is a different question with two different answers (#3062).
		if c.Workspace == WorkspaceLocalWorktree {
			return nil
		}
		// Per-TARGET, because the two blockers were never one blocker (#3062).
		//
		// A LOOPBACK target is still refused. webtab_proxy.go returns a webTabTarget
		// with no transport for TabKindWeb, so the daemon proxies it over its OWN TCP
		// stack: `--port 3000` resolves on the daemon host rather than in the
		// workspace, and can surface an unrelated daemon-local service inside the
		// session's tab. Lifting this needs a relay through the agent-server — its
		// own endpoint, not the per-tab PTY frame stream, whose lifetime is a
		// terminal subscription and which a large web response would head-of-line
		// block.
		if IsLoopbackWebTarget(target) {
			return fmt.Errorf("this session cannot open a web tab pointing at %s yet: a loopback target is reverse-proxied from the daemon host rather than from the session's off-box workspace, so it would serve the daemon's own %s — see #3062", target, target)
		}
		// A host a BROWSER would canonicalise to loopback is refused too, even where
		// Go does not read it as one. net.ParseIP rejects the shorthands 127.1 and
		// 2130706433, so IsLoopbackWebTarget calls them external — but a browser
		// resolves both to 127.0.0.1, picks the proxy path, and the proxy then
		// refuses them by that same Go predicate. Admitting one would create a tab
		// that is durable and permanently unusable, which is worse than refusing it.
		if webTargetHostIsBrowserLoopbackShorthand(target) {
			return fmt.Errorf("this session cannot open a web tab pointing at %s yet: a browser resolves that host to loopback, and a loopback target is reverse-proxied from the daemon host rather than from the session's off-box workspace — see #3062", target)
		}
		// An EXTERNAL absolute URL is admitted. It is iframed directly and never
		// touches the proxy, so the routing gap above is not a requirement it has;
		// and FromInstanceData now stages metadata-only tabs across a sandbox
		// restart, so it no longer disappears at the next daemon restart.
		return nil
	case TabNeedsLocalWorktreeRead:
		if c.Workspace != WorkspaceLocalWorktree {
			// Names the missing EDITOR, not the missing worktree path. The path is
			// how today's implementation happens to fail — the daemon's
			// code-server is rooted at a daemon-host directory — but it is not
			// what blocks this. af runs no editor inside an off-box workspace and
			// copies only the `af` binary there, so there is nothing to reach.
			// Routing is not the obstacle either: the ssh runtime already carries
			// a general TCP forward. See #3054, which carries the evidence.
			return fmt.Errorf("this session cannot open a vscode tab: af runs no editor inside an off-box workspace (docker/ssh/sandbox) — the daemon's own code-server can only serve directories on the daemon host — see #3054")
		}
		return nil
	default:
		if !c.TabManagement {
			return fmt.Errorf("this session cannot open a shell or process tab: it runs a process in the session's worktree, and this session's workspace runs off-box (docker/ssh/sandbox), so there is no local worktree to spawn it in")
		}
		return nil
	}
}

// ErrHandoffUnsupported is returned when an agent handoff (#2013) is requested
// on a backend that cannot swap its agent in place. It is a typed sentinel so
// callers can render the restriction rather than match on prose.
var ErrHandoffUnsupported = errors.New("agent handoff is only supported for local-worktree sessions")

// Backend abstracts the session lifecycle so instances can be backed by local
// tmux+git worktrees (the default) or an off-box docker, SSH, or hook runtime.
type Backend interface {
	// Start initialises the session. When firstTimeSetup is true a brand-new
	// session is created; otherwise an existing one is restored from storage.
	//
	// Each backend implements Start as two phases (#1592 Phase 1 PR4): a PROVISION
	// step that establishes WHERE the agent runs (the local git worktree, or a
	// provisioned off-box workspace) and a LAUNCH step that starts WHAT runs in it
	// (the tmux/PTY/agent process and its tabs). Start is Provision then Launch.
	// The two halves are on the interface (#1592 Phase 2 PR4) so the local
	// agent-server's provision-and-expose model can drive them separately; Start
	// stays as the combined lifecycle entry point its existing callers use.
	Start(instance *Instance, firstTimeSetup bool) error

	// Provision establishes WHERE the session runs without starting the agent
	// process — the local git worktree + tmux binding, or a remote/off-box
	// workspace. The first half of Start. See each backend's implementation for the
	// precise on-disk vs in-memory boundary.
	Provision(instance *Instance, firstTimeSetup bool) error

	// Launch starts (or restores) WHAT runs in the workspace Provision established
	// — the agent process and its tabs. The second half of Start; it owns the
	// failure-cleanup scope. A fresh worktree is removed only when the launcher
	// positively establishes that no runtime began; an unknown startup outcome
	// leaves it in place for the caller to retain (#2207).
	Launch(instance *Instance, firstTimeSetup bool) error

	// Kill terminates the session and cleans up all associated resources.
	Kill(instance *Instance) error

	// CloseAttachOnly releases resources this Instance opened to view or drive the
	// session WITHOUT destroying the underlying session, worktree, or off-box
	// workspace. It is the non-destructive sibling of Kill, used to discard a
	// duplicate Instance built from disk that lost a race to the canonical,
	// still-tracked Instance — see the daemon's findSession (#867). Killing such a duplicate
	// would tear down state the canonical Instance shares; closing only its
	// attach resources reclaims the PTY without that collateral damage.
	CloseAttachOnly(instance *Instance) error

	// Preview returns the current visible output of the session.
	Preview(instance *Instance) (string, error)

	// PreviewFullHistory returns the full scrollback history.
	PreviewFullHistory(instance *Instance) (string, error)

	// There is deliberately no Attach/AttachTerminal here (#1852). Interactive
	// attach is CLIENT-side for every runtime: the client dials the daemon's WS
	// PTY stream (apiclient.AttachStream), and the daemon resolves locality via
	// instance.AgentServer() — a local broker, or a remoteAgentServer proxy for
	// docker/ssh/hook. A backend-routed attach is therefore not a thing a caller
	// can express, which is what keeps #1837 (remote attach aimed at an erroring
	// backend stub) from recurring.

	// HasUpdated reports whether the session output changed since the last
	// check and whether the program is showing a prompt, and returns the raw
	// captured pane content so the daemon's usage-limit detector (#1146) can
	// inspect it without a second capture. content is "" when the capture or
	// remote observation is unavailable.
	HasUpdated(instance *Instance) (updated bool, hasPrompt bool, content string)

	// SendPromptCommand sends a prompt using a reliable command-based approach
	// (tmux send-keys for the local runtime). This is the SOLE prompt-delivery
	// primitive: AgentServer.SendPrompt delegates here, and it lands whether or
	// not a PTY is currently attached — the raw PTY-write SendPrompt (a 100ms
	// send-then-Enter) was deleted as dead post-migration (#1626).
	SendPromptCommand(instance *Instance, prompt string) error

	// IsAlive returns true if the underlying session is still running.
	// IsAlive reports whether the instance's agent is running, and returns an
	// error when the runtime could not be ASKED (#1917 round 8). The error is the
	// tri-state: a bool alone forces a timed-out probe to pick yes or no, and the
	// convenient pick — "yes" — is what let a wedged tmux server be counted as
	// affirmative proof of life all the way up in the daemon's poll. An
	// implementation that cannot answer must say so rather than guess.
	IsAlive(instance *Instance) (bool, error)

	// CheckAndHandleTrustPrompt auto-dismisses trust/permission prompts
	// for supported programs.
	CheckAndHandleTrustPrompt(instance *Instance) bool

	// AgentModelChange returns the active, runtime-derived model diagnostic after
	// prompt handling has had a chance to observe a safety dialog. It is read by
	// AgentServer.Snapshot so local and off-box runtimes cross the same choke point.
	AgentModelChange(instance *Instance) *AgentModelChange
	// Recover re-establishes a Lost session's backing resources — the tmux
	// session vanished out from under a live record with no kill on record
	// (#1108) — re-spawning the program in the instance's worktree. It is
	// invoked by the daemon's restore loop and by user-initiated restore
	// (af sessions restore), never as a load-time side effect (the #970 guard
	// in Start stays authoritative for loads). Every backend services it at full
	// parity since #1592 Phase 4: the sandbox runtimes (docker/ssh/hook)
	// re-provision a fresh sandbox that clones the durable branch back from
	// origin (recoverSandbox, §5.1) — there is no ErrRecoverUnsupported anymore.
	Recover(instance *Instance) error

	// Respawn re-establishes an instance's backing session in place — re-spawning
	// the agent program via the resume path (resumeProgram: claude --continue,
	// codex resume --last) — WITHOUT any liveness precondition. It is the
	// guard-free core Recover wraps with its Lost guard; the usage-limit
	// manual-retry (#1146) uses it directly because a LimitReached session (which
	// Recover's !Lost guard rejects) needs the identical re-spawn. Callers own the
	// precondition. The sandbox runtimes (docker/ssh/hook) service it through the
	// same recoverSandbox re-provision-and-clone path as Recover — no backend
	// returns an unsupported sentinel.
	Respawn(instance *Instance) error

	// SwapAgent tears down the running agent process and launches the instance's
	// CURRENT program in its place, as a fresh conversation (#2013). The caller
	// has already rewritten Instance.Program to the incoming agent and owns every
	// precondition; this is only the runtime half.
	//
	// It is deliberately not Respawn. Respawn goes through the resume path, which
	// appends the provider's "continue the most recent conversation here" flag —
	// correct when the SAME agent is coming back after its session vanished, and
	// wrong for a handoff, where the incoming agent has no conversation in this
	// worktree to continue and would be asked to resume one that does not exist.
	// A handoff is a first launch for the new agent, so it takes the first-launch
	// path.
	//
	// It is also not Restore-based: Restore on a session tmux still reports as
	// live is a pure logical rebind that never re-execs the program, and a
	// usage-limit-blocked agent IS live. Routing a swap through it would rewrite
	// the program string and leave the old agent running — a silent no-op. The
	// teardown below is what makes the re-launch actually happen.
	//
	// Backends whose workspace is off-box do not implement this: swapping the
	// agent inside a provisioned sandbox is a different lifecycle (re-launch
	// inside the sandbox, not re-provision it), so they return
	// ErrHandoffUnsupported and the Handoff capability bit is false.
	// PrepareAgentSwap resolves and validates the exact command that a handoff
	// would launch. It runs before the outgoing process is touched; SwapAgent must
	// consume this plan rather than resolving configuration again after teardown.
	PrepareAgentSwap(instance *Instance, target string) (AgentSwapPlan, error)
	SwapAgent(instance *Instance, plan AgentSwapPlan) error

	// Type returns the persisted backend identifier (local, docker, ssh, or
	// remote). Since #1592 Phase 1 this is the serialization discriminator only (the
	// load-time factory in instance_data.go) — runtime branching goes through
	// Capabilities, never Type().
	Type() string

	// Capabilities reports which optional operations this backend can service,
	// replacing Type()-based special-casing (#1592 Phase 1).
	Capabilities() Capabilities
}

// promptDeliveryStatusBackend is the additive observation-bearing delivery
// capability used by the local agent-server. Backends without it retain the
// legacy error-only contract and are reported as could-not-confirm.
type promptDeliveryStatusBackend interface {
	SendPromptCommandWithStatus(instance *Instance, prompt string) (PromptDeliveryStatus, error)
}

// AgentSwapPlan is the immutable boundary between handoff preflight and runtime
// replacement. Its fields are intentionally private: only a Backend can produce
// a plan, and callers can only hand that same value back to SwapAgent. That makes
// it impossible for the destructive half to silently launch a command different
// from the one that was checked while the outgoing agent was still alive.
type AgentSwapPlan struct {
	target              string
	program             string
	conversation        AgentConversationData
	conversationCapture ConversationCaptureSnapshot
}

// ConversationCapture returns the provider-store before-image frozen by
// preflight. The snapshot is opaque outside session: callers can pass it to the
// capture API, but cannot retarget it after the outgoing runtime is stopped.
func (p AgentSwapPlan) ConversationCapture() ConversationCaptureSnapshot {
	return p.conversationCapture
}

// webTargetHostIsBrowserLoopbackShorthand reports whether a browser parses a
// non-canonical host as a legacy IPv4 address in 127/8. Go's net.ParseIP does
// not accept these forms, but the browser canonicalises them before deciding
// whether the web tab uses the daemon proxy (#3062).
func webTargetHostIsBrowserLoopbackShorthand(target string) bool {
	parsed, err := url.Parse(strings.TrimSpace(target))
	if err != nil || parsed == nil {
		return false
	}
	host := parsed.Hostname()
	if host == "" || net.ParseIP(host) != nil {
		return false // a real IP literal; IsLoopbackWebTarget already classified it
	}
	address, ok := parseBrowserIPv4Address(host)
	return ok && byte(address>>24) == 127
}

// parseBrowserIPv4Address implements the URL-standard legacy IPv4 grammar:
// one to four decimal, octal, or 0x-prefixed components, with the final
// component filling the remaining bytes. It returns false for ordinary DNS
// names even when every letter happens to be a hexadecimal digit.
func parseBrowserIPv4Address(host string) (uint32, bool) {
	parts := strings.Split(host, ".")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	if len(parts) == 0 || len(parts) > 4 {
		return 0, false
	}

	numbers := make([]uint64, len(parts))
	for idx, part := range parts {
		number, ok := parseBrowserIPv4Number(part)
		if !ok || (idx < len(parts)-1 && number > 255) {
			return 0, false
		}
		numbers[idx] = number
	}
	lastLimit := uint64(1) << uint(8*(5-len(parts)))
	if numbers[len(numbers)-1] >= lastLimit {
		return 0, false
	}

	address := numbers[len(numbers)-1]
	for idx := 0; idx < len(numbers)-1; idx++ {
		address += numbers[idx] << uint(8*(3-idx))
	}
	return uint32(address), true
}

func parseBrowserIPv4Number(input string) (uint64, bool) {
	if input == "" {
		return 0, false
	}
	base := 10
	digits := input
	if len(input) >= 2 && input[0] == '0' && (input[1] == 'x' || input[1] == 'X') {
		base = 16
		digits = input[2:]
	} else if len(input) >= 2 && input[0] == '0' {
		base = 8
		digits = input[1:]
	}
	if digits == "" {
		return 0, true
	}
	number, err := strconv.ParseUint(digits, base, 64)
	return number, err == nil
}

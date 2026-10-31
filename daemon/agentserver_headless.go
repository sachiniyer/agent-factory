package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/runtimetoken"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/terminal"
)

// The headless single-workspace agent-server (#1592 Phase 4 PR1) — the process
// that runs inside each docker/SSH/hook workspace. It is the mirror
// image of the daemon's in-process localAgentServer: where the daemon drives a
// local session through session.AgentServer directly, this exposes ONE session's
// AgentServer over the exact HTTP/WS+token wire the daemon already speaks, so
// a remote daemon's remoteAgentServer can drive it across the process
// boundary exactly like the in-process one.
//
// It is deliberately NOT the orchestrator (§1.1): no task scheduler, no watch
// supervisor, no multi-session Manager, no disk-state ownership. It owns exactly
// one live session.AgentServer and exposes it. Docker, SSH, and hook runtimes
// provision this process; it can also be run directly for testing.
//
// Reuse, not reimplementation: the HTTP+token listener (startTCPListener), the
// auth+CORS gate (withAuth), the WS PTY broker fan-out (servePTYStream), the
// events plane (serveEvents), the REST envelope dispatch (rpcHandler), and the
// agentproto wire frames are all the Phase-1–3 seams, unchanged. This file is
// only the single-workspace glue that binds one AgentServer to those routes.

// headlessServer hosts exactly one session.AgentServer over the daemon's HTTP/WS
// surface. It is the single-workspace analogue of controlServer: where
// controlServer resolves an AgentServer out of the multi-session Manager,
// headlessServer holds the one it owns directly.
type headlessServer struct {
	// adopted is set when this server was started over a preserved worktree
	// (#5571 W8.6); its non-first-time launch respawns the agent there.
	adopted bool
	// repoPath is the repository this workspace runs against; with the title it
	// names the workspace across an agent-server restart (see editorNamespace).
	repoPath string
	// as is the single workspace's agent-server — the local in-process
	// implementation over tmux (same runtime as the daemon's local sessions),
	// exposed here over the wire.
	as session.AgentServer
	// inst is the workspace's session.Instance itself — the tab roster the
	// /v1/agent/tab-* routes bind to (through the tabs narrowing below) and the
	// resolving arm bindTab falls back to when the AgentServer lacks the
	// id-native plane. hs.as never does (it is inst.AgentServer()), so in
	// production the fallback is dead code; the field exists so the route has
	// the same bind surface the daemon's streamHandler does.
	inst *session.Instance
	// program is the host-resolved agent command, for conversation resume and
	// capture decisions (#5571 W7.2).
	program string
	// tabs is the roster the /v1/agent/tab-* routes create, close, and read —
	// in production the SAME Instance inst points at, narrowed to the five
	// methods the routes need so a wire-shape test can substitute a small fake.
	tabs tabKeeper
	// title is the workspace's session title, echoed in the startup banner so a
	// driver knows which id to name on the /v1/sessions/{id}/stream path.
	title string
	// events is a private events hub so the /v1/events route upgrades and stays
	// alive for surface parity with the daemon. A single-workspace server has no
	// orchestrator mutations to publish, so the plane is deliberately quiet — the
	// session lifecycle events belong to the daemon that drives this server, not
	// to the server itself (§1.1: not the orchestrator).
	events *eventsHub
	// archive collapses concurrent /v1/agent/archive requests onto one in-flight
	// archive (#2997 finding 2). See singleFlightArchive.
	archive singleFlightArchive
	// editor is the workspace's code-server supervisor (W5.4), created on first use.
	editor agentEditor
}

// tabKeeper is the tab-roster surface the /v1/agent/tab-* routes bind to
// (#5571 W4.3). It is *session.Instance narrowed to exactly the methods the
// routes call — the routes never touch tmux, the worktree, or persistence
// themselves — so the wire contract can be exercised without a started session.
// Every method is the Instance admission path (AddShellTabWithID keeps the
// started/archived/account fences; CloseTabByID is the same close the daemon's
// own route issues) — a remote caller gets no looser path than a local one.
type tabKeeper interface {
	// TabsData projects the roster (agent tab first) plus each tab's exit
	// evidence — the /v1/agent/tabs payload.
	TabsData() []session.TabData
	// RecordProcessTabExits probes un-recorded process tabs before a roster
	// read so a tab whose command already finished does not report a stale
	// absence — the read side of stampProcessTabExit's restore path.
	RecordProcessTabExits()
	// AddShellTabWithID spawns a shell tab, assigning the daemon-supplied id
	// verbatim (idempotency key for the replayed create).
	AddShellTabWithID(id string) (*session.Tab, error)
	// AddProcessTabWithID spawns a process tab under the supplied id; command
	// and name are the request's own fields.
	AddProcessTabWithID(id, command, requestedName string) (*session.Tab, error)
	// CloseTabByID closes the tab a stable id names — ErrTabGone when it names
	// nothing, which the shared envelope maps to 404.
	CloseTabByID(tabID string) error
}

// singleFlightArchive makes a second /v1/agent/archive request JOIN the archive
// already running instead of starting another one (#2997 finding 2).
//
// THE PROBLEM IT SOLVES, which is a property of the wire and not of this server:
// the mutating RPCs are registered through rpcHandler, which discards the request
// context, so a client that gives up ends the REQUEST and not the server-side
// mutation. The daemon gives up on its own schedule — AgentArchiveCallTimeout is
// 3 minutes and the caller's outer bound sits 30s ABOVE it — and then
// preserveSandboxBeforeReap refuses to reap and says, in its own error text, that
// "the daemon keeps retrying". That retry is the second actor: it issues another
// /v1/agent/archive while the first is still inside git add/commit/push on the
// same worktree.
//
// Two concurrent archives on one worktree is the harm — contending on index.lock
// at best, interleaving a snapshot commit with another's staging at worst.
//
// WHY JOIN RATHER THAN REFUSE. A refusal would also stop the overlap, and it was
// the obvious answer, but it throws away the thing the retry actually needs. The
// first archive pushes a branch and returns its name to a client that has already
// hung up, so the daemon never learns it — and preserveSandboxBeforeReap treats an
// unknown branch as a refusal to reap, so the session stays stuck exactly as it
// was. Joining turns the retry into the mechanism that RECOVERS the answer: it
// waits out the in-flight push and returns the same branch, so the loop converges
// instead of restarting a three-minute operation it will abandon again.
//
// WHY NOT CANCELLATION. Threading the request context into the git commands is the
// other option the issue names, and it requires deciding what cancelling between
// `add` and `commit` means — a half-staged index is not a state this code can
// safely leave behind. Joining needs no such decision, because nothing is
// interrupted.
//
// SCOPE: archive only, deliberately. Kill mutates the same workspace and is not
// fenced here, because blocking a teardown behind a three-minute push is a worse
// failure than the one being fixed. It is also not reachable from the path above:
// preserveSandboxBeforeReap refuses to reap when the archive result is unknown, so
// the retry loop re-archives rather than killing.
type singleFlightArchive struct {
	mu     sync.Mutex
	active *archiveAttempt
}

// archiveAttempt is one in-flight archive and its eventual result. done is closed
// once branch/err are written, which is what orders a joiner's read after the
// leader's write.
type archiveAttempt struct {
	done   chan struct{}
	branch string
	err    error
}

// archiveJoinTimeout bounds how long a joining request waits for the archive
// already running.
//
// Set to the CLIENT's own budget for this call, deliberately: a joiner that
// outwaited its caller would be holding a handler goroutine for an answer nobody
// is listening for any more, and since every refused reap produces another retry,
// those would accumulate one per retry for as long as a stalled push lasts —
// runGitCommand has no timeout of its own, so "as long as" can be unbounded.
//
// Expiring is not a regression on today's behaviour: the caller has given up by
// then either way. What it costs is the joiner's chance to recover the branch,
// which is only lost in the case where there was no answer to recover.
var archiveJoinTimeout = session.AgentArchiveCallTimeout

// do runs archive, or joins the one already running and returns its result.
func (s *singleFlightArchive) do(archive func() (string, error)) (string, error) {
	s.mu.Lock()
	if joined := s.active; joined != nil {
		s.mu.Unlock()
		timer := time.NewTimer(archiveJoinTimeout)
		defer timer.Stop()
		select {
		case <-joined.done:
			return joined.branch, joined.err
		case <-timer.C:
			// An error, not an empty branch. The caller must read this as "we do not
			// know", which is what makes it refuse to reap rather than reap onto a
			// branch that may not exist — the same rule archiveWithin states.
			return "", fmt.Errorf("an archive of this workspace has been running for over %s and has not finished; "+
				"not starting a second one against the same worktree", archiveJoinTimeout)
		}
	}
	attempt := &archiveAttempt{done: make(chan struct{})}
	s.active = attempt
	s.mu.Unlock()

	attempt.branch, attempt.err = archive()

	// Closed BEFORE the slot is cleared, so a request that grabbed this attempt
	// microseconds ago gets its result rather than blocking on a channel whose
	// writer has moved on. Clearing after also means the next caller starts a fresh
	// archive, which is correct: a completed archive is a snapshot of a moment, and
	// a later caller wants the state as it is then, not a cached branch name.
	close(attempt.done)
	s.mu.Lock()
	s.active = nil
	s.mu.Unlock()
	return attempt.branch, attempt.err
}

// AgentServerOptions configures a headless agent-server process.
type AgentServerOptions struct {
	// ListenAddr is the HTTP TCP bind address (host:port). "127.0.0.1:0" — the
	// loopback zero-config default — lets the kernel pick a free port, reported
	// back in the startup banner.
	ListenAddr string
	// RepoPath is the git repository the single workspace runs against.
	RepoPath string
	// Title is the session title for the single workspace (required).
	Title string
	// Program is the agent program to run (empty ⇒ the config default).
	Program string
	// ProgramResolved means Program is the final command selected by the outer
	// runtime and must not be offered to program_overrides a second time.
	ProgramResolved bool
	// SessionEnvPassthrough carries exact operator-approved variable names from
	// the outer runtime. Values remain in this host's environment.
	SessionEnvPassthrough []string
	// Branch is the exact branch the daemon named for this session (#5571 W2.4).
	// Empty derives it from this host's own branch_prefix, as before.
	Branch string
	// AdoptWorktree is an existing linked worktree this server binds instead of
	// creating one (#5571 W8.6). The provisioning daemon proved it present and on
	// Branch before starting this process.
	AdoptWorktree string
	// PolicyFile is the daemon-resolved operator policy (config.AgentPolicy).
	// When set, this process answers every config read from it and never reads
	// or creates anything under its own host's af home (#5571 W2.5); a file it
	// cannot read or parse fails the start rather than falling back to the host.
	PolicyFile string
	// Version is this binary's own build stamp, echoed in the startup banner so
	// the provisioning daemon can persist which agent-server generation the
	// session runs (#5571 W1.1 — reattach keys off it). Empty means the banner
	// omits it, which a persisted row reads as "pre-W1.1".
	Version string
}

// AgentServerInfo is the machine-readable startup banner the process prints to
// stdout as one JSON line the instant the listener binds. A driver reads it to
// learn the concrete bound address (port filled in for :0) and the bearer token
// to present — the same facts the daemon's tcpListenerInfo carries, surfaced on
// stdout because the agent-server has no daemon log the operator watches. The
// listener is plain HTTP (no TLS), so there is no cert path or fingerprint to
// carry; the token authenticates and the driver reaches it over a private
// network / tunnel (the docker runtime publishes a loopback port, ssh forwards
// one).
type AgentServerInfo struct {
	Addr  string `json:"addr"`
	Token string `json:"token"`
	Title string `json:"title"`
	// Version is the server binary's self-reported build stamp (#5571 W1.1).
	// omitempty keeps the wire byte-identical for a server that has none.
	Version string `json:"version,omitempty"`
	// Program is the command this host resolved and bound — the daemon sent
	// the configured name and flags; the executable's absolute path only
	// exists after the login-shell probe here (#5571 W2.1). The driver records
	// it as the runtime's launch evidence.
	Program string `json:"program,omitempty"`
}

// RunAgentServer builds the single workspace's agent-server, binds the HTTP+token
// listener, prints the startup banner to stdout, and blocks until SIGINT/SIGTERM,
// at which point it tears the workspace down. It is the process body behind
// `af agent-server`.
//
// The listener always requires the token here (unlike the daemon, where the TCP
// listener's token is loopback-exempt): the agent-server exists to be reached
// over the network by a remote daemon. The transport is plain HTTP (no TLS) — the
// token travels over it, so a driver reaches the agent-server over a private
// network or tunnel controlled by the off-box runtime. It reuses
// startTCPListener verbatim — the same token/gate wiring the daemon's
// `listen_addr` opt-in uses.
func RunAgentServer(opts AgentServerOptions, stdout io.Writer) error {
	if opts.Title == "" {
		return fmt.Errorf("agent-server requires a session title (--title)")
	}
	if opts.RepoPath == "" {
		return fmt.Errorf("agent-server requires a repository path (--repo)")
	}

	if opts.PolicyFile != "" {
		policy, perr := config.LoadAgentPolicyFile(opts.PolicyFile)
		if perr != nil {
			return fmt.Errorf("af agent-server: %w", perr)
		}
		config.InstallAgentPolicy(policy)
	}

	// LoadConfig honors cors_allowed_origins/default_program for a configured
	// host; a sandbox with no config file falls back to defaults.
	cfg, err := config.LoadConfig()
	if err != nil {
		cfg = config.DefaultConfig()
	}
	cfg.ListenAddr = opts.ListenAddr

	program := opts.Program
	if program == "" {
		program = cfg.DefaultProgram
	}
	branchPrefix := cfg.BranchPrefix

	// An unresolved program is an agent KEY (the internal transports send
	// --program-resolved for an already-configured command). Configured
	// program_overrides must apply while the key is still the key —
	// resolving "claude" to an absolute path first would make
	// program_overrides.claude unreachable (#5578 review).
	if !opts.ProgramResolved {
		program = session.ResolveProgramForPath(opts.RepoPath, program)
	}
	// The daemon sends the configured program name and flags, never its own
	// auto-detected path — this host's filesystem is where the executable must
	// exist (#5571 W2.1). Resolve it against the login-shell PATH (the spawn
	// environment — sshd's default PATH or a hook's env — is not the operator's
	// login PATH) and adopt that PATH so the agent pane inherits it. A program
	// the host cannot resolve fails the create now rather than producing a
	// live session whose agent pane exits 127.
	resolvedProgram, loginPATH, rerr := session.ResolveProgramOnHost(context.Background(), program)
	if rerr != nil {
		return fmt.Errorf("af agent-server: %w", rerr)
	}
	if loginPATH != "" {
		// The session directory holding the staged af keeps PATH precedence
		// over whatever the login profile put first (#5877).
		loginPATH = session.PinPATHDir(loginPATH, session.SessionBinDir(os.Args[0]))
		if err := os.Setenv("PATH", loginPATH); err != nil {
			log.WarningLog.Printf("agent-server: could not adopt login-shell PATH for the agent pane: %v", err)
		}
	}

	// An account-scoped sandbox proves executable attribution BEFORE any pane can
	// receive the scope: the resolved command must name an absolute executable, not
	// a bare name a workspace-relative PATH entry could answer (#5571 W7.4).
	if agent := os.Getenv(session.AccountScopeAgentEnv); agent != "" {
		if err := session.RequireAttributedAccountProgram(resolvedProgram, agent); err != nil {
			return fmt.Errorf("af agent-server: %w", err)
		}
	}

	instance, err := session.NewInstance(session.InstanceOptions{
		Title:   opts.Title,
		Path:    opts.RepoPath,
		Program: resolvedProgram,
		// Host resolution above produced the final command; ProgramResolved
		// keeps its contract — only a caller that flagged --program-resolved
		// pins it against the sandbox's own program_overrides.
		ProgramResolved:       opts.ProgramResolved && resolvedProgram != "",
		SessionEnvPassthrough: opts.SessionEnvPassthrough,
		// The outer daemon hands the callback pair to this process's environment
		// through the provisioner's env file; a sandbox provisioned WITHOUT one
		// (mint refused, or remote forwarding off) can never serve an af verb —
		// say so on the instance so the injected guidance stops teaching them
		// (#5571 W3.1).
		CallbackUnavailable: os.Getenv("AF_DAEMON_URL") == "" || os.Getenv("AF_DAEMON_TOKEN") == "",
		// The daemon holds this session as a remote workspace and admits its tabs
		// accordingly; the injected guidance must match (#5571 W3.3).
		HostedRemotely: true,
		// What /v1/health tells the daemon this server speaks; the guidance
		// follows it rather than a static floor (#5878).
		AdvertisedFeatures: agentServerWireFeatures,
		// This process's startup config is its frozen branch-naming snapshot, just
		// like the host daemon's. NewGitWorktree must not reload a later save.
		BranchPrefix: &branchPrefix,
		BranchName:   opts.Branch,
		// The in-sandbox agent-server ALWAYS runs the local runtime (tmux + git
		// worktree against RepoPath) — it IS the sandbox (§1.2). Force it explicitly
		// so a workspace whose repo config declares backend=docker/ssh/hook does
		// not recursively provision another sandbox inside itself.
		Backend: session.BackendLocal,
	})
	if err != nil {
		return fmt.Errorf("failed to build workspace instance: %w", err)
	}

	// The relaunch (W7.1) and agent swap respawn through the local runtime, and a
	// respawn that rebuilds a deleted worktree must make its stale-pane teardown
	// obligation durable first. The driving daemon installs those callbacks on ITS
	// instances; this one needs its own, backed by a marker in the session dir (#5824).
	sessionDir, sdErr := runtimetoken.SessionDir()
	if sdErr != nil {
		return fmt.Errorf("agent-server: %w", sdErr)
	}
	if err := instance.InstallDurableRebuildTeardown(sessionDir, session.RebuildTeardownKey(opts.RepoPath, opts.Title)); err != nil {
		return fmt.Errorf("agent-server: %w", err)
	}

	if opts.AdoptWorktree != "" {
		if err := instance.AdoptExistingWorktree(opts.AdoptWorktree); err != nil {
			return fmt.Errorf("agent-server: %w", err)
		}
	}

	hs := &headlessServer{
		adopted: opts.AdoptWorktree != "",
		as:      instance.AgentServer(),
		inst:    instance,
		program: resolvedProgram,
		tabs:    instance,
		title:   opts.Title,
		events:  newEventsHub(),
		// The workspace's crash-stable editor identity (#6092): repo + title.
		repoPath: opts.RepoPath,
	}

	// A previous agent-server for this workspace may have been killed hard and left
	// its editor running (#6092); end it now rather than when someone next opens one.
	if err := hs.acquireEditorNamespace(); err != nil && !errors.Is(err, errEditorWorkspaceBusy) {
		log.WarningLog.Printf("agent-server: sweeping a previous server's editor: %v", err)
	}

	// The bearer token this server expects comes from a 0600 file beside this
	// binary when the provisioning daemon placed one there (#5571 W1.1): a
	// per-session credential derived from the daemon's runtime-identity key,
	// replacing this host's shared daemon-token. It is read from the FILE —
	// never argv or the environment — so neither the token nor its path can
	// reach anything this server spawns. When the file is absent the listener
	// keeps the file-backed daemon-token of the AF home it runs under, the
	// pre-W1.1 shape.
	suppliedToken, err := runtimetoken.SuppliedAgentServerToken()
	if err != nil {
		return fmt.Errorf("agent-server: %w", err)
	}
	var listenerAuth *tcpListenerAuth
	if suppliedToken != "" {
		expected := suppliedToken
		listenerAuth = &tcpListenerAuth{
			expectedForRequest: func(*http.Request) (string, error) { return expected, nil },
		}
	}

	// The agent-server's token is MANDATORY for every peer — it exists to be
	// reached over the network by a remote daemon, so the token must never be
	// optional and loopback is NOT exempt (unlike the daemon web listener, #1696).
	// The zero-value policy is exactly that strict posture; pass it explicitly so
	// the intent is on the page and a future require_token=false in the host
	// config can never weaken this listener.
	closeTCP, tcpInfo, err := startTCPListener(hs.newMux(), cfg.ListenAddr, cfg, tokenGatePolicy{}, withoutWebShell, listenerAuth, nil)
	if err != nil {
		return fmt.Errorf("failed to start agent-server listener on %q: %w", opts.ListenAddr, err)
	}

	info := AgentServerInfo{
		Addr:    tcpInfo.Addr,
		Token:   tcpInfo.Token,
		Title:   opts.Title,
		Version: opts.Version,
		Program: resolvedProgram,
	}
	if suppliedToken != "" {
		// An auth override leaves tcpInfo.Token empty — the listener has no
		// single credential to advertise — so the banner names the supplied one
		// directly. The provisioning daemon verifies it matches what it derived.
		info.Token = suppliedToken
	}
	// The startup banner is a single JSON line on stdout — the agent-server's only
	// channel to hand a driver the address+token, since it runs headless with
	// no daemon log to read (mirrors tcpListenerInfo's role for the daemon).
	if data, mErr := json.Marshal(info); mErr == nil {
		fmt.Fprintln(stdout, string(data))
	}
	log.InfoLog.Printf("af agent-server listening on %s (plain HTTP) for workspace %q", info.Addr, info.Title)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	// Clean teardown: stop accepting connections, then kill the workspace so no
	// tmux session or worktree is orphaned. Durability of in-progress work is the
	// driving daemon's job (archive = push branch, epic §5), not the agent-server's.
	_ = closeTCP.close()
	if err := hs.stopEditor(); err != nil {
		log.WarningLog.Printf("agent-server: workspace editor teardown on shutdown: %v", err)
	}
	if err := hs.shutdownWorkspace(); err != nil {
		log.WarningLog.Printf("agent-server: workspace teardown on shutdown: %v", err)
	}
	return nil
}

// newMux builds the single-workspace route table. The control plane mirrors the
// session.AgentServer interface 1:1 under /v1/agent/* (the contract PR2's
// remoteAgentServer speaks), and the WS PTY + events planes are served at the
// SAME paths the daemon serves so a driver reuses the identical client
// (apiclient.DialStream dials /v1/sessions/{id}/stream). Every route is wrapped
// in the token gate by startTCPListener, exactly like the daemon mux.
func (hs *headlessServer) newMux() *http.ServeMux {
	mux := http.NewServeMux()

	// Liveness alias, matching the daemon's GET /v1/health.
	mux.HandleFunc("GET /v1/health", hs.healthHandler)

	// Control REST — the session.AgentServer interface over the shared envelope
	// dispatch (rpcHandler: POST-only, {data,error} envelope, 400/413/500 mapping).
	mux.HandleFunc("/v1/agent/provision", rpcHandler(hs.Provision))
	mux.HandleFunc("/v1/agent/launch", rpcHandler(hs.Launch))
	mux.HandleFunc("/v1/agent/restore-tabs", rpcHandler(hs.RestoreTabs))
	mux.HandleFunc("/v1/agent/expose", rpcHandler(hs.Expose))
	mux.HandleFunc("/v1/agent/snapshot", rpcHandler(hs.Snapshot))
	mux.HandleFunc("/v1/agent/preview", rpcHandler(hs.Preview))
	mux.HandleFunc("/v1/agent/alive", rpcHandler(hs.Alive))
	mux.HandleFunc("/v1/agent/send-prompt", rpcHandler(hs.SendPrompt))
	// Compatibility tripwire for older daemons. The behavior is gone, but an
	// old caller gets migration guidance instead of a successful no-op.
	mux.HandleFunc("/v1/agent/tap-enter", rpcHandler(hs.RemovedAutoYes))
	mux.HandleFunc("/v1/agent/archive", rpcHandler(hs.Archive))
	mux.HandleFunc("/v1/agent/kill", rpcHandler(hs.Kill))
	mux.HandleFunc("/v1/agent/workspace-facts", rpcHandler(hs.WorkspaceFacts))
	mux.HandleFunc("/v1/agent/stop", rpcHandler(hs.StopPreserving))
	// W7.1: stop the agent and start it again in this workspace, resuming the
	// provider's conversation. Advertised as the "relaunch" feature.
	mux.HandleFunc("/v1/agent/relaunch", rpcHandler(hs.Relaunch))
	mux.HandleFunc(agentproto.EditorPath, rpcHandler(hs.Editor))
	mux.HandleFunc(agentproto.EditorStopPath, rpcHandler(hs.EditorStop))
	// W7.5: swap the agent for another in this workspace. Advertised as the
	// "agent-swap" feature.
	mux.HandleFunc("/v1/agent/swap-agent", rpcHandler(hs.SwapAgent))
	// W7.6: rotate the account scope and relaunch. Advertised as the
	// "account-swap" feature.
	mux.HandleFunc("/v1/agent/swap-account", rpcHandler(hs.SwapAccount))
	mux.HandleFunc("/v1/agent/swap-account-status", rpcHandler(hs.SwapAccountStatus))
	// The W4.3 tab surface (#5571): stable-id create/close and the roster with
	// exit evidence, over the same envelope dispatch. These are the routes the
	// "tab-api" feature token on /v1/health advertises.
	mux.HandleFunc("/v1/agent/tab-create", rpcHandler(hs.TabCreate))
	mux.HandleFunc("/v1/agent/tab-close", rpcHandler(hs.TabClose))
	mux.HandleFunc("/v1/agent/tabs", rpcHandler(hs.Tabs))
	// #5997: the daemon names the agent tab. Advertised as "agent-tab-id".
	mux.HandleFunc("/v1/agent/agent-tab-id", rpcHandler(hs.AgentTabID))

	// WS data plane — same paths, same broker, same wire as the daemon (§1.1: all
	// over the same routes the daemon mux already serves).
	mux.HandleFunc("GET /v1/sessions/{id}/stream", hs.streamHandler)
	mux.HandleFunc("GET /v1/sessions/{id}/stream-info", hs.streamInfoHandler)
	mux.HandleFunc("GET /v1/events", hs.eventsHandler)

	// Loopback port relay (W5.1): raw TCP to 127.0.0.1:<port> in the workspace.
	mux.HandleFunc("GET "+agentproto.RelayPath, hs.relayHandler)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeHTTPError(w, r, http.StatusNotFound, fmt.Errorf("unknown route %q", r.URL.Path))
	})
	return mux
}

// agentServerWireFeatures is the capability token list /v1/health advertises
// (#5571 W4.3): `version` says which af build answered, `features` says which
// surfaces it speaks, so a driving daemon can learn what a sandbox agent-server
// offers BEFORE calling a route that might not exist on an older build — the
// additive-contract answer to wire drift, where a missing token means degrade
// rather than fail. Tokens name route clusters, not the implementation, so a
// client tests "tab-api" rather than inferring it from the version string.
var agentServerWireFeatures = []string{
	// "tab-api": POST /v1/agent/tab-create, /v1/agent/tab-close, and
	// /v1/agent/tabs — stable-id create/close plus the roster with exit
	// evidence.
	"tab-api",
	// "tab-id-addressing": ?tab_id= on the WS stream and tab_id on preview —
	// the id-addressed data plane (ws_pty.go's bindTab picks the binding).
	"tab-id-addressing",
	// "agent-tab-id": POST /v1/agent/agent-tab-id — the daemon tells the sandbox
	// which stable id its agent tab carries, so the agent tab is addressed by id
	// like every other tab (#5997).
	session.AgentTabIDFeature,
	// "relaunch": POST /v1/agent/relaunch — stop the agent and start it again
	// in this workspace with the provider's resume (#5571 W7.1).
	session.RelaunchFeature,
	// "editor": POST /v1/agent/editor and /v1/agent/relay?target=editor — the
	// workspace's own code-server, relayed to the daemon (#5571 W5.4).
	agentproto.EditorFeature,
	// "agent-swap": POST /v1/agent/swap-agent — replace the agent with another
	// in this workspace, resolving the new agent's program on this host (#5571 W7.5).
	session.SwapAgentFeature,
	// "account-swap": POST /v1/agent/swap-account — rotate the staged account
	// scope in and relaunch the agent on it (#5571 W7.6).
	session.AccountSwapFeature,
	// "tab-restore": a restart adoption's Launch carries the roster; survivors
	// are rebound, never re-run (W1.4, #6162).
	session.TabRestoreFeature,
	// "tab-restore-retry": POST /v1/agent/restore-tabs, the same rebind on a
	// running server (#6235).
	session.TabRestoreRetryFeature,
}

// agentHealthResponse is GET /v1/health's payload: liveness, the build version
// the banner also carries, and the wire features this server speaks (#5571
// W4.3).
type agentHealthResponse struct {
	OK       bool     `json:"ok"`
	Version  string   `json:"version"`
	Features []string `json:"features"`
}

// healthHandler answers GET /v1/health with liveness plus the version/features
// advertisement — the property that makes the route a capability probe, not
// just a ping.
func (hs *headlessServer) healthHandler(w http.ResponseWriter, r *http.Request) {
	writeHTTPSuccess(w, r, agentHealthResponse{
		OK:       true,
		Version:  Version(),
		Features: agentServerWireFeatures,
	})
}

// --- control REST: 1:1 mirror of session.AgentServer -----------------------

// agentLifecycleRequest carries the firstTimeSetup flag Provision/Launch take: a
// fresh create materializes the worktree and spawns; a restore reconnects.
type agentLifecycleRequest struct {
	FirstTimeSetup bool `json:"first_time_setup"`
	// RestartAdoption marks the launch that adopts a restarted agent-server's
	// surviving workspace (W1.4); only it is refused a guessed respawn.
	RestartAdoption bool `json:"restart_adoption,omitempty"`
	// SiblingTabs: non-agent tabs the daemon's row records (restart adoption).
	SiblingTabs int `json:"sibling_tabs,omitempty"`
	// Tabs: the surviving-tab roster (feature tab-restore).
	Tabs []session.TabData `json:"tabs,omitempty"`
	// Conversation is the daemon's recorded conversation for this launch to
	// resume (#5571 W7.2); honored only where this host's provider store holds it.
	Conversation *session.AgentConversationData `json:"conversation,omitempty"`
}

// agentOKResponse is the acknowledgement for the no-return interface methods.
type agentOKResponse struct {
	OK bool `json:"ok"`
	// Conversation is the conversation a launch came up on (#5571 W7.2); the
	// daemon records it as it does for a local session.
	Conversation *session.AgentConversationData `json:"conversation,omitempty"`
	// TabsRestored / TabsGone answer a restart adoption's Tabs.
	TabsRestored []string `json:"tabs_restored,omitempty"`
	TabsGone     []string `json:"tabs_gone,omitempty"`
}

func (hs *headlessServer) Provision(req agentLifecycleRequest, resp *agentOKResponse) error {
	if hs.inst != nil && req.SiblingTabs > 0 {
		hs.inst.SetPersistedSiblingTabs(req.SiblingTabs)
	}
	if err := hs.as.Provision(req.FirstTimeSetup); err != nil {
		return err
	}
	resp.OK = true
	return nil
}

func (hs *headlessServer) launchWorkspace(req agentLifecycleRequest, resp *agentOKResponse) error {
	// A restore-launch that carries the recorded conversation must not respawn a
	// vanished agent onto a guessed one (#4797): see RefuseGuessedRespawn.
	if req.RestartAdoption && !req.FirstTimeSetup && req.Conversation != nil && hs.inst != nil {
		if err := hs.inst.RefuseGuessedRespawn(*req.Conversation); err != nil {
			return err
		}
	}
	if hs.adopted && !req.FirstTimeSetup && hs.inst != nil {
		return hs.launchAdopted(req, resp)
	}
	var codexSnap session.ConversationCaptureSnapshot
	codex := tmux.DetectAgentFromCommand(hs.program) == tmux.ProgramCodex
	if hs.inst != nil && req.FirstTimeSetup && req.Conversation != nil && req.Conversation.HasID() {
		// A resume flag for a transcript this host does not hold makes the agent
		// exit on launch, so a fresh workspace starts a new conversation instead.
		if session.ConversationResumableHere(hs.program, hs.inst.GetWorktreePath(), *req.Conversation) {
			hs.inst.SetResumeConversation(*req.Conversation)
		} else {
			log.InfoLog.Printf("agent-server: conversation %s is not in this host's %s store; starting a new conversation",
				req.Conversation.ID, req.Conversation.Agent)
		}
	}
	if codex && hs.inst != nil {
		// Before the spawn, so the capture can tell the new rollout from old ones.
		// Correlated against the directory the launch command actually runs the
		// agent in (an `env -C DIR codex` moves it off the worktree), not the
		// worktree; unscoped when that is not provable (#5861).
		codexSnap = session.BeginConversationCaptureAtCodexHomeAndWorkingDir(
			codexHomeFor(hs.program, hs.inst.GetWorktreePath()),
			session.LaunchCaptureWorkingDir(hs.program, hs.inst.GetWorktreePath()))
	}
	if err := hs.as.Launch(req.FirstTimeSetup); err != nil {
		return err
	}
	resp.OK = true
	if hs.inst == nil {
		return nil
	}
	if conv := hs.inst.AgentConversation(); conv.HasID() {
		resp.Conversation = &conv
	} else if codex {
		hs.captureCodexConversation(codexSnap)
	}
	return nil
}

// agentExposeResponse is StreamEndpoint over the wire — where this session's data
// plane is reachable. For the local runtime it is Local=true with an empty URL;
// the driver then dials the WS stream on this same listener.
type agentExposeResponse struct {
	Local bool   `json:"local"`
	URL   string `json:"url"`
}

func (hs *headlessServer) Expose(_ struct{}, resp *agentExposeResponse) error {
	ep, err := hs.as.Expose()
	if err != nil {
		return err
	}
	resp.Local = ep.Local
	resp.URL = ep.URL
	return nil
}

// agentSnapshotResponse is Observation over the wire — the non-interactive poll
// the daemon reads each tick.
type agentSnapshotResponse struct {
	Updated     bool                      `json:"updated"`
	Baseline    bool                      `json:"baseline"`
	HasPrompt   bool                      `json:"has_prompt"`
	Content     string                    `json:"content"`
	ModelChange *session.AgentModelChange `json:"model_change,omitempty"`
	// Captured reports whether this server's own pane probe actually
	// captured (#5221 review): the daemon's observation streak must not count
	// a snapshot whose in-sandbox capture-pane failed. Always sent so the
	// client can distinguish a reported miss from a server that predates the
	// field.
	Captured bool `json:"captured"`
	// Conversation is the conversation identity once the server has one
	// (#5571 W7.2); omitted until then.
	Conversation *session.AgentConversationData `json:"conversation,omitempty"`
}

func (hs *headlessServer) Snapshot(_ struct{}, resp *agentSnapshotResponse) error {
	obs, err := hs.as.Snapshot()
	if err != nil {
		return err
	}
	resp.Updated = obs.Updated
	resp.Baseline = obs.Baseline
	resp.HasPrompt = obs.HasPrompt
	resp.Content = obs.Content
	resp.ModelChange = obs.ModelChange
	resp.Captured = obs.Captured
	if hs.inst != nil {
		if conv := hs.inst.AgentConversation(); conv.HasID() {
			resp.Conversation = &conv
		}
	}
	return nil
}

// agentPreviewRequest selects a tab's content; Full=true returns the entire
// scrollback history, false the visible screen. TabID addresses the tab by its
// stable id and takes precedence over the ordinal Tab, exactly as the daemon's
// own PreviewRequest resolves — a stale id is a 404 (session.ErrTabGone through
// rpcHandler), never a silent fallback to whatever sits at Tab (#5571 W4.3).
type agentPreviewRequest struct {
	TabID string `json:"tab_id,omitempty"`
	Tab   int    `json:"tab"`
	Full  bool   `json:"full"`
	// Grid asks for the per-row grid capture with cursor and size (#5571 W4.2).
	// A server that predates it ignores the field and never sets HasGrid, which
	// is how the client knows to keep the joined Content.
	Grid bool `json:"grid,omitempty"`
}

type agentPreviewResponse struct {
	Content  string         `json:"content"`
	Modes    terminal.Modes `json:"terminal_modes,omitempty"`
	HasModes bool           `json:"has_terminal_modes,omitempty"`
	// LinesAbove is the scrollback above Content; LinesAboveKnown says whether
	// it was measured (0 and "unmeasured" differ, #3169).
	LinesAbove      int  `json:"lines_above,omitempty"`
	LinesAboveKnown bool `json:"lines_above_known,omitempty"`
	// The grid answer: Grid is one line per pane row, the cursor is 0-based,
	// Rows/Cols are the pane's measured size when HasSize.
	HasGrid   bool   `json:"has_grid,omitempty"`
	Grid      string `json:"grid,omitempty"`
	HasCursor bool   `json:"has_cursor,omitempty"`
	CursorRow int    `json:"cursor_row,omitempty"`
	CursorCol int    `json:"cursor_col,omitempty"`
	HasSize   bool   `json:"has_size,omitempty"`
	Rows      uint16 `json:"rows,omitempty"`
	Cols      uint16 `json:"cols,omitempty"`
}

func (hs *headlessServer) Preview(req agentPreviewRequest, resp *agentPreviewResponse) error {
	var snapshot session.PreviewSnapshot
	var err error
	if req.TabID != "" {
		snapshot, err = hs.as.PreviewByID(req.TabID, req.Full)
	} else {
		snapshot, err = hs.as.Preview(req.Tab, req.Full)
	}
	if err != nil {
		return err
	}
	resp.Content = snapshot.Content
	resp.Modes = snapshot.Modes
	resp.HasModes = snapshot.HasModes
	resp.LinesAbove, resp.LinesAboveKnown = snapshot.LinesAbove, snapshot.LinesAboveKnown
	if !req.Grid || req.Full {
		return nil
	}
	gs, ok := hs.as.(session.PaneGridServer)
	if !ok {
		return nil
	}
	var pane session.PaneSnapshot
	var gerr error
	if req.TabID != "" {
		pane, gerr = gs.PreviewGridByID(req.TabID)
	} else {
		pane, gerr = gs.PreviewGrid(req.Tab)
	}
	if gerr != nil {
		// The joined Content above is still a valid answer; the client falls
		// back to it when HasGrid is absent.
		return nil
	}
	resp.HasGrid, resp.Grid = true, string(pane.Screen)
	resp.HasCursor, resp.CursorRow, resp.CursorCol = pane.HasCursor, pane.CursorRow, pane.CursorCol
	resp.HasSize, resp.Rows, resp.Cols = pane.HasSize, pane.Rows, pane.Cols
	if pane.HasModes {
		resp.Modes, resp.HasModes = pane.Modes, true
	}
	return nil
}

type agentAliveResponse struct {
	Alive bool `json:"alive"`
}

func (hs *headlessServer) Alive(_ struct{}, resp *agentAliveResponse) error {
	alive, err := hs.as.Alive()
	if err != nil {
		// Propagate rather than answering a confident `alive:false`. This is the
		// SERVER end of the probe the daemon uses to decide whether to re-provision
		// a sandbox, and the caller must be able to tell "the agent is gone" from
		// "nobody could tell" — reporting the latter as the former is what
		// re-provisions over live work (#1794). In practice the in-sandbox server
		// probes its own tmux in-process and never errors.
		return err
	}
	resp.Alive = alive
	return nil
}

type agentSendPromptRequest struct {
	Prompt string `json:"prompt"`
}

type agentSendPromptResponse struct {
	OK     bool                         `json:"ok"`
	Status session.PromptDeliveryStatus `json:"status"`
}

func (hs *headlessServer) SendPrompt(req agentSendPromptRequest, resp *agentSendPromptResponse) error {
	status, err := session.SendPromptWithStatus(hs.as, req.Prompt)
	if err != nil {
		return err
	}
	resp.OK = true
	resp.Status = status
	return nil
}

func (hs *headlessServer) RemovedAutoYes(_ struct{}, _ *agentOKResponse) error {
	return config.RemovedAutoYesError()
}

// agentArchiveResponse carries the pushed branch back to the driving daemon so
// it can record which branch to clone on a later restore (#1592 Phase 4 PR6).
type agentArchiveResponse struct {
	Branch string `json:"branch"`
}

// Archive commits + pushes the workspace's branch to origin, making it durable
// on GitHub before the daemon reaps this sandbox (#1592 Phase 4 PR6).
func (hs *headlessServer) Archive(_ struct{}, resp *agentArchiveResponse) error {
	// Single-flighted: a retry that arrives while an archive is still running joins
	// it instead of starting a second one on the same worktree (#2997 finding 2).
	branch, err := hs.archive.do(hs.as.Archive)
	if err != nil {
		return err
	}
	resp.Branch = branch
	return nil
}

func (hs *headlessServer) Kill(_ struct{}, resp *agentOKResponse) error {
	if err := hs.stopEditor(); err != nil {
		log.WarningLog.Printf("agent-server: workspace editor teardown: %v", err)
	}
	if err := hs.as.Kill(false); err != nil {
		return err
	}
	resp.OK = true
	return nil
}

// --- WS data plane: single-workspace variants of the daemon handlers -------

// streamHandler upgrades GET /v1/sessions/{id}/stream to a WebSocket and fans the
// single workspace's PTY stream. It is the controlServer.streamHandler shape with
// the Manager lookup replaced by the one AgentServer this server owns — the id is
// vestigial (there is exactly one workspace) but the path matches the daemon so a
// driver reuses the identical client. Subscribe happens BEFORE the upgrade so a
// bad cursor or an un-launched workspace returns an HTTP error envelope.
func (hs *headlessServer) streamHandler(w http.ResponseWriter, r *http.Request) {
	since, err := parseSince(r.URL.Query().Get("since"))
	if err != nil {
		writeHTTPError(w, r, http.StatusBadRequest, err)
		return
	}
	tab, err := parseTab(r.URL.Query().Get("tab"))
	if err != nil {
		writeHTTPError(w, r, http.StatusBadRequest, err)
		return
	}
	// The same binding selection the daemon's streamHandler makes (#5571 W4.3):
	// a ?tab_id= binds the id-native plane — which the in-sandbox server's local
	// AgentServer always has — and a missing ?tab_id= stays an ordinal for
	// legacy drivers that predate the parameter. This supersedes the "ordinal
	// only" rule this handler used to carry: the sandbox's tab set is no longer
	// fixed now that /v1/agent/tab-create can grow it, so an id-addressed
	// connection must NEVER re-resolve through a shifting ordinal.
	binding, err := bindTab(hs.as, hs.inst, r.URL.Query().Get("tab_id"), tab)
	if err != nil {
		writeHTTPError(w, r, httpStatusForTab(err), err)
		return
	}
	sub, err := binding.subscribe(since)
	if err != nil {
		writeHTTPError(w, r, httpStatusForTab(err), err)
		return
	}
	w.Header().Set(streamSeqHeader, strconv.FormatUint(uint64(sub.Seq()), 10))
	// The token gate (withAuth) and CORS allow-list already authenticated the
	// handshake; InsecureSkipVerify defers the Origin check to that gate, exactly
	// as the daemon's streamHandler does.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		_ = sub.Close()
		return
	}
	servePTYStream(binding, sub, conn)
}

// streamInfoHandler answers GET /v1/sessions/{id}/stream-info with where the
// stream is reachable. For the local runtime that is the relative stream path on
// this same listener.
func (hs *headlessServer) streamInfoHandler(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ep, err := hs.as.Expose()
	if err != nil {
		writeHTTPError(w, r, http.StatusInternalServerError, err)
		return
	}
	resp := streamInfoResponse{Local: ep.Local}
	if ep.URL != "" {
		resp.URL = ep.URL
	} else {
		resp.URL = localStreamPath(authoritativeStreamTarget(id, ""))
	}
	writeHTTPSuccess(w, r, resp)
}

// eventsHandler upgrades GET /v1/events for surface parity with the daemon. The
// hub is quiet on a single-workspace server (no orchestrator mutations), so this
// proves the plane upgrades and stays alive under the token gate rather than
// carrying session lifecycle events.
func (hs *headlessServer) eventsHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	serveEvents(hs.events, conn)
}

// WorkspaceFacts measures this server's workspace for the daemon's kill
// confirmation (#5571 W8.2). Read-only: git status/rev-list/for-each-ref/reflog.
func (hs *headlessServer) WorkspaceFacts(_ struct{}, resp *session.WorkspaceFacts) error {
	if hs.inst == nil {
		return fmt.Errorf("this agent-server has no workspace")
	}
	*resp = session.MeasureWorkspaceAt(hs.inst.GetWorktreePath(), hs.inst.Path)
	return nil
}

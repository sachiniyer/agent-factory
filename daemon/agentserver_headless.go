package daemon

import (
	"encoding/json"
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
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
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
	// as is the single workspace's agent-server — the local in-process
	// implementation over tmux (same runtime as the daemon's local sessions),
	// exposed here over the wire.
	as session.AgentServer
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

	instance, err := session.NewInstance(session.InstanceOptions{
		Title:                 opts.Title,
		Path:                  opts.RepoPath,
		Program:               program,
		ProgramResolved:       opts.ProgramResolved && opts.Program != "",
		SessionEnvPassthrough: opts.SessionEnvPassthrough,
		// The in-sandbox agent-server ALWAYS runs the local runtime (tmux + git
		// worktree against RepoPath) — it IS the sandbox (§1.2). Force it explicitly
		// so a workspace whose repo config declares backend=docker/ssh/hook does
		// not recursively provision another sandbox inside itself.
		Backend: session.BackendLocal,
	})
	if err != nil {
		return fmt.Errorf("failed to build workspace instance: %w", err)
	}

	hs := &headlessServer{
		as:     instance.AgentServer(),
		title:  opts.Title,
		events: newEventsHub(),
	}

	// The agent-server's token is MANDATORY for every peer — it exists to be
	// reached over the network by a remote daemon, so the token must never be
	// optional and loopback is NOT exempt (unlike the daemon web listener, #1696).
	// The zero-value policy is exactly that strict posture; pass it explicitly so
	// the intent is on the page and a future require_token=false in the host
	// config can never weaken this listener.
	closeTCP, tcpInfo, err := startTCPListener(hs.newMux(), cfg.ListenAddr, cfg, tokenGatePolicy{}, withoutWebShell, nil, nil)
	if err != nil {
		return fmt.Errorf("failed to start agent-server listener on %q: %w", opts.ListenAddr, err)
	}

	info := AgentServerInfo{
		Addr:  tcpInfo.Addr,
		Token: tcpInfo.Token,
		Title: opts.Title,
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
	// driving daemon's job (archive = push branch, epic §5), not this dark PR's.
	_ = closeTCP()
	if err := hs.as.Kill(); err != nil {
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

	// WS data plane — same paths, same broker, same wire as the daemon (§1.1: all
	// over the same routes the daemon mux already serves).
	mux.HandleFunc("GET /v1/sessions/{id}/stream", hs.streamHandler)
	mux.HandleFunc("GET /v1/sessions/{id}/stream-info", hs.streamInfoHandler)
	mux.HandleFunc("GET /v1/events", hs.eventsHandler)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeHTTPError(w, r, http.StatusNotFound, fmt.Errorf("unknown route %q", r.URL.Path))
	})
	return mux
}

// healthHandler answers GET /v1/health with a trivial liveness envelope.
func (hs *headlessServer) healthHandler(w http.ResponseWriter, r *http.Request) {
	writeHTTPSuccess(w, r, map[string]bool{"ok": true})
}

// --- control REST: 1:1 mirror of session.AgentServer -----------------------

// agentLifecycleRequest carries the firstTimeSetup flag Provision/Launch take: a
// fresh create materializes the worktree and spawns; a restore reconnects.
type agentLifecycleRequest struct {
	FirstTimeSetup bool `json:"first_time_setup"`
}

// agentOKResponse is the acknowledgement for the no-return interface methods.
type agentOKResponse struct {
	OK bool `json:"ok"`
}

func (hs *headlessServer) Provision(req agentLifecycleRequest, resp *agentOKResponse) error {
	if err := hs.as.Provision(req.FirstTimeSetup); err != nil {
		return err
	}
	resp.OK = true
	return nil
}

func (hs *headlessServer) Launch(req agentLifecycleRequest, resp *agentOKResponse) error {
	if err := hs.as.Launch(req.FirstTimeSetup); err != nil {
		return err
	}
	resp.OK = true
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
	HasPrompt   bool                      `json:"has_prompt"`
	Content     string                    `json:"content"`
	ModelChange *session.AgentModelChange `json:"model_change,omitempty"`
}

func (hs *headlessServer) Snapshot(_ struct{}, resp *agentSnapshotResponse) error {
	obs, err := hs.as.Snapshot()
	if err != nil {
		return err
	}
	resp.Updated = obs.Updated
	resp.HasPrompt = obs.HasPrompt
	resp.Content = obs.Content
	resp.ModelChange = obs.ModelChange
	return nil
}

// agentPreviewRequest selects tab Tab's content; Full=true returns the entire
// scrollback history, false the visible screen.
type agentPreviewRequest struct {
	Tab  int  `json:"tab"`
	Full bool `json:"full"`
}

type agentPreviewResponse struct {
	Content  string         `json:"content"`
	Modes    terminal.Modes `json:"terminal_modes,omitempty"`
	HasModes bool           `json:"has_terminal_modes,omitempty"`
}

func (hs *headlessServer) Preview(req agentPreviewRequest, resp *agentPreviewResponse) error {
	snapshot, err := hs.as.Preview(req.Tab, req.Full)
	if err != nil {
		return err
	}
	resp.Content = snapshot.Content
	resp.Modes = snapshot.Modes
	resp.HasModes = snapshot.HasModes
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
	if err := hs.as.Kill(); err != nil {
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
	sub, err := hs.as.Subscribe(tab, since)
	if err != nil {
		writeHTTPError(w, r, http.StatusInternalServerError, err)
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
	// The in-sandbox headless server is addressed by ordinal only: the orchestrator's
	// remoteAgentServer has already resolved any stable ?tab_id= to an index before
	// forwarding it here (#1738), and a sandbox's tab set is fixed for its lifetime,
	// so there is nothing to re-resolve — pin the ordinal.
	servePTYStream(ordinalTabBinding{as: hs.as, tab: tab}, sub, conn)
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
		resp.URL = localStreamPath(id, "")
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

package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/coder/websocket"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// The headless single-workspace agent-server (#1592 Phase 4 PR1) — the process
// that will later run INSIDE each docker/ssh sandbox (§1.1). It is the mirror
// image of the daemon's in-process localAgentServer: where the daemon drives a
// local session through session.AgentServer directly, this exposes ONE session's
// AgentServer over the exact HTTP/WS+TLS+token wire the daemon already speaks, so
// a remote daemon (PR2's remoteAgentServer) can drive it across the process
// boundary exactly like the in-process one.
//
// It is deliberately NOT the orchestrator (§1.1): no task scheduler, no watch
// supervisor, no multi-session Manager, no disk-state ownership. It owns exactly
// one live session.AgentServer and exposes it. The workspace is DARK in this PR:
// nothing provisions a sandbox to run it (PR4/PR5) and nothing in the daemon
// drives it yet (PR2) — it is a standalone `af agent-server` you run and hit
// directly.
//
// Reuse, not reimplementation: the TLS+token listener (startTCPListener), the
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
}

// AgentServerOptions configures a headless agent-server process.
type AgentServerOptions struct {
	// ListenAddr is the TLS TCP bind address (host:port). "127.0.0.1:0" — the
	// loopback zero-config default — lets the kernel pick a free port, reported
	// back in the startup banner.
	ListenAddr string
	// RepoPath is the git repository the single workspace runs against.
	RepoPath string
	// Title is the session title for the single workspace (required).
	Title string
	// Program is the agent program to run (empty ⇒ the config default).
	Program string
	// AutoYes enables the agent-server's AutoYes accept for the workspace.
	AutoYes bool
}

// AgentServerInfo is the machine-readable startup banner the process prints to
// stdout as one JSON line the instant the listener binds. A driver reads it to
// learn the concrete bound address (port filled in for :0), the bearer token to
// present, and the self-signed cert path/fingerprint to TOFU-pin — the same
// three facts the daemon's tcpListenerInfo carries, surfaced on stdout because
// the agent-server has no daemon log the operator watches.
type AgentServerInfo struct {
	Addr        string `json:"addr"`
	Token       string `json:"token"`
	Fingerprint string `json:"fingerprint"`
	SelfSigned  bool   `json:"self_signed"`
	CertPath    string `json:"cert_path"`
	Title       string `json:"title"`
}

// RunAgentServer builds the single workspace's agent-server, binds the TLS+token
// listener, prints the startup banner to stdout, and blocks until SIGINT/SIGTERM,
// at which point it tears the workspace down. It is the process body behind
// `af agent-server`.
//
// The listener is ALWAYS TLS + token here (unlike the daemon, where the TCP
// listener is off-by-default): the agent-server exists to be reached over the
// network by a remote daemon, so the bearer token must never ride the wire in the
// clear. It reuses startTCPListener verbatim — the same cert/token/gate wiring the
// daemon's `listen_addr` opt-in uses.
func RunAgentServer(opts AgentServerOptions, stdout io.Writer) error {
	if opts.Title == "" {
		return fmt.Errorf("agent-server requires a session title (--title)")
	}
	if opts.RepoPath == "" {
		return fmt.Errorf("agent-server requires a repository path (--repo)")
	}

	// LoadConfig honors tls_cert/tls_key/cors_allowed_origins/default_program for
	// a configured host; a sandbox with no config file falls back to defaults.
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
		Title:   opts.Title,
		Path:    opts.RepoPath,
		Program: program,
		AutoYes: opts.AutoYes,
	})
	if err != nil {
		return fmt.Errorf("failed to build workspace instance: %w", err)
	}

	hs := &headlessServer{
		as:     instance.AgentServer(),
		title:  opts.Title,
		events: newEventsHub(),
	}

	closeTCP, tcpInfo, err := startTCPListener(hs.newMux(), cfg)
	if err != nil {
		return fmt.Errorf("failed to start agent-server listener on %q: %w", opts.ListenAddr, err)
	}

	info := AgentServerInfo{
		Addr:        tcpInfo.Addr,
		Token:       tcpInfo.Token,
		Fingerprint: tcpInfo.Fingerprint,
		SelfSigned:  tcpInfo.SelfSigned,
		CertPath:    resolveCertPath(cfg, tcpInfo.SelfSigned),
		Title:       opts.Title,
	}
	// The startup banner is a single JSON line on stdout — the agent-server's only
	// channel to hand a driver the address+token+pin, since it runs headless with
	// no daemon log to read (mirrors tcpListenerInfo's role for the daemon).
	if data, mErr := json.Marshal(info); mErr == nil {
		fmt.Fprintln(stdout, string(data))
	}
	log.InfoLog.Printf("af agent-server listening on %s (self-signed=%v) for workspace %q", info.Addr, info.SelfSigned, info.Title)
	log.InfoLog.Printf("  cert fingerprint: %s", info.Fingerprint)

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

// resolveCertPath returns the certificate path a client TOFU-pins. For the
// self-signed default it is the generated cert in the af home; for a user cert it
// is the configured tls_cert (verified against system roots, so the pin is
// advisory).
func resolveCertPath(cfg *config.Config, selfSigned bool) string {
	if !selfSigned {
		return cfg.TLSCert
	}
	dir, err := config.GetConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, daemonTLSCertFileName)
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
	mux.HandleFunc("/v1/agent/tap-enter", rpcHandler(hs.TapEnter))
	mux.HandleFunc("/v1/agent/kill", rpcHandler(hs.Kill))

	// WS data plane — same paths, same broker, same wire as the daemon (§1.1: all
	// over the same routes the daemon mux already serves).
	mux.HandleFunc("GET /v1/sessions/{id}/stream", hs.streamHandler)
	mux.HandleFunc("GET /v1/sessions/{id}/stream-info", hs.streamInfoHandler)
	mux.HandleFunc("GET /v1/events", hs.eventsHandler)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeHTTPError(w, http.StatusNotFound, fmt.Errorf("unknown route %q", r.URL.Path))
	})
	return mux
}

// healthHandler answers GET /v1/health with a trivial liveness envelope.
func (hs *headlessServer) healthHandler(w http.ResponseWriter, r *http.Request) {
	writeHTTPSuccess(w, map[string]bool{"ok": true})
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
	Updated   bool   `json:"updated"`
	HasPrompt bool   `json:"has_prompt"`
	Content   string `json:"content"`
}

func (hs *headlessServer) Snapshot(_ struct{}, resp *agentSnapshotResponse) error {
	obs, err := hs.as.Snapshot()
	if err != nil {
		return err
	}
	resp.Updated = obs.Updated
	resp.HasPrompt = obs.HasPrompt
	resp.Content = obs.Content
	return nil
}

// agentPreviewRequest selects tab Tab's content; Full=true returns the entire
// scrollback history, false the visible screen.
type agentPreviewRequest struct {
	Tab  int  `json:"tab"`
	Full bool `json:"full"`
}

type agentPreviewResponse struct {
	Content string `json:"content"`
}

func (hs *headlessServer) Preview(req agentPreviewRequest, resp *agentPreviewResponse) error {
	content, err := hs.as.Preview(req.Tab, req.Full)
	if err != nil {
		return err
	}
	resp.Content = content
	return nil
}

type agentAliveResponse struct {
	Alive bool `json:"alive"`
}

func (hs *headlessServer) Alive(_ struct{}, resp *agentAliveResponse) error {
	resp.Alive = hs.as.Alive()
	return nil
}

type agentSendPromptRequest struct {
	Prompt string `json:"prompt"`
}

func (hs *headlessServer) SendPrompt(req agentSendPromptRequest, resp *agentOKResponse) error {
	if err := hs.as.SendPrompt(req.Prompt); err != nil {
		return err
	}
	resp.OK = true
	return nil
}

func (hs *headlessServer) TapEnter(_ struct{}, resp *agentOKResponse) error {
	hs.as.TapEnter()
	resp.OK = true
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
		writeHTTPError(w, http.StatusBadRequest, err)
		return
	}
	tab, err := parseTab(r.URL.Query().Get("tab"))
	if err != nil {
		writeHTTPError(w, http.StatusBadRequest, err)
		return
	}
	sub, err := hs.as.Subscribe(tab, since)
	if err != nil {
		writeHTTPError(w, http.StatusInternalServerError, err)
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
	servePTYStream(hs.as, tab, sub, conn)
}

// streamInfoHandler answers GET /v1/sessions/{id}/stream-info with where the
// stream is reachable. For the local runtime that is the relative stream path on
// this same listener.
func (hs *headlessServer) streamInfoHandler(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ep, err := hs.as.Expose()
	if err != nil {
		writeHTTPError(w, http.StatusInternalServerError, err)
		return
	}
	resp := streamInfoResponse{Local: ep.Local}
	if ep.URL != "" {
		resp.URL = ep.URL
	} else {
		resp.URL = localStreamPath(id, "")
	}
	writeHTTPSuccess(w, resp)
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

package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/terminal"
)

// remoteAgentServer is the daemon-side AgentServer for a runtime whose agent runs
// in a SANDBOX behind an authed URL (#1592 Phase 4 PR2): a real `af agent-server`
// (PR1) reachable over HTTP/WS+token, on another host/container. It is the
// mirror image of localAgentServer — where localAgentServer drives an in-process
// tmux session directly, this is an HTTP/WS CLIENT to an out-of-process
// agent-server, satisfying the SAME session.AgentServer interface so the daemon's
// observation/delivery paths above it do not change (§0, §1.1).
//
//   - The control plane (Provision/Launch/Expose/Snapshot/Preview/Alive/
//     SendPrompt/Kill) is REST to /v1/agent/* — the 1:1 mirror the
//     agent-server exposes.
//   - The data plane (Subscribe/Input/Resize) is the WS PTY stream to the
//     agent-server, reached through the SAME ptyBroker fan-out the local runtime
//     uses — with a remote WS clientlessChannel in place of the tmux one. So one
//     WS to the sandbox is fanned to N local subscribers, ?since replay and the
//     multi-writer input path work identically, and none of the broker logic is
//     reimplemented (§ boundaries: reuse Phase 1-3, no protocol reimpl).
//
// Transport is Phase-3's: plain HTTP/WS (no TLS — af terminates none of its own;
// the sandbox is reached over a private hop, a docker-published loopback port or
// an ssh-forwarded one) + a bearer token on every REST call and WS handshake, and
// agentproto for the PTY wire frames. It cannot import apiclient (that package
// sits above daemon, which sits above session — a cycle), so it shares the pieces
// that matter through the agentproto/apiproto leaves the whole stack already
// agrees on: the token header (agentproto.AuthHeader), the WS codec
// (agentproto.ReadMessage/WriteFrame), and the {data,error} envelope (apiproto).
// The thin HTTP/WS plumbing below is the only local piece.
//
// The off-box runtimes all terminate here after provisioning an `af agent-server`;
// an out-of-process integration test also starts one on loopback and drives it
// through this client.
type remoteAgentServer struct {
	rc *remoteAgentClient
	// inst owns the authoritative stable-id→ordinal roster for daemon-created
	// remote runtimes. The in-sandbox protocol is ordinal-shaped, so PreviewByID
	// holds inst's roster lock across the bounded REST capture (#2200). nil only
	// for the transport-only NewRemoteAgentServer constructor used by integrations.
	inst *Instance
	// restartAdoption is set on the short-lived server a W1.4 restart uses to
	// adopt the surviving workspace; it rides the Launch request.
	restartAdoption bool
	// teardown reaps the off-box workspace AFTER its in-sandbox agent is killed over
	// REST. nil for an out-of-process `af agent-server` the caller owns. Run
	// best-effort in Kill so a session kill also removes its sandbox; idempotent
	// (the runtime serializes it).
	teardown func() error

	mu sync.Mutex
	// brokers holds one lazy ptyBroker per tab index, exactly like localAgentServer
	// — each fans a single remote WS to N local subscribers. The channel behind
	// each is a remoteClientlessChannel (the remote WS) rather than a tmux one.
	brokers map[int]*ptyBroker
	// idBrokers is brokers keyed by stable tab id, for the id-addressed plane
	// (#5571 W4.4); see agentserver_remote_tabid.go. idPlaneKnown/idPlane cache
	// the sandbox's tab-id-addressing advertisement.
	idBrokers map[string]*ptyBroker
	// goneTabs are ids whose tab closed: terminal for subscriptions (#5963).
	goneTabs map[string]bool
	// agentTabSynced is the agent-tab id the sandbox is known to hold (#5997).
	agentTabSynced string
	// ordBound holds the brokers of tabs opened through the ordinal plane,
	// keyed by the stable id they resolved to (agentserver_remote_ordinal.go).
	ordBound map[string]*ptyBroker
	// boundChannel and idChannel, when set, replace the remote WS channel a
	// broker is built over (test seams; production leaves them nil).
	boundChannel func(tabID string) clientlessChannel
	idChannel    func(tabID string) clientlessChannel
	idPlaneKnown bool
	idPlane      bool
	features     []string

	// relays are the open loopback port relays (W5.1); Kill closes them.
	relays map[*trackedRelayConn]struct{}
	// closed latches once Kill has run so a Subscribe/Input/Resize racing the kill
	// cannot lazily resurrect a broker (and dial a fresh WS to a torn-down sandbox
	// that never gets closed) — the local runtime's #1632 guard, same reasoning.
	closed bool
}

var _ AgentServer = (*remoteAgentServer)(nil)

// AgentServerEndpoint is the runtime handle that points an Instance at a remote
// agent-server (#1592 Phase 4 PR2): the authed URL of the `af agent-server` in the
// sandbox plus the auth material to reach it. A nil endpoint ⇒ the local
// in-process runtime (the default, unchanged). Phase 4 PR3 generalizes this into a
// Runtime that PROVISIONS the sandbox and fills these in; PR2 only consumes them.
type AgentServerEndpoint struct {
	// URL is the agent-server's plain-HTTP base URL — `http://host:port` or
	// `ws://host:port` (equivalent; both select the same plaintext transport,
	// only the authority is used). A wss://host:port is rejected — the
	// agent-server is HTTP-only.
	URL string
	// Token is the bearer credential presented on every REST call and WS handshake.
	Token string
}

// NewRemoteAgentServer builds a remoteAgentServer that drives the `af agent-server`
// reachable at ep.URL for a single workspace titled `title`. It validates the URL
// up front (no dial) so a bad endpoint fails at construction rather than on first
// use — which is why Instance.AgentServer() (infallible) can build one from an
// endpoint validated at NewInstance. The integration test constructs one here
// directly against a real out-of-process agent-server.
func NewRemoteAgentServer(ep AgentServerEndpoint, title string) (AgentServer, error) {
	rc, err := newRemoteAgentClient(ep, title)
	if err != nil {
		return nil, err
	}
	return &remoteAgentServer{rc: rc}, nil
}

// noteAdoption counts a delivery against the Instance this server drives, before
// the write reaches the sandbox (#3865).
//
// It tolerates a nil Instance because NewRemoteAgentServer builds one: that
// constructor drives a workspace at a URL directly, with no daemon-held session
// behind it, so no on_complete teardown can be owed for it and there is nothing
// for a delivery to make stale. Every server the daemon reaches through
// Instance.AgentServer() is bound (agentserver_local.go), and the local runtime
// is deliberately NOT given this tolerance — a nil Instance there would be a
// construction bug, and it should say so rather than silently stop counting.
func (s *remoteAgentServer) noteAdoption() error {
	if s.inst == nil {
		return nil
	}
	return s.inst.NoteAdoptionDelivery()
}

func (s *remoteAgentServer) Provision(firstTimeSetup bool) error {
	req := agentLifecycleReq{FirstTimeSetup: firstTimeSetup}
	if s.restartAdoption && s.inst != nil {
		// The restarted server's fresh instance holds only its agent tab; tell it
		// how many other tabs the daemon's durable row records (W1.4, #6162).
		req.SiblingTabs = s.inst.nonAgentTabCount()
	}
	err := s.rc.call("/v1/agent/provision", req, nil)
	if err == nil {
		s.idNative() // warm the feature advertisement (#5571 W4.5)
	}
	return err
}

func (s *remoteAgentServer) Launch(firstTimeSetup bool) error {
	req := agentLifecycleReq{FirstTimeSetup: firstTimeSetup, RestartAdoption: s.restartAdoption}
	if s.restartAdoption && s.inst != nil {
		// Hand the restarted server the surviving tabs' roster — only if it says it
		// can rebind them (W1.4, #6162). Otherwise they stay unknown and are
		// retried later (#6235, #6236).
		req.Tabs = s.inst.restoreRosterForRestart(s.featureVerdict(tabRestoreFeature))
	}
	// Send the recorded conversation so a launch resumes exactly it (#5571 W7.2).
	// The server resumes it only where its own store still holds it.
	if s.inst != nil {
		if conv := s.inst.AgentConversation(); conv.HasID() {
			req.Conversation = &conv
		}
	}
	var resp agentLaunchResp
	err := s.rc.call("/v1/agent/launch", req, &resp)
	if err == nil {
		s.idNative() // warm the feature advertisement (#5571 W4.5)
		if s.inst != nil && resp.Conversation != nil {
			s.inst.recordReportedConversationFrom(s.rc, *resp.Conversation)
		}
		if s.inst != nil && len(req.Tabs) > 0 {
			s.inst.applyRestoreOutcome(req.Tabs, resp.TabsRestored, resp.TabsGone)
		}
	}
	return err
}

func (s *remoteAgentServer) Expose() (StreamEndpoint, error) {
	// The daemon PROXIES a remote session's stream: it subscribes to the sandbox
	// (Subscribe below dials the remote WS) and re-fans to its own subscribers, so
	// from a TUI's perspective the stream is still reachable at the daemon's local
	// path — the daemon relays. Returning Local=true keeps the daemon's stream
	// handler on its proxy path and the TUI unchanged. The stream-info-returns-a-URL
	// indirection (a browser dialing the sandbox directly) is not implemented —
	// browsers proxy through the daemon instead.
	return StreamEndpoint{Local: true}, nil
}

func (s *remoteAgentServer) Snapshot() (Observation, error) {
	var resp agentSnapshotResp
	if err := s.rc.call("/v1/agent/snapshot", struct{}{}, &resp); err != nil {
		return Observation{}, err
	}
	// A pre-field sandbox reports no capture bit — and its own snapshot path
	// swallows a failed capture-pane into the same all-false shape an idle
	// pane produces, so the wire cannot tell the two apart. Resolve the
	// absence from the frame's own positive evidence: an updated/prompt/
	// baseline flag or non-empty content provably read the pane, while an
	// all-false legacy frame stays unconfirmed rather than counting as
	// observed silence (#5221 review). A server new enough to send the field
	// is authoritative — including its honest no.
	captured := resp.Updated || resp.HasPrompt || resp.Baseline || resp.Content != ""
	if resp.Captured != nil {
		captured = *resp.Captured
	}
	// The server captures a conversation the provider only exposes after launch
	// (a Codex rollout) and reports it on later polls (#5571 W7.2).
	if s.inst != nil && resp.Conversation != nil {
		s.inst.recordReportedConversationFrom(s.rc, *resp.Conversation)
	}
	return Observation{
		Updated:     resp.Updated,
		Baseline:    resp.Baseline,
		HasPrompt:   resp.HasPrompt,
		Content:     resp.Content,
		ModelChange: cloneAgentModelChange(resp.ModelChange),
		Captured:    captured,
	}, nil
}

func (s *remoteAgentServer) Preview(tab int, full bool) (PreviewSnapshot, error) {
	return s.rc.preview(tab, full)
}

func (s *remoteAgentServer) PreviewByID(tabID string, full bool) (PreviewSnapshot, error) {
	if s.nativeFor(tabID) {
		return s.rc.previewByID(tabID, full)
	}
	if s.inst == nil {
		return PreviewSnapshot{}, fmt.Errorf("remote session %q cannot resolve stable tab id %q without its owning instance", s.rc.title, tabID)
	}
	var snapshot PreviewSnapshot
	_, err := s.inst.previewByIDAsOrdinal(tabID, func(tab int) (string, error) {
		var err error
		snapshot, err = s.rc.preview(tab, full)
		return snapshot.Content, err
	})
	return snapshot, err
}

// Alive asks the in-sandbox agent-server whether its agent is running. The error
// is the whole point of the signature here: this is a REST call across a real
// network hop, so it distinguishes "the sandbox answered: the agent is gone"
// (false, nil — authoritative) from "the sandbox never answered" (false, err —
// UNKNOWN). Swallowing the error into a bare false made a transport blip
// indistinguishable from a dead agent, and callers re-provisioned on it (#1794).
// WorkspaceFacts asks the agent-server to measure its workspace (W8.2). A server
// predating the route answers 404, which surfaces as an error: the caller reports
// the workspace as unmeasured, never as clean.
func (s *remoteAgentServer) WorkspaceFacts() (WorkspaceFacts, error) {
	var facts WorkspaceFacts
	if err := s.rc.call("/v1/agent/workspace-facts", struct{}{}, &facts); err != nil {
		return WorkspaceFacts{}, err
	}
	return facts, nil
}

func (s *remoteAgentServer) Alive() (bool, error) {
	var resp agentAliveResp
	if err := s.rc.call("/v1/agent/alive", struct{}{}, &resp); err != nil {
		return false, err
	}
	return resp.Alive, nil
}

func (s *remoteAgentServer) SendPrompt(prompt string) error {
	_, err := s.SendPromptWithStatus(prompt)
	return err
}

func (s *remoteAgentServer) SendPromptWithStatus(prompt string) (PromptDeliveryStatus, error) {
	// Adoption, counted before the write (#3865). SendPrompt above routes through
	// here, so this is the one bump for both.
	if err := s.noteAdoption(); err != nil {
		return PromptNotDelivered, err
	}
	var resp agentSendPromptResp
	if err := s.rc.callWithin("/v1/agent/send-prompt", agentSendPromptReq{Prompt: prompt}, &resp, agentSendPromptCallTimeout); err != nil {
		return PromptCouldNotConfirm, err
	}
	if !resp.Status.Valid() {
		// An older sandbox returns only {"ok":true}. That confirms the RPC, not
		// pane delivery, so version skew is honest uncertainty.
		return PromptCouldNotConfirm, nil
	}
	return resp.Status, nil
}

// trustLiveGeneration is unused: no tmux, no generation cohort (#3413).
// StopPreservingWorkspace stops the remote agent and closes this side's data
// plane without running the runtime teardown, so the workspace stays on its host
// (#5571 W8.5).
func (s *remoteAgentServer) StopPreservingWorkspace() error {
	s.mu.Lock()
	s.closed = true
	all := s.takeBrokersLocked()
	s.mu.Unlock()
	for _, br := range all {
		br.close()
	}
	s.closeRelays()
	if err := s.rc.call("/v1/agent/stop", struct{}{}, nil); err != nil {
		// The agent was not proven stopped, so the session carries on: lift the
		// latch so the next subscribe, input or resize dials a fresh broker rather
		// than failing "is being terminated" for as long as the row lives (#5869).
		s.mu.Lock()
		s.closed = false
		s.mu.Unlock()
		return err
	}
	return nil
}

func (s *remoteAgentServer) Kill(_ bool) error {
	// Tear every tab's data plane down first (closing each remote WS so subscribers
	// see io.EOF), latch closed under the same lock that snapshots the brokers so a
	// racing Subscribe can't resurrect one, then kill the remote workspace over REST.
	s.mu.Lock()
	s.closed = true
	all := s.takeBrokersLocked()
	s.mu.Unlock()
	for _, br := range all {
		br.close()
	}
	s.closeRelays()
	// Kill the in-sandbox workspace over REST, THEN reap the sandbox itself. Both
	// run even if the REST kill fails (the container must not leak because its
	// agent-server was already down) — the errors are joined so a caller sees
	// both. teardown is idempotent, so a Kill retry after a partial failure is safe.
	killErr := s.rc.call("/v1/agent/kill", struct{}{}, nil)
	if s.teardown != nil {
		if terr := s.teardown(); terr != nil {
			return errors.Join(killErr, terr)
		}
	}
	return killErr
}

// Archive pushes the sandbox's session branch to origin over the control REST
// (#1592 Phase 4 PR6): the in-sandbox agent-server owns the worktree, so the
// push happens THERE (it commits any uncommitted work and pushes), and this
// returns the branch name the daemon records so a later restore clones it back.
// No teardown here — the daemon runs Kill (which reaps the sandbox) after the
// branch is durable, so archive is push-then-teardown.
func (s *remoteAgentServer) Archive() (string, error) {
	var resp agentArchiveResp
	// Its own budget, not the shared control one: this call pushes a branch.
	if err := s.rc.callWithin("/v1/agent/archive", struct{}{}, &resp, AgentArchiveCallTimeout); err != nil {
		return "", err
	}
	return resp.Branch, nil
}

// deadRemoteAgentServer is the AgentServer for a remote-workspace instance
// (docker/ssh/sandbox/hook) whose runtime wiring has been torn down: remoteClient is nil,
// so there is no live `af agent-server` to reach. AgentServer() returns it — never
// a localAgentServer — for such an instance (#2005): a remote backend's data-plane
// methods delegate back to i.AgentServer(), so a localAgentServer wrapping one
// mutually recurses until the daemon poll overflows its stack. Two instances land
// in this state:
//
//   - a sandbox session loaded inert from disk (its container is gone after a
//     daemon restart, awaiting an explicit restore — instance_data.go), and
//   - one left Lost by a FAILED lost-recovery (reprovisionRemote succeeded, Start
//     failed, teardownAfterStartFailure cleared remoteClient) that stays
//     started+Lost so the #1128 restore loop keeps retrying.
//
// Every method reports the sandbox as gone/unreachable — the truth — which is
// exactly what the poll's remote-probe-failure path (#1794) and the restore loop's
// Alive gate already expect. It carries no client and no locks: it is a pure,
// stateless refusal, so AgentServer() can build one under i.mu just like the other
// two impls.
type deadRemoteAgentServer struct {
	title string
	// teardown is non-nil only for a kill-tombstoned row whose off-box cleanup
	// identity was restored from storage. There is still no agent-server client,
	// but finishUserKill must be able to reap the sandbox itself after restart.
	teardown func() error
}

var _ AgentServer = (*deadRemoteAgentServer)(nil)

// ErrRemoteSandboxNotProvisioned marks the clientless AgentServer sentinel used
// when af knows this session has no provisioned runtime to contact. It is
// session-specific absence evidence, unlike a transport error from an endpoint
// that may still be alive behind a broken network path.
var ErrRemoteSandboxNotProvisioned = errors.New("remote sandbox is not provisioned")

func (s *deadRemoteAgentServer) err() error {
	return fmt.Errorf("%w for session %q", ErrRemoteSandboxNotProvisioned, s.title)
}

func (s *deadRemoteAgentServer) Provision(bool) error            { return s.err() }
func (s *deadRemoteAgentServer) Launch(bool) error               { return s.err() }
func (s *deadRemoteAgentServer) Expose() (StreamEndpoint, error) { return StreamEndpoint{}, s.err() }
func (s *deadRemoteAgentServer) Snapshot() (Observation, error)  { return Observation{}, s.err() }
func (s *deadRemoteAgentServer) Preview(int, bool) (PreviewSnapshot, error) {
	return PreviewSnapshot{}, s.err()
}
func (s *deadRemoteAgentServer) PreviewByID(string, bool) (PreviewSnapshot, error) {
	return PreviewSnapshot{}, s.err()
}

// Alive returns the typed not-provisioned error rather than an ordinary transport
// error. Restore must re-provision an inert record after restart or a failed
// recovery, while a real endpoint that merely cannot answer remains unknown.
func (s *deadRemoteAgentServer) Alive() (bool, error) { return false, s.err() }

func (s *deadRemoteAgentServer) SendPrompt(string) error { return s.err() }

func (s *deadRemoteAgentServer) Subscribe(int, Seq) (PTYSubscription, error) { return nil, s.err() }
func (s *deadRemoteAgentServer) Input(int, []byte) error                     { return s.err() }
func (s *deadRemoteAgentServer) Resize(int, uint16, uint16) error            { return s.err() }

// Kill is normally a no-op success: an ordinary inert sandbox has already been
// reaped. A tombstoned row is different — its persisted teardown handle is the
// work still owed after a daemon restart, so run it before the record can go.
// trustLiveGeneration is unused: no tmux, no generation cohort (#3413).
func (s *deadRemoteAgentServer) Kill(_ bool) error {
	if s.teardown != nil {
		return s.teardown()
	}
	return nil
}

func (s *deadRemoteAgentServer) Archive() (string, error) { return "", s.err() }

// --- remoteAgentClient: the thin REST + WS transport to one agent-server ---

// remoteWSWriteTimeout bounds a single INPUT/RESIZE write to the sandbox socket so
// a wedged connection surfaces as an error rather than blocking a broker goroutine.
const remoteWSWriteTimeout = 10 * time.Second

// remoteAgentDialTimeout bounds the TCP connect to the sandbox (a real network
// hop), and remoteAgentCallTimeout bounds a whole control REST round-trip.
const remoteAgentCallTimeout = 30 * time.Second

// var, not const, so transport phase-bound tests can shrink the dial budget.
var remoteAgentDialTimeout = 10 * time.Second

// remoteAgentWSHandshakeTimeout bounds the WS UPGRADE handshake on the internal
// daemon→agent-server stream dial — the 101 exchange after the TCP connect. Plain
// HTTP has no TLS handshake to time out, so without this a wedged agent-server
// that accepts the TCP connection but never answers the upgrade would hang the
// daemon's capture goroutine forever (the #1730 half-open class, on the internal
// hop). It bounds ONLY the handshake: coder/websocket's Dial context does not
// govern the established stream, so a long-lived PTY subscription is never
// severed. A var (not const) so a test can shrink it to prove the bound fires.
var remoteAgentWSHandshakeTimeout = 10 * time.Second

// remoteAgentClient is the transport to ONE `af agent-server`: a token-bearing
// plain-HTTP client for the /v1/agent/* control REST and a matching WS dialer for
// the /v1/sessions/{title}/stream data plane. It is the session-package analogue
// of apiclient.Client (which session cannot import — the cycle), sharing the
// pieces that define the contract through the agentproto/apiproto leaves.
type remoteAgentClient struct {
	httpClient *http.Client
	transport  *http.Transport
	httpBase   string // http://host:port
	wsBase     string // ws://host:port
	token      string
	title      string // the sandbox's single-workspace session title (the stream path id)
}

// newRemoteAgentClient builds the transport for ep, validating the URL up front
// (no dial). It mirrors apiclient.NewRemote: the transport is plain HTTP/WS (no
// TLS — the sandbox is reached over a private hop) and the token rides every
// request.
func newRemoteAgentClient(ep AgentServerEndpoint, title string) (*remoteAgentClient, error) {
	if title == "" {
		return nil, fmt.Errorf("remote agent-server requires a workspace title")
	}
	httpBase, wsBase, err := splitHTTPBaseURL(ep.URL)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: remoteAgentDialTimeout}
	transport := &http.Transport{DialContext: dialer.DialContext}
	return &remoteAgentClient{
		httpClient: &http.Client{
			Transport: transport,
		},
		transport: transport,
		httpBase:  httpBase,
		wsBase:    wsBase,
		token:     ep.Token,
		title:     title,
	}, nil
}

// call POSTs req as JSON to the agent-server control route at `path`, decodes the
// shared {data,error} envelope, and unmarshals the data member into resp (nil resp
// ⇒ success/failure only). It is the client-side twin of the agent-server's
// rpcHandler dispatch — the same envelope the daemon's own httpserver speaks —
// with the bearer token on every call.
// agentArchiveCallTimeout is the archive route's own budget. Archive is not a
// control round-trip like the others: the in-sandbox side commits any
// uncommitted work and pushes a branch to origin, which legitimately takes tens
// of seconds on a large tree or a slow link. Under the shared 30s ceiling a
// perfectly healthy push was reported as a failure, and recovery refuses on a
// failed push — so a slow sandbox became repeatedly unrestorable while its push
// was still progressing server-side (Codex on #2923).
// Exported so the daemon's own pre-reap bound can be ordered ABOVE it: a caller
// that gave up first would leave the in-sandbox git work running unbounded.
const AgentArchiveCallTimeout = 3 * time.Minute

// agentSendPromptCallTimeout is the send-prompt route's own budget: the
// in-sandbox submit's worst case (two individually tmux-bounded delivery
// attempts plus the #3293 redelivery wait — the submit is a SEQUENCE of
// bounded commands, not itself bounded by the shared 30s ceiling) plus
// transport slack. The outer call must OUTLIVE the inner submit: the headless
// handler does not observe the request context, so a daemon that gives up
// mid-retry leaves the sandbox delivering anyway and the caller likely to
// re-send an already-delivered instruction — the exact double prompt the
// #3293 boundary exists to prevent. Sized from the submit path's own bound so
// a retimed delivery cannot silently outgrow it; the pairing is pinned by
// TestSendPromptCallBudgetCoversTheInSandboxRetry.
var agentSendPromptCallTimeout = tmux.SendPromptWorstCaseBound() + 15*time.Second

func (c *remoteAgentClient) call(path string, req, resp any) error {
	return c.callWithin(path, req, resp, remoteAgentCallTimeout)
}

// callWithin is call with an explicit budget, for the one route whose work is
// not a health check.
func (c *remoteAgentClient) callWithin(path string, req, resp any, timeout time.Duration) error {
	return c.doJSON(http.MethodPost, path, req, resp, timeout)
}

// doJSON is callWithin's plumbing generalized over the HTTP method: GET
// /v1/health is the one non-POST route on the wire, and it shares the same
// token, envelope, and error-code reconstruction. A nil req sends no body.
// agentAnswered wraps an error the agent-server returned in its envelope — it ran
// the request and refused — as opposed to a transport failure, which leaves the
// request's effect unknown. The message is unchanged.
type agentAnswered struct{ err error }

func (e *agentAnswered) Error() string { return e.err.Error() }
func (e *agentAnswered) Unwrap() error { return e.err }

func (c *remoteAgentClient) doJSON(method, path string, req, resp any, timeout time.Duration) error {
	var body io.Reader
	if req != nil {
		data, err := json.Marshal(req)
		if err != nil {
			return fmt.Errorf("remote agent-server: marshal request: %w", err)
		}
		body = bytes.NewReader(data)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, method, c.httpBase+path, body)
	if err != nil {
		return fmt.Errorf("remote agent-server: build request: %w", err)
	}
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	httpReq.Header.Set(agentproto.AuthHeader, agentproto.BearerScheme+c.token)

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("remote agent-server: %s %s: %w", method, path, err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return fmt.Errorf("remote agent-server: read %s response: %w", path, err)
	}
	// Decode into a RawMessage-backed envelope so the data member is unmarshaled in
	// a second pass only when there is no error — the same two-pass decode apiclient
	// uses, so a failure body's null data is never interpreted as the typed struct.
	var env struct {
		Data  json.RawMessage         `json:"data"`
		Error *apiproto.EnvelopeError `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("remote agent-server: malformed %s response envelope: %w", path, err)
	}
	if env.Error != nil {
		err := fmt.Errorf("%s", env.Error.Message)
		if env.Error.Code == apiproto.ErrorCodeResourceExhausted {
			// The server verified a kernel resource limit behind this
			// failure; the errno cannot cross the wire, so the code
			// reconstructs its class. Keep it in the chain — the restore
			// classifier distinguishes "the sandbox's disk is full" from
			// "the push itself is broken" on exactly this (#5295 review).
			err = fmt.Errorf("%s: %w", env.Error.Message, apiproto.ErrRemoteResourceExhausted)
		}
		if env.Error.Code == apiproto.ErrorCodeDeadlineExceeded {
			// Same class, different deadline: the operation hit a bounded
			// wait INSIDE the sandbox — the git push's own network deadline —
			// which would otherwise arrive as an uncoded message string.
			// Reconstructing context.DeadlineExceeded lets the preserve call
			// site's bare-deadline arm see the stall (#5295 review).
			err = fmt.Errorf("%s: %w", env.Error.Message, context.DeadlineExceeded)
		}
		if env.Error.Code == apiproto.ErrorCodeTabGone {
			// The status line alone cannot name this class — an unknown ROUTE
			// on an older server is a 404 too — so the server emits the code
			// only for a genuine stale-id refusal, and ErrTabGone is
			// reconstructed for the caller's errors.Is check exactly as the
			// classes above (#5571 W4.3).
			err = fmt.Errorf("%s: %w", env.Error.Message, ErrTabGone)
		}
		return &agentAnswered{err: err}
	}
	if resp != nil {
		if err := json.Unmarshal(env.Data, resp); err != nil {
			return fmt.Errorf("remote agent-server: malformed %s response data: %w", path, err)
		}
	}
	return nil
}

// preview fetches tab `tab`'s content over the control REST — shared by the
// AgentServer.Preview surface and the broker's fresh-subscriber repaint (Snapshot).
func (c *remoteAgentClient) preview(tab int, full bool) (PreviewSnapshot, error) {
	return c.previewReq(agentPreviewReq{Tab: tab, Full: full})
}

// previewByID is preview addressed by the tab's STABLE id (#5571 W4.3): the
// sandbox binds the identified target itself — atomically, inside its own
// roster — so the id never round-trips through an ordinal a close/reorder can
// shift (#1779). ErrTabGone when the id names no live tab (the envelope's
// tab_gone code reconstructs it). Only call it against a server that advertises
// the "tab-id-addressing" feature: an older one rejects the unknown `tab_id`
// field outright under strict decoding, which is the honest failure — but a
// caller that checks health first avoids paying the round-trip.
func (c *remoteAgentClient) previewByID(tabID string, full bool) (PreviewSnapshot, error) {
	return c.previewReq(agentPreviewReq{TabID: tabID, Full: full})
}

func (c *remoteAgentClient) previewReq(req agentPreviewReq) (PreviewSnapshot, error) {
	var resp agentPreviewResp
	if err := c.call("/v1/agent/preview", req, &resp); err != nil {
		return PreviewSnapshot{}, err
	}
	return PreviewSnapshot{
		Content:         resp.Content,
		Modes:           resp.Modes,
		HasModes:        resp.HasModes,
		LinesAbove:      resp.LinesAbove,
		LinesAboveKnown: resp.LinesAboveKnown,
	}, nil
}

// previewGrid is the repaint capture: the pane as one line per row with its
// cursor, modes and size (#5571 W4.2). A sandbox that predates the grid answers
// with the joined Content and no has_grid, and the result is then today's
// screen-only snapshot — same bytes, no cursor, no size — so an old agent-server
// degrades instead of failing.
func (c *remoteAgentClient) previewGrid(tab int) (PaneSnapshot, error) {
	return c.previewGridReq(agentPreviewReq{Tab: tab, Grid: true})
}

// previewGridByID is previewGrid addressed by stable tab id (#5571 W4.4).
func (c *remoteAgentClient) previewGridByID(tabID string) (PaneSnapshot, error) {
	return c.previewGridReq(agentPreviewReq{TabID: tabID, Grid: true})
}

func (c *remoteAgentClient) previewGridReq(req agentPreviewReq) (PaneSnapshot, error) {
	var resp agentPreviewResp
	if err := c.call("/v1/agent/preview", req, &resp); err != nil {
		return PaneSnapshot{}, err
	}
	if !resp.HasGrid {
		return PaneSnapshot{Screen: []byte(resp.Content), Modes: resp.Modes, HasModes: resp.HasModes}, nil
	}
	return PaneSnapshot{
		Screen:    []byte(resp.Grid),
		CursorRow: resp.CursorRow, CursorCol: resp.CursorCol, HasCursor: resp.HasCursor,
		Modes: resp.Modes, HasModes: resp.HasModes,
		Rows: resp.Rows, Cols: resp.Cols, HasSize: resp.HasSize,
	}, nil
}

// createTab asks the sandbox to spawn one tab under the daemon's STABLE id
// (#5571 W4.3). The id travels verbatim — the driving daemon is the id
// authority, and the id is the create's idempotency key: a retry of the same
// create answers the tab the first call made (see agentTabCreateReq). The
// response is the sandbox's own roster row — one shape shared with tabs().
func (c *remoteAgentClient) createTab(id, kind, command, name string) (TabData, error) {
	var resp agentTabCreateResp
	if err := c.call("/v1/agent/tab-create",
		agentTabCreateReq{ID: id, Kind: kind, Command: command, Name: name}, &resp); err != nil {
		return TabData{}, err
	}
	return resp.Tab, nil
}

// closeTab asks the sandbox to close the tab a stable id names — never an
// ordinal. ErrTabGone when it names no live tab (reconstructed from the
// envelope's tab_gone code), which a W4.4 reconciler reads as "already closed"
// rather than a failure.
func (c *remoteAgentClient) closeTab(id, gen string) error {
	return c.call("/v1/agent/tab-close", agentTabCloseReq{ID: id, Gen: gen}, nil)
}

// tabs reads the sandbox's roster: every tab with its stable id, kind, name,
// and — for process tabs — the exit evidence (status, time, stopped_by) the
// server refreshed before answering (#5571 W4.3).
func (c *remoteAgentClient) tabs() ([]TabData, error) {
	var resp agentTabsResp
	if err := c.call("/v1/agent/tabs", struct{}{}, &resp); err != nil {
		return nil, err
	}
	return resp.Tabs, nil
}

// health GETs the agent-server's /v1/health — the capability probe a driver
// runs before depending on a W4.3+ surface. `version` says which af build
// answered; `features` says which route clusters it speaks, so the caller
// degrades on a missing token instead of calling a route that may not exist.
func (c *remoteAgentClient) health() (agentHealthResp, error) {
	var resp agentHealthResp
	if err := c.doJSON(http.MethodGet, "/v1/health", nil, &resp, remoteAgentCallTimeout); err != nil {
		return agentHealthResp{}, err
	}
	return resp, nil
}

// dialStream opens the WS PTY subscription to tab `tab` (or the tab named by
// stable id `tabID`, #5571 W4.3 — see dialStreamByID for when that is safe to
// send). A nonzero `since` is the caller's cursor into the sandbox's output
// ring, sent as ?since= so the remote broker replays the gap produced while a
// previous socket was down (#5571 W1.2); 0 subscribes at the live tail. The
// token rides both the Authorization header and the ?access_token= query — the
// agent-server honors either, and the query mirrors the browser WS path Phase 5
// needs. The read limit is raised off the 32 KiB default: a repaint of a wide
// pane is a single large binary frame.
func (c *remoteAgentClient) dialStream(ctx context.Context, tab int, tabID string, since uint64) (*websocket.Conn, error) {
	q := url.Values{}
	if tabID != "" {
		q.Set("tab_id", tabID)
	} else if tab != 0 {
		q.Set("tab", strconv.Itoa(tab))
	}
	if since != 0 {
		q.Set("since", strconv.FormatUint(since, 10))
	}
	q.Set(agentproto.AccessTokenQueryParam, c.token)
	u := c.wsBase + "/v1/sessions/" + url.PathEscape(c.title) + "/stream?" + q.Encode()

	opts := &websocket.DialOptions{
		HTTPClient: c.httpClient,
		HTTPHeader: http.Header{agentproto.AuthHeader: []string{agentproto.BearerScheme + c.token}},
	}
	// Bound the UPGRADE response independently of the TCP connect (#2670). The
	// WebSocket-only clone starts ResponseHeaderTimeout after the request is
	// written; the shared REST transport remains free of a response-header bound.
	wsTransport := c.transport.Clone()
	wsTransport.ResponseHeaderTimeout = remoteAgentWSHandshakeTimeout
	defer wsTransport.CloseIdleConnections()
	wsClient := *c.httpClient
	wsClient.Transport = wsTransport
	opts.HTTPClient = &wsClient
	// ResponseHeaderTimeout does not cover a blocked request write. The larger
	// overall backstop lets TCP connect and the 101 response each use their full
	// independent budget while still bounding a peer that stops reading.
	dialCtx, cancel := context.WithTimeout(ctx, remoteAgentDialTimeout+remoteAgentWSHandshakeTimeout)
	defer cancel()
	conn, resp, err := websocket.Dial(dialCtx, u, opts)
	if err != nil {
		// A stale stable id is a 404 on the handshake (the sandbox's tab_gone
		// refusal); surface it as ErrTabGone so the caller stops addressing it.
		if tabID != "" && resp != nil && resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("remote agent-server: tab id %q: %w", tabID, ErrTabGone)
		}
		return nil, fmt.Errorf("remote agent-server: dial pty stream: %w",
			agentproto.RedactAccessTokenError(err, c.token))
	}
	conn.SetReadLimit(4 << 20)
	return conn, nil
}

// dialStreamByID is dialStream addressed by the tab's STABLE id (#5571 W4.3):
// the sandbox binds the id once, at subscribe time, inside its own roster — an
// id-addressed connection can never be retargeted by a close/reorder the way
// re-resolving an ordinal can. Only dial it against a server advertising the
// "tab-id-addressing" feature on /v1/health: an OLDER agent-server parses no
// ?tab_id= and would silently bind the AGENT tab — the query carries no strict
// decode to refuse on, so the features check is the only guard.
func (c *remoteAgentClient) dialStreamByID(ctx context.Context, tabID string) (*websocket.Conn, error) {
	return c.dialStream(ctx, 0, tabID, 0)
}

// splitHTTPBaseURL validates an agent-server base URL and derives its REST
// (http://host:port) and WS (ws://host:port) authorities. It accepts the
// plaintext schemes http/ws interchangeably (the agent-server serves REST and WS
// on one HTTP listener) and rejects the TLS schemes wss/https — the agent-server
// is HTTP-only (af terminates no TLS; the sandbox is reached over a private hop).
// Only scheme+authority are used. It mirrors apiclient's parseDaemonURL but with
// agent-server-appropriate error text — including its u.Hostname() host check,
// which (unlike u.Host) rejects the hostless `http://:8443` form instead of
// deferring it to an opaque dial error (#1784).
func splitHTTPBaseURL(raw string) (httpBase, wsBase string, err error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", "", fmt.Errorf("invalid agent-server URL %q: %w", raw, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "ws":
	case "https", "wss":
		return "", "", fmt.Errorf("agent-server URL %q uses a TLS scheme, but the agent-server is HTTP-only; use http:// or ws://", raw)
	default:
		return "", "", fmt.Errorf("agent-server URL %q must be an http:// or ws:// URL", raw)
	}
	if u.Hostname() == "" {
		return "", "", fmt.Errorf("invalid agent-server URL %q: missing host; use http://HOST:PORT", raw)
	}
	return "http://" + u.Host, "ws://" + u.Host, nil
}

// --- wire structs: the /v1/agent/* request/response shapes (mirror PR1) ---
//
// These match daemon/agentserver_headless.go's agent* types field-for-field (the
// server side). They are duplicated as unexported session types rather than
// imported because daemon imports session (a cycle the other way); the shared
// contract is the JSON tags, which the round-trip test pins.

type agentLifecycleReq struct {
	FirstTimeSetup bool `json:"first_time_setup"`
	// Conversation is the daemon's recorded conversation for a launch to resume
	// (#5571 W7.2). Absent for a first launch and for a pre-field server.
	Conversation *AgentConversationData `json:"conversation,omitempty"`
	// RestartAdoption marks the launch that adopts a RESTARTED agent-server's
	// surviving workspace (W1.4). Only that launch is refused a lazy respawn onto
	// a guessed conversation; every other restore (an archive restore, a Lost
	// recovery) keeps resuming the way it always has.
	RestartAdoption bool `json:"restart_adoption,omitempty"`
	// SiblingTabs is how many non-agent tabs the daemon's row records, sent with a
	// restart adoption's Provision so the host can refuse to rebuild under them.
	SiblingTabs int `json:"sibling_tabs,omitempty"`
	// Tabs is the roster of off-box shell/process tabs a restarted server should
	// rebind to their surviving tmux sessions (feature tab-restore, #6162). Sent
	// only to a server that advertises the feature; the commands in it are never run.
	Tabs []TabData `json:"tabs,omitempty"`
}

// agentLaunchResp carries back the conversation the server launched or resumed.
type agentLaunchResp struct {
	Conversation *AgentConversationData `json:"conversation,omitempty"`
	// TabsRestored and TabsGone answer a restart adoption's Tabs: the ids rebound
	// to a live session, and the ids whose session tmux confirmed absent.
	TabsRestored []string `json:"tabs_restored,omitempty"`
	TabsGone     []string `json:"tabs_gone,omitempty"`
}

type agentSnapshotResp struct {
	Updated     bool              `json:"updated"`
	Baseline    bool              `json:"baseline"`
	HasPrompt   bool              `json:"has_prompt"`
	Content     string            `json:"content"`
	ModelChange *AgentModelChange `json:"model_change,omitempty"`
	// Captured is a pointer so "server predates the field" stays
	// distinguishable from "server reports its capture failed" (#5221
	// review): nil resolves to the legacy contract — an all-false frame is
	// unconfirmed, positive evidence marks the capture real.
	Captured *bool `json:"captured,omitempty"`
	// Conversation is the conversation identity the server has for the agent, once
	// it has one (#5571 W7.2).
	Conversation *AgentConversationData `json:"conversation,omitempty"`
}

type agentPreviewReq struct {
	// TabID addresses by stable id and wins over Tab, mirroring the daemon's
	// own PreviewRequest precedence — a stale id is a tab_gone 404, never an
	// ordinal fallback (#5571 W4.3).
	TabID string `json:"tab_id,omitempty"`
	Tab   int    `json:"tab"`
	Full  bool   `json:"full"`
	Grid  bool   `json:"grid,omitempty"`
}

type agentPreviewResp struct {
	Content         string         `json:"content"`
	Modes           terminal.Modes `json:"terminal_modes,omitempty"`
	HasModes        bool           `json:"has_terminal_modes,omitempty"`
	LinesAbove      int            `json:"lines_above,omitempty"`
	LinesAboveKnown bool           `json:"lines_above_known,omitempty"`
	HasGrid         bool           `json:"has_grid,omitempty"`
	Grid            string         `json:"grid,omitempty"`
	HasCursor       bool           `json:"has_cursor,omitempty"`
	CursorRow       int            `json:"cursor_row,omitempty"`
	CursorCol       int            `json:"cursor_col,omitempty"`
	HasSize         bool           `json:"has_size,omitempty"`
	Rows            uint16         `json:"rows,omitempty"`
	Cols            uint16         `json:"cols,omitempty"`
}

type agentAliveResp struct {
	Alive bool `json:"alive"`
}

type agentSendPromptReq struct {
	Prompt string `json:"prompt"`
}

type agentSendPromptResp struct {
	OK     bool                 `json:"ok"`
	Status PromptDeliveryStatus `json:"status"`
}

type agentArchiveResp struct {
	Branch string `json:"branch"`
}

// --- wire structs: the W4.3 stable-id tab surface (#5571) ---
//
// These mirror daemon/agentserver_headless.go's agentTab*/agentHealth* types
// field-for-field, the same duplication rule as the rest of this file: the JSON
// tags are the contract, and the round-trip tests pin them.

// agentTabCreateReq asks the sandbox to spawn a tab under the daemon's stable
// id. Kind is the tabKindVocabulary wire name — only "shell" and "process" are
// creatable here. ID is verbatim AND the idempotency key: a retry repeating
// id+kind (and command, for a process tab) answers the tab the first create
// made; a same-id request of a different shape refuses. Empty ID mints.
type agentTabCreateReq struct {
	ID      string `json:"id,omitempty"`
	Kind    string `json:"kind"`
	Command string `json:"command,omitempty"`
	Name    string `json:"name,omitempty"`
}

// agentTabCreateResp carries the created tab as the sandbox's roster row — the
// same TabData shape /v1/agent/tabs reports, so the create answer can never
// drift from the roster.
type agentTabCreateResp struct {
	Tab TabData `json:"tab"`
}

// agentTabCloseReq names the tab to close by stable id only.
type agentTabCloseReq struct {
	ID string `json:"id"`
	// Gen makes the close conditional on the tab's incarnation, checked
	// atomically on the sandbox (#5996). Empty closes whatever holds the id.
	Gen string `json:"gen,omitempty"`
}

// agentTabsResp is the sandbox's roster: every tab as TabData, exit evidence
// included.
type agentTabsResp struct {
	Tabs []TabData `json:"tabs"`
}

// agentHealthResp is /v1/health's payload: liveness plus the version/features
// advertisement. Features is nil against a server that predates the field —
// the caller degrades on a missing TOKEN, and a missing field is just zero
// tokens.
type agentHealthResp struct {
	OK       bool     `json:"ok"`
	Version  string   `json:"version"`
	Features []string `json:"features"`
}

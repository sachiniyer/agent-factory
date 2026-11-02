package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/session"
)

// The W4.3 stable-id tab surface on the headless agent-server (#5571): the
// /v1/agent/tab-* routes keyed by the driving daemon's id, the roster with exit
// evidence, the version/features advertisement, and the id-addressed data plane
// — all behind the same envelope dispatch and the same ErrTabGone→404 mapping
// the daemon's own surface uses. These exercise the WIRE contract over a fake
// roster (tabKeeper) so the route layer is proven without tmux; the session
// package's tab_spawn_id_test.go pins the roster semantics the keeper narrows.

// fakeTabKeeper is an in-memory tabKeeper: a small roster plus call recording,
// modeling just enough of the Instance's contract for the wire tests — the
// daemon-supplied id verbatim and the same-id replay the route relies on.
type fakeTabKeeper struct {
	mu         sync.Mutex
	rows       []session.TabData
	probed     int
	closeErr   error
	addErr     error
	added      []agentTabCreateRequest
	closed     []string
	nextSuffix int
}

func (f *fakeTabKeeper) TabsData() []session.TabData {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]session.TabData(nil), f.rows...)
}

func (f *fakeTabKeeper) RecordProcessTabExits() {
	f.mu.Lock()
	f.probed++
	f.mu.Unlock()
}

func (f *fakeTabKeeper) AddShellTabWithID(id string) (*session.Tab, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.added = append(f.added, agentTabCreateRequest{ID: id, Kind: session.TabKindName(session.TabKindShell)})
	return f.appendLocked(id, session.TabKindShell, "", "shell")
}

func (f *fakeTabKeeper) AddProcessTabWithID(id, command, requestedName string) (*session.Tab, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.added = append(f.added, agentTabCreateRequest{ID: id, Kind: session.TabKindName(session.TabKindProcess), Command: command, Name: requestedName})
	return f.appendLocked(id, session.TabKindProcess, command, requestedName)
}

// appendLocked mirrors the real contract's edge the route leans on: an id
// already on the roster names that row (the replay), otherwise a fresh row
// lands — minting an id when the caller supplied none.
func (f *fakeTabKeeper) appendLocked(id string, kind session.TabKind, command, name string) (*session.Tab, error) {
	if f.addErr != nil {
		return nil, f.addErr
	}
	for _, row := range f.rows {
		if row.ID == id && id != "" {
			return &session.Tab{ID: row.ID, Name: row.Name, Kind: row.Kind}, nil
		}
	}
	if id == "" {
		f.nextSuffix++
		id = fmt.Sprintf("minted-%d", f.nextSuffix)
	}
	if name == "" {
		name = "tab"
	}
	row := session.TabData{ID: id, Name: name, Kind: kind, Command: command}
	f.rows = append(f.rows, row)
	return &session.Tab{ID: id, Name: name, Kind: kind}, nil
}

func (f *fakeTabKeeper) CloseTabByID(tabID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, tabID)
	return f.closeErr
}

// fakeIDAddressableServer extends the headless fake with the id-native data
// plane (session.TabAddressableServer), so bindTab's ?tab_id= arm is exercisable
// without tmux. It records the ids each op bound.
type fakeIDAddressableServer struct {
	*fakeHeadlessAgentServer
	mu             sync.Mutex
	subscribedIDs  []string
	inputIDs       []string
	resizeIDs      []string
	previewedIDs   []string
	subErr         error
	previewByIDErr error
}

func newFakeIDAddressableServer() *fakeIDAddressableServer {
	return &fakeIDAddressableServer{fakeHeadlessAgentServer: newFakeHeadlessAgentServer()}
}

func (f *fakeIDAddressableServer) SubscribeTab(tabID string, _ session.Seq) (session.PTYSubscription, error) {
	f.mu.Lock()
	f.subscribedIDs = append(f.subscribedIDs, tabID)
	err := f.subErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &fakeSub{ch: make(chan session.PTYEvent, 16)}, nil
}

func (f *fakeIDAddressableServer) InputTab(tabID string, b []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inputIDs = append(f.inputIDs, tabID)
	return f.Input(0, b)
}

func (f *fakeIDAddressableServer) ResizeTab(tabID string, rows, cols uint16) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resizeIDs = append(f.resizeIDs, tabID)
	return nil
}

func (f *fakeIDAddressableServer) PreviewByID(tabID string, _ bool) (session.PreviewSnapshot, error) {
	f.mu.Lock()
	f.previewedIDs = append(f.previewedIDs, tabID)
	err := f.previewByIDErr
	f.mu.Unlock()
	if err != nil {
		return session.PreviewSnapshot{}, err
	}
	return f.Preview(0, false)
}

// headlessPost drives a POST through hs.newMux directly — the token gate lives
// in startTCPListener, exercised by the round-trip test, so route-level tests
// go straight at the mux.
func headlessPost(t *testing.T, hs *headlessServer, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	hs.newMux().ServeHTTP(rec, req)
	return rec
}

func TestHeadlessTabCreate_ShellAndProcessByID(t *testing.T) {
	tabs := &fakeTabKeeper{rows: []session.TabData{{ID: "agent-id", Kind: session.TabKindAgent, Name: "agent"}}}
	hs := &headlessServer{as: newFakeHeadlessAgentServer(), tabs: tabs, title: "probe", events: newEventsHub()}

	// A shell create carrying the daemon's stable id assigns it verbatim.
	rec := headlessPost(t, hs, "/v1/agent/tab-create", `{"id":"daemon-sh-1","kind":"shell"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var created struct {
		Tab session.TabData `json:"tab"`
	}
	env := decodeEnvelope(t, rec)
	require.Nil(t, env.Error)
	raw, _ := json.Marshal(env.Data)
	require.NoError(t, json.Unmarshal(raw, &created))
	assert.Equal(t, "daemon-sh-1", created.Tab.ID, "the supplied id is assigned verbatim")
	assert.Equal(t, session.TabKindShell, created.Tab.Kind)

	// A process create carries command + name through to the spawn.
	rec = headlessPost(t, hs, "/v1/agent/tab-create",
		`{"id":"daemon-proc-1","kind":"process","command":"./deploy.sh","name":"deploy"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	env = decodeEnvelope(t, rec)
	require.Nil(t, env.Error)
	raw, _ = json.Marshal(env.Data)
	require.NoError(t, json.Unmarshal(raw, &created))
	assert.Equal(t, "daemon-proc-1", created.Tab.ID)
	assert.Equal(t, session.TabKindProcess, created.Tab.Kind)
	assert.Equal(t, "./deploy.sh", created.Tab.Command)

	tabs.mu.Lock()
	added := append([]agentTabCreateRequest(nil), tabs.added...)
	tabs.mu.Unlock()
	require.Len(t, added, 2)
	assert.Equal(t, agentTabCreateRequest{ID: "daemon-sh-1", Kind: "shell"}, added[0])
	assert.Equal(t, agentTabCreateRequest{ID: "daemon-proc-1", Kind: "process", Command: "./deploy.sh", Name: "deploy"}, added[1])
}

func TestHeadlessTabCreate_Refusals(t *testing.T) {
	tabs := &fakeTabKeeper{}
	hs := &headlessServer{as: newFakeHeadlessAgentServer(), tabs: tabs, title: "probe", events: newEventsHub()}

	for _, tc := range []struct {
		name    string
		body    string
		wantMsg string
	}{
		// Strict decoding runs before the handler, so the body can only carry
		// agentTabCreateRequest's own fields — a `url` member would be refused
		// as an unknown field and never reach the kind switch.
		{"unsupported kind web", `{"kind":"web","id":"w1"}`, "unsupported tab kind"},
		{"empty kind", `{"id":"x"}`, "unsupported tab kind"},
		{"process without command", `{"kind":"process","id":"p1"}`, "non-empty command"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := headlessPost(t, hs, "/v1/agent/tab-create", tc.body)
			env := decodeEnvelope(t, rec)
			require.NotNil(t, env.Error, "want a failure envelope")
			assert.Contains(t, env.Error.Message, tc.wantMsg)
		})
	}

	// A server built without a roster refuses honestly instead of panicking —
	// the wire-shape tests that never set hs.tabs must get an envelope, not a
	// crash in the handler.
	bare := &headlessServer{as: newFakeHeadlessAgentServer(), events: newEventsHub()}
	rec := headlessPost(t, bare, "/v1/agent/tab-create", `{"kind":"shell"}`)
	env := decodeEnvelope(t, rec)
	require.NotNil(t, env.Error)
	assert.Contains(t, env.Error.Message, "no tab roster")
}

func TestHeadlessTabClose_ByIDAndStaleIDIs404(t *testing.T) {
	tabs := &fakeTabKeeper{}
	hs := &headlessServer{as: newFakeHeadlessAgentServer(), tabs: tabs, title: "probe", events: newEventsHub()}

	rec := headlessPost(t, hs, "/v1/agent/tab-close", `{"id":"daemon-sh-1"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	tabs.mu.Lock()
	closed := tabs.closed
	tabs.mu.Unlock()
	assert.Equal(t, []string{"daemon-sh-1"}, closed, "the close names the stable id, never an ordinal")

	// Empty id fails validation.
	rec = headlessPost(t, hs, "/v1/agent/tab-close", `{"id":""}`)
	require.NotNil(t, decodeEnvelope(t, rec).Error)

	// A stale/unknown id is ErrTabGone: the shared rpcHandlerCtx mapping must
	// answer 404 AND carry the tab_gone code — the status line alone cannot
	// distinguish it from an unknown route's 404 (#5571 W4.3).
	tabs.closeErr = session.ErrTabGone
	rec = headlessPost(t, hs, "/v1/agent/tab-close", `{"id":"never-minted"}`)
	require.Equal(t, http.StatusNotFound, rec.Code, "ErrTabGone maps to 404 in the shared dispatch")
	env := decodeEnvelope(t, rec)
	require.NotNil(t, env.Error)
	assert.Equal(t, apiproto.ErrorCodeTabGone, env.Error.Code,
		"the envelope must carry tab_gone so a remote client reconstructs ErrTabGone")
}

func TestHeadlessTabs_RosterCarriesExitEvidence(t *testing.T) {
	died := time.Unix(1726000000, 0)
	tabs := &fakeTabKeeper{rows: []session.TabData{
		{ID: "agent-id", Kind: session.TabKindAgent, Name: "agent"},
		{ID: "deploy-id", Kind: session.TabKindProcess, Name: "deploy", Command: "./deploy.sh",
			Exit: &session.TabExitData{Status: 7, StatusKnown: true, At: died, StoppedBy: session.TabStoppedByAccountScope}},
	}}
	hs := &headlessServer{as: newFakeHeadlessAgentServer(), tabs: tabs, title: "probe", events: newEventsHub()}

	rec := headlessPost(t, hs, "/v1/agent/tabs", ``)
	require.Equal(t, http.StatusOK, rec.Code)
	env := decodeEnvelope(t, rec)
	require.Nil(t, env.Error)
	var resp agentTabsResponse
	raw, _ := json.Marshal(env.Data)
	require.NoError(t, json.Unmarshal(raw, &resp))
	require.Len(t, resp.Tabs, 2)
	assert.Equal(t, session.TabKindAgent, resp.Tabs[0].Kind, "the agent row rides at index 0")
	proc := resp.Tabs[1]
	assert.Equal(t, "deploy-id", proc.ID)
	require.NotNil(t, proc.Exit, "the roster row carries the process exit evidence")
	assert.Equal(t, 7, proc.Exit.Status)
	assert.True(t, proc.Exit.StatusKnown)
	assert.True(t, proc.Exit.At.Equal(died))
	assert.Equal(t, session.TabStoppedByAccountScope, proc.Exit.StoppedBy)

	tabs.mu.Lock()
	probed := tabs.probed
	tabs.mu.Unlock()
	assert.Equal(t, 1, probed,
		"the roster read refreshes exit evidence first — a live sandbox stamps exits here, not on restore")
}

func TestHeadlessHealth_AdvertisesVersionAndFeatures(t *testing.T) {
	SetVersion("9.9.9-w43")
	defer SetVersion("")

	hs := &headlessServer{as: newFakeHeadlessAgentServer(), events: newEventsHub()}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	hs.newMux().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	env := decodeEnvelope(t, rec)
	require.Nil(t, env.Error)
	var health agentHealthResponse
	raw, _ := json.Marshal(env.Data)
	require.NoError(t, json.Unmarshal(raw, &health))
	assert.True(t, health.OK)
	assert.Equal(t, "9.9.9-w43", health.Version, "health reports the build version the banner carries")
	assert.Contains(t, health.Features, "tab-api", "health advertises the tab routes it serves")
	assert.Contains(t, health.Features, "tab-id-addressing", "health advertises the id-addressed data plane")
	assert.Contains(t, health.Features, "relaunch", "health advertises POST /v1/agent/relaunch (W7.1)")
}

// TestAgentServerBannerCarriesVersion pins the banner arm of the advertisement:
// the first thing a driver reads must name the build that answered, matching
// /v1/health's `version` (#5571 W4.3).
func TestAgentServerBannerCarriesVersion(t *testing.T) {
	SetVersion("9.9.9-w43")
	defer SetVersion("")

	data, err := json.Marshal(AgentServerInfo{Addr: "127.0.0.1:1", Token: "t", Title: "probe", Version: Version()})
	require.NoError(t, err)
	var banner map[string]any
	require.NoError(t, json.Unmarshal(data, &banner))
	assert.Equal(t, "9.9.9-w43", banner["version"], "the startup banner advertises the build version")
}

func TestHeadlessPreview_TabIDRoutesToPreviewByID(t *testing.T) {
	as := newFakeIDAddressableServer()
	hs := &headlessServer{as: as, title: "probe", events: newEventsHub()}

	// tab_id takes precedence over the ordinal — the same precedence the
	// daemon's own PreviewRequest resolves.
	rec := headlessPost(t, hs, "/v1/agent/preview", `{"tab_id":"daemon-sh-1","tab":0,"full":false}`)
	require.Equal(t, http.StatusOK, rec.Code)
	as.mu.Lock()
	previewedIDs := as.previewedIDs
	as.mu.Unlock()
	assert.Equal(t, []string{"daemon-sh-1"}, previewedIDs,
		"a tab_id preview must bind the id-native surface, not the ordinal")

	// A stale id answers 404 + tab_gone through the same shared mapping.
	as.mu.Lock()
	as.previewByIDErr = session.ErrTabGone
	as.mu.Unlock()
	rec = headlessPost(t, hs, "/v1/agent/preview", `{"tab_id":"never-minted"}`)
	require.Equal(t, http.StatusNotFound, rec.Code)
	env := decodeEnvelope(t, rec)
	require.NotNil(t, env.Error)
	assert.Equal(t, apiproto.ErrorCodeTabGone, env.Error.Code)
}

// TestHeadlessStream_TabIDBindsIDNative proves the ?tab_id= arm end to end over
// a real loopback WS: the bind resolves through the id-native plane
// (SubscribeTab, NOT the ordinal Subscribe), the handshake upgrades, and input
// the client writes lands on the same id. The ordinal arm (?tab=) still
// reaches the legacy Subscribe — pinned so the additive contract holds.
func TestHeadlessStream_TabIDBindsIDNative(t *testing.T) {
	as := newFakeIDAddressableServer()
	hs := &headlessServer{as: as, title: "probe", events: newEventsHub()}
	srv := httptest.NewServer(hs.newMux())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// --- id-addressed ---------------------------------------------------------
	conn, _, err := websocket.Dial(ctx,
		"ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/sessions/probe/stream?tab_id=daemon-sh-1",
		nil)
	require.NoError(t, err, "?tab_id= against the id-native runtime must upgrade")
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	as.mu.Lock()
	subscribedIDs := as.subscribedIDs
	as.mu.Unlock()
	require.Equal(t, []string{"daemon-sh-1"}, subscribedIDs,
		"the stream must bind SubscribeTab by stable id, never the ordinal Subscribe")

	// Input written on the id-addressed socket reaches the same id.
	require.NoError(t, agentproto.WriteFrame(ctx, conn, agentproto.InputFrame([]byte("x"))))
	require.Eventually(t, func() bool {
		as.mu.Lock()
		defer as.mu.Unlock()
		return len(as.inputIDs) == 1 && as.inputIDs[0] == "daemon-sh-1"
	}, 3*time.Second, 10*time.Millisecond, "stream input must land on the bound stable id")

	// A stale id is refused BEFORE the upgrade with the shared 404 mapping.
	staleCtx, staleCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer staleCancel()
	as.mu.Lock()
	as.subErr = session.ErrTabGone
	as.mu.Unlock()
	_, resp, err := websocket.Dial(staleCtx,
		"ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/sessions/probe/stream?tab_id=never-minted",
		nil)
	require.Error(t, err, "a stale tab id must refuse the handshake")
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "ErrTabGone on the stream is a 404 pre-upgrade")

	// --- the ordinal arm is untouched ------------------------------------------
	as.mu.Lock()
	as.subErr = nil
	as.mu.Unlock()
	ordConn, _, err := websocket.Dial(ctx,
		"ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/sessions/probe/stream?tab=0",
		nil)
	require.NoError(t, err, "a legacy ?tab= dial must still upgrade")
	defer func() { _ = ordConn.Close(websocket.StatusNormalClosure, "") }()

	as.mu.Lock()
	subscribedIDs = as.subscribedIDs
	ordSubs := as.fakeHeadlessAgentServer.subs
	as.mu.Unlock()
	// Both id-addressed dials recorded on SubscribeTab (the stale one binds
	// before its refusal); the ordinal dial must add NOTHING there — it belongs
	// to the ordinal Subscribe alone.
	assert.Equal(t, []string{"daemon-sh-1", "never-minted"}, subscribedIDs,
		"the ordinal arm must never touch the id-native plane")
	assert.Len(t, ordSubs, 1, "the ordinal arm binds the ordinal Subscribe")
}

// TestHeadlessStream_TabIDStaleRefusedBeforeUpgrade pins the subscribe-order
// guarantee the stream handler inherits: binding.subscribe runs before
// websocket.Accept, so a stale id answers with an HTTP envelope — never an
// upgrade that only then discovers the tab is gone.
func TestHeadlessStream_TabIDStaleRefusedBeforeUpgrade(t *testing.T) {
	as := newFakeIDAddressableServer()
	as.subErr = session.ErrTabGone
	hs := &headlessServer{as: as, title: "probe", events: newEventsHub()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/probe/stream?tab_id=never-minted", nil)
	hs.newMux().ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code, "a stale tab id is a 404 envelope, not an upgrade")
	body, _ := io.ReadAll(rec.Body)
	assert.Contains(t, string(body), apiproto.ErrorCodeTabGone,
		"the refusal envelope names the tab_gone code for the remote client")
}

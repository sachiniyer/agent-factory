package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// These are the end-to-end regression tests for "CLI: Attaching to archived
// sessions fails with opaque WebSocket 500 instead of restore guidance".
//
// agentServerForStreamTarget (daemon/manager_sessions.go) now refuses a non-live
// target with promptTargetLivenessError before the WS upgrade, so the daemon's
// /stream and /stream-info routes answer an Archived session with a non-101 HTTP
// response carrying the actionable restore off-ramp — not by handing the instance
// through to a WS upgrade that then fails deep at broker subscribe with a generic
// 500 whose body the WebSocket client drops. streamGet (stream_warmup_test.go)
// asks for the upgrade on /stream so a recorder that cannot be hijacked proves the
// refusal fired before websocket.Accept.

// seedReadyArchivedManager builds a ready manager over a fresh home with one
// Archived local instance tracked under title, the shape attach resolves when a
// user types a remembered (archived) title. The instance is inert (ObserveLiveness
// sets the liveness axis without touching the worktree/tmux), so it stands up
// without a real tmux, exactly the case the bug reports.
func seedReadyArchivedManager(t *testing.T, title string) (*Manager, string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if !manager.Ready() {
		t.Fatal("NewManager restores synchronously; its manager must be ready so the request reaches resolution, not the warm-up gate")
	}
	inst, _ := newCountingInstance(t, title, repoPath)
	if err := inst.Transition(session.ObserveLiveness(session.LiveArchived)); err != nil {
		t.Fatalf("archive transition: %v", err)
	}
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(repo.ID, title)] = inst
	manager.mu.Unlock()
	return manager, repo.ID
}

// TestStreamHandler_RefusesArchivedBeforeUpgrade pins the /stream route: a ready
// daemon resolving an Archived session refuses BEFORE websocket.Accept with the
// actionable liveness error (404 + the restore off-ramp envelope), not a generic
// 500 at broker subscribe. The body names the session as Archived and points at
// `af sessions restore`, exactly as promptTargetLivenessError does for send-prompt.
func TestStreamHandler_RefusesArchivedBeforeUpgrade(t *testing.T) {
	const title = "archived-target"
	manager, repoID := seedReadyArchivedManager(t, title)
	mux := newHTTPMux(&controlServer{manager: manager})

	rec := streamGet(t, mux, fmt.Sprintf("/v1/sessions/%s/stream?repo_id=%s", title, repoID))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for an archived session (got %d) — the liveness gate must refuse before the upgrade (body: %s)",
			http.StatusNotFound, rec.Code, rec.Body.String())
	}
	// The refusal must fire BEFORE the upgrade: a recorder is not a Hijacker, so
	// an attempted upgrade would surface as a non-gate failure. A plain 404 with
	// no WS accept header proves the gate refused rather than upgrading then dying.
	if rec.Code == http.StatusSwitchingProtocols || rec.Header().Get("Sec-WebSocket-Accept") != "" {
		t.Fatal("the archived refusal upgraded the connection first; it must be a plain HTTP 404 before websocket.Accept")
	}
	if ct := rec.Header().Get("Content-Type"); !contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want the JSON failure envelope every pre-upgrade stream error uses", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `is Archived`) {
		t.Fatalf("stream refusal must name the session as Archived, got: %s", body)
	}
	if !strings.Contains(body, "af sessions restore -- "+title) {
		t.Fatalf("stream refusal must surface the actionable restore off-ramp, got: %s", body)
	}
}

// TestStreamInfoHandler_RefusesArchived is the same contract for the
// /stream-info route. Before the gate it returned 200 with a stream URL for an
// archived target — actively reporting it as attachable; now it refuses with the
// same actionable 404 the dial path produces, so a client asking "can I attach?"
// gets the same guidance as the attach itself.
func TestStreamInfoHandler_RefusesArchived(t *testing.T) {
	const title = "archived-info"
	manager, repoID := seedReadyArchivedManager(t, title)
	mux := newHTTPMux(&controlServer{manager: manager})

	rec := streamGet(t, mux, fmt.Sprintf("/v1/sessions/%s/stream-info?repo_id=%s", title, repoID))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for an archived session's stream-info (body: %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `is Archived`) || !strings.Contains(body, "af sessions restore -- "+title) {
		t.Fatalf("stream-info refusal must surface the Archived restore off-ramp, got: %s", body)
	}
}

// TestStreamHandler_LiveSessionStillUpgrades is the non-regression half: a live
// (Running) session still upgrades and streams, so the liveness gate did not
// widen into a live-session refusal. It reuses the warm-up suite's ready-seed
// helper, which builds a live agent tab that Accept can upgrade against a recorder
// (the recorder is not hijacked, so the upgrade reports its no-hijack failure
// AFTER the gate admitted the session — proving the gate passed).
func TestStreamHandler_LiveSessionStillUpgrades(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	const title = "live-streamer"
	startedLocalTabInstance(t, manager, repo.ID, repoPath, title, "af_"+title+"_agent")
	mux := newHTTPMux(&controlServer{manager: manager})

	rec := streamGet(t, mux, fmt.Sprintf("/v1/sessions/%s/stream?repo_id=%s", title, repo.ID))
	// A Running session must pass the liveness gate. It then attempts
	// websocket.Accept, which fails against a non-Hijackable recorder — so the
	// status is NOT the 404 the liveness gate returns; it is the upgrade's own
	// failure to write the handshake. A 404 here would mean the gate refused a
	// live session (regression); any other status means the gate admitted it and
	// the request proceeded to the recorder-bound upgrade.
	if rec.Code == http.StatusNotFound && contains(rec.Body.String(), `is Archived`) {
		t.Fatalf("a Running session was refused by the liveness gate; the gate must admit live sessions (body: %s)", rec.Body.String())
	}
}

// TestStreamTargetLivenessError_FitsDialBufferForLongTitle pins the cap the WS
// stream path applies so a long title cannot push the refusal envelope past the
// 1 KiB body coder/websocket.Dial buffers on a non-101 handshake. Session titles
// are uncapped by validateTitleShapeLocked (it checks shape, not length), and the
// Archived refusal embeds the title twice (the %q label and the shellsuggest
// restore command), so without the cap a long-enough title truncates the JSON and
// the client's notServedDetail falls back to a 200-byte title snippet — losing the
// "is Archived" status and the restore off-ramp the gate exists to surface. A
// short title stays byte-identical to the send-prompt path.
func TestStreamTargetLivenessError_FitsDialBufferForLongTitle(t *testing.T) {
	// A short title fits verbatim: the stream refusal is the same error the
	// send-prompt path returns, so the cap is a no-op for the common case.
	shortTitle := "archived-target"
	short := streamTargetLivenessError(shortTitle, session.LiveArchived)
	prompt := promptTargetLivenessError(shortTitle, session.LiveArchived)
	if short == nil || prompt == nil {
		t.Fatalf("Archived liveness must refuse; got stream=%v prompt=%v", short, prompt)
	}
	if short.Error() != prompt.Error() {
		t.Fatalf("a short title must keep the stream refusal byte-identical to the send-prompt path\nstream: %q\nprompt: %q", short.Error(), prompt.Error())
	}
	if streamRefusalEnvelopeSize(short.Error()) > streamLivenessErrorBodyLimit {
		t.Fatalf("short refusal envelope %d exceeds the dial buffer %d", streamRefusalEnvelopeSize(short.Error()), streamLivenessErrorBodyLimit)
	}

	// A title long enough to overflow the buffer verbatim keeps the status and the
	// restore command with the REAL title (restore resolves by exact title), and
	// still fits: only the title echo in the label is dropped.
	longTitle := strings.Repeat("x", 600)
	long := streamTargetLivenessError(longTitle, session.LiveArchived)
	if long == nil {
		t.Fatal("Archived liveness must refuse even for a long title")
	}
	if size := streamRefusalEnvelopeSize(long.Error()); size > streamLivenessErrorBodyLimit {
		t.Fatalf("refusal envelope %d exceeds the dial buffer %d (message: %q)", size, streamLivenessErrorBodyLimit, long.Error())
	}
	if !strings.Contains(long.Error(), "is Archived") {
		t.Fatalf("refusal lost the liveness status: %q", long.Error())
	}
	if want := "af sessions restore -- " + longTitle; !strings.Contains(long.Error(), want) {
		t.Fatalf("refusal must carry the restore command with the real, unaltered title: %q", long.Error())
	}
	if strings.Contains(long.Error(), "…") {
		t.Fatalf("refusal must not present a shortened title: %q", long.Error())
	}

	// A title too long for any command to fit gets generic guidance, never a
	// runnable-looking command with a wrong argument, and is decided without
	// rendering the title at all.
	huge := strings.Repeat("y", 4<<20)
	gen := streamTargetLivenessError(huge, session.LiveArchived)
	if gen == nil || streamRefusalEnvelopeSize(gen.Error()) > streamLivenessErrorBodyLimit {
		t.Fatalf("a huge title must still get a refusal that fits the buffer: %v", gen)
	}
	if !strings.Contains(gen.Error(), "is Archived") || !strings.Contains(gen.Error(), "af sessions restore -- <title>") || strings.Contains(gen.Error(), "yyyy") {
		t.Fatalf("huge-title refusal must be generic guidance with no title fragment: %q", gen.Error())
	}
	for _, l := range []session.Liveness{session.LiveLost, session.LiveDead} {
		if e := streamTargetLivenessError(huge, l); e == nil || streamRefusalEnvelopeSize(e.Error()) > streamLivenessErrorBodyLimit {
			t.Fatalf("liveness %v: huge title must still refuse within the buffer: %v", l, e)
		}
	}

	// A nil liveness (a live session) stays nil — the cap is only applied to a
	// refusal, never invents one.
	if got := streamTargetLivenessError(longTitle, session.LiveRunning); got != nil {
		t.Fatalf("a live session must not be refused; got %v", got)
	}
}

// TestStreamHandler_RefusesArchivedLongTitleFitsDialBuffer is the end-to-end half:
// the /stream route's refusal body for a long-title Archived session must fit the
// 1 KiB body coder/websocket.Dial buffers and still carry the restore off-ramp —
// the property the cap preserves on the wire.
func TestStreamHandler_RefusesArchivedLongTitleFitsDialBuffer(t *testing.T) {
	longTitle := strings.Repeat("x", 600)
	manager, repoID := seedReadyArchivedManager(t, longTitle)
	mux := newHTTPMux(&controlServer{manager: manager})

	rec := streamGet(t, mux, fmt.Sprintf("/v1/sessions/%s/stream?repo_id=%s", longTitle, repoID))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for an archived session (body: %s)", rec.Code, rec.Body.String())
	}
	if rec.Code == http.StatusSwitchingProtocols || rec.Header().Get("Sec-WebSocket-Accept") != "" {
		t.Fatal("the archived refusal upgraded the connection first; it must be a plain HTTP 404 before websocket.Accept")
	}
	body := rec.Body.String()
	if len(body) > streamLivenessErrorBodyLimit {
		t.Fatalf("refusal body %d bytes exceeds the WS dial buffer %d; the long title pushed the guidance out of reach (body: %s)",
			len(body), streamLivenessErrorBodyLimit, body)
	}
	if !strings.Contains(body, `is Archived`) {
		t.Fatalf("stream refusal for a long title must still name the session as Archived, got: %s", body)
	}
	if !strings.Contains(body, "af sessions restore") {
		t.Fatalf("stream refusal for a long title must still surface the restore off-ramp, got: %s", body)
	}
	// The envelope the client parses (notServedDetail unmarshals it) must be valid
	// JSON, not a truncated fragment the cap was meant to prevent.
	var env struct {
		Error *apiproto.EnvelopeError `json:"error"`
	}
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&env); err != nil {
		t.Fatalf("refusal body must be valid JSON the dial client can unmarshal; the cap keeps it whole: %v (body: %s)", err, body)
	}
	if env.Error == nil || env.Error.Message == "" {
		t.Fatalf("refusal body must carry the daemon failure envelope, got: %s", body)
	}
}

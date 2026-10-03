package integration_test

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// TestWebCreateSessionOnHookBackend is the evidence behind the two
// session.create.opt.* web cells in parity/inventory.json. The ledger held the
// web at "partial" because #1968 watched a browser create PROVISION a hook
// sandbox but the session timed out waiting for its program — nobody had shown
// a working remote session whose create went through the browser's request
// path. Reachable is not proven, so the row stayed open.
//
// This test supplies that proof by driving the EXACT endpoints the browser
// speaks — the same loop TestWebActionsLoop drives for a local session — with
// the one difference under test: the modal's body carries "backend":"hook",
// the field web/src/api.ts createSession sets only when the ListBackends picker
// chose it (absent is what means "repo default"; the repo config here carries
// remote_hooks but deliberately NO backend key, so the create lands on hook
// solely because the field asked).
//
// The remote itself is the mock from TestRemoteHookRoundTripMockRemote:
// launch_cmd clones repo@origin and starts a real `af agent-server`, delete_cmd
// reaps it by PID. What this adds is everything between the browser and that
// fixture — HTTP create, the readiness wait, the events rail, the stream
// attach — which is exactly the span #1968 could not prove.
//
// Needs the container fence (git + tmux + spawnable af agent-server):
// make test-container GOTESTARGS="./integration -run TestWebCreateSessionOnHookBackend"
func TestWebCreateSessionOnHookBackend(t *testing.T) {
	testguard.SkipDarwinPTYStream(t)
	h := newHarness(t)

	// launch_cmd is handed --repo <CloneURL>, which originRemoteURL resolves
	// from this repo's origin — a local bare clone stands in for GitHub, the
	// same trick the round-trip test uses (self-contained, no network).
	bare := filepath.Join(t.TempDir(), "repo.git")
	runExternal(t, "", "git", "clone", "--bare", h.repo, bare)
	runExternal(t, h.repo, "git", "remote", "add", "origin", bare)

	state := t.TempDir()
	launch := writeMockHookLaunch(t, filepath.Join(h.repo, "launch.sh"), h.bin, state)
	del := writeMockHookDelete(t, filepath.Join(h.repo, "delete.sh"), state)
	// remote_hooks WITHOUT backend: an explicit backend=hook in the request is
	// the only thing routing this create off the local default, which is what
	// makes the test discriminate — drop the field from the body and the
	// assertions below (an agent-server PID, a remote stream) cannot hold.
	writeFile(t, filepath.Join(h.repo, ".agent-factory", "config.json"),
		fmt.Sprintf(`{"remote_hooks":{"launch_cmd":%q,"delete_cmd":%q}}`, launch, del), 0644)

	h.startDaemon()

	// --- the live rail, as the browser subscribes on login -------------------
	ev := h.dialEvents(t)
	defer ev.close()

	// --- create via the modal's verbatim CreateSession body ------------------
	// web/src/api.ts createSession sends title_base + repo_path + program +
	// prompt, and sets backend ONLY when the picker chose one — mirrored here.
	var created struct {
		Instance struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"instance"`
		Warning string `json:"warning"`
	}
	err := h.tryHTTPPost("/v1/CreateSession", map[string]any{
		"title_base": "webhook",
		"repo_path":  h.repo,
		"program":    tmux.ProgramClaude,
		"prompt":     "",
		"backend":    "hook",
	}, &created)
	if err != nil {
		t.Fatalf("CreateSession backend=hook over the web's own endpoint: %v", err)
	}
	// From here the create is COMMITTED: launch_cmd may already have spawned the
	// agent-server, so every later failure must still reap it. This cleanup
	// registers after `state`'s t.TempDir, so it runs BEFORE that dir is removed
	// (LIFO) — the harness's own cleanupSessions runs after, when delete.sh
	// could no longer read its pidfile.
	killed := false
	t.Cleanup(func() {
		if killed || created.Instance.Title == "" {
			return
		}
		_ = h.tryHTTPPost("/v1/KillSession", map[string]any{"title": created.Instance.Title, "repo_id": ""}, nil)
		deadline := time.Now().Add(15 * time.Second)
		for mockHookServerAlive(state, session.Slugify(created.Instance.Title)) && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
	})
	if created.Warning != "" {
		t.Fatalf("CreateSession returned a committed-but-failed warning: %s", created.Warning)
	}
	id, title := created.Instance.ID, created.Instance.Title
	if id == "" || title == "" {
		t.Fatalf("created session missing id/title: %+v", created)
	}
	// The create RPC returns only after the readiness wait, so reaching this
	// line already refutes the #1968 symptom (a session parked waiting for its
	// program). The program is the harness fake-agent via program_overrides —
	// resolved daemon-side and handed to launch_cmd as --program-resolved —
	// which prints the "❯" ready prompt over the REMOTE pane.
	t.Logf("STEP 1: CreateSession backend=hook returned ready session title=%q id=%q", title, id)

	// The launch_cmd provisioned the sandbox: a real af agent-server is up.
	slug := session.Slugify(title)
	if !mockHookServerAlive(state, slug) {
		t.Fatal("expected the mock launch_cmd to have started an af agent-server")
	}
	t.Log("STEP 2: launch_cmd provisioned the workspace — agent-server alive")

	// --- the created event reaches the rail, carrying the id -----------------
	ev.waitEvent(t, agentproto.EventSessionCreated, func(d eventData) bool {
		return d.ID == id && d.Title == title
	})
	t.Log("STEP 3: session.created event arrived on the rail carrying id")

	// --- attach the terminal and prove the remote program answers ------------
	// This is the half #1968 could not show: the browser's stream attach for a
	// remote session must reach through the daemon's proxy to the agent-server,
	// and typed input must come back echoed (the program is `cat` behind the
	// fake-agent wrapper).
	term := h.dialWS(t, "/v1/sessions/"+id+"/stream")
	defer term.close()
	term.sendInput(t, []byte("hello-from-web-remote\n"))
	term.waitOutput(t, "hello-from-web-remote")
	t.Log("STEP 4: attached the remote session's terminal by id and typed — the pane echoed it")

	// --- kill via KillSession (the confirm modal's action) -------------------
	if err := h.tryHTTPPost("/v1/KillSession", map[string]any{
		"title":   title,
		"repo_id": "",
	}, nil); err != nil {
		t.Fatalf("KillSession: %v", err)
	}
	killed = true
	waitUntil(t, 5*time.Second, "attached remote terminal received MsgExit on kill", func() bool {
		return term.sawExit()
	})
	t.Log("STEP 5: killed via KillSession; the attached remote terminal got MsgExit")

	ev.waitEvent(t, agentproto.EventSessionKilled, func(d eventData) bool {
		return d.ID == id && d.Title == title
	})

	// delete_cmd reaps the provisioned agent-server — no sandbox leak.
	waitUntil(t, 30*time.Second, "delete_cmd reaps the af agent-server after KillSession", func() bool {
		return !mockHookServerAlive(state, slug)
	})
	t.Log("STEP 6: delete_cmd reaped the agent-server — hook web round-trip complete")
}

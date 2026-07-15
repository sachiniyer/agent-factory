package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// The #1786 regression: a REMOTE session that hits a usage-limit wall and whose
// agent exits while blocked must resume against the sandbox its respawn
// PROVISIONED, not the one the respawn tore down.
//
// The local runtime cannot express this bug — localAgentServer resolves
// i.backend on every call, so a stale capture still lands on the live backend —
// which is why the existing limit tests (limit_resume_test.go) pass either way.
// A remote session's agent-server is an HTTP client PINNED to one sandbox URL at
// construction, so holding one across a respawn silently addresses a dead host.
// These tests therefore drive two mock sandboxes and assert WHICH one the prompt
// reaches.

// mockSandbox stands in for an `af agent-server` in a sandbox: it serves the two
// control routes the limit-resume path drives (/v1/agent/alive to pick the
// respawn arm, /v1/agent/send-prompt to re-deliver the prompt) in the apiproto
// {data,error} envelope the remote client decodes, and records every prompt it
// received so a test can prove the resume targeted it.
type mockSandbox struct {
	srv *httptest.Server

	mu      sync.Mutex
	alive   bool
	prompts []string
}

func newMockSandbox(t *testing.T, alive bool) *mockSandbox {
	t.Helper()
	s := &mockSandbox{alive: alive}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/agent/alive", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		resp := map[string]any{"alive": s.alive}
		s.mu.Unlock()
		writeSandboxEnvelope(t, w, resp)
	})
	mux.HandleFunc("/v1/agent/send-prompt", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Prompt string `json:"prompt"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("mock sandbox: decode send-prompt: %v", err)
		}
		s.mu.Lock()
		s.prompts = append(s.prompts, req.Prompt)
		s.mu.Unlock()
		writeSandboxEnvelope(t, w, struct{}{})
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func writeSandboxEnvelope(t *testing.T, w http.ResponseWriter, data any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
		t.Errorf("mock sandbox: encode envelope: %v", err)
	}
}

func (s *mockSandbox) endpoint() session.AgentServerEndpoint {
	return session.AgentServerEndpoint{URL: s.srv.URL, Token: "tok"}
}

func (s *mockSandbox) gotPrompts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.prompts...)
}

// sandboxBackend impersonates a re-provisionable sandbox backend (docker/ssh/
// hook). Type() reports "docker" so reprovisionRemote resolves the docker
// Runtime — which the test swaps for a mock via session.SetRuntimeForTest — and
// Respawn re-provisions + rebinds through the exported RestoreSandbox, the same
// reprovisionRemote-then-Start core the real backends reach via recoverSandbox.
// Start is overridden because the embedded FakeBackend's Start blocks until the
// test releases it, while the respawn here must run to completion inline.
type sandboxBackend struct {
	*session.FakeBackend
	respawns int32
	mu       sync.Mutex
}

func newSandboxBackend() *sandboxBackend {
	return &sandboxBackend{FakeBackend: session.NewFakeBackend()}
}

func (b *sandboxBackend) Type() string { return "docker" }

// Capabilities matches what backend_docker.go actually reports: an off-box
// workspace that advertises Recover. The embedded FakeBackend claims a LOCAL
// worktree, which would route this double around the remote-only branches the
// resume path takes (#1794) and quietly test the wrong runtime.
func (b *sandboxBackend) Capabilities() session.Capabilities {
	return session.Capabilities{
		Workspace:        session.WorkspaceRemote,
		Archive:          true,
		Recover:          true,
		InteractiveInput: true,
	}
}

func (b *sandboxBackend) Start(*session.Instance, bool) error { return nil }

func (b *sandboxBackend) Respawn(i *session.Instance) error {
	b.mu.Lock()
	b.respawns++
	b.mu.Unlock()
	if err := i.RestoreSandbox(); err != nil {
		return err
	}
	_ = i.Transition(session.ConfirmLive())
	return nil
}

func (b *sandboxBackend) respawnCount() int32 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.respawns
}

// mockRuntime provisions the REPLACEMENT sandbox: the endpoint a respawn rebinds
// the instance onto.
type mockRuntime struct {
	backend  session.Backend
	endpoint session.AgentServerEndpoint
}

func (r mockRuntime) Provision(session.ProvisionSpec) (session.ProvisionResult, error) {
	ep := r.endpoint
	return session.ProvisionResult{Backend: r.backend, Endpoint: &ep}, nil
}

// newRemoteLimitedSession builds a remote session parked at a usage-limit wall
// whose agent has exited (old sandbox reports alive=false), registers it with the
// manager, and wires a respawn that re-provisions onto `fresh`. Returns the
// manager, repoID, the parked instance, and the backend that records respawns.
func newRemoteLimitedSession(t *testing.T, old, fresh *mockSandbox, prompt string) (*Manager, string, *session.Instance, *sandboxBackend) {
	t.Helper()
	manager, repoID, repoPath := newStatusTestManager(t)

	oldBackend := newSandboxBackend()
	freshBackend := newSandboxBackend()

	// The respawn's re-provision resolves the docker Runtime out of the registry;
	// point it at the fresh mock sandbox instead of a real container.
	t.Cleanup(session.SetRuntimeForTest(session.BackendDocker, func() session.Runtime {
		return mockRuntime{backend: freshBackend, endpoint: fresh.endpoint()}
	}))
	// Build the session against the OLD sandbox: the backend factory supplies the
	// sandbox backend, RemoteAgentServer pins its agent-server at the old endpoint.
	restoreFactory := session.SetBackendFactoryForTest(func(session.InstanceOptions, string) (session.Backend, error) {
		return oldBackend, nil
	})
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:             "limited-remote",
		Path:              repoPath,
		Program:           "claude",
		RemoteAgentServer: &session.AgentServerEndpoint{URL: old.srv.URL, Token: "tok"},
	})
	restoreFactory()
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}

	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)
	inst.Prompt = prompt
	inst.SetLimitReached(time.Now().Add(-time.Hour))

	seedDiskInstance(t, repoID, inst.Title, repoPath)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(repoID, inst.Title)] = inst
	manager.mu.Unlock()
	return manager, repoID, inst, oldBackend
}

// TestResumeFromLimit_RemoteRespawnTargetsFreshSandbox is the #1786 regression.
// The agent exited while blocked at the limit wall, so resume takes the respawn
// arm; the respawn re-provisions a FRESH sandbox and rebinds the instance to its
// endpoint. The prompt must reach that fresh sandbox. Pre-fix, resumeFromLimit
// captured the agent-server BEFORE the respawn and reused it after, so the prompt
// went to the torn-down sandbox and the limit was never cleared.
func TestResumeFromLimit_RemoteRespawnTargetsFreshSandbox(t *testing.T) {
	oldSandbox := newMockSandbox(t, false) // agent exited while blocked
	freshSandbox := newMockSandbox(t, true)

	manager, repoID, inst, backend := newRemoteLimitedSession(t, oldSandbox, freshSandbox, "finish the migration")

	if err := manager.resumeFromLimit(ResumeFromLimitRequest{Title: inst.Title, RepoID: repoID}); err != nil {
		t.Fatalf("resumeFromLimit: %v", err)
	}

	if got := backend.respawnCount(); got != 1 {
		t.Fatalf("a dead remote agent must be re-spawned exactly once, got %d", got)
	}
	if got := oldSandbox.gotPrompts(); len(got) != 0 {
		t.Errorf("prompt went to the torn-down sandbox (#1786 stale agent-server): %v", got)
	}
	want := []string{"finish the migration"}
	if got := freshSandbox.gotPrompts(); len(got) != 1 || got[0] != want[0] {
		t.Errorf("re-provisioned sandbox prompts = %v, want %v", got, want)
	}
	if inst.LimitReached() {
		t.Error("limit liveness must be cleared once the resume lands on the fresh sandbox")
	}
}

// TestResumeFromLimit_RemoteLiveStallKeepsSandbox is the other arm: a remote
// agent that is merely STALLED at the wall (still alive) needs no respawn, so the
// prompt must go to its existing sandbox. It fences the fix — re-fetching the
// agent-server must not disturb the no-respawn path.
func TestResumeFromLimit_RemoteLiveStallKeepsSandbox(t *testing.T) {
	liveSandbox := newMockSandbox(t, true) // stalled, but the agent is up
	freshSandbox := newMockSandbox(t, true)

	manager, repoID, inst, backend := newRemoteLimitedSession(t, liveSandbox, freshSandbox, "keep going")

	if err := manager.resumeFromLimit(ResumeFromLimitRequest{Title: inst.Title, RepoID: repoID}); err != nil {
		t.Fatalf("resumeFromLimit: %v", err)
	}

	if got := backend.respawnCount(); got != 0 {
		t.Fatalf("a live stall needs no re-spawn, got %d", got)
	}
	if got := freshSandbox.gotPrompts(); len(got) != 0 {
		t.Errorf("a live stall must not re-provision a sandbox, but one got prompts: %v", got)
	}
	want := []string{"keep going"}
	if got := liveSandbox.gotPrompts(); len(got) != 1 || got[0] != want[0] {
		t.Errorf("live sandbox prompts = %v, want %v", got, want)
	}
	if inst.LimitReached() {
		t.Error("limit liveness must be cleared after resuming a live stall")
	}
}

// TestResumeLimitedSessions_RemoteBlipDoesNotRespawn is codex's #3 on PR #1804:
// the limit-resume path bypassing the remote-loss debounce.
//
// The poll's debounce guards the Lost path. It does NOT guard this one, and this
// one runs LATER IN THE SAME TICK: RunDaemon calls RefreshStatuses, then
// RestoreLostSessions, then ResumeLimitedSessions. A limit-parked remote session
// whose sandbox blips is left at LiveLimitReached by the poll (correctly — one
// unanswered probe is not death), and then auto-resume picks it up, sees its own
// probe fail, and takes the re-spawn arm. For docker/ssh/hook that Respawn is
// recoverSandbox: a brand-new sandbox, the branch cloned from origin, and the
// original container left running and unreferenced with all its unpushed work.
// So with limit_auto_resume on and the window due, a single blip re-provisioned
// anyway and the debounce bought nothing.
//
// The fix is the DISTINCTION rather than a second debounce: an unanswered probe
// (probeUnknown) can never reach the re-spawn arm. Here the sandbox stops
// answering while its container keeps running — it must not be re-provisioned,
// and the session must stay parked for a later retry.
func TestResumeLimitedSessions_RemoteBlipDoesNotRespawn(t *testing.T) {
	oldSandbox := newMockSandbox(t, true) // stalled at the wall, container alive
	freshSandbox := newMockSandbox(t, true)

	manager, _, inst, backend := newRemoteLimitedSession(t, oldSandbox, freshSandbox, "keep going")
	manager.cfg.LimitAutoResume = true

	// The blip: the agent-server stops answering. The sandbox is NOT gone — it is
	// still running, holding commits that were never pushed.
	oldSandbox.srv.Close()

	manager.ResumeLimitedSessions()

	if got := backend.respawnCount(); got != 0 {
		t.Fatalf("respawns = %d, want 0 — a single unanswered probe re-provisioned a limit-parked remote, orphaning its live sandbox and every unpushed commit on it (#1794)", got)
	}
	if got := freshSandbox.gotPrompts(); len(got) != 0 {
		t.Fatalf("a replacement sandbox was provisioned and prompted (%v) on the strength of one blip", got)
	}
	if !inst.LimitReached() {
		t.Error("the session must stay parked at the wall so a later tick can retry once the transport recovers, not be silently resolved by a failed probe")
	}
}

// TestResumeLimitedSessions_RemoteDeadAgentStillRespawns fences the fix above
// from becoming "never re-spawn a remote". The #1786 case must still work: the
// sandbox ANSWERS that its agent exited while blocked at the wall. That is
// authoritative, not a blip, so the re-spawn fires immediately with no debounce
// — and the prompt lands on the fresh sandbox.
func TestResumeLimitedSessions_RemoteDeadAgentStillRespawns(t *testing.T) {
	oldSandbox := newMockSandbox(t, false) // answers: the agent exited
	freshSandbox := newMockSandbox(t, true)

	manager, _, inst, backend := newRemoteLimitedSession(t, oldSandbox, freshSandbox, "finish the migration")
	manager.cfg.LimitAutoResume = true

	manager.ResumeLimitedSessions()

	if got := backend.respawnCount(); got != 1 {
		t.Fatalf("respawns = %d, want 1 — an ANSWERED dead-agent report is authoritative and must re-spawn at once; the debounce is only for probes that could not be answered", got)
	}
	want := []string{"finish the migration"}
	if got := freshSandbox.gotPrompts(); len(got) != 1 || got[0] != want[0] {
		t.Fatalf("fresh sandbox prompts = %v, want %v", got, want)
	}
	if inst.LimitReached() {
		t.Error("the limit must be cleared once the resume lands")
	}
}

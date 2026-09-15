package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// These tests pin the daemon-side program gate that closes the 60s
// readiness-timeout bug for raw RPC callers. The bug: Manager.CreateSession only
// defaulted an EMPTY Program; a non-empty value flowed unvalidated into
// provisioning, where sandboxWorkspace.agentName() returns "" for any program
// that runs no known agent (its Claude fallback fires only for an EMPTY
// program). The empty agent strips every per-agent credential from the launch
// environment (the deliberate fail-closed), the agent fails to become ready, and
// the request blocks for the full 60s readiness timeout with an embedded pane
// snippet instead of an immediate, program-naming validation error.
//
// The fix lives at the RPC boundary (controlServer.createSession — the single
// entry point for both the net/rpc control socket and the HTTP /v1/CreateSession
// route), NOT in Manager.CreateSession. That placement is load-bearing: the
// daemon's own root-agent ensure loop and DeliverPrompt auto-create call
// Manager.CreateSession DIRECTLY with programs a raw RPC caller cannot (including
// root_agents.program entries that are custom commands running NO known agent,
// e.g. "/opt/bare-root"). Validating in Manager.CreateSession would reject those
// and break the root-agent ensure flow; validating at the RPC boundary rejects
// only external input. TestControlServer_CreateSession_DirectManagerCallSkipsGate
// pins that contract.

// TestControlServer_CreateSession_RejectsUnsupportedProgram: the RPC entry point
// rejects a program that runs no known agent up front, with a program-naming
// validation error carrying the same guidance the sibling surfaces give.
func TestControlServer_CreateSession_RejectsUnsupportedProgram(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installInstantBackend(t)
	repoPath := setupControlRepo(t)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	server := &controlServer{manager: manager}

	// A plausible typo / pre-upgrade future agent name, a bare unknown agent, a
	// shell that is not an agent, and whitespace-only: each is a program whose
	// executable is not a recognized agent, so each would trigger the bug.
	for _, program := range []string{
		"my-custom-agent",
		"notanagent",
		"bash",
		"   ",
	} {
		var resp CreateSessionResponse
		err := server.CreateSession(CreateSessionRequest{
			Title:    "unsupported",
			RepoPath: repoPath,
			Program:  program,
		}, &resp)
		if err == nil {
			t.Fatalf("expected unsupported program %q to be rejected", program)
		}
		// The rejection names the offending program, not a pane snippet.
		if !strings.Contains(err.Error(), program) {
			t.Fatalf("rejection for %q must name the offending program, got: %v", program, err)
		}
		// It lists the known agents, so the caller is steered to a valid choice.
		if !strings.Contains(err.Error(), tmux.SupportedProgramsString()) {
			t.Fatalf("rejection for %q must list the known agents, got: %v", program, err)
		}
		// It points at program_overrides, the same migration guidance config/CLI
		// give, so a raw RPC caller is steered rather than left to parse a pane.
		if !strings.Contains(err.Error(), "program_overrides") {
			t.Fatalf("rejection for %q must include the program_overrides guidance, got: %v", program, err)
		}
		// Critically: an immediate validation error, NOT the 60s readiness timeout.
		if strings.Contains(err.Error(), "timed out") {
			t.Fatalf("rejection for %q surfaced as a readiness timeout instead of an immediate validation error: %v", program, err)
		}
		// Nothing was created: the response carries no instance.
		if resp.Instance.ID != "" {
			t.Fatalf("rejection for %q must not create an instance, got %+v", program, resp.Instance)
		}
	}
}

// TestControlServer_CreateSession_RejectsUnsupportedProgramBeforeReservation: the
// validation fires before any title is reserved or sandbox provisioned, so a
// rejected program leaves the requested title free. A supported-program create
// with the same title succeeding immediately after a rejection proves the title
// was never held.
func TestControlServer_CreateSession_RejectsUnsupportedProgramBeforeReservation(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installInstantBackend(t)
	repoPath := setupControlRepo(t)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	server := &controlServer{manager: manager}

	const title = "reusable-title"
	var resp CreateSessionResponse
	if err := server.CreateSession(CreateSessionRequest{
		Title:    title,
		RepoPath: repoPath,
		Program:  "my-custom-agent",
	}, &resp); err == nil {
		t.Fatal("expected unsupported program to be rejected before reserving the title")
	}

	// The title was never reserved, so a supported-program create reusing it
	// succeeds immediately rather than colliding with held state.
	var resp2 CreateSessionResponse
	if err := server.CreateSession(CreateSessionRequest{
		Title:    title,
		RepoPath: repoPath,
		Program:  tmux.ProgramClaude,
	}, &resp2); err != nil {
		t.Fatalf("supported program create after a rejected one must succeed (title was not held), got: %v", err)
	}
	if resp2.Instance.Title != title {
		t.Fatalf("reused title = %q, want %q", resp2.Instance.Title, title)
	}
}

// TestControlServer_CreateSession_AcceptsEverySupportedProgram: no false rejection
// of any tmux.SupportedPrograms entry. The validation must accept exactly the
// enum, data-driven off the same slice every other surface checks, so adding an
// agent cannot silently break daemon creates. It runs the FULL create
// (provisioning + readiness wait) per program, so it also confirms the accepted
// program reaches a ready session rather than merely passing the gate.
func TestControlServer_CreateSession_AcceptsEverySupportedProgram(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installUniversalReadyBackend(t)
	repoPath := setupControlRepo(t)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	server := &controlServer{manager: manager}

	for _, program := range tmux.SupportedPrograms {
		var resp CreateSessionResponse
		if err := server.CreateSession(CreateSessionRequest{
			Title:    "pg-" + program,
			RepoPath: repoPath,
			Program:  program,
		}, &resp); err != nil {
			t.Fatalf("supported program %q must be accepted and reach ready, got: %v", program, err)
		}
		if resp.Instance.ID == "" {
			t.Fatalf("supported program %q must create an instance, got empty", program)
		}
	}
}

// TestControlServer_CreateSession_AcceptsResolvedCommandPrograms: the gate keys
// off tmux.DetectAgentExecutable, not a bare-enum check, so a fully-resolved
// command string whose executable is a known agent is ACCEPTED over the RPC
// boundary. A bare-enum check (config.ValidateProgramEnum, the bug report's
// suggested fix) would reject every one of these; DetectAgentExecutable keeps
// them working.
func TestControlServer_CreateSession_AcceptsResolvedCommandPrograms(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installUniversalReadyBackend(t)
	repoPath := setupControlRepo(t)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	server := &controlServer{manager: manager}

	// Each program's executable is a recognized agent (pinned by the
	// precondition), so it must NOT be rejected by the RPC gate.
	resolvedCommands := []struct {
		program string
		agent   string
	}{
		{"/opt/claude --model opus", tmux.ProgramClaude},
		{"/home/user/.local/bin/claude --dangerously-skip-permissions", tmux.ProgramClaude},
		{"CLAUDE_CONFIG_DIR=$HOME/.claude claude", tmux.ProgramClaude},
		{"codex", tmux.ProgramCodex},
	}
	for _, tc := range resolvedCommands {
		if got := tmux.DetectAgentExecutable(tc.program); got != tc.agent {
			t.Fatalf("precondition: DetectAgentExecutable(%q) = %q, want %q — this test only proves the RPC gate accepts programs whose executable is a known agent", tc.program, got, tc.agent)
		}
		var resp CreateSessionResponse
		if err := server.CreateSession(CreateSessionRequest{
			Title:    "cmd-" + sanitizeForTitle(tc.program),
			RepoPath: repoPath,
			Program:  tc.program,
		}, &resp); err != nil {
			t.Fatalf("resolved-command program %q (-> %q) must be accepted, not rejected as unsupported; got: %v", tc.program, tc.agent, err)
		}
	}
}

// TestControlServer_CreateSession_RejectsCompoundCommandWithNonAgentExecutable:
// a compound command that embeds an agent token as an ARGUMENT ("./collect
// codex") is rejected by the RPC gate because its executable ("./collect") does
// not run a recognized agent. This is the correct behavior: the gate checks the
// executable, not any token in the command, so appending a supported agent name
// as an argument cannot bypass the gate.
func TestControlServer_CreateSession_RejectsCompoundCommandWithNonAgentExecutable(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installInstantBackend(t)
	repoPath := setupControlRepo(t)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	server := &controlServer{manager: manager}

	// Precondition: DetectAgentFromCommand still sees the embedded token, so
	// the old (any-token) check would accept this — the fix is in switching to
	// DetectAgentExecutable (executable-only).
	if got := tmux.DetectAgentFromCommand("./collect codex"); got != tmux.ProgramCodex {
		t.Fatalf("precondition: DetectAgentFromCommand(\"./collect codex\") = %q, want %q", got, tmux.ProgramCodex)
	}
	if got := tmux.DetectAgentExecutable("./collect codex"); got != "" {
		t.Fatalf("precondition: DetectAgentExecutable(\"./collect codex\") = %q, want \"\" (executable is not an agent)", got)
	}

	var resp CreateSessionResponse
	err = server.CreateSession(CreateSessionRequest{
		Title:    "compound-non-agent-exec",
		RepoPath: repoPath,
		Program:  "./collect codex",
	}, &resp)
	if err == nil {
		t.Fatal("expected \"./collect codex\" to be rejected (executable is not a recognized agent), got success")
	}
	if resp.Instance.ID != "" {
		t.Fatalf("rejected program must not create an instance, got %+v", resp.Instance)
	}
}

// TestControlServer_CreateSession_EmptyProgramDefaultsAndSucceeds: the gate must
// not break the empty-program path. An empty program is the no-explicit-program
// path: the gate skips it and Manager.CreateSession defaults it to the repo's
// resolved program (DefaultConfig -> claude), which then proceeds to a ready
// session.
func TestControlServer_CreateSession_EmptyProgramDefaultsAndSucceeds(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installInstantBackend(t)
	repoPath := setupControlRepo(t)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	server := &controlServer{manager: manager}

	var resp CreateSessionResponse
	if err := server.CreateSession(CreateSessionRequest{
		Title:    "defaulted",
		RepoPath: repoPath,
		// Program intentionally empty: resolves to the daemon's default.
	}, &resp); err != nil {
		t.Fatalf("empty program must default and succeed, got: %v", err)
	}
	if !tmux.IsSupportedProgram(resp.Instance.Program) {
		t.Fatalf("empty program must be defaulted to a supported agent, got Program %q in %+v", resp.Instance.Program, resp.Instance)
	}
}

// TestControlServer_CreateSession_DirectManagerCallSkipsGate pins the
// load-bearing design choice: the program gate is on the RPC boundary
// (controlServer.createSession), NOT in Manager.CreateSession. The daemon's own
// root-agent ensure loop (rootagent_create.go → createVerifiedRoot →
// m.CreateSession) and DeliverPrompt auto-create (delivery.go → m.CreateSession)
// call Manager.CreateSession DIRECTLY, bypassing controlServer, with programs a
// raw RPC caller cannot send — including root_agents.program entries that are
// custom commands running NO known agent (e.g. "/opt/bare-root", exercised by
// TestEnsureRootAgentsCreatesRootAtBareCloneWorktree). This test asserts a direct
// manager.CreateSession call with such a program SUCCEEDS (the gate does not
// fire), so the root-agent ensure flow keeps working. Moving the gate into
// Manager.CreateSession would make this test fail and break that flow.
func TestControlServer_CreateSession_DirectManagerCallSkipsGate(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installUniversalReadyBackend(t)
	repoPath := setupControlRepo(t)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// A custom non-agent program (the root-agent ensure shape) handed DIRECTLY to
	// Manager.CreateSession — bypassing controlServer — must succeed (no gate).
	// DetectAgentFromCommand returns "" for this (the basename is not a known
	// agent), so the SAME string over the RPC boundary would be rejected.
	if got := tmux.DetectAgentFromCommand("/opt/bare-root"); got != "" {
		t.Fatalf("precondition: DetectAgentFromCommand(\"/opt/bare-root\") = %q, want \"\" — this test proves a program the RPC gate REJECTS is still accepted by a direct manager call", got)
	}
	data, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title:    "root-agent-shape",
		RepoPath: repoPath,
		Program:  "/opt/bare-root",
	})
	if err != nil {
		t.Fatalf("direct Manager.CreateSession with a custom non-agent program (the root-agent ensure contract) must succeed — the gate is on the RPC boundary, not the manager; got: %v", err)
	}
	if data.Title != "root-agent-shape" {
		t.Fatalf("direct create title = %q, want %q", data.Title, "root-agent-shape")
	}
}

// universalReadyBackend is a FakeBackend whose Preview returns pane content that
// satisfies task.isReadyContent for EVERY tmux.SupportedPrograms agent, so a full
// CreateSession + readiness wait succeeds for any program. The per-agent
// readiness glyphs differ (claude "❯", codex a line-leading "›", aider "\n> " or
// "Aider v", gemini "╰", amp a contiguous "╭─…╮ │ … │ ╰─…╯" box, opencode a "┃"
// row directly above a "╹▀" bottom rule, devin "❭"); for a resolved agent not in
// the enum isReadyContent's default branch treats any non-blank pane as ready,
// which this also satisfies. See task/runner.go:isReadyContent.
type universalReadyBackend struct {
	*session.FakeBackend
}

func (universalReadyBackend) Preview(*session.Instance) (string, error) {
	return readyContentForAllAgents, nil
}

// readyContentForAllAgents carries one ready marker per supported agent, each on
// its own line so the positional matchers (codex's line-leading "›", amp's
// contiguous top→interior→bottom frame, opencode's "┃" row above the "╹▀" bottom
// rule) parse independently. isReadyContent's per-agent arm for a given program
// finds its own marker here and returns true.
var readyContentForAllAgents = strings.Join([]string{
	"ready",
	"❯",          // claude
	"›",          // codex (line-leading glyph)
	"Aider v0.0", // aider banner
	"> ",         // aider prompt ("\n> ")
	"╰",          // gemini box corner
	"❭",          // devin composer glyph
	"╭──────╮",   // amp frame top rule
	"│ ▌▌  │",    // amp frame interior
	"╰──────╯",   // amp frame bottom rule (also satisfies gemini's "╰")
	"┃ █  ┃",     // opencode frame interior (directly above its bottom rule)
	"╹▀▀▀▀╹",     // opencode frame bottom rule
	"",
}, "\n")

// installUniversalReadyBackend swaps the backend factory for one whose Preview
// satisfies every supported agent's readiness heuristic. A package-var restore
// (via SetBackendFactoryForTest) re-installs the real factory on cleanup, the
// same pattern installInstantBackend uses.
func installUniversalReadyBackend(t *testing.T) {
	t.Helper()
	restore := session.SetBackendFactoryForTest(func(opts session.InstanceOptions, absPath string) (session.Backend, error) {
		backend := session.NewFakeBackend()
		backend.CompleteStart()
		return universalReadyBackend{backend}, nil
	})
	t.Cleanup(restore)
}

// TestControlServer_CreateSession_HttpRejectsUnsupportedProgram is the live
// cross-check for the OTHER external surface in the bug report — the token-gated
// HTTP/JSON plane (POST /v1/CreateSession). It binds the real HTTP mux
// (newHTTPMux, the same route table the daemon's listener serves) and POSTs a
// JSON body with an unsupported program. The request must return an immediate
// error envelope carrying the validation message, not block for the 60s
// readiness timeout. (Token auth is a listener-level concern, so this exercises
// the route handler directly — the bug is in the handler, not the gate.)
func TestControlServer_CreateSession_HttpRejectsUnsupportedProgram(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installInstantBackend(t)
	repoPath := setupControlRepo(t)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	mux := newHTTPMux(&controlServer{manager: manager})

	body, err := json.Marshal(CreateSessionRequest{
		Title:    "http-unsupported",
		RepoPath: repoPath,
		Program:  "my-custom-agent",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/CreateSession", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()

	start := time.Now()
	mux.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	if rec.Code == http.StatusOK {
		t.Fatalf("expected an error response, got 200: %s", rec.Body.String())
	}
	bodyStr := rec.Body.String()
	// The error envelope's message names the program, the known-agent list, and
	// the program_overrides guidance — the same message the wire/CLI surfaces give.
	if !strings.Contains(bodyStr, "my-custom-agent") {
		t.Fatalf("HTTP error body must name the offending program, got: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "known agents") || !strings.Contains(bodyStr, "program_overrides") {
		t.Fatalf("HTTP error body must list the known agents and program_overrides guidance, got: %s", bodyStr)
	}
	// Fail-fast: the HTTP handler returns in well under the 60s readiness timeout.
	if elapsed > 5*time.Second {
		t.Fatalf("HTTP rejection took %s — expected an immediate (<5s) validation error, not a readiness-timeout-class stall", elapsed)
	}
}

// TestControlServer_CreateSession_HttpAcceptsSupportedProgram is the happy-path
// control for the HTTP cross-check: the SAME route accepts a supported program
// and returns a 200 success envelope with the created instance.
func TestControlServer_CreateSession_HttpAcceptsSupportedProgram(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installInstantBackend(t)
	repoPath := setupControlRepo(t)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	mux := newHTTPMux(&controlServer{manager: manager})

	body, err := json.Marshal(CreateSessionRequest{
		Title:    "http-supported",
		RepoPath: repoPath,
		Program:  tmux.ProgramClaude,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/CreateSession", strings.NewReader(string(body)))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP create with a supported program must return 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestControlServer_DeliverPrompt_ExistingSessionWithNonAgentProgramDelivers:
// the program gate on the auto-create path must NOT reject a DeliverPrompt to
// an EXISTING session whose stored program is not a recognized agent (e.g. a
// root session running "/opt/bare-root"). The gate only applies when an absent
// session would be auto-created; when the target already exists, req.Program is
// never read by Manager.CreateSession, so validation would only block legitimate
// delivery.
func TestControlServer_DeliverPrompt_ExistingSessionWithNonAgentProgramDelivers(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)

	// Register an existing session with a non-agent program (the root-session
	// shape). The gate must not block delivery to this already-running session.
	fakeBackend := session.NewFakeBackend()
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "bare-root",
		Path:    repoPath,
		Program: "/opt/bare-root",
	})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	inst.SetBackend(fakeBackend)
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)
	seedDiskInstance(t, repoID, "bare-root", repoPath)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(repoID, "bare-root")] = inst
	manager.mu.Unlock()

	server := &controlServer{manager: manager}
	var resp DeliverPromptResponse
	err = server.DeliverPrompt(DeliverPromptRequest{
		Title:    "bare-root",
		RepoPath: repoPath,
		Program:  "/opt/bare-root",
		Prompt:   "do something",
	}, &resp)
	if err != nil {
		t.Fatalf("DeliverPrompt to an existing session with a non-agent program must not be rejected by the program gate; got: %v", err)
	}
}

// sanitizeForTitle reduces a resolved command to a tmux-safe title suffix for
// the unique-title requirement of sequential creates.
func sanitizeForTitle(s string) string {
	r := strings.NewReplacer(" ", "-", "/", "-", "$", "", ".", "")
	return r.Replace(s)
}

// TestControlServer_CreateSession_RpcWireRejectsUnsupportedProgram is the live
// cross-check: it exercises the REAL net/rpc transport (gob over the unix
// socket) the bug report's "raw RPC caller over the unix socket" uses.
// startControlServer binds the real control service on the daemon socket, and
// callDaemonNoEnsure dials it and calls controlServer.CreateSession over the wire
// — the same path TestControlServerCreateAndKillSession uses for a happy-path
// create. With an unsupported program the wire call must return the immediate
// validation error, not block for the 60s readiness timeout.
func TestControlServer_CreateSession_RpcWireRejectsUnsupportedProgram(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installInstantBackend(t)
	repoPath := setupControlRepo(t)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	closeServer, err := startControlServer(manager, nil, nil, nil)
	if err != nil {
		t.Fatalf("startControlServer: %v", err)
	}
	t.Cleanup(func() { _ = closeServer() })

	start := time.Now()
	var resp CreateSessionResponse
	err = callDaemonNoEnsure("CreateSession", CreateSessionRequest{
		Title:    "wire-unsupported",
		RepoPath: repoPath,
		Program:  "my-custom-agent",
	}, &resp)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected the wire RPC to reject the unsupported program, got success: %+v", resp)
	}
	// The error carries the validation message, naming the program and the list.
	if !strings.Contains(err.Error(), "my-custom-agent") || !strings.Contains(err.Error(), "known agents") {
		t.Fatalf("wire RPC rejection must name the program and the known-agent list, got: %v", err)
	}
	// The fail-fast property: the wire call returns in well under the 60s readiness
	// timeout the bug surfaced. 5s is a generous upper bound for a local socket
	// round-trip + validation; the bug would take ~60s.
	if elapsed > 5*time.Second {
		t.Fatalf("wire RPC rejection took %s — expected an immediate (<5s) validation error, not a readiness-timeout-class stall", elapsed)
	}
	if resp.Instance.ID != "" {
		t.Fatalf("wire RPC rejection must not create an instance, got %+v", resp.Instance)
	}
}

// TestControlServer_CreateSession_RpcWireAcceptsSupportedProgram is the
// happy-path control for the wire cross-check: the SAME real net/rpc transport
// accepts a supported program and creates a ready session, proving the gate
// does not over-reject legitimate creates over the wire.
func TestControlServer_CreateSession_RpcWireAcceptsSupportedProgram(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installInstantBackend(t)
	repoPath := setupControlRepo(t)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	closeServer, err := startControlServer(manager, nil, nil, nil)
	if err != nil {
		t.Fatalf("startControlServer: %v", err)
	}
	t.Cleanup(func() { _ = closeServer() })

	var resp CreateSessionResponse
	if err := callDaemonNoEnsure("CreateSession", CreateSessionRequest{
		Title:    "wire-supported",
		RepoPath: repoPath,
		Program:  tmux.ProgramClaude,
	}, &resp); err != nil {
		t.Fatalf("wire RPC create with a supported program must succeed, got: %v", err)
	}
	if resp.Instance.ID == "" || resp.Instance.Program != tmux.ProgramClaude {
		t.Fatalf("wire RPC create must return the created instance, got %+v", resp.Instance)
	}
}

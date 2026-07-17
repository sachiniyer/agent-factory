package configagent

import (
	"errors"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// These tests never reach a daemon: createSession is stubbed, so nothing here
// dials the control socket, touches the real AF home, or creates a session.
// AGENT_FACTORY_HOME is pointed at a throwaway dir for every test, so the
// config.LoadConfig inside Spawn materializes defaults there.

// tempAFHome points AGENT_FACTORY_HOME at a fresh temp dir, so a test can never
// read or write the real ~/.agent-factory.
func tempAFHome(t *testing.T) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
}

// stubCreate replaces the daemon round trip and returns a pointer to a counter of
// how many times it was called, plus the last request it saw.
func stubCreate(t *testing.T, fn func(daemon.CreateSessionRequest) (*session.InstanceData, error)) {
	t.Helper()
	prev := createSession
	createSession = fn
	t.Cleanup(func() { createSession = prev })
}

// stubResolve replaces the repo config resolver so a test can choose the agent
// and its program_overrides without materializing a git repo.
func stubResolve(t *testing.T, cfg config.Config) {
	t.Helper()
	prev := resolveConfigForRepo
	resolveConfigForRepo = func(string) (*config.ResolvedConfig, error) {
		return &config.ResolvedConfig{Config: cfg}, nil
	}
	t.Cleanup(func() { resolveConfigForRepo = prev })
}

// TestSpawnMissingProgramReturnsTypedErrorAndCreatesNothing is the never-hang
// guarantee. Spawning a binary that does not exist does not fail fast — it fails
// as an opaque readiness timeout minutes later, because the agent never reaches
// a ready prompt. So the check must happen BEFORE the daemon round trip, and the
// assertion that matters is that createSession was never called.
func TestSpawnMissingProgramReturnsTypedErrorAndCreatesNothing(t *testing.T) {
	tempAFHome(t)
	stubResolve(t, config.Config{
		DefaultProgram:   "claude",
		ProgramOverrides: map[string]string{"claude": "/nonexistent/definitely-not-installed --flag"},
	})

	created := 0
	stubCreate(t, func(daemon.CreateSessionRequest) (*session.InstanceData, error) {
		created++
		return &session.InstanceData{}, nil
	})

	_, err := Spawn(Options{Mode: ModeOnboard, RepoPath: "/tmp/some-repo"})
	if err == nil {
		t.Fatal("expected a missing program to be rejected")
	}
	if created != 0 {
		t.Fatalf("no session may be created when the agent binary is missing, got %d create call(s)", created)
	}

	var pe *ProgramUnavailableError
	if !errors.As(err, &pe) {
		t.Fatalf("error must be a *ProgramUnavailableError so a caller can render a one-line fallback, got %T: %v", err, err)
	}
	if pe.Agent != "claude" {
		t.Errorf("typed error should name the agent, got %q", pe.Agent)
	}
	if !strings.Contains(pe.Command, "/nonexistent/definitely-not-installed") {
		t.Errorf("typed error should carry the resolved command, got %q", pe.Command)
	}
	// The message is preflight's, so it stays actionable: what is missing and how to fix it.
	if !strings.Contains(err.Error(), "not installed or not on PATH") {
		t.Errorf("error should be preflight's actionable message, got: %v", err)
	}
}

// TestSpawnSendsInPlaceRequest pins the create request's load-bearing fields.
// InPlace=true is what makes a config session create no branch and no worktree;
// AutoYes=false is what keeps it an interactive walkthrough (and keeps claude's
// --permission-mode bypassPermissions off, since that append is gated on AutoYes).
func TestSpawnSendsInPlaceRequest(t *testing.T) {
	tempAFHome(t)
	// /bin/sh always exists in the test container, so preflight passes and the
	// test exercises the request rather than the missing-binary path.
	stubResolve(t, config.Config{
		DefaultProgram:   "codex",
		ProgramOverrides: map[string]string{"codex": "/bin/sh"},
	})

	var got daemon.CreateSessionRequest
	stubCreate(t, func(req daemon.CreateSessionRequest) (*session.InstanceData, error) {
		got = req
		return &session.InstanceData{Title: "config"}, nil
	})

	data, err := Spawn(Options{Mode: ModeOnboard, RepoPath: "/tmp/some-repo"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if data == nil || data.Title != "config" {
		t.Fatalf("spawn should return the created session, got %+v", data)
	}

	if !got.InPlace {
		t.Error("InPlace must be true: a config session must create no branch and no worktree")
	}
	if got.AutoYes {
		t.Error("AutoYes must be false: this is an interactive walkthrough, and auto-yes would answer the user's own questions")
	}
	if got.Program != "codex" {
		t.Errorf("Program should be the repo's resolved default_program, got %q", got.Program)
	}
	if got.RepoPath != "/tmp/some-repo" {
		t.Errorf("RepoPath should be the caller's repo, got %q", got.RepoPath)
	}
	if got.ForceRemote {
		t.Error("ForceRemote must be false: in-place is local-only")
	}
	// TitleBase (not Title) so a second press auto-suffixes instead of colliding.
	if got.TitleBase != configSessionTitleBase || got.Title != "" {
		t.Errorf("expected TitleBase=%q with an empty Title so the daemon can auto-suffix, got Title=%q TitleBase=%q",
			configSessionTitleBase, got.Title, got.TitleBase)
	}
	if session.IsReservedTitle(got.TitleBase) {
		t.Errorf("config session title base %q collides with the reserved root-agent name", got.TitleBase)
	}
}

// TestSpawnDeliversBriefingAsThePrompt pins the delivery seam: the briefing rides
// in as the session Prompt, which the daemon's create path already hands to
// task.StartAndSendPrompt (Start → WaitForReady → trust prompt → SendPrompt).
// If this ever became a flag or a file, an unknown flag would kill the agent at
// exec and surface as a readiness timeout.
func TestSpawnDeliversBriefingAsThePrompt(t *testing.T) {
	tempAFHome(t)
	stubResolve(t, config.Config{
		DefaultProgram:   "codex",
		ProgramOverrides: map[string]string{"codex": "/bin/sh"},
	})

	var got daemon.CreateSessionRequest
	stubCreate(t, func(req daemon.CreateSessionRequest) (*session.InstanceData, error) {
		got = req
		return &session.InstanceData{}, nil
	})

	if _, err := Spawn(Options{Mode: ModeChange, RepoPath: "/tmp/some-repo"}); err != nil {
		t.Fatalf("spawn: %v", err)
	}

	if got.Prompt == "" {
		t.Fatal("the briefing must be delivered as the session Prompt")
	}
	for _, want := range []string{
		"What do you want to change?", // the ModeChange opening
		"### `listen_addr`",           // the manifest
		"Do not run git.",             // the scope fence
		"af config set require_token true",
	} {
		if !strings.Contains(got.Prompt, want) {
			t.Errorf("prompt is missing %q", want)
		}
	}
}

// TestSpawnRequiresARepoPath pins the in-place precondition: the seam resolves a
// repo root and current branch from this path, so an empty one must fail with a
// clear message rather than a git error from three layers down.
func TestSpawnRequiresARepoPath(t *testing.T) {
	tempAFHome(t)
	created := 0
	stubCreate(t, func(daemon.CreateSessionRequest) (*session.InstanceData, error) {
		created++
		return &session.InstanceData{}, nil
	})

	if _, err := Spawn(Options{Mode: ModeOnboard}); err == nil {
		t.Fatal("expected an empty repo path to be rejected")
	}
	if created != 0 {
		t.Fatal("no session may be created without a repo path")
	}
}

// TestSpawnBriefsWithGlobalConfigValues pins the deliberate split: the PROGRAM
// comes from the repo-resolved config (so an in-repo default_program picks the
// agent the user actually gets here), while the BRIEFING describes the GLOBAL
// config (because that is the only file `af config set` writes). Briefing the
// agent on repo-resolved values would show it numbers it cannot change.
func TestSpawnBriefsWithGlobalConfigValues(t *testing.T) {
	tempAFHome(t)

	// Global config on disk says daemon_poll_interval = 7777.
	if _, err := config.SetGlobalConfigValue("daemon_poll_interval", "7777"); err != nil {
		t.Fatalf("seed global config: %v", err)
	}
	// The repo-resolved view says the agent is codex.
	stubResolve(t, config.Config{
		DefaultProgram:     "codex",
		ProgramOverrides:   map[string]string{"codex": "/bin/sh"},
		DaemonPollInterval: 1234, // must NOT be what the briefing shows
	})

	var got daemon.CreateSessionRequest
	stubCreate(t, func(req daemon.CreateSessionRequest) (*session.InstanceData, error) {
		got = req
		return &session.InstanceData{}, nil
	})

	if _, err := Spawn(Options{Mode: ModeOnboard, RepoPath: "/tmp/some-repo"}); err != nil {
		t.Fatalf("spawn: %v", err)
	}

	if got.Program != "codex" {
		t.Errorf("program should come from the repo-resolved config, got %q", got.Program)
	}
	if !strings.Contains(got.Prompt, "current: 7777") {
		t.Error("briefing should show the GLOBAL config value (7777) — that is the file `af config set` writes")
	}
	if strings.Contains(got.Prompt, "current: 1234") {
		t.Error("briefing must not show repo-resolved values the agent cannot write")
	}
}

// TestSpawnPinsTheLocalBackend is the targeting lock, and it is the whole
// premise of the feature rather than a default worth inheriting.
//
// The config agent exists to inspect THE USER'S OWN machine and fix THEIR
// configuration. Leaving Backend empty makes the daemon resolve it from the
// repo's config (session/instance_factory.go resolveBackendKind: empty Backend
// and no ForceRemote falls through to resolveRepoConfig -> ParseBackendKind), so
// a repo declaring `backend = "docker"` / `ssh` / `hook` would spawn the config
// agent on the REMOTE. It would then faithfully inspect the wrong machine and
// report confidently about an environment the user does not have — worse than
// failing outright, because nothing about it looks broken.
//
// So local is pinned explicitly at the spawn site. A remote config session has no
// meaningful semantics today; if one ever does, that is a deliberate change, not
// an inherited one.
func TestSpawnPinsTheLocalBackend(t *testing.T) {
	tempAFHome(t)
	stubResolve(t, config.Config{
		DefaultProgram:   "codex",
		ProgramOverrides: map[string]string{"codex": "/bin/sh"},
	})

	var got daemon.CreateSessionRequest
	stubCreate(t, func(req daemon.CreateSessionRequest) (*session.InstanceData, error) {
		got = req
		return &session.InstanceData{}, nil
	})

	if _, err := Spawn(Options{Mode: ModeOnboard, RepoPath: "/tmp/some-repo"}); err != nil {
		t.Fatalf("spawn: %v", err)
	}

	if got.Backend != config.BackendLocal {
		t.Errorf("Backend = %q, want %q. An empty Backend lets the daemon resolve the runtime from the "+
			"repo's config, so a repo with `backend = \"docker\"` would run the config agent in a container — "+
			"inspecting the wrong machine and reporting success. Pin local at the spawn site.",
			got.Backend, config.BackendLocal)
	}
	if got.ForceRemote {
		t.Error("ForceRemote must stay false: the config agent is local by premise")
	}
}

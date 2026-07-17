package configagent

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
)

// These tests never reach a daemon: the spawn seam is stubbed, so nothing here
// dials the control socket, starts a tmux session, or touches the real AF home.
// AGENT_FACTORY_HOME points at a throwaway dir for every test, so the
// config.LoadConfig inside Spawn materializes defaults there.

// tempAFHome points AGENT_FACTORY_HOME at a fresh temp dir, so a test can never
// read or write the real ~/.agent-factory.
func tempAFHome(t *testing.T) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
}

// stubSpawn replaces the daemon round trip so a test can observe the request
// without a daemon or a tmux server.
func stubSpawn(t *testing.T, fn func(daemon.SpawnConfigAgentRequest) (string, error)) {
	t.Helper()
	prev := spawnViaDaemon
	spawnViaDaemon = fn
	t.Cleanup(func() { spawnViaDaemon = prev })
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

// TestSpawnCreatesNoInstance is THE constraint this seam exists to satisfy, and
// it is enforced structurally rather than by assertion.
//
// The config agent must never be a row in the session list. An Instance IS a row:
// it is persisted to instances.json, and Snapshot() builds the roster by
// iterating the same map the WS attach route resolves against. So the only way to
// guarantee "no row" is to never create an Instance — which is what this pins.
//
// The proof is the request type itself: it carries a program and a briefing and
// NOTHING that could make a session — no Title, no TitleBase, no RepoPath, no
// InPlace, no Backend, no AutoYes. There is no field to get wrong.
//
// That also retires a whole class of bug by construction. The old seam needed a
// test that Backend was pinned to local, because an empty Backend silently
// inherited the repo's `backend = "docker"` and would have run the config agent
// on the wrong machine — inspecting an environment the user does not have and
// reporting success. Here there is no backend to inherit: a bare tmux session is
// local because there is nowhere else for it to be.
func TestSpawnCreatesNoInstance(t *testing.T) {
	tempAFHome(t)
	stubResolve(t, config.Config{
		DefaultProgram:   "codex",
		ProgramOverrides: map[string]string{"codex": "/bin/sh"},
	})

	var got daemon.SpawnConfigAgentRequest
	stubSpawn(t, func(req daemon.SpawnConfigAgentRequest) (string, error) {
		got = req
		return "af-config-1", nil
	})

	name, err := Spawn(Options{Mode: ModeOnboard, RepoPath: "/tmp/some-repo"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if name != "af-config-1" {
		t.Fatalf("spawn should return the tmux session name to attach to, got %q", name)
	}
	if got.Program != "/bin/sh" {
		t.Errorf("Program should be the RESOLVED command (program_overrides applied), got %q", got.Program)
	}

	// The request must stay incapable of creating a session. If a field is added
	// that could, this fails and someone has to justify it.
	rt := reflect.TypeOf(daemon.SpawnConfigAgentRequest{})
	allowed := map[string]bool{"Program": true, "Prompt": true}
	for i := range rt.NumField() {
		if f := rt.Field(i); !allowed[f.Name] {
			t.Errorf("daemon.SpawnConfigAgentRequest grew field %q. This request must not be able to create a "+
				"session: an Instance is a row in the session list, and the config agent must never be one.", f.Name)
		}
	}
}

// TestSpawnMissingProgramReturnsTypedErrorAndCreatesNothing is the never-hang
// guarantee. Spawning a binary that does not exist does not fail fast — it fails
// as an opaque readiness timeout minutes later, because the agent never reaches a
// ready prompt. So the check must happen BEFORE the daemon round trip, and the
// assertion that matters is that the spawn was never attempted.
func TestSpawnMissingProgramReturnsTypedErrorAndCreatesNothing(t *testing.T) {
	tempAFHome(t)
	stubResolve(t, config.Config{
		DefaultProgram:   "claude",
		ProgramOverrides: map[string]string{"claude": "/nonexistent/definitely-not-installed --flag"},
	})

	spawned := 0
	stubSpawn(t, func(daemon.SpawnConfigAgentRequest) (string, error) {
		spawned++
		return "af-config-1", nil
	})

	_, err := Spawn(Options{Mode: ModeOnboard, RepoPath: "/tmp/some-repo"})
	if err == nil {
		t.Fatal("expected a missing program to be rejected")
	}
	if spawned != 0 {
		t.Fatalf("nothing may be spawned when the agent binary is missing, got %d spawn call(s)", spawned)
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
	if !strings.Contains(err.Error(), "not installed or not on PATH") {
		t.Errorf("error should be preflight's actionable message, got: %v", err)
	}
}

// TestSpawnDeliversBriefingAsThePrompt pins the delivery seam: the briefing rides
// in as the request's Prompt, which the daemon delivers over a tmux paste buffer
// (stdin-streamed, so unbounded) once the agent is ready. If it ever became a CLI
// flag, an unknown flag would kill the agent at exec and surface as a readiness
// timeout.
func TestSpawnDeliversBriefingAsThePrompt(t *testing.T) {
	tempAFHome(t)
	stubResolve(t, config.Config{
		DefaultProgram:   "codex",
		ProgramOverrides: map[string]string{"codex": "/bin/sh"},
	})

	var got daemon.SpawnConfigAgentRequest
	stubSpawn(t, func(req daemon.SpawnConfigAgentRequest) (string, error) {
		got = req
		return "af-config-1", nil
	})

	if _, err := Spawn(Options{Mode: ModeChange, RepoPath: "/tmp/some-repo"}); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if got.Prompt == "" {
		t.Fatal("the briefing must be delivered as the request Prompt")
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

// TestSpawnWithoutARepoFallsBackToGlobal pins that the config agent works outside
// a repo. It edits the GLOBAL config and runs at AF home, so a repo is only ever
// a hint about which agent the user prefers here — never a requirement. The old
// in-place seam DID require one (it resolved a repo root and current branch);
// this one must not inherit that constraint.
func TestSpawnWithoutARepoFallsBackToGlobal(t *testing.T) {
	tempAFHome(t)
	if _, err := config.SetGlobalConfigValue("default_program", "codex"); err != nil {
		t.Fatalf("seed global config: %v", err)
	}
	if _, err := config.SetGlobalConfigValue("program_overrides.codex", "/bin/sh"); err != nil {
		t.Fatalf("seed override: %v", err)
	}

	var got daemon.SpawnConfigAgentRequest
	stubSpawn(t, func(req daemon.SpawnConfigAgentRequest) (string, error) {
		got = req
		return "af-config-1", nil
	})

	if _, err := Spawn(Options{Mode: ModeOnboard}); err != nil {
		t.Fatalf("spawn with no repo path must work: %v", err)
	}
	if got.Program != "/bin/sh" {
		t.Errorf("with no repo, the agent comes from the global config, got %q", got.Program)
	}
}

// TestSpawnBriefsWithGlobalConfigValues pins the deliberate split: the PROGRAM
// comes from the repo-resolved config (so an in-repo default_program picks the
// agent the user actually gets here), while the BRIEFING describes the GLOBAL
// config (because that is the only file `af config set` writes). Briefing the
// agent on repo-resolved values would show it numbers it cannot change.
func TestSpawnBriefsWithGlobalConfigValues(t *testing.T) {
	tempAFHome(t)
	if _, err := config.SetGlobalConfigValue("daemon_poll_interval", "7777"); err != nil {
		t.Fatalf("seed global config: %v", err)
	}
	stubResolve(t, config.Config{
		DefaultProgram:     "codex",
		ProgramOverrides:   map[string]string{"codex": "/bin/sh"},
		DaemonPollInterval: 1234, // must NOT be what the briefing shows
	})

	var got daemon.SpawnConfigAgentRequest
	stubSpawn(t, func(req daemon.SpawnConfigAgentRequest) (string, error) {
		got = req
		return "af-config-1", nil
	})

	if _, err := Spawn(Options{Mode: ModeOnboard, RepoPath: "/tmp/some-repo"}); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if got.Program != "/bin/sh" {
		t.Errorf("program should come from the repo-resolved config, got %q", got.Program)
	}
	if !strings.Contains(got.Prompt, "current: 7777") {
		t.Error("briefing should show the GLOBAL config value (7777) — that is the file `af config set` writes")
	}
	if strings.Contains(got.Prompt, "current: 1234") {
		t.Error("briefing must not show repo-resolved values the agent cannot write")
	}
}

// TestReapIsBestEffort pins that reaping is safe with nothing to reap: the TUI
// calls it whenever the takeover ends, including on paths where the spawn never
// produced a session.
func TestReapIsBestEffort(t *testing.T) {
	called := 0
	prev := reapViaDaemon
	reapViaDaemon = func(string) error { called++; return nil }
	t.Cleanup(func() { reapViaDaemon = prev })

	if err := Reap(""); err != nil {
		t.Errorf("reaping an empty name should be a no-op, got %v", err)
	}
	if called != 0 {
		t.Error("an empty session name must not reach the daemon")
	}
	if err := Reap("af-config-1"); err != nil {
		t.Errorf("reap: %v", err)
	}
	if called != 1 {
		t.Errorf("expected one reap call, got %d", called)
	}
}

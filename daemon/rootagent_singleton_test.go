package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// These tests cover the #2216 Phase 6 PR2 daemon integration: the canonical
// [root_agent] singleton (global + personal-project) resolved through
// config.ResolveRootAgent, alongside the legacy root_agents map. They are
// hermetic on the same rules as rootagent_test.go — temp AGENT_FACTORY_HOME,
// the in-process fake backend, no real daemon.

// registerTestProject registers repoPath as a durable project and returns it.
func registerTestProject(t *testing.T, repoPath string) config.Project {
	t.Helper()
	p, err := config.RegisterProject(repoPath)
	if err != nil {
		t.Fatalf("RegisterProject(%s): %v", repoPath, err)
	}
	return p
}

// writePersonalRootAgent writes a registered project's personal config.toml with
// a [root_agent] table whose body is the given TOML (e.g. `enabled = true`). It
// bypasses the write path so resolver/daemon behavior is exercised directly.
func writePersonalRootAgent(t *testing.T, projectID, body string) {
	t.Helper()
	path, err := config.ProjectConfigTomlPath(projectID)
	if err != nil {
		t.Fatalf("ProjectConfigTomlPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir personal config dir: %v", err)
	}
	content := "[root_agent]\n" + body + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write personal config: %v", err)
	}
}

// loadGlobalConfigWithRootAgent writes a global config.toml carrying a
// [root_agent] table and loads it through config.LoadConfig, so the loaded
// Config's source shape records that `enabled` was set — the presence the global
// layer extractor reads. AGENT_FACTORY_HOME must already point at a temp home.
func loadGlobalConfigWithRootAgent(t *testing.T, body string) *config.Config {
	t.Helper()
	dir, err := config.GetConfigDir()
	if err != nil {
		t.Fatalf("GetConfigDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir AF home: %v", err)
	}
	content := "[root_agent]\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, config.TomlConfigFileName), []byte(content), 0o600); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}
	cfg, err := config.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}

// repoID resolves repoPath to its daemon repo ID.
func repoID(t *testing.T, repoPath string) string {
	t.Helper()
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath(%s): %v", repoPath, err)
	}
	return repo.ID
}

// TestEnsureRootAgentsSingletonOnlyProjectMaterializes: a registered project
// enabled purely by its personal [root_agent] — with NO legacy root_agents entry
// — is now visited by the ensure loop (the old early-return on an empty map
// skipped it entirely), gets a root created, and its resolved program flows
// through losslessly. This is the whole point of PR2's enumeration rewrite.
func TestEnsureRootAgentsSingletonOnlyProjectMaterializes(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	// A custom program doubles as the lossless-forward check: the resolved
	// profile's program must reach CreateSession verbatim.
	writePersonalRootAgent(t, project.ID, "enabled = true\nprogram = \"/opt/claude --model opus\"")

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	manager.EnsureRootAgents()

	if len(*seen) != 1 {
		t.Fatalf("a singleton-only project must materialize a root, got %d creates", len(*seen))
	}
	opts := (*seen)[0]
	if opts.Title != session.RootSessionTitle || !opts.InPlace {
		t.Fatalf("root must be the reserved title created in place, got title=%q inPlace=%v", opts.Title, opts.InPlace)
	}
	if opts.Program != "/opt/claude --model opus" {
		t.Fatalf("the resolved personal program must reach CreateSession verbatim, got %q", opts.Program)
	}
	if findRootInstance(t, manager, repoPath) == nil {
		t.Fatalf("root instance not registered for the singleton-only project")
	}
	if !manager.repoRootAgentWillMaterialize(repoID(t, repoPath)) {
		t.Fatalf("repoRootAgentWillMaterialize must be true for a singleton-only project the loop creates — else a send-prompt to it is wrongly rejected (#1835)")
	}
}

// TestEnsureRootAgentsGlobalEnablesRegisteredProject: a global [root_agent]
// enabled=true fans out to a registered project with no legacy entry and no
// personal override — the global default's registered-project-only reach.
func TestEnsureRootAgentsGlobalEnablesRegisteredProject(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	registerTestProject(t, repoPath)
	cfg := loadGlobalConfigWithRootAgent(t, "enabled = true")

	manager, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	manager.EnsureRootAgents()

	if len(*seen) != 1 {
		t.Fatalf("a global enabled=true must fan out to the registered project, got %d creates", len(*seen))
	}
	if findRootInstance(t, manager, repoPath) == nil {
		t.Fatalf("root instance not registered for the globally-enabled project")
	}
	if !manager.repoRootAgentWillMaterialize(repoID(t, repoPath)) {
		t.Fatalf("repoRootAgentWillMaterialize must be true for a globally-enabled registered project")
	}
}

// TestEnsureRootAgentsPersonalDisablesLegacyRoot is THE case #2216 exists for: a
// registered project carries a legacy root_agents entry (which alone means
// enabled=true, program unset — the empty entry af writes for every registered
// project) AND a personal enabled=false. The personal disable must win, so NO
// root is created and repoRootAgentWillMaterialize reports false. With legacy
// outranking personal this would be an unbreakable always-on root — the silent
// no-op this epic kills.
func TestEnsureRootAgentsPersonalDisablesLegacyRoot(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	writePersonalRootAgent(t, project.ID, "enabled = false")

	// Legacy entry present for the same repo (the ubiquitous empty entry).
	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	manager.EnsureRootAgents()

	if len(*seen) != 0 {
		t.Fatalf("a personal enabled=false must disable a legacy-enabled root, got %d creates", len(*seen))
	}
	if findRootInstance(t, manager, repoPath) != nil {
		t.Fatalf("no root instance may exist for a personal-disabled project")
	}
	if manager.repoRootAgentWillMaterialize(repoID(t, repoPath)) {
		t.Fatalf("repoRootAgentWillMaterialize must be false for a personal-disabled root — a send-prompt must not wait for a root that will never come")
	}
}

// TestEnsureRootAgentsLegacyOnlyUnchangedThroughResolver: a plain legacy entry
// with no project registration and no global/personal layer still ensures a
// root, exactly as before PR2 — proving the resolver rewrite preserved the
// legacy contract end to end (create AND willMaterialize).
func TestEnsureRootAgentsLegacyOnlyUnchangedThroughResolver(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	manager.EnsureRootAgents()

	if len(*seen) != 1 {
		t.Fatalf("a legacy entry must still ensure a root through the resolver, got %d creates", len(*seen))
	}
	if !manager.repoRootAgentWillMaterialize(repoID(t, repoPath)) {
		t.Fatalf("repoRootAgentWillMaterialize must be true for a legacy-enabled root")
	}
}

// TestRootAgentEnsureAndWillMaterializeAgree drives the two call sites across
// every layer combination in ONE manager and asserts they never disagree: a
// root is created exactly when repoRootAgentWillMaterialize is true. Divergence
// is the specific trap this PR guards — a root created but not marked
// materializing (or vice versa) rejects a send-prompt at the reserved-name gate.
func TestRootAgentEnsureAndWillMaterializeAgree(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)

	// Four repos exercising each path. legacyOnly is not registered as a project;
	// the rest are registered so they carry personal layers.
	legacyOnly := setupControlRepo(t)
	personalOn := setupControlRepo(t)
	disabledLegacy := setupControlRepo(t)
	registeredIdle := setupControlRepo(t)

	pOn := registerTestProject(t, personalOn)
	pDisabled := registerTestProject(t, disabledLegacy)
	registerTestProject(t, registeredIdle) // registered, no override, no legacy → idle

	writePersonalRootAgent(t, pOn.ID, "enabled = true")
	writePersonalRootAgent(t, pDisabled.ID, "enabled = false")

	cfg := config.DefaultConfig()
	cfg.RootAgents = map[string]config.RootAgentConfig{
		legacyOnly:     {},
		disabledLegacy: {}, // legacy-enabled, but personal disables it
	}

	manager, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	manager.EnsureRootAgents()

	cases := []struct {
		name        string
		repoPath    string
		wantEnabled bool
	}{
		{"legacy only", legacyOnly, true},
		{"personal enabled, no legacy", personalOn, true},
		{"legacy + personal disabled", disabledLegacy, false},
		{"registered but unconfigured", registeredIdle, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			created := findRootInstance(t, manager, tc.repoPath) != nil
			willMaterialize := manager.repoRootAgentWillMaterialize(repoID(t, tc.repoPath))
			if created != willMaterialize {
				t.Fatalf("ensure loop and repoRootAgentWillMaterialize disagree: created=%v willMaterialize=%v", created, willMaterialize)
			}
			if created != tc.wantEnabled {
				t.Fatalf("want root enabled=%v, got created=%v", tc.wantEnabled, created)
			}
		})
	}
}
